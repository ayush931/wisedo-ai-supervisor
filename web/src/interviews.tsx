import React, { useCallback, useEffect, useRef, useState } from "react";
import type {
    AgentTurnV1,
    ConfigurationDraftRequestInputV1,
    ConfigurationDraftRequestV1,
    ConfigurationDraftRunV1,
    EvidenceSourcePageV1,
    InterviewAnswerV1,
    InterviewSessionPageV1,
    InterviewSessionV1,
    KnowledgeDiscoveryInventoryPageV1,
    QuestionPageV1,
} from "../../contracts/typescript/oee";
import { allPages, AmbiguousError, ApiError, Intents, classify, isConflict, mutate, request } from "./api";
import { label } from "./labels";
import { draftLink } from "./master-data";
import { ProposedFacts } from "./proposed-facts";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Field } from "@astryxdesign/core/Field";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextArea } from "@astryxdesign/core/TextArea";
import { Token } from "@astryxdesign/core/Token";
import { VStack } from "@astryxdesign/core/VStack";
import { Link, Problem, Reasons, Status, Time, Val, canSupervise, lengthError, short, usePolling, useSession } from "./ui";

// WIS-271 interviews: a participant answers role questions, may ask the agent for follow-up
// questions, and may ask it to draft candidate configuration. Every model-worded string is
// untrusted (rule 08) and renders as plain React text, never HTML or a link. A run's outcome is
// shown only from the server's own run record; nothing is reported as done before that (WIS-278).

type Question = QuestionPageV1["questions"][number];
type Kind = ConfigurationDraftRequestInputV1["kinds"][number];
type Source = EvidenceSourcePageV1["sources"][number];

const kinds: { kind: Kind; text: string }[] = [
    { kind: "line", text: "Lines" },
    { kind: "item", text: "Items" },
    { kind: "routing", text: "Routings" },
    { kind: "workstation_profile", text: "Workstation profiles" },
];
const maxSources = 20;
const pollMs = 2000;
// A run the server has not finished after this long is past its own 10-minute activity bound plus
// the finalizer; the buttons come back and polling slows, while the server's reaper closes the run.
const overdueMs = 12 * 60_000;
const slowPollMs = 30_000;

// The last run per session survives a page refresh so a running poll can resume (per-viewer only).
function remembered(key: string): string {
    try {
        return sessionStorage.getItem(key) ?? "";
    } catch {
        return "";
    }
}
function remember(key: string, value: string) {
    try {
        sessionStorage.setItem(key, value);
    } catch {}
}

const agentOff = (e: unknown) => e instanceof ApiError && e.problem.status === 503 && e.problem.code === "AGENT_UNAVAILABLE";
// On an active interview, 409 LIFECYCLE_CONFLICT on an agent request means a run is still open.
const runStillOpen = (e: unknown) => e instanceof ApiError && e.problem.status === 409 && e.problem.code === "LIFECYCLE_CONFLICT";
const runStillOpenText = "A previous run is still being closed; try again in a few minutes.";

export function InterviewList() {
    const { site, navigate, online } = useSession();
    const [rows, setRows] = useState<InterviewSessionV1[] | null>(null);
    const [next, setNext] = useState("");
    const [error, setError] = useState<unknown>(null);
    const [createError, setCreateError] = useState<unknown>(null);
    const [inventories, setInventories] = useState<KnowledgeDiscoveryInventoryPageV1["inventories"] | null>(null);
    const [inventoryNext, setInventoryNext] = useState("");
    const [inventoryError, setInventoryError] = useState<unknown>(null);
    const [inventoryLoading, setInventoryLoading] = useState(false);
    // The chosen discovery inventory: a process can have several checklists (WIS-617).
    const [selectedInventory, setSelectedInventory] = useState("");
    const [busy, setBusy] = useState(false);
    const intents = useRef(new Intents()).current;
    const canStart = site.role !== "viewer";

    const loadInventories = useCallback(async (cursor = "") => {
        setInventoryLoading(true);
        setInventoryError(null);
        try {
            const page = await request<KnowledgeDiscoveryInventoryPageV1>(`/v1/sites/${site.id}/discovery-inventories?limit=50${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
            setInventories((prior) => [...(cursor ? prior ?? [] : []), ...page.inventories]);
            setInventoryNext(page.next_cursor ?? "");
            if (!cursor) setSelectedInventory(page.inventories.length === 1 && !page.next_cursor ? page.inventories[0].inventory_id : "");
        } catch (e) {
            setInventoryError(e);
        } finally {
            setInventoryLoading(false);
        }
    }, [site.id]);

    const load = useCallback(async (cursor = "") => {
        setError(null);
        try {
            const page = await request<InterviewSessionPageV1>(`/v1/sites/${site.id}/interviews?limit=50${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
            setRows((prior) => [...(cursor ? prior ?? [] : []), ...page.sessions]);
            setNext(page.next_cursor ?? "");
        } catch (e) {
            setError(e);
        }
    }, [site.id]);
    useEffect(() => {
        void load();
    }, [load]);
    useEffect(() => {
        setInventories(null);
        setInventoryNext("");
        setSelectedInventory("");
        if (canStart) void loadInventories();
    }, [loadInventories, canStart]);

    const startBlocked = busy || !online || inventories === null || (!!inventories.length && !selectedInventory);
    async function start(e: React.FormEvent) {
        e.preventDefault();
        if (startBlocked) return;
        setBusy(true);
        setCreateError(null);
        const chosen = inventories?.find((inventory) => inventory.inventory_id === selectedInventory);
        const body = { role: site.role, ...(chosen ? { process_id: chosen.process_id, inventory_id: chosen.inventory_id } : {}) };
        try {
            const created = await mutate<InterviewSessionV1>(intents, "create", `/v1/sites/${site.id}/interviews`, body);
            navigate(`/sites/${site.id}/interviews/${created.session_id}`);
        } catch (err) {
            setCreateError(err);
        } finally {
            setBusy(false);
        }
    }

    // One option per inventory, in list order: the process and which checklist it covers. A process
    // name shared by two processes carries a short ID so the two stay distinguishable.
    const processNames = new Map<string, string>();
    for (const inventory of inventories ?? []) processNames.set(inventory.process_id, inventory.process_display_name);
    const nameCounts = new Map<string, number>();
    for (const name of processNames.values()) nameCounts.set(name, (nameCounts.get(name) ?? 0) + 1);

    const more = next ? "+" : "";
    return (
        <VStack as="section" gap={3} aria-labelledby="interviews-title">
            <Heading level={1} id="interviews-title">
                Interviews
            </Heading>
            <HStack gap={2} wrap="wrap">
                <Card padding={3} className="kpi">
                    <Text size="sm" color="secondary">Sessions</Text>
                    <strong><Val v={rows ? `${rows.length}${more}` : null} /></strong>
                </Card>
                <Card padding={3} className="kpi">
                    <Text size="sm" color="secondary">Active</Text>
                    <strong><Val v={rows ? `${rows.filter((r) => r.status === "active").length}${more}` : null} /></strong>
                </Card>
            </HStack>
            {canStart ? (
                <form onSubmit={start} aria-label="Start an interview">
                    <HStack gap={2} align="end" wrap="wrap">
                        <HStack gap={1} align="center">
                            <Text color="secondary">Role</Text> <Status code={site.role} />
                        </HStack>
                        {inventories === null ? (
                            inventoryError === null && <Text role="status" color="secondary">Loading process inventories…</Text>
                        ) : inventories.length ? (
                            <Field label="Process inventory" inputID="interview-process-inventory">
                                <select id="interview-process-inventory" required value={selectedInventory} onChange={(e) => setSelectedInventory(e.currentTarget.value)}>
                                    <option value="">Select a process inventory</option>
                                    {(inventories ?? []).map((inventory) => (
                                        <option key={inventory.inventory_id} value={inventory.inventory_id}>
                                            {inventory.process_display_name}
                                            {nameCounts.get(inventory.process_display_name)! > 1 ? ` · ${short(inventory.process_id)}` : ""}
                                            {` — ${label(inventory.capability).text}`}
                                        </option>
                                    ))}
                                </select>
                            </Field>
                        ) : (
                            <Text color="secondary">No process inventories are registered at this site. You can start an unbound interview, but it will have no process-specific questions. To add one, contact a site owner to register an area and process as a discovery inventory, then reload this page.</Text>
                        )}
                        <Button type="submit" variant="primary" label={busy ? "Starting…" : "Start interview"} isDisabled={startBlocked} />
                    </HStack>
                </form>
            ) : (
                <Text color="secondary">Viewers cannot be interviewed. An owner, supervisor or operator starts their own interview.</Text>
            )}
            <Problem error={inventoryError} />
            {inventoryError !== null && inventories === null && <Button label={inventoryLoading ? "Loading…" : "Retry process inventories"} onClick={() => void loadInventories()} isDisabled={inventoryLoading} />}
            {inventoryLoading && inventories !== null && <Text role="status" color="secondary">Loading more process inventories…</Text>}
            {inventoryNext && <Button label="Load more process inventories" onClick={() => void loadInventories(inventoryNext)} isDisabled={inventoryLoading} />}
            <Problem error={createError} />
            <Problem error={error} />
            {!rows && !error && <Text role="status" color="secondary">Loading interviews…</Text>}
            {rows &&
                (rows.length ? (
                    <Table aria-label="Interview sessions" density="compact">
                        <TableHeader>
                            <TableRow isHeaderRow>
                                <TableHeaderCell scope="col">Session</TableHeaderCell>
                                <TableHeaderCell scope="col">Role</TableHeaderCell>
                                <TableHeaderCell scope="col">Status</TableHeaderCell>
                                <TableHeaderCell scope="col">Rev</TableHeaderCell>
                                <TableHeaderCell scope="col">Updated</TableHeaderCell>
                            </TableRow>
                        </TableHeader>
                        <TableBody>
                            {rows.map((s) => (
                                <TableRow key={s.session_id}>
                                    <TableHeaderCell scope="row">
                                        <Link to={`/sites/${site.id}/interviews/${s.session_id}`}>Interview {short(s.session_id)}</Link>
                                    </TableHeaderCell>
                                    <TableCell>{s.role}</TableCell>
                                    <TableCell><Status code={s.status} /></TableCell>
                                    <TableCell>r{s.revision}</TableCell>
                                    <TableCell><Time iso={s.updated_at} /></TableCell>
                                </TableRow>
                            ))}
                        </TableBody>
                    </Table>
                ) : (
                    <Text color="secondary">No interviews yet. Start one above.</Text>
                ))}
            {next && <Button label="Load more" onClick={() => void load(next)} />}
        </VStack>
    );
}

export function InterviewPage({ sessionId }: { sessionId: string }) {
    const { site } = useSession();
    const base = `/v1/sites/${site.id}/interviews/${sessionId}`;
    const [session, setSession] = useState<InterviewSessionV1 | null>(null);
    const [questions, setQuestions] = useState<Question[] | null>(null);
    const [error, setError] = useState<unknown>(null);
    const [refreshing, setRefreshing] = useState(false);
    const [serverAgentUnavailable, setServerAgentUnavailable] = useState(false);
    // Only the latest refresh may write questions or clear `refreshing`: a manual Refresh can
    // overlap the refresh an answer triggers, and pages can land in any order.
    const refreshSeq = useRef(0);
    // The receipt lives here: the answered question moves lists and its row remounts.
    const [saved, setSaved] = useState<InterviewAnswerV1 | null>(null);

    useEffect(() => () => { refreshSeq.current++; }, [base]);

    // Previous data stays on screen while a refresh is in flight.
    const refresh = useCallback(async () => {
        const n = ++refreshSeq.current;
        setRefreshing(true);
        try {
            const [s, q] = await Promise.all([
                request<InterviewSessionV1>(base),
                allPages(async (cursor) => {
                    const page = await request<QuestionPageV1>(`${base}/questions?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
                    return { items: page.questions, next_cursor: page.next_cursor };
                }),
            ]);
            if (n !== refreshSeq.current) return;
            setSession(s);
            setQuestions(q);
            setError(null);
        } catch (e) {
            if (n === refreshSeq.current) setError(e);
        } finally {
            if (n === refreshSeq.current) setRefreshing(false);
        }
    }, [base]);
    const answered = useCallback((a: InterviewAnswerV1) => {
        setSaved(a);
        void refresh();
    }, [refresh]);
    useEffect(() => {
        void refresh();
    }, [refresh]);

    if (!session) return (
        <VStack as="section" gap={2}>
            <Heading level={1}>Interview {short(sessionId)}</Heading>
            <Problem error={error} />
            {!error && <Text role="status" color="secondary">Loading interview…</Text>}
        </VStack>
    );
    const active = session.status === "active";
    const agentUnavailable = !site.agent_available || serverAgentUnavailable;
    const agentUnavailableReason = serverAgentUnavailable && site.agent_available
        ? "Agent support is currently unavailable. Template questions and answers still work; follow-ups and drafts are unavailable."
        : site.agent_unavailable_reason_code === "AGENT_NOT_CONFIGURED"
            ? "Agent support is not configured. Template questions and answers still work; follow-ups and drafts are unavailable."
            : "Agent mode is disabled. Template questions and answers still work; follow-ups and drafts are unavailable.";
    const open = (questions ?? []).filter((q) => q.answer_state === "unanswered");
    const done = (questions ?? []).filter((q) => q.answer_state !== "unanswered");
    return (
        <VStack as="section" gap={3} aria-labelledby="interview-title">
            <HStack justify="between" align="center" gap={2} wrap="wrap">
                <VStack gap={1}>
                    <Heading level={1} id="interview-title">
                        Interview {short(session.session_id)}
                    </Heading>
                    <HStack gap={1} align="center" wrap="wrap">
                        <Status code={session.status} />
                        <Text size="sm" color="secondary">
                            {session.role} interview · revision {session.revision} · updated <Time iso={session.updated_at} />
                            {refreshing && <span role="status"> · refreshing…</span>}
                        </Text>
                    </HStack>
                </VStack>
                <Button label="Refresh" onClick={() => void refresh()} isDisabled={refreshing} />
            </HStack>
            <Problem error={error} />
            {!active && <Banner status="warning" title={`This interview is ${session.status}. Answers and agent requests are closed.`} />}
            <Heading level={2}>
                Open questions <Text color="secondary">{questions ? open.length : "…"}</Text>
            </Heading>
            {saved && (
                <div role="status">
                    <Banner status="success" title={`Answer saved as evidence revision ${saved.source_revision}.`} />
                </div>
            )}
            {questions && !open.length && (
                <Text color="secondary">
                    {session.process_id ? "No open questions right now." : "This interview is not bound to a process inventory, so there are no discovery questions."}
                </Text>
            )}
            <VStack as="ol" gap={2} style={bare}>
                {open.map((q) => <QuestionItem key={q.question_id} q={q} base={base} active={active} onSaved={answered} onConflict={refresh} />)}
            </VStack>
            {done.length > 0 && (
                <details>
                    <summary>Answered or dismissed ({done.length})</summary>
                    <VStack as="ol" gap={2} style={bare}>
                        {done.map((q) => <QuestionItem key={q.question_id} q={q} base={base} active={false} onSaved={answered} onConflict={refresh} />)}
                    </VStack>
                </details>
            )}
            <Heading level={2}>Agent</Heading>
            {!canSupervise(site.role) ? (
                <Text color="secondary">Asking the agent for questions or drafts needs the owner or supervisor role at this site.</Text>
            ) : agentUnavailable ? (
                <div role="status">
                    <Banner status="warning" title={agentUnavailableReason} />
                </div>
            ) : null}
            {canSupervise(site.role) && (
                <>
                    <FollowUp session={session} base={base} disabled={!active || agentUnavailable} onOff={() => setServerAgentUnavailable(true)} onDone={refresh} onConflict={refresh} />
                    <DraftRequest session={session} disabled={!active || agentUnavailable} onOff={() => setServerAgentUnavailable(true)} onConflict={refresh} />
                </>
            )}
        </VStack>
    );
}

const bare = { listStyle: "none", padding: 0, margin: 0 } as const;
const plain = { border: 0, padding: 0, margin: 0, minWidth: 0 } as const;

function QuestionItem({ q, base, active, onSaved, onConflict }: { q: Question; base: string; active: boolean; onSaved: (a: InterviewAnswerV1) => void; onConflict: () => void }) {
    const { online } = useSession();
    const [text, setText] = useState("");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const intents = useRef(new Intents()).current;
    // One client_answer_id per typed answer: a retry of the same text reuses it.
    const answerID = useRef<{ text: string; id: string } | null>(null);
    const length = [...text].length;

    async function save(e: React.FormEvent) {
        e.preventDefault();
        if (answerID.current?.text !== text) answerID.current = { text, id: crypto.randomUUID() };
        const body = { question_id: q.question_id, question_revision: q.revision, text, client_answer_id: answerID.current.id };
        setBusy(true);
        setError(null);
        try {
            const receipt = await mutate<InterviewAnswerV1>(intents, "answer", `${base}/answers`, body);
            answerID.current = null;
            setText("");
            onSaved(receipt);
        } catch (err) {
            setError(err);
            // A stale question revision (412) or a closed interview (409): reload, keep the text.
            if (isConflict(err)) onConflict();
        } finally {
            setBusy(false);
        }
    }

    return (
        <li>
            <Card padding={3} className="interview-question">
                <VStack gap={2}>
                    <Text as="p" weight="semibold" className="interview-prompt">
                        {q.prompt}
                    </Text>
                    <HStack gap={1} align="center" wrap="wrap">
                        {q.prompt_origin === "agent" ? (
                            <span title="Worded by the agent from the deterministic requirement; check it against the source">
                                <Token label="AI-worded" color="orange" size="sm" icon={<span aria-hidden="true">✦</span>} />
                            </span>
                        ) : (
                            <span title="Deterministic discovery template">
                                <Token label="Template" color="gray" size="sm" icon={<span aria-hidden="true">▤</span>} />
                            </span>
                        )}
                        <Status code={q.answer_state} /> <Status code={`question_${q.reason}`} />
                        <Text size="sm" color="secondary">
                            {q.source_span_ids.length > 0 && <span> · cites {q.source_span_ids.length} source span{q.source_span_ids.length === 1 ? "" : "s"}</span>}
                            <span className="identifier"> · requirement {short(q.requirement_id)} · r{q.revision}</span>
                        </Text>
                    </HStack>
                    {active && q.answer_state === "unanswered" && (
                        <form onSubmit={save}>
                            <VStack gap={1}>
                                <TextArea label="Your answer" value={text} rows={3} onChange={(v) => setText(v)} />
                                <HStack justify="between" align="center" gap={2}>
                                    <Text size="sm" color="secondary" hasTabularNumbers>
                                        {length}/8000
                                    </Text>
                                    {lengthError(text, 8000) && (
                                        <Text size="sm" role="alert" style={{ color: "var(--color-text-error)" }}>
                                            {lengthError(text, 8000)!.message}
                                        </Text>
                                    )}
                                    <Button type="submit" variant="primary" label={busy ? "Saving…" : "Save answer"} isDisabled={busy || !online || !text.trim() || !!lengthError(text, 8000)} />
                                </HStack>
                                <Problem error={error} />
                            </VStack>
                        </form>
                    )}
                </VStack>
            </Card>
        </li>
    );
}

// RunStatus is the server's run record: state or outcome as text + icon, and the reason codes.
function RunStatus({ run, what }: { run: { state: string; outcome_status?: string; outcome_reasons: readonly string[] }; what: string }) {
    const code = run.state === "running" ? "running" : run.outcome_status || run.state;
    return (
        <Text as="p" role="status" aria-live="polite" size="sm" color="secondary">
            {what}: <Status code={code} /> <Reasons codes={run.outcome_reasons} />
            {run.outcome_reasons.length > 0 && run.state !== "running" && <Text color="primary"> {reasonSentence(run.outcome_reasons[0])}</Text>}
        </Text>
    );
}

// A readable sentence for the first reason; the code stays visible next to it.
const reasonSentences: Record<string, string> = {
    TURN_ACTIVITY_FAILED: "The run did not finish in time and was stopped. Nothing was saved.",
    NO_CONFIGURATION_DRAFTS: "The agent found nothing it could draft with citations.",
    NO_NEW_INTERVIEW_OUTPUT: "The agent had no new questions.",
    DRAFT_KIND_FORBIDDEN: "The agent proposed a kind you did not ask for, so the whole output was refused.",
    CITATION_NOT_IN_SCOPE: "The agent cited something outside this interview's sources, so the whole output was refused.",
    FIELD_UNCITED: "A drafted field had no citation, so the whole output was refused.",
    SCHEMA_INVALID: "The agent's output did not pass validation, so it was refused.",
    EVIDENCE_REVOKED: "A source was revoked while the run was working; nothing was saved.",
    BUDGET_EXHAUSTED: "The run used its budget before finishing; nothing was saved.",
    QUESTION_LIMIT: "The agent proposed more questions than allowed, so the output was refused.",
    REQUIREMENT_NOT_IN_SCOPE: "The agent asked about a requirement outside this interview, so the output was refused.",
    RUN_CANCELED: "The run was canceled.",
    SPECIALIST_FAILED: "The model call failed, so nothing was proposed or saved.",
    TOOL_NOT_ALLOWED: "The agent asked for a tool it is not allowed to use, so the run was stopped.",
};
const reasonSentence = (code: string) => reasonSentences[code] ?? "";

const retryable = (e: unknown) => e instanceof AmbiguousError || (e instanceof ApiError && classify(e.problem) === "retry");

// useRun polls one run until it is terminal. A previous run's record or error is never shown for a
// new ID; a reply older than the latest request is dropped, so a late "running" cannot overwrite a
// finished run. A non-retryable error stops polling, a 404 calls onGone, and an overdue run stops
// blocking the buttons.
function useRun<T extends { run_id: string; state: string }>(path: string, onGone: () => void) {
    const [latest, setLatest] = useState<{ path: string; run: T } | null>(null);
    const [failure, setFailure] = useState<{ path: string; error: unknown } | null>(null);
    const [since, setSince] = useState<{ path: string; at: number } | null>(null);
    const [now, setNow] = useState(() => Date.now());
    const sequence = useRef(0);
    const run = latest?.path === path ? latest.run : null;
    const error = failure?.path === path ? failure.error : null;
    const startedAt = since?.path === path ? since.at : null;
    const overdue = startedAt !== null && now - startedAt > overdueMs;
    const refresh = useCallback(() => {
        if (!path) return;
        const n = ++sequence.current;
        request<T>(path).then(
            (r) => {
                if (n !== sequence.current) return;
                const at = Date.now();
                setLatest({ path, run: r });
                setFailure(null);
                setNow(at);
                setSince((prior) => (prior?.path === path ? prior : { path, at }));
            },
            (e: unknown) => {
                if (n !== sequence.current) return;
                setFailure({ path, error: e });
                if (e instanceof ApiError && e.problem.status === 404) onGone();
            },
        );
    }, [path, onGone]);
    const live = !!path && !(error && !retryable(error)) && (!run || run.state === "running");
    usePolling(refresh, live ? (overdue ? slowPollMs : pollMs) : null);
    return { run, error, running: live && !overdue, overdue, minutes: startedAt === null ? 0 : Math.floor((now - startedAt) / 60_000) };
}

// AgentProblem names an open previous run plainly instead of the generic conflict text.
function AgentProblem({ error, active }: { error: unknown; active: boolean }) {
    if (active && runStillOpen(error))
        return (
            <div role="status">
                <Banner status="warning" title={runStillOpenText} />
            </div>
        );
    return <Problem error={error} />;
}

// RunWatch shows one run as the server reports it, or why it cannot be shown.
function RunWatch({ what, runID, gone, watch, empty }: {
    what: string;
    runID: string;
    gone: string;
    watch: { run: { state: string; outcome_status?: string; outcome_reasons: readonly string[] } | null; error: unknown; overdue: boolean; minutes: number };
    empty: string;
}) {
    const { run, error, overdue, minutes } = watch;
    return (
        <>
            {gone && <Text as="p" role="status" size="sm" color="secondary">{what} {short(gone)} is no longer available to you. You can start a new one.</Text>}
            {runID && run && <RunStatus run={run} what={`${what} ${short(runID)}`} />}
            {runID && !run && !error && <Text as="p" role="status" size="sm" color="secondary">Checking run {short(runID)}…</Text>}
            {runID && overdue && run?.state === "running" && (
                <div role="status">
                    <Banner status="warning" title={`Still running after ${minutes} min. The server closes a stuck run with a reason, which can take up to 30 minutes; a new request may be refused until then.`} />
                </div>
            )}
            {runID && run && run.state !== "running" && empty && <Text as="p" size="sm" color="secondary">{empty}</Text>}
            <Problem error={error} />
        </>
    );
}

function FollowUp({ session, base, disabled, onOff, onDone, onConflict }: { session: InterviewSessionV1; base: string; disabled: boolean; onOff: () => void; onDone: () => void; onConflict: () => void }) {
    const { online } = useSession();
    const memory = `supervisor.interview.turn.${session.session_id}`;
    const [runID, setRunID] = useState(() => remembered(memory));
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const intents = useRef(new Intents()).current;
    const requestID = useRef("");
    const [gone, setGone] = useState("");
    const forget = useCallback(() => {
        remember(memory, "");
        setGone(runID);
        setRunID("");
    }, [memory, runID]);
    const watch = useRun<AgentTurnV1>(runID ? `${base}/agent-turns/${runID}` : "", forget);
    const { run, running } = watch;
    const finished = useRef("");
    useEffect(() => {
        if (run && run.state !== "running" && finished.current !== run.run_id) {
            finished.current = run.run_id;
            onDone();
        }
    }, [run, onDone]);

    async function ask() {
        requestID.current ||= crypto.randomUUID();
        setBusy(true);
        setError(null);
        try {
            const started = await mutate<AgentTurnV1>(intents, "turn", `${base}/agent-turns`, { request_id: requestID.current }, session.revision);
            requestID.current = "";
            remember(memory, started.run_id);
            setGone("");
            setRunID(started.run_id);
        } catch (e) {
            if (agentOff(e)) onOff();
            else {
                setError(e);
                // 409 (a run already in progress, or the interview closed) and 412 both reload.
                if (isConflict(e)) onConflict();
            }
        } finally {
            setBusy(false);
        }
    }

    return (
        <VStack gap={2} align="start">
            <Button label={busy ? "Asking…" : "Ask agent for follow-up questions"} onClick={ask} isDisabled={disabled || busy || running || !online} />
            <AgentProblem error={error} active={!disabled} />
            <RunWatch what="Follow-up run" runID={runID} gone={gone} watch={watch}
                empty={run && run.question_ids.length > 0 ? `${run.question_ids.length} new question${run.question_ids.length === 1 ? "" : "s"} added above, labelled AI-worded.` : "No new questions were added."} />
            {runID && run && run.state !== "running" && <ProposedFacts entityIDs={run.entity_ids} claimIDs={run.claim_ids} />}
        </VStack>
    );
}

function DraftRequest({ session, disabled, onOff, onConflict }: { session: InterviewSessionV1; disabled: boolean; onOff: () => void; onConflict: () => void }) {
    const { site, navigate, online } = useSession();
    const memory = `supervisor.interview.drafts.${session.session_id}`;
    const [chosen, setChosen] = useState<Set<Kind>>(new Set(["line"]));
    const [sources, setSources] = useState<Source[] | null>(null);
    const [sourceNext, setSourceNext] = useState("");
    const [sourceLoading, setSourceLoading] = useState(false);
    const [sourceError, setSourceError] = useState<unknown>(null);
    const [picked, setPicked] = useState<Set<string>>(new Set());
    const [runID, setRunID] = useState(() => remembered(memory));
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const intents = useRef(new Intents()).current;
    const [gone, setGone] = useState("");
    const sourceRequest = useRef(0);
    const forget = useCallback(() => {
        remember(memory, "");
        setGone(runID);
        setRunID("");
    }, [memory, runID]);
    const watch = useRun<ConfigurationDraftRunV1>(runID ? `/v1/sites/${site.id}/configuration-draft-requests/${runID}` : "", forget);
    const { run, running } = watch;

    const loadSources = useCallback(async (cursor = "") => {
        const requestID = ++sourceRequest.current;
        setSourceLoading(true);
        setSourceError(null);
        try {
            const page = await request<EvidenceSourcePageV1>(`/v1/sites/${site.id}/sources?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
            if (requestID !== sourceRequest.current) return;
            // A source that shifted across pages keeps its first position (one checkbox per key).
            setSources((prior) => {
                const kept = cursor ? prior ?? [] : [];
                const seen = new Set(kept.map((x) => x.source_id));
                return [...kept, ...page.sources.filter((x) => !seen.has(x.source_id) && seen.add(x.source_id))];
            });
            setSourceNext(page.next_cursor ?? "");
        } catch (e) {
            if (requestID === sourceRequest.current) setSourceError(e);
        } finally {
            if (requestID === sourceRequest.current) setSourceLoading(false);
        }
    }, [site.id]);

    useEffect(() => {
        void loadSources();
        return () => { sourceRequest.current++; };
    }, [loadSources, session.session_id]);

    const toggle = <T,>(set: Set<T>, value: T) => {
        const next = new Set(set);
        if (!next.delete(value)) next.add(value);
        return next;
    };

    const draftBlocked = disabled || busy || running || !online || chosen.size === 0;
    async function draft(e: React.FormEvent) {
        e.preventDefault();
        if (draftBlocked) return;
        const body: ConfigurationDraftRequestInputV1 = { session_id: session.session_id, source_ids: [...picked].sort(), kinds: kinds.map((k) => k.kind).filter((k) => chosen.has(k)) };
        setBusy(true);
        setError(null);
        try {
            const started = await mutate<ConfigurationDraftRequestV1>(intents, "drafts", `/v1/sites/${site.id}/configuration-draft-requests`, body);
            remember(memory, started.run_id);
            setGone("");
            setRunID(started.run_id);
        } catch (err) {
            if (agentOff(err)) onOff();
            else {
                setError(err);
                // 409 (a run already in progress, or the interview closed): reload; choices stay.
                if (isConflict(err)) onConflict();
            }
        } finally {
            setBusy(false);
        }
    }

    function openDraft(id: string) {
        draftLink.open = id;
        navigate(`/sites/${site.id}/master-data`);
    }

    return (
        <form onSubmit={draft} aria-labelledby="draft-request-title">
            <Card padding={3}>
                <VStack gap={3} align="start">
                    <Heading level={3} id="draft-request-title">
                        Draft configuration from this interview
                    </Heading>
                    <fieldset style={plain}>
                        <legend>
                            <Text weight="semibold">What to draft</Text>
                        </legend>
                        <HStack gap={3} wrap="wrap">
                            {kinds.map((k) => (
                                <CheckboxInput key={k.kind} label={k.text} value={chosen.has(k.kind)} onChange={() => setChosen(toggle(chosen, k.kind))} />
                            ))}
                        </HStack>
                    </fieldset>
                    <fieldset style={plain}>
                        <legend>
                            <Text weight="semibold">Extra sources</Text>{" "}
                            <Text size="sm" color="secondary" hasTabularNumbers>
                                (this interview's answers are always included; {picked.size}/{maxSources} selected)
                            </Text>
                        </legend>
                        <VStack gap={1}>
                            <Problem error={sourceError} />
                            {!sources && !sourceError && <Text role="status" size="sm" color="secondary">Loading sources…</Text>}
                            {sourceLoading && sources !== null && <Text role="status" size="sm" color="secondary">Loading more sources…</Text>}
                            {sourceError !== null && sources === null && <Button label="Retry sources" onClick={() => void loadSources()} isDisabled={sourceLoading} />}
                            {sources && !sources.length && !sourceNext && <Text size="sm" color="secondary">No other sources at this site.</Text>}
                            <VStack gap={1} style={{ maxHeight: "14rem", overflow: "auto" }}>
                                {sources?.map((s) => (
                                    <CheckboxInput
                                        key={s.source_id}
                                        label={`${s.title} ${short(s.source_id)} · r${s.revision}${s.model_extracted ? " · model-extracted text (OCR)" : ""}`}
                                        value={picked.has(s.source_id)}
                                        isDisabled={!picked.has(s.source_id) && picked.size >= maxSources}
                                        onChange={() => setPicked(toggle(picked, s.source_id))}
                                    />
                                ))}
                            </VStack>
                            {sourceNext && (
                                <Button label={sourceLoading ? "Loading…" : sourceError ? "Retry sources" : "Load more sources"} onClick={() => void loadSources(sourceNext)} isDisabled={sourceLoading} />
                            )}
                        </VStack>
                    </fieldset>
                    <HStack gap={2} align="center">
                        <Button type="submit" variant="primary" label={busy ? "Requesting…" : "Draft configuration"} isDisabled={draftBlocked} />
                        {chosen.size === 0 && <Text size="sm" color="secondary">Choose at least one kind.</Text>}
                    </HStack>
                    <AgentProblem error={error} active={!disabled} />
                    <RunWatch what="Draft run" runID={runID} gone={gone} watch={watch} empty={run && run.draft_ids.length === 0 ? "No drafts were created." : ""} />
                    {run && run.draft_ids.length > 0 && (
                        <VStack as="ul" gap={1} aria-label="Drafts created by this run" style={bare}>
                            {run.draft_ids.map((id) => (
                                <li key={id}>
                                    <HStack gap={1} align="center">
                                        <Text>
                                            Draft <span className="identifier">{short(id)}</span>
                                        </Text>
                                        <Button label={`Open draft ${short(id)} in Drafts`} variant="ghost" size="sm" onClick={() => openDraft(id)} />
                                    </HStack>
                                </li>
                            ))}
                        </VStack>
                    )}
                </VStack>
            </Card>
        </form>
    );
}
