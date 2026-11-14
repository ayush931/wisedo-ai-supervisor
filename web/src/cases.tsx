import React, { useCallback, useEffect, useRef, useState } from "react";
import type { CasePageV1, CaseV1, KnowledgeEntityV1 } from "../../contracts/typescript/oee";
import { Badge, type BadgeProps } from "@astryxdesign/core/Badge";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { EmptyState } from "@astryxdesign/core/EmptyState";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextArea } from "@astryxdesign/core/TextArea";
import { VStack } from "@astryxdesign/core/VStack";
import { ApiError, Intents, mutate, outcomeText, request } from "./api";
import { label } from "./labels";
import { Actor, Details, Link, Problem, Refreshed, Time, Val, lengthError, short, usePolling, useSession } from "./ui";
import { AdvisorSuggestion } from "./suggestion";

// WIS-273 case inbox: production-shortfall cases as the server holds them. The page is a view of
// server authority: actions are offered by role, but the API decides, and a state is shown only
// from a server response. Entity names are source-derived text and render as plain text.

type Transition = CaseV1["transitions"][number];
type Clarification = CaseV1["clarifications"][number];
type Trigger = CaseV1["triggers"][number];

// Case states have their own words: "proposed" here means a proposal is ready for review, not a
// finished agent run as in the shared label table. Never colour alone: every state has an icon.
const caseStates: Record<string, [text: string, icon: string, tone: string]> = {
    detected: ["Detected", "◎", "warn"],
    investigating: ["Investigating", "◐", "muted"],
    needs_input: ["Needs input", "▲", "warn"],
    proposed: ["Proposal ready", "◆", "ok"],
    awaiting_approval: ["Awaiting approval", "◆", "warn"],
    executing: ["Executing", "▶", "ok"],
    observing: ["Observing outcome", "◐", "ok"],
    escalated: ["Escalated", "▲", "bad"],
    resolved: ["Resolved", "✓", "ok"],
    canceled: ["Canceled", "■", "muted"],
};
const terminal = (state: string) => state === "resolved" || state === "canceled";
const activePollMs = 10_000; // an active case (WIS-278)
const listPollMs = 30_000; // the inbox, like the overview

// System identities are not site members: they are shown as the service they are, never as a
// person (rules 05 and 08), and never looked up in the member directory.
const systemActors: Record<string, string> = {
    "supervisor:evaluator": "Shortfall evaluator (system)",
    "supervisor:temporal": "Case workflow (system)",
    "supervisor:worker": "Detected-case sweep (system)",
    "supervisor:agent": "Investigation agent (system)",
    "supervisor:executor": "Approved-action executor (system)",
    "supervisor:observer": "Outcome observer (system)",
};
export function CaseActor({ a }: { a: { issuer: string; subject: string } }) {
    if (a.issuer.startsWith("supervisor:")) return <span title={`${a.issuer} ${a.subject}`}>{systemActors[a.issuer] ?? `Supervisor system (${a.issuer.slice("supervisor:".length)})`}</span>;
    return <Actor a={a} />;
}

// Case events in words; an unknown code is shown as it is.
const caseEvents: Record<string, string> = {
    trigger: "two short windows",
    context_ready: "context ready",
    evidence_missing: "evidence missing",
    clarified: "clarification recorded",
    proposal_validated: "proposal validated",
    approval_requested: "approval requested",
    proposal_rejected: "proposal rejected, expired or stale",
    approval_granted: "approval granted",
    task_authoritative: "task created",
    closed_by_supervisor: "closed by a supervisor",
    fatal: "stopped by a policy or validation failure",
    wait_deadline: "no answer within 24 h",
    cancel: "canceled",
    resume: "resumed",
    reopen: "reopened",
};
const eventText = (code: string) => caseEvents[code] ?? code;

// A transition reason is either a person's words or a stable reason code from the server; a code
// is shown in words with the code itself in the tooltip.
function ReasonText({ code }: { code: string }) {
    if (!/^[A-Z][A-Z0-9_]*$/.test(code)) return <>{code}</>;
    return <span title={code}>{label(code).text}</span>;
}

// StatePill is a state Badge: icon and words, the server code in the tooltip. The label is a
// fragment so Badge sets no title of its own (same pattern as Status in ui.tsx).
const toneVariant: Record<string, BadgeProps["variant"]> = { ok: "green", warn: "orange", bad: "red", muted: "neutral" };
export function StatePill({ code, text, icon, tone }: { code: string; text: string; icon: string; tone: string }) {
    return (
        <span title={code}>
            <Badge
                variant={toneVariant[tone] ?? "neutral"}
                label={
                    <>
                        <span aria-hidden="true">{icon}</span> {text}
                    </>
                }
            />
        </span>
    );
}

export function CaseState({ state }: { state: string }) {
    const [text, icon, tone] = caseStates[state] ?? [state, "•", "muted"];
    return <StatePill code={state} text={text} icon={icon} tone={tone} />;
}

// Kpi is a number tile; specs read `.kpi strong`.
export function Kpi({ label: name, children, title }: { label: string; children: React.ReactNode; title?: string }) {
    return (
        <Card padding={3} className="kpi">
            <Text size="sm" color="secondary">{name}</Text>
            <strong title={title}>{children}</strong>
        </Card>
    );
}

// Notice is the page-level result line: a receipt (success) or a conflict that submitted nothing.
export type NoticeT = { text: string; ok: boolean } | null;
export function Notice({ n }: { n: NoticeT }) {
    if (!n) return null;
    return (
        <div role="status">
            <Banner status={n.ok ? "success" : "warning"} title={n.text} />
        </div>
    );
}
export function Offline() {
    return (
        <div role="status">
            <Banner status="warning" title="You are offline. Actions are disabled; anything you typed is kept." />
        </div>
    );
}

// EntityName shows a knowledge entity's current display name; while loading, or when it cannot be
// read, the short ID stays visible instead (unknown is never blank).
const entityCache = new Map<string, Promise<KnowledgeEntityV1>>();
export function EntityName({ id, kind }: { id: string; kind: string }) {
    const { site } = useSession();
    const [name, setName] = useState<string | null>(null);
    useEffect(() => {
        let live = true;
        const key = `${site.id}/${id}`;
        let pending = entityCache.get(key);
        if (!pending) {
            pending = request<KnowledgeEntityV1>(`/v1/sites/${site.id}/entities/${id}`);
            entityCache.set(key, pending);
            pending.catch(() => entityCache.delete(key));
        }
        pending.then((e) => live && setName(e.display_name), () => live && setName(null));
        return () => {
            live = false;
        };
    }, [site.id, id]);
    return <span title={`${kind} ${id}`}>{name ?? `${kind} ${short(id)}`}</span>;
}

export function CaseList() {
    const { site } = useSession();
    const [rows, setRows] = useState<CasePageV1["items"] | null>(null);
    const [next, setNext] = useState("");
    const [error, setError] = useState<unknown>(null);
    const [at, setAt] = useState<Date | null>(null);
    const [loadingMore, setLoadingMore] = useState(false);
    const pages = useRef(1);
    const generation = useRef(0);

    // Every poll re-reads all pages loaded so far and replaces the whole list, taking the cursor
    // from the last of them. Cases are ordered by ID, so a new case can land on any page: replacing
    // only the first page would hide new cases, drop rows pushed off page 1 and leave later rows
    // stale (WIS-273 review). A slower response from an older walk is ignored.
    const refresh = useCallback(async () => {
        const mine = ++generation.current;
        try {
            const items: CasePageV1["items"][number][] = [];
            let cursor = "";
            for (let n = 0; n < pages.current; n++) {
                const page = await request<CasePageV1>(`/v1/sites/${site.id}/cases?limit=50${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
                items.push(...page.items);
                cursor = page.next_cursor ?? "";
                if (!cursor) break;
            }
            if (mine !== generation.current) return;
            setRows(items);
            setNext(cursor);
            setError(null);
            setAt(new Date());
        } catch (e) {
            if (mine === generation.current) setError(e);
        }
    }, [site.id]);
    usePolling(useCallback(() => void refresh(), [refresh]), listPollMs);

    async function more() {
        setLoadingMore(true);
        pages.current++;
        await refresh();
        setLoadingMore(false);
    }

    const count = (pick: (state: string) => boolean) => {
        const n = rows?.filter((c) => pick(c.state)).length;
        // While more pages exist the count covers only the loaded cases: shown as "at least".
        return n === undefined ? null : next ? `${n}+` : n;
    };
    const partial = next ? "Counted over the cases loaded so far" : undefined;
    return (
        <section aria-labelledby="cases-title">
            <VStack gap={3}>
                <HStack justify="between" align="center" gap={2} wrap="wrap">
                    <Heading level={1} id="cases-title">Cases</Heading>
                    <Refreshed at={at} onRefresh={() => void refresh()} />
                </HStack>
                <Problem error={error} />
                {!rows && !error && (
                    <div role="status" aria-label="Loading cases…">
                        <Skeleton height={240} />
                    </div>
                )}
                {rows && (
                    <HStack gap={2} wrap="wrap">
                        <Kpi label="Open" title={partial}><Val v={count((s) => !terminal(s))} /></Kpi>
                        <Kpi label="Needs input" title={partial}><Val v={count((s) => s === "needs_input")} /></Kpi>
                        <Kpi label="Awaiting approval" title={partial}><Val v={count((s) => s === "awaiting_approval")} /></Kpi>
                        <Kpi label="Escalated" title={partial}><Val v={count((s) => s === "escalated")} /></Kpi>
                    </HStack>
                )}
                {rows &&
                    (rows.length ? (
                        <Table aria-label="Cases" density="compact">
                            <TableHeader>
                                <TableRow isHeaderRow>
                                    <TableHeaderCell scope="col">Case</TableHeaderCell>
                                    <TableHeaderCell scope="col">Line</TableHeaderCell>
                                    <TableHeaderCell scope="col">Order</TableHeaderCell>
                                    <TableHeaderCell scope="col">State</TableHeaderCell>
                                    <TableHeaderCell scope="col">Episode</TableHeaderCell>
                                    <TableHeaderCell scope="col">Detected</TableHeaderCell>
                                    <TableHeaderCell scope="col">Latest window end</TableHeaderCell>
                                </TableRow>
                            </TableHeader>
                            <TableBody>
                                {rows.map((c) => (
                                    <TableRow key={c.id}>
                                        <TableHeaderCell scope="row">
                                            <Link to={`/sites/${site.id}/cases/${c.id}`}>Case {short(c.id)}</Link>
                                        </TableHeaderCell>
                                        <TableCell><EntityName id={c.line_id} kind="Line" /></TableCell>
                                        <TableCell><EntityName id={c.order_id} kind="Order" /></TableCell>
                                        <TableCell><CaseState state={c.state} /></TableCell>
                                        <TableCell>{c.episode}</TableCell>
                                        <TableCell><Time iso={c.detected_at} /></TableCell>
                                        <TableCell><Time iso={c.latest_window_end} /></TableCell>
                                    </TableRow>
                                ))}
                            </TableBody>
                        </Table>
                    ) : (
                        <EmptyState
                            title="No cases"
                            description="A case opens when two consecutive 15-minute windows of an approved plan fall short."
                            headingLevel={2}
                        />
                    ))}
                {next && (
                    <div>
                        <Button label={loadingMore ? "Loading…" : "Load more cases"} onClick={() => void more()} isDisabled={loadingMore} />
                    </div>
                )}
            </VStack>
        </section>
    );
}

type TimelineEntry =
    | { at: string; revision: number; kind: "transition"; t: Transition }
    | { at: string; revision: number; kind: "clarification"; c: Clarification }
    | { at: string; revision: number; kind: "trigger"; g: Trigger };
const kindOrder = { clarification: 0, transition: 1, trigger: 2 } as const;

export function CasePage({ caseId }: { caseId: string }) {
    const { site, online, navigate } = useSession();
    const base = `/v1/sites/${site.id}/cases/${caseId}`;
    const [c, setCase] = useState<CaseV1 | null>(null);
    const [error, setError] = useState<unknown>(null);
    const [at, setAt] = useState<Date | null>(null);
    const [notice, setNotice] = useState<NoticeT>(null);
    const load = useCallback(async () => {
        try {
            const v = await request<CaseV1>(base);
            // A poll that started before a mutation must not put an older revision back on screen.
            setCase((prior) => (prior && prior.id === v.id && v.revision < prior.revision ? prior : v));
            setError(null);
            setAt(new Date());
        } catch (e) {
            setError(e);
        }
    }, [base]);
    usePolling(useCallback(() => void load(), [load]), c && terminal(c.state) ? null : activePollMs);

    if (!c)
        return error ? (
            <Problem error={error} />
        ) : (
            <div role="status" aria-label="Loading case…">
                <Skeleton height={240} />
            </div>
        );

    const timeline: TimelineEntry[] = [
        ...c.transitions.map((t) => ({ at: t.created_at, revision: t.revision, kind: "transition" as const, t })),
        ...c.clarifications.map((x) => ({ at: x.created_at, revision: x.case_revision, kind: "clarification" as const, c: x })),
        // A later qualifying window that joined this active episode changes no state, so it has no
        // revision of its own: it sits after the last transition recorded before it (the opener is
        // already the r1 transition).
        ...c.triggers
            .filter((g) => !g.opened)
            .map((g) => ({
                at: g.created_at,
                revision: Math.max(1, ...c.transitions.filter((t) => Date.parse(t.created_at) <= Date.parse(g.created_at)).map((t) => t.revision)),
                kind: "trigger" as const,
                g,
            })),
    ]
        // Within a revision the order is fixed by kind, not by time: a clarification at rN is the cause
        // of the clarified transition rN (migration 049, written after it in the same transaction) and
        // reads before it; a join placed at rN locked the case after that transaction, so it is later
        // than both.
        .sort((a, b) => a.revision - b.revision || kindOrder[a.kind] - kindOrder[b.kind] || Date.parse(a.at) - Date.parse(b.at));

    // A mutation answers with the case as the server now holds it; that is the only source of the
    // new state. A conflict reloads the current revision for review and resubmits nothing.
    const applied = (v: CaseV1, text: string) => {
        setCase(v);
        setNotice({ text: `${text} The case is now at revision ${v.revision}.`, ok: true });
        setAt(new Date());
    };
    // The form that hit the conflict may unmount when the reloaded state no longer offers it, so the
    // explanation lives on the page, not in the form.
    const conflicted = (e: unknown) => {
        if (!(e instanceof ApiError && e.problem.status === 412)) return;
        setNotice({ text: outcomeText.conflict + " Nothing was submitted.", ok: false });
        void load();
    };

    return (
        <section aria-labelledby="case-title">
            <VStack gap={3}>
            <HStack justify="between" align="center" gap={2} wrap="wrap">
                <VStack gap={1}>
                    <Heading level={1} id="case-title">Case {short(c.id)}</Heading>
                    <Text as="p" size="sm" color="secondary">
                        <CaseState state={c.state} /> · episode {c.episode} · revision {c.revision}
                    </Text>
                </VStack>
                <Refreshed at={at} onRefresh={() => void load()} />
            </HStack>
            <Problem error={error} context="The case could not be refreshed; the view below may be out of date." />
            <Notice n={notice} />
            <MetadataList columns="multi">
                <MetadataListItem label="Line"><EntityName id={c.line_id} kind="Line" /></MetadataListItem>
                <MetadataListItem label="Order"><EntityName id={c.order_id} kind="Order" /></MetadataListItem>
                <MetadataListItem label="Exception">{c.exception_kind === "production_shortfall" ? "Production shortfall" : c.exception_kind}</MetadataListItem>
                <MetadataListItem label="Detected"><Time iso={c.detected_at} /></MetadataListItem>
                <MetadataListItem label="Latest window end"><Time iso={c.latest_window_end} /></MetadataListItem>
                <MetadataListItem label="Trigger records">{c.trigger_evidence_ids.length}</MetadataListItem>
                {c.resolution_reason !== null && <MetadataListItem label="Closing reason">{c.resolution_reason}</MetadataListItem>}
                {c.previous_case_id && (
                    <MetadataListItem label="Previous episode">
                        <Link to={`/sites/${site.id}/cases/${c.previous_case_id}`}>Case {short(c.previous_case_id)}</Link>
                    </MetadataListItem>
                )}
            </MetadataList>
            <Details
                items={[
                    ["Case ID", c.id],
                    ["Context snapshot", c.context_snapshot_id],
                    ["Context revision", c.context_revision],
                    ["Context digest", c.context_digest],
                    ["Workflow", c.workflow_id],
                ]}
            />
            <AdvisorSuggestion base={base} />
            <Heading level={2}>Timeline</Heading>
            <ol className="timeline" aria-label="Case timeline">
                {timeline.map((e) =>
                    e.kind === "transition" ? (
                        <li key={`t${e.t.revision}`}>
                            <strong>
                                r{e.t.revision}: {e.t.from_state === "none" ? "Opened as " : <><CaseState state={e.t.from_state} /> → </>}
                                <CaseState state={e.t.to_state} />
                            </strong>{" "}
                            <Text size="sm" color="secondary">
                                <span title={e.t.event}>{eventText(e.t.event)}</span> · <CaseActor a={e.t.actor} /> · <Time iso={e.t.created_at} />
                            </Text>
                            {e.t.reason && <Text as="p">Reason: <ReasonText code={e.t.reason} /></Text>}
                        </li>
                    ) : e.kind === "trigger" ? (
                        <li key={`g${e.g.source_event_id}`}>
                            <strong>Another shortfall joined this case</strong>{" "}
                            <Text size="sm" color="secondary">
                                <CaseActor a={e.g.actor} /> · <Time iso={e.g.created_at} />
                            </Text>
                            <Text as="p">
                                The case did not change state. Window context <span title={e.g.context_snapshot_id}>{short(e.g.context_snapshot_id)}</span>, source record{" "}
                                <span title={e.g.source_event_id}>{short(e.g.source_event_id)}</span>.
                            </Text>
                        </li>
                    ) : (
                        <li key={`c${e.c.id}`}>
                            <strong>Clarification</strong>{" "}
                            <Text size="sm" color="secondary">
                                <CaseActor a={e.c.actor} /> · <Time iso={e.c.created_at} />
                            </Text>
                            <Text as="p" className="clarification">{e.c.body}</Text>
                        </li>
                    ),
                )}
            </ol>
            {!online && <Offline />}
            {c.state === "needs_input" && (site.role === "supervisor" || site.role === "operator") && (
                <ClarifyForm c={c} base={base} onApplied={applied} onConflict={conflicted} asked={[...c.transitions].reverse().find((t) => t.to_state === "needs_input")?.reason ?? null} />
            )}
            {site.role === "supervisor" && !terminal(c.state) && <ReasonAction c={c} base={base} action="cancel" onApplied={applied} onConflict={conflicted} />}
            {site.role === "supervisor" && terminal(c.state) && (
                <ReasonAction c={c} base={base} action="reopen" onApplied={(v) => navigate(`/sites/${site.id}/cases/${v.id}`)} onConflict={conflicted} />
            )}
            </VStack>
        </section>
    );
}

type ActionProps = { c: CaseV1; base: string; onApplied: (v: CaseV1, text: string) => void; onConflict: (e: unknown) => void };

function ClarifyForm({ c, base, onApplied, onConflict, asked }: ActionProps & { asked: string | null }) {
    const { online } = useSession();
    const [body, setBody] = useState("");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const intents = useRef(new Intents()).current;
    const tooLong = lengthError(body, 4000);
    async function submit(e: React.FormEvent) {
        e.preventDefault();
        if (tooLong) return;
        setBusy(true);
        setError(null);
        try {
            const v = await mutate<CaseV1>(intents, "clarify", `${base}/clarifications`, { expected_revision: c.revision, body, new_context_id: null }, c.revision);
            setBody("");
            onApplied(v, "Your clarification was recorded as case evidence.");
        } catch (err) {
            setError(err);
            onConflict(err);
        } finally {
            setBusy(false);
        }
    }
    return (
        <Card padding={4}>
            <form onSubmit={submit} aria-labelledby="clarify-title">
                <VStack gap={2}>
                    <Heading level={2} id="clarify-title">Answer the open question</Heading>
                    <Text as="p">Asked: {asked ? <ReasonText code={asked} /> : "no reason was recorded"}</Text>
                    <TextArea
                        label="Clarification"
                        description="Stored as attributed case evidence; the investigation resumes. It never edits an observation."
                        isRequired
                        status={tooLong}
                        value={body}
                        onChange={(v) => setBody(v)}
                        rows={4}
                    />
                    <Problem error={error} />
                    <div>
                        <Button type="submit" variant="primary" label={busy ? "Recording…" : "Record clarification"} isDisabled={busy || !online || !body.trim() || !!tooLong} />
                    </div>
                </VStack>
            </form>
        </Card>
    );
}

// ReasonAction is cancel (an active case) or reopen (a resolved or canceled case): a supervisor
// with a recent sign-in, a required reason, and the consequence stated before the click.
function ReasonAction({ c, base, action, onApplied, onConflict }: ActionProps & { action: "cancel" | "reopen" }) {
    const { online } = useSession();
    const [reason, setReason] = useState("");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const intents = useRef(new Intents()).current;
    const copy =
        action === "cancel"
            ? { title: "Cancel this case", button: "Cancel case", done: "The case was canceled.", consequence: "The investigation stops and no proposal from this episode can be approved. A task already created is not undone; cancel it separately." }
            : { title: "Reopen as a new episode", button: "Reopen case", done: "A new episode was opened.", consequence: "This episode's history stays as it is; a new episode starts from detection." };
    const tooLong = lengthError(reason, 2000);
    async function submit(e: React.FormEvent) {
        e.preventDefault();
        if (tooLong) return;
        setBusy(true);
        setError(null);
        try {
            const v = await mutate<CaseV1>(intents, action, `${base}/${action}`, { expected_revision: c.revision, reason }, c.revision);
            setReason("");
            onApplied(v, copy.done);
        } catch (err) {
            setError(err);
            onConflict(err);
        } finally {
            setBusy(false);
        }
    }
    const id = `case-${action}`;
    return (
        <Card padding={4}>
            <form onSubmit={submit} aria-labelledby={`${id}-title`}>
                <VStack gap={2}>
                    <Heading level={2} id={`${id}-title`}>{copy.title}</Heading>
                    <Text as="p" size="sm" color="secondary">
                        {copy.consequence} Applies to revision {c.revision}; if the case has changed, nothing is submitted and the current revision is reloaded.
                    </Text>
                    <TextArea label="Reason" isRequired status={tooLong} value={reason} onChange={(v) => setReason(v)} rows={2} />
                    <Problem error={error} />
                    <div>
                        <Button
                            type="submit"
                            variant={action === "cancel" ? "destructive" : "primary"}
                            label={busy ? "Submitting…" : copy.button}
                            isDisabled={busy || !online || !reason.trim() || !!tooLong}
                        />
                    </div>
                </VStack>
            </form>
        </Card>
    );
}
