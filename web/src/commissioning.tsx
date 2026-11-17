import React, {
    useCallback,
    useEffect,
    useId,
    useMemo,
    useRef,
    useState,
} from "react";
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
import type {
    EvidenceSourcePageV1,
    EvidenceSourceV1,
    KnowledgeDiscoveryInventoryPageV1,
    KnowledgeDiscoveryInventoryV1,
    KnowledgeEntityPageV1,
    KnowledgeEntityV1,
} from "../../contracts/typescript/oee";
import { Intents, allPages, isConflict, mutate, request } from "./api";
import { Problem, Val, clearDraft, loadDraft, saveDraft, short, useSession } from "./ui";

// tick sizes a native checkbox or radio; the legacy input rule gives every input a 44 px touch height.
const tick = { minHeight: 0, width: 18, height: 18, padding: 0, margin: 0 } as const;

type Entity = KnowledgeEntityPageV1["entities"][number];
type Inventory = KnowledgeDiscoveryInventoryPageV1["inventories"][number];
type Source = EvidenceSourcePageV1["sources"][number];
type Kind = "area" | "process";

const NOTE = "note"; // the "write a commissioning note" choice in the source picker
const VALUE_STREAM = "process_understanding";

type Loaded = {
    entities: Entity[];
    inventories: Inventory[];
    sources: Source[];
};

// A pending add keeps its generated IDs until the server confirms it, so a retry after a lost
// answer sends the same IDs and Idempotency-Keys (rule 07) instead of creating a second entity.
type PendingEntity = {
    form: string;
    entityID: string;
    noteID: string;
    note?: { id: string; revision: number };
};
type PendingInventory = { form: string; inventoryID: string };

// ProcessCommissioning (WIS-644) is where an owner registers what interviews are about: the
// areas and processes of the site, each citing a source, and the value-stream checklist of a
// process. A model never proposes these (rule 11). The server lets supervisors and operators add
// entities too, but commissioning is an owner's decision here; everyone else reads. Success is
// shown only from the server's receipt; a 412 or 409 reloads the lists, a 428 asks the owner to
// confirm their identity (the checklist needs a recent sign-in), and a lost answer keeps the
// same keys for the retry.
export function ProcessCommissioning() {
    const { site, online } = useSession();
    const base = `/v1/sites/${site.id}`;
    const owner = site.role === "owner";
    const [data, setData] = useState<Loaded | null>(null);
    const [loadError, setLoadError] = useState<unknown>(null);
    const intents = useMemo(() => new Intents(), []);

    const load = useCallback(async () => {
        setLoadError(null);
        try {
            const page = <T,>(
                path: string,
                pick: (p: T) => readonly unknown[],
            ) =>
                allPages(async (cursor) => {
                    const p = await request<T & { next_cursor?: string }>(
                        `${base}${path}?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`,
                    );
                    return { items: pick(p), next_cursor: p.next_cursor };
                });
            const [entities, inventories, sources] = await Promise.all([
                page<KnowledgeEntityPageV1>(
                    "/entities",
                    (p) => p.entities,
                ) as Promise<Entity[]>,
                page<KnowledgeDiscoveryInventoryPageV1>(
                    "/discovery-inventories",
                    (p) => p.inventories,
                ) as Promise<Inventory[]>,
                owner
                    ? (page<EvidenceSourcePageV1>(
                          "/sources",
                          (p) => p.sources,
                      ) as Promise<Source[]>)
                    : Promise.resolve([] as Source[]),
            ]);
            setData({
                entities: entities.filter(
                    (e) => e.type === "area" || e.type === "process",
                ),
                inventories,
                sources,
            });
        } catch (e) {
            setLoadError(e);
        }
    }, [base, owner]);
    useEffect(() => {
        void load();
    }, [load]);

    const areas = data?.entities.filter((e) => e.type === "area") ?? [];
    const processes = data?.entities.filter((e) => e.type === "process") ?? [];
    const checklists = (processID: string) =>
        data?.inventories.filter((i) => i.process_id === processID) ?? [];

    const started = (processID: string) =>
        checklists(processID).some((i) => i.capability === VALUE_STREAM);
    return (
        <section aria-labelledby="commissioning-title">
            <VStack gap={3}>
                <Heading level={1} id="commissioning-title">
                    Processes
                </Heading>
                <div className="kpis">
                    <Kpi label="Areas" value={data && areas.length} />
                    <Kpi label="Processes" value={data && processes.length} />
                    <Kpi
                        label="Checklists started"
                        value={
                            data &&
                            processes.filter((p) => started(p.entity_id)).length
                        }
                    />
                </div>
                {loadError !== null && (
                    <Problem
                        error={loadError}
                        context="The site's areas and processes could not be loaded."
                    />
                )}
                {data === null && loadError === null && (
                    <Skeleton height={160} />
                )}
                {data && (
                    <>
                        {owner ? (
                            <HStack gap={3} wrap="wrap" align="start">
                                <AddEntity
                                    base={base}
                                    online={online}
                                    sources={data.sources}
                                    intents={intents}
                                    reload={load}
                                />
                                <StartChecklist
                                    base={base}
                                    online={online}
                                    areas={areas}
                                    processes={processes}
                                    inventories={data.inventories}
                                    intents={intents}
                                    reload={load}
                                />
                            </HStack>
                        ) : (
                            <Text as="p" color="secondary">
                                Only an owner of this site adds areas, processes
                                and checklists.
                            </Text>
                        )}
                        <Heading level={2}>Areas</Heading>
                        {areas.length === 0 ? (
                            <Text as="p" color="secondary">
                                No areas registered yet.
                            </Text>
                        ) : (
                            <EntityList label="Areas">
                                {areas.map((a) => (
                                    <EntityRow key={a.entity_id} entity={a} />
                                ))}
                            </EntityList>
                        )}
                        <Heading level={2}>Processes</Heading>
                        {processes.length === 0 ? (
                            <Text as="p" color="secondary">
                                No processes registered yet.
                            </Text>
                        ) : (
                            <EntityList label="Processes">
                                {processes.map((p) => (
                                    <EntityRow key={p.entity_id} entity={p}>
                                        {started(p.entity_id) ? (
                                            <Badge
                                                variant="green"
                                                label="✓ value-stream checklist started"
                                            />
                                        ) : (
                                            <Badge
                                                variant="neutral"
                                                label="no value-stream checklist yet"
                                            />
                                        )}
                                    </EntityRow>
                                ))}
                            </EntityList>
                        )}
                    </>
                )}
            </VStack>
        </section>
    );
}

function Kpi({ label, value }: { label: string; value: number | null }) {
    return (
        <Card padding={3} className="kpi">
            <Text size="sm" color="secondary">
                {label}
            </Text>
            <strong>
                <Val v={value} />
            </strong>
        </Card>
    );
}

function EntityList({
    label,
    children,
}: {
    label: string;
    children: React.ReactNode;
}) {
    return (
        <Card padding={0}>
            <ul aria-label={label} style={{ listStyle: "none", margin: 0, padding: 0 }}>
                {children}
            </ul>
        </Card>
    );
}

// EntityRow is one area or process: its name, then its revision and the source it cites.
function EntityRow({
    entity,
    children,
}: {
    entity: Entity;
    children?: React.ReactNode;
}) {
    return (
        <li
            aria-label={entity.display_name}
            style={{
                padding: "8px 12px",
                borderBottom: "1px solid var(--color-border)",
            }}
        >
            <HStack gap={2} align="center" wrap="wrap">
                <Text weight="semibold">{entity.display_name}</Text>
                <Text size="sm" color="secondary" hasTabularNumbers>
                    r{entity.current_revision} · cites {short(entity.source_id)} r
                    {entity.source_revision}
                </Text>
                {!entity.sources_current && (
                    <Badge variant="orange" label="source changed since" />
                )}
                {children}
            </HStack>
        </li>
    );
}

function AddEntity({
    base,
    online,
    sources,
    intents,
    reload,
}: {
    base: string;
    online: boolean;
    sources: Source[];
    intents: Intents;
    reload: () => Promise<void>;
}) {
    const [kind, setKind] = useState<Kind>("process");
    const [name, setName] = useState("");
    const [cite, setCite] = useState("");
    const [note, setNote] = useState("");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const [done, setDone] = useState("");
    const pending = useRef<PendingEntity | null>(null);
    const source = sources.find((s) => s.source_id === cite);
    const ready =
        name.trim() !== "" &&
        (cite === NOTE ? note.trim() !== "" : source !== undefined);

    async function add(e: React.FormEvent) {
        e.preventDefault();
        if (!ready || busy || !online) return;
        const form = JSON.stringify({
            kind,
            name: name.trim(),
            cite,
            note: cite === NOTE ? note.trim() : "",
            revision: source?.revision ?? 0,
        });
        if (pending.current?.form !== form)
            pending.current = {
                form,
                entityID: crypto.randomUUID(),
                noteID: crypto.randomUUID(),
            };
        const p = pending.current;
        setBusy(true);
        setError(null);
        setDone("");
        try {
            let cited = source
                ? { id: source.source_id, revision: source.revision }
                : p.note;
            if (cite === NOTE && !p.note) {
                const s = await mutate<EvidenceSourceV1>(
                    intents,
                    `note:${p.noteID}`,
                    `${base}/sources`,
                    {
                        source_id: p.noteID,
                        kind: "manual",
                        title: `Commissioning note: ${name.trim()}`.slice(
                            0,
                            200,
                        ),
                        text: note.trim(),
                    },
                    0,
                );
                p.note = { id: s.source_id, revision: s.revision };
                cited = p.note;
            }
            if (!cited) return;
            const v = await mutate<KnowledgeEntityV1>(
                intents,
                `entity:${p.entityID}`,
                `${base}/entities`,
                {
                    entity_id: p.entityID,
                    type: kind,
                    display_name: name.trim(),
                    source_id: cited.id,
                    source_revision: cited.revision,
                },
                0,
            );
            pending.current = null;
            setName("");
            setNote("");
            setDone(
                `${v.type === "area" ? "Area" : "Process"} "${v.display_name}" added (revision ${v.revision}).`,
            );
            await reload();
        } catch (err) {
            setError(err);
            // A changed source or a taken ID: show what is current now and let the owner pick again.
            if (isConflict(err)) {
                pending.current = null;
                await reload();
            }
        } finally {
            setBusy(false);
        }
    }

    const id = useId();
    return (
        <Card padding={4} width={420} maxWidth="100%">
            <form aria-label="Add an area or process" onSubmit={add}>
                <VStack gap={3}>
                    <Heading level={2}>Add an area or process</Heading>
                    <fieldset style={{ border: 0, margin: 0, padding: 0 }}>
                        <legend>Type</legend>
                        <HStack gap={3}>
                            {(["area", "process"] as const).map((k) => (
                                <label key={k} style={{ display: "flex", alignItems: "center", gap: 8 }}>
                                    <input
                                        type="radio"
                                        style={tick}
                                        name="commission-kind"
                                        value={k}
                                        checked={kind === k}
                                        onChange={() => setKind(k)}
                                    />{" "}
                                    {k === "area" ? "Area" : "Process"}
                                </label>
                            ))}
                        </HStack>
                    </fieldset>
                    <Field label="Name" inputID={`${id}-name`}>
                        <input
                            id={`${id}-name`}
                            required
                            maxLength={200}
                            value={name}
                            onChange={(e) => setName(e.currentTarget.value)}
                        />
                    </Field>
                    <Field label="Cites" inputID={`${id}-cites`}>
                        <select
                            id={`${id}-cites`}
                            required
                            value={cite}
                            onChange={(e) => setCite(e.currentTarget.value)}
                        >
                            <option value="">Choose a source…</option>
                            {sources.map((s) => (
                                <option key={s.source_id} value={s.source_id}>
                                    {s.title} · {s.kind} · r{s.revision}
                                </option>
                            ))}
                            <option value={NOTE}>
                                A commissioning note I write now
                            </option>
                        </select>
                    </Field>
                    {cite === NOTE && (
                        <Field label="Commissioning note" inputID={`${id}-note`}>
                            <textarea
                                id={`${id}-note`}
                                required
                                maxLength={4000}
                                rows={3}
                                style={{ width: "100%", padding: 8, font: "inherit", color: "inherit", background: "transparent", border: "1px solid var(--color-border)", borderRadius: 8 }}
                                value={note}
                                onChange={(e) => setNote(e.currentTarget.value)}
                            />
                        </Field>
                    )}
                    <div>
                        <Button
                            type="submit"
                            variant="primary"
                            label={
                                busy
                                    ? "Adding…"
                                    : kind === "area"
                                      ? "Add area"
                                      : "Add process"
                            }
                            isDisabled={busy || !online || !ready}
                        />
                    </div>
                    {done && <p role="status">{done}</p>}
                    <Problem
                        error={error}
                        context="The area or process was not added."
                    />
                </VStack>
            </form>
        </Card>
    );
}

function StartChecklist({
    base,
    online,
    areas,
    processes,
    inventories,
    intents,
    reload,
}: {
    base: string;
    online: boolean;
    areas: Entity[];
    processes: Entity[];
    inventories: Inventory[];
    intents: Intents;
    reload: () => Promise<void>;
}) {
    const draftKey = `checklist:${base}`;
    const [processID, setProcessID] = useState(() => loadDraft<{ processID: string; areaID: string; reason: string }>(draftKey)?.processID ?? "");
    const [areaID, setAreaID] = useState(() => loadDraft<{ processID: string; areaID: string; reason: string }>(draftKey)?.areaID ?? "");
    const [reason, setReason] = useState(() => loadDraft<{ processID: string; areaID: string; reason: string }>(draftKey)?.reason ?? "");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const [done, setDone] = useState("");
    const pending = useRef<PendingInventory | null>(null);

    useEffect(() => {
        if (processID || areaID || reason.trim()) {
            saveDraft(draftKey, { processID, areaID, reason });
        } else {
            clearDraft(draftKey);
        }
    }, [processID, areaID, reason, draftKey]);
    const process = processes.find((p) => p.entity_id === processID);
    const area = areas.find((a) => a.entity_id === areaID);
    const started = inventories.some(
        (i) => i.process_id === processID && i.capability === VALUE_STREAM,
    );
    const ready =
        process !== undefined &&
        area !== undefined &&
        reason.trim() !== "" &&
        !started;

    async function start(e: React.FormEvent) {
        e.preventDefault();
        if (!ready || busy || !online || !process || !area) return;
        const body = {
            area_id: area.entity_id,
            area_revision: area.current_revision,
            process_id: process.entity_id,
            process_revision: process.current_revision,
            capability: VALUE_STREAM,
            reason: reason.trim(),
        };
        const form = JSON.stringify(body);
        if (pending.current?.form !== form)
            pending.current = { form, inventoryID: crypto.randomUUID() };
        const p = pending.current;
        setBusy(true);
        setError(null);
        setDone("");
        try {
            const v = await mutate<KnowledgeDiscoveryInventoryV1>(
                intents,
                `inventory:${p.inventoryID}`,
                `${base}/discovery-inventories`,
                { inventory_id: p.inventoryID, ...body },
                0,
            );
            pending.current = null;
            clearDraft(draftKey);
            setReason("");
            setDone(
                `Value-stream checklist started for "${process.display_name}" with ${v.requirements.length} questions to cover.`,
            );
            await reload();
        } catch (err) {
            setError(err);
            if (isConflict(err)) {
                pending.current = null;
                await reload();
            }
        } finally {
            setBusy(false);
        }
    }

    const id = useId();
    return (
        <Card padding={4} width={420} maxWidth="100%">
            <form aria-label="Start a value-stream checklist" onSubmit={start}>
                <VStack gap={3}>
                    <Heading level={2}>Start a value-stream checklist</Heading>
                    <Field label="Process" inputID={`${id}-process`}>
                        <select
                            id={`${id}-process`}
                            required
                            value={processID}
                            onChange={(e) => setProcessID(e.currentTarget.value)}
                        >
                            <option value="">Choose a process…</option>
                            {processes.map((p) => (
                                <option key={p.entity_id} value={p.entity_id}>
                                    {p.display_name}
                                </option>
                            ))}
                        </select>
                    </Field>
                    <Field label="Area" inputID={`${id}-area`}>
                        <select
                            id={`${id}-area`}
                            required
                            value={areaID}
                            onChange={(e) => setAreaID(e.currentTarget.value)}
                        >
                            <option value="">Choose an area…</option>
                            {areas.map((a) => (
                                <option key={a.entity_id} value={a.entity_id}>
                                    {a.display_name}
                                </option>
                            ))}
                        </select>
                    </Field>
                    <Field label="Reason" inputID={`${id}-reason`}>
                        <input
                            id={`${id}-reason`}
                            required
                            maxLength={1000}
                            value={reason}
                            onChange={(e) => setReason(e.currentTarget.value)}
                        />
                    </Field>
                    {started && (
                        <Text as="p" size="sm" color="secondary">
                            This process already has a value-stream checklist;
                            its interviews use that one.
                        </Text>
                    )}
                    <HStack gap={2} align="center" wrap="wrap">
                        <Button
                            type="submit"
                            variant="primary"
                            label={busy ? "Starting…" : "Start checklist"}
                            isDisabled={busy || !online || !ready}
                        />
                        <Text size="sm" color="secondary">
                            Needs a recent sign-in.
                        </Text>
                    </HStack>
                    {done && <p role="status">{done}</p>}
                    <Problem error={error} context="The checklist was not started." />
                </VStack>
            </form>
        </Card>
    );
}
