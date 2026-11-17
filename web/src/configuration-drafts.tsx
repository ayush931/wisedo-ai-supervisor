import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Badge } from "@astryxdesign/core/Badge";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { createStaticSource, Typeahead, TypeaheadItem } from "@astryxdesign/core/Typeahead";
import { VisuallyHidden } from "@astryxdesign/core/VisuallyHidden";
import { VStack } from "@astryxdesign/core/VStack";
import type { ConfigurationDraftPageV1, ConfigurationDraftV1 } from "../../contracts/typescript/oee";
import { Intents, isConflict, mutate, request } from "./api";
import { label } from "./labels";
import { Actor, Problem, StationName, Status, Time, short, useSession, useStations } from "./ui";

// tick sizes a native checkbox or radio; the legacy input rule gives every input a 44 px touch height.
const tick = { minHeight: 0, width: 18, height: 18, padding: 0, margin: 0 } as const;

// Proposal F configuration drafts (human path). A draft is never live configuration; promotion
// appends through the normal revision path and is the approval act (rule 05), so the promoter
// may not be the author or any editor. The server decides every rule shown here; the UI only
// states the reason up front. Draft values and agent text render as plain text (no HTML, no links).

type Draft = ConfigurationDraftV1;
type Summary = ConfigurationDraftPageV1["drafts"][number];
type Citation = Draft["revisions"][number]["citations"][number];
type Leaf = { path: string; value: unknown };
type Values = Record<string, string | boolean>;
type StatusFilter = "draft" | "promoted" | "discarded" | "";

const kindText: Record<Draft["kind"], string> = { line: "Line", item: "Item", routing: "Routing", workstation_profile: "Workstation profile" };
const objectPath: Record<Draft["kind"], (id: string) => string> = {
    line: (id) => `lines/${id}`,
    item: (id) => `items/${id}`,
    routing: (id) => `routings/${id}`,
    workstation_profile: (id) => `workstations/${id}/profile`,
};

// leaves mirrors the server's draftLeaves: every leaf as a JSON pointer (~ → ~0, / → ~1); empty
// arrays and objects are leaves; /schema_version carries no fact and is not shown.
export function leaves(body: unknown, path = ""): Leaf[] {
    if (Array.isArray(body) && body.length) return body.flatMap((v, i) => leaves(v, `${path}/${i}`));
    if (body && typeof body === "object" && !Array.isArray(body) && Object.keys(body).length)
        return Object.entries(body).flatMap(([k, v]) => leaves(v, `${path}/${k.replaceAll("~", "~0").replaceAll("/", "~1")}`));
    return path === "/schema_version" ? [] : [{ path, value: body }];
}

const shown = (v: unknown): string | boolean => (typeof v === "boolean" ? v : v === null || v === undefined ? "" : typeof v === "object" ? JSON.stringify(v) : String(v));
const valuesOf = (body: unknown): Values => Object.fromEntries(leaves(body).map((l) => [l.path, shown(l.value)]));
const editable = (v: unknown) => v === null || typeof v !== "object";

// withValues rebuilds the body with each edited leaf, keeping the leaf's JSON type. A number that
// does not parse stays text so the server's strict decoder names the field (never guessed).
function withValues(body: unknown, values: Values): unknown {
    const out = structuredClone(body) as Record<string, unknown>;
    for (const { path, value } of leaves(body)) {
        if (!editable(value) || !(path in values)) continue;
        const next = values[path];
        const typed =
            typeof value === "number" && typeof next === "string" && /^-?\d+(\.\d+)?$/.test(next)
                ? Number(next)
                : value === null && next === ""
                  ? null
                  : next;
        const keys = path.slice(1).split("/").map((k) => k.replaceAll("~1", "/").replaceAll("~0", "~"));
        let at: Record<string, unknown> = out;
        for (const k of keys.slice(0, -1)) at = at[k] as Record<string, unknown>;
        at[keys[keys.length - 1]] = typed;
    }
    return out;
}

const fieldName = (path: string) => path.slice(1).replaceAll("/", " › ").replaceAll("_", " ");
type Choice = { id: string; name: string };
type Choices = { line: readonly Choice[]; item: readonly Choice[] };
type PickItem = { id: string; label: string; auxiliaryData: { meta: string } };
const pick = (items: readonly PickItem[], id: string, fallback: string): PickItem => items.find((i) => i.id === id) ?? { id, label: fallback, auxiliaryData: { meta: short(id) } };

export function ConfigurationDrafts({ names, choices, onChanged, onOpenObject, initialOpen = "" }: { names: ReadonlyMap<string, string>; choices: Choices; onChanged: () => void; onOpenObject: (kind: string, id: string) => void; initialOpen?: string }) {
    const { site } = useSession();
    const [filter, setFilter] = useState<StatusFilter>("draft");
    const [rows, setRows] = useState<Summary[] | null>(null);
    const [next, setNext] = useState("");
    const [error, setError] = useState<unknown>(null);
    const [open, setOpen] = useState(initialOpen);
    const [version, setVersion] = useState(0);
    // Only the latest request may write: a slow page for an old filter is dropped.
    const latest = useRef(0);

    const load = useCallback(async (cursor = "") => {
        const n = ++latest.current;
        setError(null);
        try {
            const page = await request<ConfigurationDraftPageV1>(`/v1/sites/${site.id}/configuration-drafts?limit=50${filter ? `&status=${filter}` : ""}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
            if (n !== latest.current) return;
            setRows((prior) => [...(cursor ? prior ?? [] : []), ...page.drafts]);
            setNext(page.next_cursor ?? "");
        } catch (e) {
            if (n === latest.current) setError(e);
        }
    }, [site.id, filter]);
    useEffect(() => {
        setRows(null);
        void load();
        return () => {
            latest.current++;
        };
    }, [load, version]);

    if (open)
        return <DraftDetail id={open} names={names} choices={choices} onClose={() => { setOpen(""); setVersion((v) => v + 1); }} onChanged={onChanged} onOpenObject={onOpenObject} />;

    return (
        <section aria-labelledby="drafts-title">
            <VStack gap={3}>
                <HStack justify="between" align="center" gap={2} wrap="wrap">
                    <Heading level={2} id="drafts-title">Drafts</Heading>
                    {/* Pressed buttons, not SegmentedControl: that one adds about 30 kB gzipped to the bundle. */}
                    <HStack role="group" aria-label="Status" gap={1}>
                        {([["draft", "Open"], ["promoted", "Promoted"], ["discarded", "Discarded"], ["", "All"]] as const).map(([value, text]) => (
                            <Button key={text} label={text} size="sm" variant={filter === value ? "primary" : "ghost"} aria-pressed={filter === value} onClick={() => setFilter(value)} />
                        ))}
                    </HStack>
                </HStack>
                <Problem error={error} />
                {!rows && !error && <div role="status" aria-label="Loading drafts"><Skeleton height={160} /></div>}
                {rows && (rows.length ? (
                    <>
                        <Table aria-label="Configuration drafts" density="compact">
                            <TableHeader><TableRow isHeaderRow>
                                {["Draft", "Kind", "Target", "Author", "Status", "Citations", "Rev", "Updated"].map((h) => <TableHeaderCell key={h} scope="col">{h}</TableHeaderCell>)}
                                <TableHeaderCell scope="col"><VisuallyHidden>Open</VisuallyHidden></TableHeaderCell>
                            </TableRow></TableHeader>
                            <TableBody>{rows.map((d) => (
                                <TableRow key={d.draft_id}>
                                    <TableHeaderCell scope="row"><span className="identifier">{short(d.draft_id)}</span></TableHeaderCell>
                                    <TableCell>{kindText[d.kind]}</TableCell>
                                    <TableCell><Target kind={d.kind} id={d.target_object_id} expected={d.target_expected_revision} names={names} /></TableCell>
                                    <TableCell><Author kind={d.author_kind} /></TableCell>
                                    <TableCell><Status code={d.status} /></TableCell>
                                    <TableCell><CitationCount total={d.citation_count} unavailable={d.unavailable_citation_count} /></TableCell>
                                    <TableCell>r{d.current_revision}</TableCell>
                                    <TableCell><Time iso={d.updated_at} /></TableCell>
                                    <TableCell><Button label={`Open draft ${short(d.draft_id)}`} size="sm" variant="secondary" onClick={() => setOpen(d.draft_id)}>Open</Button></TableCell>
                                </TableRow>
                            ))}</TableBody>
                        </Table>
                        {next && <div><Button label="Load more" variant="ghost" onClick={() => load(next)} /></div>}
                    </>
                ) : <Text as="p" color="secondary">No {filter === "draft" ? "open " : ""}drafts.</Text>)}
            </VStack>
        </section>
    );
}

function Target({ kind, id, expected, names }: { kind: Draft["kind"]; id?: string; expected?: number; names: ReadonlyMap<string, string> }) {
    if (!id) return <>new {kindText[kind].toLowerCase()}</>;
    const name = kind === "workstation_profile" ? <StationName id={id} /> : names.get(id) ?? short(id);
    return <>revise {name} r{expected}</>;
}

function Author({ kind }: { kind: Draft["author_kind"] }) {
    return kind === "agent"
        ? <Badge variant="orange" label={<><span aria-hidden="true">◇</span> Agent draft — not live configuration</>} />
        : <Badge variant="neutral" label="Human draft" />;
}

function CitationCount({ total, unavailable }: { total: number; unavailable: number }) {
    if (!total) return <Text size="sm" color="secondary">none</Text>;
    return unavailable
        ? <Badge variant="red" label={<><span aria-hidden="true">▲</span> {unavailable} unavailable</>} />
        : <Badge variant="green" label={<><span aria-hidden="true">✓</span> {total}/{total} available</>} />;
}

function CitationChip({ c }: { c: Citation }) {
    const { site } = useSession();
    const [quote, setQuote] = useState<string | null>(null);
    const [error, setError] = useState<unknown>(null);
    const where = `source ${short(c.source_id)} r${c.source_revision}`;
    if (!c.available) {
        const reason = label(c.unavailable_reason ?? "unavailable");
        return <span title={`${c.unavailable_reason ?? "UNAVAILABLE"} · span ${c.span_id}`}><Badge variant="red" label={<><span aria-hidden="true">{reason.icon}</span> {reason.text} · {where}</>} /></span>;
    }
    return (
        <details className="cite" onToggle={(e) => {
            if (e.currentTarget.open && quote === null)
                request<{ text: string }>(`/v1/sites/${site.id}/source-spans/${c.span_id}`).then((s) => setQuote(s.text), setError);
        }}>
            <summary className="mini"><span aria-hidden="true">✓</span> Cited · {where}</summary>
            {quote !== null ? <blockquote className="cite-quote">{quote}</blockquote> : error ? <Problem error={error} /> : <p role="status" className="meta">Loading quote…</p>}
        </details>
    );
}

function DraftDetail({ id, names, onClose, onChanged, onOpenObject }: { id: string; names: ReadonlyMap<string, string>; onClose: () => void; onChanged: () => void; onOpenObject: (kind: string, id: string) => void }) {
    const { site, me, online } = useSession();
    const base = `/v1/sites/${site.id}/configuration-drafts/${id}`;
    const intents = useMemo(() => new Intents(), []);
    const [draft, setDraft] = useState<Draft | null>(null);
    const [values, setValues] = useState<Values>({});
    const [error, setError] = useState<unknown>(null);
    const [busy, setBusy] = useState("");
    const [notice, setNotice] = useState<React.ReactNode>(null);
    const [compare, setCompare] = useState<{ mine: Values; server: Draft } | null>(null);
    const [dialog, setDialog] = useState<"" | "promote" | "discard">("");
    const alertRef = useRef<HTMLDivElement>(null);
    const promoteRef = useRef<HTMLButtonElement>(null);
    const discardRef = useRef<HTMLButtonElement>(null);
    // A dialog stays open while its request runs, then closes and focus moves to the outcome.
    const settle = (name: "promote" | "discard", run: () => Promise<Draft>, done: (d: Draft) => void) =>
        void act(name, run, done).finally(() => {
            setDialog("");
            requestAnimationFrame(() => alertRef.current?.focus());
        });

    const show = useCallback((d: Draft, reset = true) => {
        setDraft(d);
        if (reset) setValues(valuesOf(d.revisions[d.current_revision - 1].body));
    }, []);
    const reload = useCallback(async (reset = true) => {
        const d = await request<Draft>(base);
        show(d, reset);
        return d;
    }, [base, show]);
    useEffect(() => {
        reload().catch(setError);
    }, [reload]);

    async function act(name: string, run: () => Promise<Draft>, done: (d: Draft) => void) {
        setBusy(name);
        setError(null);
        setNotice(null);
        try {
            const d = await run();
            show(d);
            done(d);
            onChanged();
            requestAnimationFrame(() => alertRef.current?.focus());
        } catch (e) {
            setError(e);
            // Every 409/412 reloads the draft so the generic text ("reloaded") stays true.
            if (isConflict(e)) {
                const mine = values;
                const server = await reload(false).catch(() => null);
                if (server && name === "save" && (e as { problem: { status: number } }).problem.status === 412) setCompare({ mine, server });
                else if (server) show(server);
            }
            requestAnimationFrame(() => alertRef.current?.focus());
        } finally {
            setBusy("");
        }
    }

    if (!draft) return error ? <Problem error={error} /> : <div role="status" aria-label="Loading draft"><Skeleton height={240} /></div>;

    const rev = draft.revisions[draft.current_revision - 1];
    const fields = leaves(rev.body);
    const open = draft.status === "draft";
    const dirty = fields.some((f) => editable(f.value) && shown(f.value) !== values[f.path]);
    const unavailable = rev.citations.filter((c) => !c.available).length;
    const created = draft.created_by?.subject === me.subject;
    const edited = draft.revisions.some((r) => r.editor?.subject === me.subject);
    const blocked =
        created ? "You created this draft; another supervisor must promote it."
        : edited ? "You edited this draft; another supervisor must promote it."
        : unavailable ? `${unavailable} ${unavailable === 1 ? "citation" : "citations"} unavailable.`
        : dirty ? "Save or reset your edits first."
        : !online ? "Offline." : "";
    const disabled = !online || !!busy;

    return (
        <section aria-labelledby="draft-title" aria-busy={!!busy}>
            <VStack gap={3}>
            <div><Button label="← All drafts" variant="ghost" size="sm" onClick={onClose} /></div>
            <Heading level={2} id="draft-title">
                {kindText[draft.kind]} draft <span className="identifier">{short(draft.draft_id)}</span>{" "}
                <Text color="secondary" weight="normal">· <Target kind={draft.kind} id={draft.target_object_id} expected={draft.target_expected_revision} names={names} /> · r{draft.current_revision}</Text>
            </Heading>
            <HStack gap={2} align="center" wrap="wrap">
                <Author kind={draft.author_kind} />
                <Status code={draft.status} />
                <CitationCount total={rev.citations.length} unavailable={unavailable} />
                {draft.created_by && <Text size="sm" color="secondary">Created by <Actor a={draft.created_by} /></Text>}
            </HStack>
            <div ref={alertRef} tabIndex={-1}>
                <Problem error={error} />
                {notice && <div role="status"><Banner status="success" title={<>✓ {notice}</>} /></div>}
            </div>
            {draft.promotion && (
                <HStack gap={2} align="center" wrap="wrap">
                    <Text size="sm" color="secondary">Promoted by <Actor a={draft.promotion.promoted_by} /> · <Time iso={draft.promotion.promoted_at} /> as r{draft.promotion.object_revision}</Text>
                    {draft.kind !== "workstation_profile" && <Button label={`Open ${kindText[draft.kind].toLowerCase()}`} variant="ghost" size="sm" onClick={() => onOpenObject(draft.kind, draft.promotion!.object_id)} />}
                </HStack>
            )}
            {compare && (
                <div role="region" aria-label="Compare with the server revision">
                    <VStack gap={2}>
                        <Banner status="warning" title={`This draft changed on the server (now r${compare.server.current_revision}). Your edits were not saved.`} />
                        <Table aria-label="Your edits compared with the server revision" density="compact">
                            <TableHeader><TableRow isHeaderRow><TableHeaderCell scope="col">Field</TableHeaderCell><TableHeaderCell scope="col">Your edit</TableHeaderCell><TableHeaderCell scope="col">Server r{compare.server.current_revision}</TableHeaderCell></TableRow></TableHeader>
                            <TableBody>{leaves(compare.server.revisions[compare.server.current_revision - 1].body).filter((l) => editable(l.value) && compare.mine[l.path] !== shown(l.value)).map((l) => (
                                <TableRow key={l.path}><TableHeaderCell scope="row">{fieldName(l.path)}</TableHeaderCell><TableCell>{String(compare.mine[l.path] ?? "—")}</TableCell><TableCell>{String(shown(l.value))}</TableCell></TableRow>
                            ))}</TableBody>
                        </Table>
                        <HStack gap={2}>
                            <Button label={`Reload r${compare.server.current_revision}`} variant="primary" onClick={() => { show(compare.server); setCompare(null); setError(null); }} />
                            <Button label={`Keep my edits on r${compare.server.current_revision}`} variant="secondary" onClick={() => { setValues(compare.mine); setCompare(null); setError(null); }} />
                        </HStack>
                    </VStack>
                </div>
            )}
            <form onSubmit={(e) => {
                e.preventDefault();
                if (!dirty || disabled || !open) return;
                const body = { body: withValues(rev.body, values), field_spans: [] };
                void act("save", () => mutate<Draft>(intents, "save", `${base}/revisions`, body, draft.current_revision), (d) => setNotice(`Saved r${d.current_revision}`));
            }}>
                <VStack gap={3}>
                <Table aria-label="Draft fields" density="compact">
                    <TableHeader><TableRow isHeaderRow><TableHeaderCell scope="col">Field</TableHeaderCell><TableHeaderCell scope="col">Value</TableHeaderCell><TableHeaderCell scope="col">Citations</TableHeaderCell></TableRow></TableHeader>
                    <TableBody>{fields.map((f) => {
                        const cites = rev.citations.filter((c) => c.path === f.path);
                        const inputID = `draft-field-${f.path}`;
                        const v = values[f.path];
                        return (
                            <TableRow key={f.path}>
                                <TableHeaderCell scope="row">{!editable(f.value) || !open ? fieldName(f.path) : <label htmlFor={inputID}>{fieldName(f.path)}</label>}</TableHeaderCell>
                                <TableCell>{!editable(f.value) || !open
                                    ? String(shown(f.value))
                                    : typeof f.value === "boolean"
                                      ? <input id={inputID} type="checkbox" style={tick} checked={v === true} onChange={(e) => setValues({ ...values, [f.path]: e.currentTarget.checked })} />
                                      : <input id={inputID} type="text" inputMode={typeof f.value === "number" ? "decimal" : undefined} value={String(v ?? "")} onChange={(e) => setValues({ ...values, [f.path]: e.currentTarget.value })} />}
                                </TableCell>
                                <TableCell>{cites.length ? <span className="chiplist">{cites.map((c) => <CitationChip key={c.span_id} c={c} />)}</span> : <Text size="sm" color="secondary">uncited</Text>}</TableCell>
                            </TableRow>
                        );
                    })}</TableBody>
                </Table>
                {rev.dropped_citations.length > 0 && (
                    <div role="note" aria-label="Dropped citations">
                        <Banner status="warning" collapsible={false} title={`Citations not carried into r${rev.revision}`}>
                            <ul>{rev.dropped_citations.map((d) => (
                                <li key={`${d.path}-${d.span_id}`}>{fieldName(d.path)}: <span className="mini" title={d.reason}>{label(d.reason).text}</span> <span className="identifier">span {short(d.span_id)}</span></li>
                            ))}</ul>
                        </Banner>
                    </div>
                )}
                {open && (
                    <HStack gap={2} align="center" wrap="wrap" role="toolbar" aria-label="Draft actions">
                        <Button type="submit" label={`Save r${draft.current_revision + 1}`} variant="secondary" isDisabled={disabled || !dirty}>{busy === "save" && <span className="spinner" aria-label="Sending" />}Save r{draft.current_revision + 1}</Button>
                        {dirty && <Button label="Reset edits" variant="ghost" onClick={() => setValues(valuesOf(rev.body))} />}
                        <Button ref={promoteRef} label={`Promote r${draft.current_revision}`} variant="primary" isDisabled={disabled || !!blocked} onClick={() => setDialog("promote")}>
                            Promote r{draft.current_revision}{blocked && <span className="button-reason"> — {blocked}</span>}
                        </Button>
                        <Button ref={discardRef} label="Discard" variant="destructive" isDisabled={disabled} onClick={() => setDialog("discard")} />
                    </HStack>
                )}
                </VStack>
            </form>
            <details>
                <summary>Revisions ({draft.revisions.length})</summary>
                <Table aria-label="Draft revisions" density="compact">
                    <TableHeader><TableRow isHeaderRow>{["Rev", "Edited by", "Citations", "Dropped", "Digest"].map((h) => <TableHeaderCell key={h} scope="col">{h}</TableHeaderCell>)}</TableRow></TableHeader>
                    <TableBody>{[...draft.revisions].reverse().map((r) => (
                        <TableRow key={r.revision}>
                            <TableCell>r{r.revision}</TableCell>
                            <TableCell>{r.edited_by_kind === "agent" ? "Agent" : <Actor a={r.editor} />} · <Time iso={r.created_at} /></TableCell>
                            <TableCell>{r.citations.length}</TableCell>
                            <TableCell>{r.dropped_citations.length}</TableCell>
                            <TableCell><span className="identifier">{r.body_digest.slice(0, 12)}</span></TableCell>
                        </TableRow>
                    ))}</TableBody>
                </Table>
            </details>
            </VStack>
            {dialog === "promote" && (
                <PromoteDialog draft={draft} fields={fields} busy={busy === "promote"} disabled={disabled} onCancel={() => { setDialog(""); requestAnimationFrame(() => promoteRef.current?.focus()); }} onPromote={() =>
                    settle("promote", () => mutate<Draft>(intents, "promote", `${base}/promotions`, {}, draft.current_revision), (d) => setNotice(
                        <>Promoted as {kindText[d.kind].toLowerCase()} r{d.promotion?.object_revision}</>,
                    ))
                } />
            )}
            {dialog === "discard" && (
                <ConfirmDialog title={`Discard ${kindText[draft.kind].toLowerCase()} draft ${short(draft.draft_id)}?`} action="Confirm discard" busy={busy === "discard"} disabled={disabled}
                    onCancel={() => { setDialog(""); requestAnimationFrame(() => discardRef.current?.focus()); }}
                    onConfirm={() => settle("discard", () => mutate<Draft>(intents, "discard", `${base}/discards`, {}, draft.current_revision), () => setNotice("Draft discarded"))}>
                    <ul className="consequences">
                        <li>The draft closes for good; it cannot be promoted or edited afterwards.</li>
                        <li>Live configuration is unchanged.</li>
                    </ul>
                </ConfirmDialog>
            )}
        </section>
    );
}

// PromoteDialog shows what promotion changes against the live object revision, if any.
function PromoteDialog({ draft, fields, busy, disabled, onCancel, onPromote }: { draft: Draft; fields: Leaf[]; busy: boolean; disabled: boolean; onCancel: () => void; onPromote: () => void }) {
    const { site } = useSession();
    const [live, setLive] = useState<{ revision: number; values: Values } | null>(null);
    const [error, setError] = useState<unknown>(null);
    useEffect(() => {
        // A first workstation profile (expected r0) has no live revision to compare.
        if (!draft.target_object_id || !draft.target_expected_revision) return;
        request<{ current_revision: number; revisions: readonly { revision: number; body?: unknown; profile?: unknown }[] }>(`/v1/sites/${site.id}/${objectPath[draft.kind](draft.target_object_id)}`).then((o) => {
            const r = o.revisions.find((x) => x.revision === o.current_revision);
            setLive({ revision: o.current_revision, values: valuesOf(r?.body ?? r?.profile ?? {}) });
        }, setError);
    }, [site.id, draft.kind, draft.target_object_id]);
    const changes = fields.filter((f) => !live || live.values[f.path] !== shown(f.value));
    const stale = live && draft.target_expected_revision !== undefined && live.revision !== draft.target_expected_revision;
    return (
        <ConfirmDialog title={`Promote ${kindText[draft.kind].toLowerCase()} draft r${draft.current_revision}?`} action="Confirm promotion" busy={busy} disabled={disabled} onCancel={onCancel} onConfirm={onPromote}>
            <ul className="consequences">
                <li>{draft.target_object_id ? `Appends a new revision to the live ${kindText[draft.kind].toLowerCase()} (written against r${draft.target_expected_revision}).` : `Creates a new live ${kindText[draft.kind].toLowerCase()}.`}</li>
                <li>Promotion is the approval: the server refuses the draft's author or editors and any unavailable citation.</li>
            </ul>
            {stale && <div role="note"><Banner status="warning" title={`▲ The live object is now r${live.revision}; the server will refuse. Discard this draft and create a new one.`} /></div>}
            <Problem error={error} />
            <Table aria-label="Changes on promotion" density="compact">
                <TableHeader><TableRow isHeaderRow><TableHeaderCell scope="col">Field</TableHeaderCell>{draft.target_object_id && <TableHeaderCell scope="col">Live{live ? ` r${live.revision}` : ""}</TableHeaderCell>}<TableHeaderCell scope="col">Draft</TableHeaderCell></TableRow></TableHeader>
                <TableBody>{changes.map((f) => (
                    <TableRow key={f.path}><TableHeaderCell scope="row">{fieldName(f.path)}</TableHeaderCell>{draft.target_object_id && <TableCell>{live ? String(live.values[f.path] ?? "—") : "…"}</TableCell>}<TableCell>{String(shown(f.value))}</TableCell></TableRow>
                ))}</TableBody>
            </Table>
            {draft.target_object_id && live && !changes.length && <Text as="p" size="sm" color="secondary">No field differs from the live revision.</Text>}
        </ConfirmDialog>
    );
}

// ConfirmDialog is a modal confirmation. It cannot be dismissed while its request runs; Cancel
// and Escape call onCancel, which returns focus to the button that opened it.
function ConfirmDialog({ title, action, busy, disabled, onCancel, onConfirm, children }: { title: string; action: string; busy: boolean; disabled: boolean; onCancel: () => void; onConfirm: () => void; children: React.ReactNode }) {
    return (
        <Dialog isOpen purpose="form" aria-busy={busy} onOpenChange={(isOpen) => { if (!isOpen && !busy) onCancel(); }}>
            <DialogHeader title={title} />
            <VStack gap={3} padding={4}>
                {children}
                <HStack gap={2}>
                    <Button label={action} variant="primary" isDisabled={disabled} onClick={onConfirm}>
                        {busy && <span className="spinner" aria-label="Sending" />}
                        {action}
                    </Button>
                    <Button label="Cancel" variant="secondary" isDisabled={busy} onClick={onCancel} />
                </HStack>
            </VStack>
        </Dialog>
    );
}
