import React, { useCallback, useEffect, useId, useRef, useState } from "react";
import { Badge } from "@astryxdesign/core/Badge";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Field } from "@astryxdesign/core/Field";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { Text } from "@astryxdesign/core/Text";
import { VStack } from "@astryxdesign/core/VStack";
import type { EvidenceSpanV1, KnowledgeDiscoveryInventoryPageV1, KnowledgeGraphV1, KnowledgeInventoryV1, KnowledgeReadV1, KnowledgeRouteSummaryV1, KnowledgeSnapshotV1 } from "../../contracts/typescript/oee";
import { AmbiguousError, ApiError, Intents, allPages, classify, mutate, request } from "./api";
import { entityTypes, literalText, predicates } from "./proposed-facts";
import { Announce, Problem, Time, canSupervise, short, useSession } from "./ui";

type Graph = NonNullable<KnowledgeReadV1["graph"]>;
type Claim = Graph["claims"][number];
type Entity = Graph["entities"][number];

// Predicates read from a process's materials, one hop out: suppliers, where material is received
// and stored, where it is checked, and who receives what is shipped.
const MATERIAL_PREDICATES = ["supplied_by", "received_at", "stored_at", "inspected_at", "ships_to", "lead_time_days", "inspection_method", "sample_size"];

// The stages of the view, each the predicates that show it, in the order material flows.
const STAGES: [title: string, predicates: string[]][] = [
    ["Suppliers", ["supplied_by", "lead_time_days"]],
    ["Receiving and storage", ["received_at", "stored_at"]],
    ["Routing", ["part_of", "precedes", "performs", "consumes", "produces"]],
    ["Times", ["cycle_time_seconds", "setup_time_seconds", "batch_size", "yield_ratio", "capacity_per_hour"]],
    ["Quality", ["inspected_at", "inspection_method", "sample_size"]],
    ["Dispatch", ["ships_to"]],
    ["Responsibility", ["responsible_role", "escalation_after_seconds"]],
];

// Why a process has no approved view yet, in words (KnowledgeReadV1 reason codes).
const notReady: Record<string, string> = {
    NO_SNAPSHOT: "No approved snapshot of the process knowledge has been sealed yet. A reviewer seals one once facts are approved.",
    ENTITY_NOT_IN_SNAPSHOT: "This process is not in the latest approved snapshot yet. Approve facts about it and seal a new snapshot.",
    SNAPSHOT_STALE: "The latest approved snapshot is out of date: knowledge changed after it was sealed. A reviewer seals a new one.",
    SNAPSHOT_UNAVAILABLE: "The latest approved snapshot cites a source that has since been revoked, so it cannot be shown.",
};

// ProcessGraph (WIS-618) shows what the latest approved snapshot says about one process, from
// suppliers to dispatch. It is read-only and shows only approved, snapshotted facts; each fact
// opens the passages it cites.
export function ProcessGraph() {
    const { site } = useSession();
    const [processes, setProcesses] = useState<{ id: string; name: string }[] | null>(null);
    const [selected, setSelected] = useState("");
    const [error, setError] = useState<unknown>(null);
    const [sealed, setSealed] = useState(0);
    const load = useCallback(async () => {
        setError(null);
        try {
            const all = await allPages(async (cursor) => {
                const page = await request<KnowledgeDiscoveryInventoryPageV1>(`/v1/sites/${site.id}/discovery-inventories?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
                return { items: page.inventories, next_cursor: page.next_cursor };
            });
            const seen = new Map<string, string>();
            for (const inventory of all) if (!seen.has(inventory.process_id)) seen.set(inventory.process_id, inventory.process_display_name);
            const list = [...seen].map(([id, name]) => ({ id, name }));
            setProcesses(list);
            if (list.length === 1) setSelected(list[0].id);
        } catch (e) {
            setError(e);
        }
    }, [site.id]);
    useEffect(() => {
        void load();
    }, [load]);

    const pickerID = useId();
    return (
        <section aria-labelledby="graph-title">
            <VStack gap={3}>
                <Heading level={1} id="graph-title">
                    Approved process
                </Heading>
                <PublishSnapshot onPublished={() => setSealed((n) => n + 1)} />
                {error !== null && <Problem error={error} />}
                <div role="status">
                    <Announce text={processes === null && error === null ? "Loading processes…" : ""} />
                </div>
                {processes === null && error === null && <Skeleton height={40} width={320} />}
                {processes?.length === 0 && <Text as="p" color="secondary">No process is registered for discovery at this site yet.</Text>}
                {processes && processes.length > 0 && (
                    <Field label="Process" inputID={pickerID} width={320}>
                        <select id={pickerID} value={selected} onChange={(e) => setSelected(e.currentTarget.value)}>
                            <option value="">Select a process</option>
                            {processes.map((p) => (
                                <option key={p.id} value={p.id}>
                                    {p.name}
                                </option>
                            ))}
                        </select>
                    </Field>
                )}
                {selected && <ProcessView key={`${selected}:${sealed}`} processID={selected} />}
            </VStack>
        </section>
    );
}

// PublishSnapshot (WIS-641) seals the site's approved knowledge into a new immutable snapshot, the
// only content chat, shift suggestions and the advisor read. A snapshot covers the whole site, not
// one process. The server allows owners and supervisors with a recent sign-in (428 otherwise), takes
// the knowledge inventory revision just read as If-Match and refuses a stale one (412), which
// reloads it here. The snapshot ID and the idempotency key are kept while the outcome is unknown, so
// a retry after a lost connection replays instead of sealing a second snapshot.
function PublishSnapshot({ onPublished }: { onPublished: () => void }) {
    const { site } = useSession();
    const [inventory, setInventory] = useState<number | null>(null);
    const [reason, setReason] = useState("");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const [done, setDone] = useState<KnowledgeSnapshotV1 | null>(null);
    const intents = useRef(new Intents());
    const pending = useRef<{ reason: string; revision: number; id: string } | null>(null);
    const loadInventory = useCallback(async () => {
        try {
            setInventory((await request<KnowledgeInventoryV1>(`/v1/sites/${site.id}/knowledge-inventory`)).revision);
        } catch (e) {
            setError(e);
        }
    }, [site.id]);
    useEffect(() => {
        if (canSupervise(site.role)) void loadInventory();
    }, [loadInventory, site.role]);

    const reasonID = useId();
    if (!canSupervise(site.role)) return <Text as="p" color="secondary">An owner or supervisor publishes approved knowledge as a snapshot.</Text>;

    async function publish(e: React.FormEvent) {
        e.preventDefault();
        const text = reason.trim();
        if (inventory === null || !text || busy) return;
        const prior = pending.current;
        const intent = prior && prior.reason === text && prior.revision === inventory ? prior : { reason: text, revision: inventory, id: crypto.randomUUID() };
        pending.current = intent;
        setBusy(true);
        setError(null);
        setDone(null);
        try {
            const out = await mutate<KnowledgeSnapshotV1>(intents.current, "snapshot", `/v1/sites/${site.id}/process-snapshots`, { snapshot_id: intent.id, reason: text }, inventory);
            pending.current = null;
            setDone(out);
            setReason("");
            onPublished();
            await loadInventory();
        } catch (err) {
            if (!(err instanceof AmbiguousError || (err instanceof ApiError && classify(err.problem) === "retry"))) pending.current = null;
            setError(err);
            if (err instanceof ApiError && classify(err.problem) === "conflict") await loadInventory();
        } finally {
            setBusy(false);
        }
    }

    return (
        <Card padding={4}>
            <form aria-label="Publish approved knowledge" onSubmit={(e) => void publish(e)}>
                <VStack gap={2}>
                    <HStack gap={2} align="center" wrap="wrap">
                        <Heading level={2}>Publish approved knowledge</Heading>
                        {inventory !== null && <Badge variant="neutral" label={`Knowledge revision ${inventory}.`} />}
                    </HStack>
                    <Text as="p" size="sm" color="secondary">
                        Seals every approved fact at this site. Chat and shift suggestions read only the latest snapshot.
                    </Text>
                    <HStack gap={2} align="end" wrap="wrap">
                        <Field label="Snapshot reason" inputID={reasonID} width={360}>
                            <input id={reasonID} required maxLength={1000} value={reason} onChange={(e) => setReason(e.currentTarget.value)} />
                        </Field>
                        <Button type="submit" variant="primary" label={busy ? "Publishing…" : "Publish snapshot"} isDisabled={busy || inventory === null || !reason.trim()} />
                    </HStack>
                    {done && (
                        <p role="status">
                            Snapshot {short(done.snapshot_id)} published at knowledge revision {done.inventory_revision}.
                        </p>
                    )}
                    {error !== null && <Problem error={error} />}
                </VStack>
            </form>
        </Card>
    );
}

function ProcessView({ processID }: { processID: string }) {
    const { site } = useSession();
    const [read, setRead] = useState<KnowledgeReadV1 | null>(null);
    const [graph, setGraph] = useState<{ entities: Map<string, Entity>; claims: Claim[]; truncated: boolean } | null>(null);
    const [error, setError] = useState<unknown>(null);
    useEffect(() => {
        let live = true;
        (async () => {
            const base = `/v1/sites/${site.id}`;
            const head = await request<KnowledgeReadV1>(`${base}/knowledge?entity_id=${processID}&depth=2`);
            if (!live) return;
            setRead(head);
            if (head.readiness !== "READY" || !head.graph || !head.snapshot_id) return;
            const entities = new Map(head.graph.entities.map((e) => [e.entity_id, e]));
            const claims = new Map(head.graph.claims.map((c) => [c.claim_id, c]));
            let truncated = head.graph.truncated;
            // Suppliers, locations and customers sit three hops from the process: read them from its
            // materials in the same snapshot, at the same instant.
            const materials = head.graph.entities.filter((e) => e.type === "material").map((e) => e.entity_id);
            for (let i = 0; i < materials.length; i += 20) {
                const query = new URLSearchParams({ as_of: head.as_of, hops: "1", direction: "outgoing" });
                for (const id of materials.slice(i, i + 20)) query.append("root", id);
                for (const p of MATERIAL_PREDICATES) query.append("predicate", p);
                const more = await request<KnowledgeGraphV1>(`${base}/process-snapshots/${head.snapshot_id}/graph?${query}`);
                for (const e of more.entities) entities.set(e.entity_id, e);
                for (const c of more.claims) claims.set(c.claim_id, c);
                truncated = truncated || more.truncated;
            }
            if (live) setGraph({ entities, claims: [...claims.values()], truncated });
        })().catch((e: unknown) => live && setError(e));
        return () => {
            live = false;
        };
    }, [processID, site.id]);

    if (error !== null) return <Problem error={error} />;
    if (read === null || (read.readiness === "READY" && graph === null))
        return (
            <div role="status" aria-label="Loading approved knowledge">
                <Skeleton height={240} />
            </div>
        );
    if (read.readiness !== "READY" || !graph)
        return (
            <div role="alert">
                <Banner status="warning" title={notReady[read.reason_code ?? ""] ?? `Not ready: ${read.reason_code ?? "unknown reason"}.`} />
            </div>
        );

    const name = (id?: string) => (id ? graph.entities.get(id)?.display_name ?? `entity ${short(id)}` : "");
    const kind = (id: string) => entityTypes[graph.entities.get(id)?.type ?? ""] ?? "";
    return (
        <VStack gap={3}>
            <HStack gap={2} align="center" wrap="wrap">
                <Text size="sm" color="secondary" hasTabularNumbers>
                    Snapshot {short(read.snapshot_id ?? "")} sealed <Time iso={read.captured_at} />
                </Text>
                {graph.truncated && <Badge variant="orange" label="more facts exist than can be shown at once" />}
            </HStack>
            <RouteSummary snapshotID={read.snapshot_id ?? ""} processID={processID} asOf={read.as_of} />
            <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(320px, 1fr))", gap: 12 }}>
                {STAGES.map(([title, wanted]) => {
                    const facts = graph.claims.filter((c) => wanted.includes(c.predicate)).sort((a, b) => name(a.subject_id).localeCompare(name(b.subject_id)));
                    return (
                        <section key={title} aria-label={title}>
                            <Card padding={4} height="100%">
                                <VStack gap={2}>
                                    <HStack gap={2} align="center">
                                        <Heading level={2}>{title}</Heading>
                                        <Badge variant={facts.length ? "green" : "neutral"} label={<>{facts.length}</>} />
                                    </HStack>
                                    {facts.length === 0 ? (
                                        <Text as="p" size="sm" color="secondary">No approved facts for this stage yet.</Text>
                                    ) : (
                                        <ul style={{ listStyle: "none", margin: 0, padding: 0, display: "grid", gap: 6 }}>
                                            {facts.map((c) => (
                                                <li key={c.claim_id}>
                                                    <Fact
                                                        text={`${kind(c.subject_id)} ${name(c.subject_id)} ${predicates[c.predicate] ?? c.predicate} ${c.object_entity_id ? `${kind(c.object_entity_id)} ${name(c.object_entity_id)}` : c.literal ? literalText(c.literal) : ""}`.trim()}
                                                        spans={c.source_span_ids}
                                                    />
                                                </li>
                                            ))}
                                        </ul>
                                    )}
                                </VStack>
                            </Card>
                        </section>
                    );
                })}
            </div>
        </VStack>
    );
}

// Fact is one approved fact as plain text; its cited passages load on demand.
function Fact({ text, spans }: { text: string; spans: readonly string[] }) {
    const { site } = useSession();
    const [passages, setPassages] = useState<EvidenceSpanV1[] | null>(null);
    const [error, setError] = useState<unknown>(null);
    const [open, setOpen] = useState(false);
    async function show() {
        setOpen(!open);
        if (open || passages) return;
        try {
            setPassages(await Promise.all(spans.map((id) => request<EvidenceSpanV1>(`/v1/sites/${site.id}/source-spans/${id}`))));
        } catch (e) {
            setError(e);
        }
    }
    return (
        <>
            <span>{text}</span>{" "}
            <Button label={open ? "Hide source" : "Show source"} variant="ghost" size="sm" aria-expanded={open} onClick={() => void show()} />
            {open && error !== null && <Problem error={error} context="The cited passage is unavailable." />}
            {open && passages && (
                <ul aria-label={`Sources of: ${text}`}>
                    {passages.map((p) => (
                        <li key={p.id}>
                            <pre className="passage">{p.text}</pre>
                        </li>
                    ))}
                </ul>
            )}
        </>
    );
}

// Why a route cannot be ordered (KnowledgeRouteSummaryV1 reason codes). Nothing is guessed.
const routeReason: Record<string, string> = {
    ROUTE_EMPTY: "No approved operation is part of this process yet.",
    ROUTE_INCOMPLETE: "The approved order has a gap: some operations are not linked to the next one.",
    ROUTE_BRANCHED: "The approved order branches: an operation has two next or two previous operations.",
    ROUTE_CYCLE: "The approved order loops back on itself.",
    ROUTE_LEAVES_PROCESS: "An approved order fact links an operation of this process to one outside it.",
};

type RouteValue = KnowledgeRouteSummaryV1["steps"][number]["cycle_time_seconds"];

function valueText(v: RouteValue) {
    if (v.status === "known" && v.value !== null) return `${v.value} ${v.unit}`;
    if (v.status === "conflict") return "approved values disagree";
    return "missing";
}

// RouteSummary (WIS-642) is the process route computed on the server from the same snapshot:
// operations in their approved order and exact totals. A total is shown only when every step
// has its value; a missing value is never read as zero.
function RouteSummary({ snapshotID, processID, asOf }: { snapshotID: string; processID: string; asOf: string }) {
    const { site } = useSession();
    const [route, setRoute] = useState<KnowledgeRouteSummaryV1 | null>(null);
    const [error, setError] = useState<unknown>(null);
    useEffect(() => {
        let live = true;
        const query = new URLSearchParams({ process_id: processID, as_of: asOf });
        request<KnowledgeRouteSummaryV1>(`/v1/sites/${site.id}/process-snapshots/${snapshotID}/route-summary?${query}`).then(
            (r) => live && setRoute(r),
            (e: unknown) => live && setError(e),
        );
        return () => {
            live = false;
        };
    }, [site.id, snapshotID, processID, asOf]);

    if (error !== null) return <Problem error={error} context="The route could not be computed." />;
    if (route === null)
        return (
            <div role="status" aria-label="Computing the route">
                <Skeleton height={120} />
            </div>
        );
    const ready = route.readiness === "READY";
    const total = (t: KnowledgeRouteSummaryV1["cycle_time_seconds"], what: string) =>
        t.value !== null ? `${t.value} ${t.unit}` : ready ? `not known: ${t.steps_missing} of ${route.step_count} steps have no approved ${what}` : "not computed";
    const lead = route.longest_lead_time_days;
    return (
        <section aria-label="Route">
            <Card padding={4}>
                <VStack gap={3}>
                    <Heading level={2}>Route</Heading>
                    <dl aria-label="Route totals" className="kpis" style={{ margin: 0 }}>
                        <Total label="Operations" known value={String(route.step_count)} />
                        <Total label="Total cycle time" known={route.cycle_time_seconds.value !== null} value={total(route.cycle_time_seconds, "cycle time")} />
                        <Total label="Total setup time" known={route.setup_time_seconds.value !== null} value={total(route.setup_time_seconds, "setup time")} />
                        <Total
                            label="Longest supplier lead time"
                            known={lead.value !== null}
                            value={lead.value !== null ? `${lead.value} ${lead.unit === "day" ? "days" : lead.unit}${lead.entity_name ? ` (${lead.entity_name})` : ""}` : ready ? "no approved lead time" : "not computed"}
                        >
                            {lead.materials_without_lead_time > 0 && <Text size="sm" color="secondary">{` · ${lead.materials_without_lead_time} of ${lead.materials} materials have no approved lead time`}</Text>}
                            {lead.source_span_ids.length > 0 && (
                                <>
                                    {" "}
                                    <Fact text="" spans={lead.source_span_ids} />
                                </>
                            )}
                        </Total>
                    </dl>
                    {ready ? (
                        <ol aria-label="Operations in order" style={{ margin: 0, paddingInlineStart: 20, display: "grid", gap: 4 }}>
                            {route.steps.map((step) => (
                                <li key={step.entity_id}>
                                    <strong>{step.display_name}</strong> · cycle {valueText(step.cycle_time_seconds)} · setup {valueText(step.setup_time_seconds)}
                                    {step.cycle_time_seconds.source_span_ids.length > 0 && (
                                        <>
                                            {" "}
                                            <Fact text="" spans={step.cycle_time_seconds.source_span_ids} />
                                        </>
                                    )}
                                </li>
                            ))}
                        </ol>
                    ) : (
                        <>
                            <div role="alert">
                                <Banner status="warning" title={route.reason_codes.map((code) => routeReason[code] ?? `Route not ready: ${code}.`).join(" ")} />
                            </div>
                            {route.steps.length > 0 && (
                                <ul aria-label="Operations, order unknown" style={{ margin: 0 }}>
                                    {route.steps.map((step) => (
                                        <li key={step.entity_id}>{step.display_name}</li>
                                    ))}
                                </ul>
                            )}
                        </>
                    )}
                </VStack>
            </Card>
        </section>
    );
}

// Total is one route total as a tile. A value the server could not compute stays words, never 0.
function Total({ label, known, value, children }: { label: string; known: boolean; value: string; children?: React.ReactNode }) {
    return (
        <Card padding={3} className="kpi">
            <dt>
                <Text size="sm" color="secondary">
                    {label}
                </Text>
            </dt>
            <dd style={{ margin: 0 }}>
                {known ? <strong>{value}</strong> : <Text size="sm">{value}</Text>}
                {children}
            </dd>
        </Card>
    );
}
