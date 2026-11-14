import { useCallback, useEffect, useRef, useState } from "react";
import type { EvidenceAuthorityV1, EvidenceCurrentnessV1, EvidenceSourcePageV1, EvidenceSourceV1 } from "../../contracts/typescript/oee";
import { ApiError, Intents, classify, mutate, request } from "./api";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Token } from "@astryxdesign/core/Token";
import { VStack } from "@astryxdesign/core/VStack";
import { Problem, Time, short, useSession } from "./ui";

// The latest decision of a kind, or none yet. A 404 on a decision read of a readable source means
// no decision has been recorded; any other failure stays an error, never "none".
async function latest<T>(path: string): Promise<T | null> {
    try {
        return await request<T>(path);
    } catch (e) {
        if (e instanceof ApiError && e.problem.status === 404) return null;
        throw e;
    }
}

type State = { source: EvidenceSourceV1; authority: EvidenceAuthorityV1 | null; currentness: EvidenceCurrentnessV1 | null };

// SourceAuthority (WIS-641) shows whether a document is the site's authoritative, current source,
// and lets the site owner decide it: designate (or withdraw) authority, then confirm the designated
// revision is current. A claim can be approved only against a cited source the owner designated
// (REVIEW_BLOCKED otherwise), so this is the step between uploading an SOP and reviewing its facts.
// Both decisions are owner-only on the server (evidence.Service begin with revoke=true) and need a
// recent sign-in (428). Every decision binds the source revision and state revision just read and
// the decision head as If-Match; a 409 SOURCE_CHANGED or 412 reloads and asks the owner to look
// again. What is shown as decided comes only from the server's receipt and the reread that follows.
export function SourceAuthority({ sourceID }: { sourceID: string }) {
    const { site } = useSession();
    const owner = site.role === "owner";
    const base = `/v1/sites/${site.id}/sources/${sourceID}`;
    const [state, setState] = useState<State | null>(null);
    const [loadError, setLoadError] = useState<unknown>(null);
    const [error, setError] = useState<unknown>(null);
    const [reason, setReason] = useState("");
    const [busy, setBusy] = useState(false);
    const [done, setDone] = useState("");
    // WIS-625: designating a model-read (OCR) source needs the owner's explicit check; never implied.
    // The check is of one revision's text: a reload to another revision (409 SOURCE_CHANGED) clears it.
    const [checkedFor, setCheckedFor] = useState<number | null>(null);
    const checked = state !== null && checkedFor === state.source.revision;
    const intents = useRef(new Intents());

    const load = useCallback(async () => {
        setLoadError(null);
        try {
            const source = await request<EvidenceSourceV1>(base);
            const [authority, currentness] = await Promise.all([latest<EvidenceAuthorityV1>(`${base}/authority`), latest<EvidenceCurrentnessV1>(`${base}/currentness`)]);
            setState({ source, authority, currentness });
        } catch (e) {
            setLoadError(e);
        }
    }, [base]);
    useEffect(() => {
        void load();
    }, [load]);

    async function decide(kind: "designate" | "withdraw" | "confirm_current") {
        if (!state || !reason.trim()) return;
        const { source, authority, currentness } = state;
        setBusy(true);
        setError(null);
        setDone("");
        try {
            if (kind === "confirm_current") {
                if (!authority) return;
                const body = {
                    source_revision: source.revision,
                    expected_source_state_revision: source.state_revision,
                    authority_revision: authority.revision,
                    authority_decision_id: authority.decision_id,
                    verdict: "confirm_current",
                    reason: reason.trim(),
                };
                const out = await mutate<EvidenceCurrentnessV1>(intents.current, "currentness", `${base}/currentness-decisions`, body, currentness?.current_revision ?? 0);
                setDone(`Confirmed current. Decision ${short(out.verification_id)} recorded.`);
            } else {
                const body = {
                    source_revision: source.revision,
                    expected_source_state_revision: source.state_revision,
                    verdict: kind,
                    reason: reason.trim(),
                    ...(kind === "designate" && source.extraction ? { confirm_model_extracted: checked } : {}),
                };
                const out = await mutate<EvidenceAuthorityV1>(intents.current, "authority", `${base}/authority-decisions`, body, authority?.current_revision ?? 0);
                setDone(`${kind === "designate" ? "Marked authoritative" : "Authority withdrawn"}. Decision ${short(out.decision_id)} recorded.`);
            }
            setReason("");
            setCheckedFor(null);
            await load();
        } catch (e) {
            setError(e);
            // Someone else decided, or the source changed: show what is current now.
            if (e instanceof ApiError && classify(e.problem) === "conflict") await load();
        } finally {
            setBusy(false);
        }
    }

    if (loadError !== null) return <Problem error={loadError} context="The document's authority could not be read." />;
    if (!state) return <Text color="secondary">Loading authority…</Text>;
    const { source, authority, currentness } = state;
    const modelRead = source.extraction !== undefined;
    const designated = authority?.verdict === "designate";
    const eligible = designated && authority.review_evidence_eligible;
    const current = eligible && currentness?.verdict === "confirm_current" && currentness.verification_current && currentness.authority_decision_id === authority.decision_id;
    return (
        <Card padding={3} role="group" aria-label={`Authority of ${source.title}`}>
            <VStack gap={2}>
                <HStack gap={1} align="center" wrap="wrap">
                    <Text weight="semibold">{eligible ? "Authoritative" : designated ? "Authoritative for an earlier revision" : "Not authoritative"}</Text>
                    {authority && (
                        <Text size="sm" color="secondary">
                            · {authority.verdict === "designate" ? "designated" : "withdrawn"} <Time iso={authority.created_at} /> for revision {authority.source_revision}
                        </Text>
                    )}
                </HStack>
                <HStack gap={1} align="center" wrap="wrap">
                    <Text weight="semibold">{current ? "Confirmed current" : "Not confirmed current"}</Text>
                    {current && currentness?.verified_at && (
                        <Text size="sm" color="secondary">
                            · <Time iso={currentness.verified_at} />
                        </Text>
                    )}
                </HStack>
                {modelRead && (
                    <div role="note">
                        <Banner
                            status="warning"
                            title="Text read by a model"
                            description={`(${source.extraction?.model}). It has lower authority than a document whose text was stored as written, and may contain reading errors.`}
                        />
                    </div>
                )}
                {owner ? (
                    <form
                        onSubmit={(e) => {
                            e.preventDefault();
                            if (!eligible) {
                                if (!modelRead || checked) void decide("designate");
                            } else if (!current) void decide("confirm_current");
                        }}
                    >
                        <VStack gap={2}>
                            <TextInput label="Decision reason" isRequired value={reason} {...{ maxLength: 1000 }} onChange={(v) => setReason(v)} />
                            {!eligible && modelRead && (
                                <>
                                    <figure style={{ margin: 0 }}>
                                        <figcaption>
                                            <Text size="sm" color="secondary">
                                                Text of revision {source.revision} as read by {source.extraction?.model}
                                            </Text>
                                        </figcaption>
                                        <pre tabIndex={0} aria-label={`Model-read text of ${source.title}`} style={{ maxHeight: "16rem", overflow: "auto", whiteSpace: "pre-wrap" }}>
                                            {source.text}
                                        </pre>
                                    </figure>
                                    <CheckboxInput
                                        label="I compared this text with the original page. Designating it lets this text support facts that are reviewed separately."
                                        value={checked}
                                        onChange={(on) => setCheckedFor(on ? source.revision : null)}
                                    />
                                </>
                            )}
                            <HStack gap={2} wrap="wrap">
                                {!eligible && <Button type="submit" variant="primary" label="Mark authoritative" isDisabled={busy || !reason.trim() || (modelRead && !checked)} />}
                                {eligible && !current && <Button type="submit" variant="primary" label="Confirm current" isDisabled={busy || !reason.trim()} />}
                                {designated && <Button label="Withdraw authority" isDisabled={busy || !reason.trim()} onClick={() => void decide("withdraw")} />}
                            </HStack>
                        </VStack>
                    </form>
                ) : (
                    <Text color="secondary">Only the site owner marks a document authoritative or confirms it is current.</Text>
                )}
                {done && (
                    <div role="status">
                        <Banner status="success" title={done} />
                    </div>
                )}
                {error !== null && <Problem error={error} />}
            </VStack>
        </Card>
    );
}

// SiteDocuments lists the site's document sources, 50 at a time in source order, so the owner can
// decide the authority of a document uploaded earlier. Interview answers and manual notes are
// sources too, but only documents are designated here. A change of refresh reloads the list in
// place, keeping an open authority panel and what the owner typed in it.
export function SiteDocuments({ refresh = 0 }: { refresh?: number }) {
    const { site } = useSession();
    const [docs, setDocs] = useState<EvidenceSourcePageV1["sources"][number][] | null>(null);
    const [next, setNext] = useState<string | undefined>();
    const [error, setError] = useState<unknown>(null);
    const [loading, setLoading] = useState(false);
    const [open, setOpen] = useState<string | null>(null);
    // Rows a refresh could not check (not a 404 or 403): kept, and checked again next time.
    const [retrying, setRetrying] = useState<ReadonlySet<string>>(new Set());
    // The cursors of the pages loaded so far ("" is the first), so a refresh reloads all of them.
    const cursors = useRef<string[]>([""]);
    // The rows shown now, for a refresh to compare against.
    const shown = useRef<EvidenceSourcePageV1["sources"][number][] | null>(null);
    shown.current = docs;

    const page = useCallback(
        async (cursor: string) => {
            const out = await request<EvidenceSourcePageV1>(`/v1/sites/${site.id}/sources?limit=50${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
            return { docs: out.sources.filter((s) => s.kind === "document"), next: out.next_cursor || undefined };
        },
        [site.id],
    );
    const load = useCallback(
        async (cursor?: string) => {
            setLoading(true);
            setError(null);
            try {
                const got = await page(cursor ?? "");
                if (cursor) cursors.current = [...cursors.current, cursor];
                else cursors.current = [""];
                setDocs((prior) => (cursor && prior ? [...prior, ...got.docs.filter((d) => !prior.some((p) => p.source_id === d.source_id))] : got.docs));
                setNext(got.next);
            } catch (e) {
                setError(e);
            } finally {
                setLoading(false);
            }
        },
        [page],
    );
    useEffect(() => {
        void load();
    }, [load]);
    // A refresh reloads every page loaded so far in place, keeping rows it no longer finds there: an
    // open authority panel (on any page) and what the owner typed in it stay.
    useEffect(() => {
        if (refresh === 0) return;
        let live = true;
        (async () => {
            try {
                const all: EvidenceSourcePageV1["sources"][number][] = [];
                let last: string | undefined;
                for (const cursor of cursors.current) {
                    const got = await page(cursor);
                    all.push(...got.docs.filter((d) => !all.some((p) => p.source_id === d.source_id)));
                    last = got.next;
                }
                // Rows are in source order, so a new document can push a shown row past the pages
                // reloaded. Such a row is read again by itself: it stays (with an open panel) only
                // while it is still a document readable here; a removed, revoked or hidden one goes,
                // and its panel closes.
                const missing = (shown.current ?? []).filter((p) => !all.some((d) => d.source_id === p.source_id));
                // Only a 404 or 403 (gone, revoked or not readable here) drops it; any other failure
                // keeps the row and its panel, marked to be checked again on the next refresh.
                const kept: typeof all = [];
                const unchecked = new Set<string>();
                for (const row of missing) {
                    try {
                        const now = await request<EvidenceSourceV1>(`/v1/sites/${site.id}/sources/${row.source_id}`);
                        if (now.kind === "document") kept.push(row);
                    } catch (e) {
                        if (!(e instanceof ApiError && (e.problem.status === 404 || e.problem.status === 403))) {
                            kept.push(row);
                            unchecked.add(row.source_id);
                        }
                    }
                }
                if (!live) return;
                const rows = [...all, ...kept];
                setDocs(rows);
                setRetrying(unchecked);
                setOpen((current) => (current && !rows.some((d) => d.source_id === current) ? null : current));
                setNext(last);
            } catch (e) {
                if (live) setError(e);
            }
        })();
        return () => {
            live = false;
        };
    }, [refresh, page, site.id]);

    return (
        <VStack as="section" gap={2} aria-labelledby="site-documents-title">
            <HStack gap={2} align="center">
                <Heading level={2} id="site-documents-title">
                    Site documents
                </Heading>
                {docs !== null && <Token label={`${docs.length}${next ? "+" : ""}`} size="sm" />}
            </HStack>
            {error !== null && <Problem error={error} context="The site's documents could not be listed." />}
            {docs === null && error === null && <Text color="secondary">Loading documents…</Text>}
            {docs !== null && docs.length === 0 && !next && <Text color="secondary">No documents uploaded at this site yet.</Text>}
            {docs !== null && docs.length > 0 && (
                <VStack as="ul" gap={1} aria-label="Site documents" style={{ listStyle: "none", padding: 0, margin: 0 }}>
                    {docs.map((d) => (
                        <li key={d.source_id} aria-label={d.title}>
                            <Card padding={2}>
                                <VStack gap={2}>
                                    <HStack gap={2} align="center" wrap="wrap">
                                        <Text weight="semibold">{d.title}</Text>
                                        <Text size="sm" color="secondary" hasTabularNumbers>
                                            r{d.revision} · {short(d.source_id)}
                                        </Text>
                                        {d.model_extracted && <Token label="model-extracted text (OCR)" color="orange" size="sm" />}
                                        {retrying.has(d.source_id) && (
                                            <Text size="sm" color="secondary" role="status">
                                                could not be checked just now; checking again on the next refresh
                                            </Text>
                                        )}
                                        <Button
                                            label={open === d.source_id ? "Hide authority" : "Authority"}
                                            variant="ghost"
                                            size="sm"
                                            aria-expanded={open === d.source_id}
                                            onClick={() => setOpen(open === d.source_id ? null : d.source_id)}
                                        />
                                    </HStack>
                                    {open === d.source_id && <SourceAuthority sourceID={d.source_id} />}
                                </VStack>
                            </Card>
                        </li>
                    ))}
                </VStack>
            )}
            {next && <Button label={loading ? "Loading…" : "Load more documents"} onClick={() => void load(next)} isDisabled={loading} />}
        </VStack>
    );
}
