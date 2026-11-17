import React, { useCallback, useRef, useState } from "react";
import type { EvidenceSourceV1, TaskPageV1, TaskV1 } from "../../contracts/typescript/oee";
import { AmbiguousError, ApiError, Intents, allPages, classify, mutate, outcomeText, request } from "./api";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { EmptyState } from "@astryxdesign/core/EmptyState";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextArea } from "@astryxdesign/core/TextArea";
import { VStack } from "@astryxdesign/core/VStack";
import { CaseActor, EntityName, Kpi, Notice, Offline, StatePill, type NoticeT } from "./cases";
import { Actor, Dec, Details, Link, Problem, Readiness, Refreshed, Time, Val, lengthError, short, usePolling, useSession } from "./ui";

// WIS-630 tasks: the internal investigation tasks the deterministic executor creates when a site
// supervisor's approval of a proposal (never by whoever raised or edited it) is consumed (WIS-277). There is no way to create a
// task here. Every state on screen comes from a server response, and the API decides every action;
// the controls only mirror its rules. Completion is the assignee's report; the outcome observation
// is a separate measurement and is never shown as recovery caused by the task (rule 06).

type Summary = TaskPageV1["items"][number];
type TaskEvent = TaskV1["events"][number];
type Outcome = NonNullable<TaskV1["outcome"]>;

// Never colour alone: every state has an icon and words.
const taskStates: Record<string, [text: string, icon: string, tone: string]> = {
    assigned: ["Assigned", "◎", "warn"],
    acknowledged: ["Acknowledged", "◐", "muted"],
    in_progress: ["In progress", "▶", "ok"],
    blocked: ["Blocked", "▲", "bad"],
    completed: ["Completed (reported)", "✓", "ok"],
    canceled: ["Canceled", "■", "muted"],
};
const terminal = (state: string) => state === "completed" || state === "canceled";
const activePollMs = 10_000; // an active task (WIS-278)
const listPollMs = 30_000;

export function TaskState({ state }: { state: string }) {
    const [text, icon, tone] = taskStates[state] ?? [state, "•", "muted"];
    return <StatePill code={state} text={text} icon={icon} tone={tone} />;
}

const eventWords: Record<string, string> = {
    create: "created from the approved action",
    acknowledge: "acknowledged",
    start: "started",
    block: "blocked",
    complete: "completed with a report",
    cancel: "canceled",
    reopen: "reopened for correction",
};

// The outcome is measured from production windows after completion. Its words never claim that
// the task caused a change; observed_improvement is threshold recovery, labelled as an observation.
const outcomeWords: Record<string, [text: string, icon: string, tone: string]> = {
    pending: ["Waiting for windows after completion", "◐", "muted"],
    observed_improvement: ["Recovery observed after completion (not proof the task caused it)", "◆", "ok"],
    no_improvement: ["No recovery observed after completion", "▲", "warn"],
    insufficient_data: ["Not enough trustworthy data to compare", "▲", "warn"],
    not_comparable: ["Not comparable with the case", "■", "muted"],
};

const templateWords: Record<string, string> = { inspect_report_only: "Inspect and report only" };

// isAssignee mirrors the API: the assignee acts only while an active operator or supervisor.
const isAssignee = (t: { assignee_subject: string }, me: string, role: string) => t.assignee_subject === me && (role === "operator" || role === "supervisor");

export function TaskList() {
    const { site, me } = useSession();
    const [rows, setRows] = useState<Summary[] | null>(null);
    const [error, setError] = useState<unknown>(null);
    const [at, setAt] = useState<Date | null>(null);
    const [onlyMine, setOnlyMine] = useState(site.role === "operator");
    const generation = useRef(0);

    // Every refresh walks all pages (stable ID order, next_cursor) so the "assigned to you" filter
    // never hides a task that sits on a later page. A slower answer from an older walk is ignored.
    const refresh = useCallback(async () => {
        const mine = ++generation.current;
        try {
            const items = await allPages<Summary>(async (cursor) => {
                const page = await request<TaskPageV1>(`/v1/sites/${site.id}/tasks?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
                return { items: page.items, next_cursor: page.next_cursor ?? undefined };
            });
            if (mine !== generation.current) return;
            setRows(items);
            setError(null);
            setAt(new Date());
        } catch (e) {
            if (mine === generation.current) setError(e);
        }
    }, [site.id]);
    usePolling(useCallback(() => void refresh(), [refresh]), listPollMs);

    const shown = rows?.filter((t) => !onlyMine || t.assignee_subject === me.subject) ?? null;
    const count = (pick: (state: string) => boolean) => shown?.filter((t) => pick(t.state)).length ?? null;
    return (
        <section aria-labelledby="tasks-title">
            <VStack gap={3}>
                <HStack justify="between" align="center" gap={2} wrap="wrap">
                    <Heading level={1} id="tasks-title">Tasks</Heading>
                    <Refreshed at={at} onRefresh={() => void refresh()} />
                </HStack>
                {site.role === "operator" ? (
                    <Text as="p" size="sm" color="secondary">Showing tasks assigned to you.</Text>
                ) : (
                    <CheckboxInput label="Only tasks assigned to me" value={onlyMine} onChange={(v) => setOnlyMine(v)} />
                )}
                <Problem error={error} />
                {!rows && !error && (
                    <div role="status" aria-label="Loading tasks…">
                        <Skeleton height={240} />
                    </div>
                )}
                {shown && (
                    <HStack gap={2} wrap="wrap">
                        <Kpi label="Open"><Val v={count((s) => !terminal(s))} /></Kpi>
                        <Kpi label="Assigned"><Val v={count((s) => s === "assigned")} /></Kpi>
                        <Kpi label="Blocked"><Val v={count((s) => s === "blocked")} /></Kpi>
                        <Kpi label="Completed"><Val v={count((s) => s === "completed")} /></Kpi>
                    </HStack>
                )}
                {shown &&
                    (shown.length ? (
                        <Table aria-label="Tasks" density="compact">
                            <TableHeader>
                                <TableRow isHeaderRow>
                                    <TableHeaderCell scope="col">Task</TableHeaderCell>
                                    <TableHeaderCell scope="col">State</TableHeaderCell>
                                    <TableHeaderCell scope="col">Assignee</TableHeaderCell>
                                    <TableHeaderCell scope="col">Line</TableHeaderCell>
                                    <TableHeaderCell scope="col">Due</TableHeaderCell>
                                    <TableHeaderCell scope="col">Created</TableHeaderCell>
                                </TableRow>
                            </TableHeader>
                            <TableBody>
                                {shown.map((t) => (
                                    <TableRow key={t.id}>
                                        <TableHeaderCell scope="row">
                                            <Link to={`/sites/${site.id}/tasks/${t.id}`}>
                                                Task · <EntityName id={t.line_id} kind="Line" />
                                            </Link>{" "}
                                            <span className="identifier">{short(t.id)}</span>
                                        </TableHeaderCell>
                                        <TableCell><TaskState state={t.state} /></TableCell>
                                        <TableCell><Actor a={{ subject: t.assignee_subject }} /></TableCell>
                                        <TableCell><EntityName id={t.line_id} kind="Line" /></TableCell>
                                        <TableCell><Time iso={t.due_at} /></TableCell>
                                        <TableCell><Time iso={t.created_at} /></TableCell>
                                    </TableRow>
                                ))}
                            </TableBody>
                        </Table>
                    ) : (
                        <EmptyState
                            title={onlyMine ? "No tasks are assigned to you." : "No tasks"}
                            description={onlyMine ? undefined : "A task is created only when a site supervisor approves a proposal they did not raise or edit."}
                            headingLevel={2}
                        />
                    ))}
            </VStack>
        </section>
    );
}

export function TaskPage({ taskId }: { taskId: string }) {
    const { site, me, online } = useSession();
    const base = `/v1/sites/${site.id}/tasks/${taskId}`;
    const [t, setTask] = useState<TaskV1 | null>(null);
    const [error, setError] = useState<unknown>(null);
    const [at, setAt] = useState<Date | null>(null);
    const [notice, setNotice] = useState<NoticeT>(null);
    const load = useCallback(async () => {
        try {
            const v = await request<TaskV1>(base);
            // A poll that started before a mutation must not put an older revision back on screen.
            setTask((prior) => (prior && prior.id === v.id && v.revision < prior.revision ? prior : v));
            setError(null);
            setAt(new Date());
        } catch (e) {
            setError(e);
        }
    }, [base]);
    const watching = !t || !terminal(t.state) || t.outcome?.status === "pending";
    usePolling(useCallback(() => void load(), [load]), watching ? activePollMs : null);

    if (!t)
        return error ? (
            <Problem error={error} />
        ) : (
            <div role="status" aria-label="Loading task…">
                <Skeleton height={240} />
            </div>
        );

    const applied = (v: TaskV1, text: string) => {
        setTask(v);
        setNotice({ text: `${text} The task is now at revision ${v.revision}.`, ok: true });
        setAt(new Date());
    };
    // A conflict reloads the current revision for review and resubmits nothing. The form that hit
    // it may disappear when the reloaded state no longer offers it, so the notice is page-level.
    const conflicted = (e: unknown, extra = "") => {
        if (!(e instanceof ApiError && (e.problem.status === 412 || e.problem.status === 409))) return;
        setNotice({ text: `${outcomeText.conflict} Nothing was submitted.${extra}`, ok: false });
        void load();
    };

    const assignee = isAssignee(t, me.subject, site.role);
    const supervisor = site.role === "supervisor";
    const props = { t, base, onApplied: applied, onConflict: conflicted };
    return (
        <section aria-labelledby="task-title">
            <VStack gap={3}>
                <HStack justify="between" align="center" gap={2} wrap="wrap">
                    <VStack gap={1}>
                        <Heading level={1} id="task-title">Task {short(t.id)}</Heading>
                        <Text as="p" size="sm" color="secondary">
                            <TaskState state={t.state} /> · revision {t.revision}
                        </Text>
                    </VStack>
                    <Refreshed at={at} onRefresh={() => void load()} />
                </HStack>
                <Problem error={error} context="The task could not be refreshed; the view below may be out of date." />
                <Notice n={notice} />

                <Heading level={2}>Approved action</Heading>
                <Text as="p" className="clarification">{t.description}</Text>
                {/* A native dl: specs select dl[aria-label='Approved action'], and MetadataList puts aria-label on its wrapper div. */}
                <dl className="facts" aria-label="Approved action">
                    <dt>Template</dt>
                    <dd title={t.template}>{templateWords[t.template] ?? t.template}</dd>
                    <dt>Assignee</dt>
                    <dd><Actor a={{ subject: t.assignee_subject }} /></dd>
                    <dt>Asset</dt>
                    <dd><EntityName id={t.asset_id} kind="Asset" /></dd>
                    <dt>Line</dt>
                    <dd><EntityName id={t.line_id} kind="Line" /></dd>
                    <dt>Order</dt>
                    <dd><EntityName id={t.order_id} kind="Order" /></dd>
                    <dt>Case</dt>
                    <dd><Link to={`/sites/${site.id}/cases/${t.case_id}`}>Case {short(t.case_id)}</Link></dd>
                    <dt>Due</dt>
                    <dd><Time iso={t.due_at} /></dd>
                </dl>
                <Text as="p" size="sm" color="secondary">This template asks for inspection and a report. It does not authorize adjusting equipment, releasing quality or restarting production.</Text>
                <Details items={[["Task ID", t.id], ["Action ID", t.action_id], ["Revision", t.revision]]} />

                <Heading level={2}>History</Heading>
                <ol className="timeline" aria-label="Task history">
                    {t.events.map((e) => <HistoryItem key={e.revision} e={e} />)}
                </ol>

                <Heading level={2}>Completion</Heading>
                {t.completions.length ? (
                    <ol className="timeline" aria-label="Completion reports">
                        {t.completions.map((c) => (
                            <li key={c.epoch}>
                                <strong>Report {c.epoch}</strong>{" "}
                                <Text size="sm" color="secondary">
                                    <Actor a={c.actor} /> · <Time iso={c.created_at} /> · at task revision {c.task_revision}
                                </Text>
                                <Text as="p" className="clarification">{c.completion_note}</Text>
                                <Text as="p" size="sm" color="secondary">
                                    Evidence: {c.evidence.map((ev, i) => (
                                        <React.Fragment key={ev.source_id}>
                                            {i > 0 && ", "}
                                            <span title={ev.source_id}>source {short(ev.source_id)} r{ev.revision}</span>
                                        </React.Fragment>
                                    ))}
                                </Text>
                            </li>
                        ))}
                    </ol>
                ) : (
                    <Text as="p" color="secondary">Not completed yet.</Text>
                )}

                <OutcomeView o={t.outcome} completed={t.state === "completed"} />

                {!online && <Offline />}
                {!assignee && !supervisor && !terminal(t.state) && <Text as="p" size="sm" color="secondary">Only the assignee or a site supervisor can change this task.</Text>}
                {assignee && t.state === "assigned" && <Transition {...props} action="acknowledge" />}
                {assignee && t.state === "acknowledged" && <Transition {...props} action="start" />}
                {assignee && t.state === "blocked" && <Transition {...props} action="start" reason />}
                {assignee && t.state === "in_progress" && <CompleteForm {...props} />}
                {supervisor && (t.state === "acknowledged" || t.state === "in_progress") && <Transition {...props} action="block" reason />}
                {supervisor && !terminal(t.state) && <Transition {...props} action="cancel" reason />}
                {supervisor && t.state === "completed" && <Transition {...props} action="reopen" reason />}
            </VStack>
        </section>
    );
}

function HistoryItem({ e }: { e: TaskEvent }) {
    return (
        <li>
            <strong>
                r{e.revision}: {e.from_state ? <><TaskState state={e.from_state} /> → </> : "Created as "}
                <TaskState state={e.to_state} />
            </strong>{" "}
            <Text size="sm" color="secondary">
                <span title={e.event}>{eventWords[e.event] ?? e.event}</span> · <CaseActor a={e.actor} /> · <Time iso={e.created_at} />
            </Text>
            {e.reason && <Text as="p">Reason: {e.reason}</Text>}
        </li>
    );
}

function OutcomeView({ o, completed }: { o: Outcome | null; completed: boolean }) {
    const [text, icon, tone] = o ? (outcomeWords[o.status] ?? [o.status, "•", "muted"]) : ["", "", ""];
    return (
        <section aria-labelledby="outcome-title">
            <Card padding={4}>
                <VStack gap={2}>
                    <Heading level={2} id="outcome-title">Outcome observation</Heading>
                    <Text as="p" size="sm" color="secondary">Measured separately from completion, from the production windows after the report. An observation, not a cause.</Text>
                    {!o ? (
                        <Text as="p">{completed ? "No observation has been recorded yet." : "Observed only after the task is completed."}</Text>
                    ) : (
                        <>
                            <div>
                                <StatePill code={o.status} text={text} icon={icon} tone={tone} />
                            </div>
                            {o.interpretation && <Text as="p">{o.interpretation}</Text>}
                            {o.windows.length > 0 && (
                                <Table aria-label="Outcome windows" density="compact">
                                    <TableHeader>
                                        <TableRow isHeaderRow>
                                            <TableHeaderCell scope="col">Window</TableHeaderCell>
                                            <TableHeaderCell scope="col">Readiness</TableHeaderCell>
                                            <TableHeaderCell scope="col">Expected</TableHeaderCell>
                                            <TableHeaderCell scope="col">Actual</TableHeaderCell>
                                            <TableHeaderCell scope="col">Deficit</TableHeaderCell>
                                        </TableRow>
                                    </TableHeader>
                                    <TableBody>
                                        {o.windows.map((w) => (
                                            <TableRow key={w.start}>
                                                <TableHeaderCell scope="row"><Time iso={w.start} />–<Time iso={w.end} /></TableHeaderCell>
                                                <TableCell><Readiness value={w.readiness} reasons={w.reason_codes} /></TableCell>
                                                <TableCell><Dec v={w.expected_good_units_exact} unit={w.unit} digits={0} /></TableCell>
                                                <TableCell><Dec v={w.actual_good_units_exact} unit={w.unit} digits={0} /></TableCell>
                                                <TableCell><Dec v={w.deficit_good_units_exact} unit={w.unit} digits={0} /></TableCell>
                                            </TableRow>
                                        ))}
                                    </TableBody>
                                </Table>
                            )}
                            <Text as="p" size="sm" color="secondary">Rule {o.rule_version} · observed <Time iso={o.observed_at} /></Text>
                        </>
                    )}
                </VStack>
            </Card>
        </section>
    );
}

type ActionProps = { t: TaskV1; base: string; onApplied: (v: TaskV1, text: string) => void; onConflict: (e: unknown, extra?: string) => void };

const copy: Record<string, { title: string; button: string; done: string; consequence: string }> = {
    acknowledge: { title: "Acknowledge this task", button: "Acknowledge", done: "You acknowledged the task.", consequence: "Tells your supervisor you have seen it. Nothing else changes." },
    start: { title: "Start work", button: "Start", done: "Work on the task started.", consequence: "Marks the task in progress so you can complete it with a report." },
    block: { title: "Block this task", button: "Block task", done: "The task was blocked.", consequence: "The assignee cannot complete it until they restart it with a reason." },
    cancel: { title: "Cancel this task", button: "Cancel task", done: "The task was canceled.", consequence: "The task stops. Its history is kept; it cannot be completed afterwards." },
    reopen: { title: "Reopen for correction", button: "Reopen task", done: "The task was reopened.", consequence: "The earlier report stays in the history; the assignee completes it again with a new report." },
};

function Transition({ t, base, action, reason: needsReason, onApplied, onConflict }: ActionProps & { action: string; reason?: boolean }) {
    const { online } = useSession();
    const [reason, setReason] = useState("");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const intents = useRef(new Intents()).current;
    const c = copy[action];
    const title = action === "start" && needsReason ? "Restart after a block" : c.title;
    const tooLong = needsReason ? lengthError(reason, 2000) : undefined;
    async function submit(e: React.FormEvent) {
        e.preventDefault();
        if (tooLong) return;
        setBusy(true);
        setError(null);
        try {
            const body = needsReason ? { expected_revision: t.revision, reason } : { expected_revision: t.revision };
            const v = await mutate<TaskV1>(intents, action, `${base}/${action}`, body, t.revision);
            setReason("");
            onApplied(v, c.done);
        } catch (err) {
            setError(err);
            onConflict(err);
        } finally {
            setBusy(false);
        }
    }
    const id = `task-${action}`;
    return (
        <Card padding={4}>
            <form onSubmit={submit} aria-labelledby={`${id}-title`}>
                <VStack gap={2}>
                    <Heading level={2} id={`${id}-title`}>{title}</Heading>
                    <Text as="p" size="sm" color="secondary">
                        {c.consequence} Applies to revision {t.revision}; if the task has changed, nothing is submitted and the current revision is reloaded.
                    </Text>
                    {needsReason && <TextArea label="Reason" isRequired status={tooLong} value={reason} onChange={(v) => setReason(v)} rows={2} />}
                    <Problem error={error} />
                    <div>
                        <Button
                            type="submit"
                            variant={action === "cancel" || action === "block" ? "destructive" : "primary"}
                            label={busy ? "Submitting…" : c.button}
                            isDisabled={busy || !online || (needsReason && !reason.trim()) || !!tooLong}
                        />
                    </div>
                </VStack>
            </form>
        </Card>
    );
}

// CompleteForm makes two writes: the report becomes a manual evidence source the assignee wrote
// (POST /sources, a new source at If-Match 0), then complete cites it. If the second write fails,
// the saved report is reused on the next try instead of being written again; changing the report
// text makes a new source.
function CompleteForm({ t, base, onApplied, onConflict }: ActionProps) {
    const { site, online } = useSession();
    const [report, setReport] = useState("");
    const [note, setNote] = useState("");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const [kept, setKept] = useState("");
    const intents = useRef(new Intents()).current;
    const saved = useRef<{ text: string; sourceId: string; created: boolean } | null>(null);
    const reportTooLong = lengthError(report, 20000);
    const noteTooLong = lengthError(note, 4000);
    async function submit(e: React.FormEvent) {
        e.preventDefault();
        if (reportTooLong || noteTooLong) return;
        setBusy(true);
        setError(null);
        setKept("");
        let stage = "report";
        try {
            if (!saved.current || saved.current.text !== report) saved.current = { text: report, sourceId: crypto.randomUUID(), created: false };
            const s = saved.current;
            if (!s.created) {
                await mutate<EvidenceSourceV1>(intents, `report:${s.sourceId}`, `/v1/sites/${site.id}/sources`, { source_id: s.sourceId, kind: "manual", title: `Task ${short(t.id)} report`, text: s.text }, 0);
                s.created = true;
            }
            stage = "complete";
            const v = await mutate<TaskV1>(intents, "complete", `${base}/complete`, { expected_revision: t.revision, completion_note: note, evidence_ids: [s.sourceId] }, t.revision);
            setReport("");
            setNote("");
            saved.current = null;
            onApplied(v, "Your report was recorded and the task is completed.");
        } catch (err) {
            setError(err);
            // A lost answer (connection or 429/5xx) may hide a committed completion: say it is unknown.
            // The key is kept (mutate), so submitting the same report and note again replays it.
            const unknown = err instanceof AmbiguousError || (err instanceof ApiError && classify(err.problem) === "retry");
            const evidence = ` Your report was saved as evidence (source ${short(saved.current!.sourceId)})`;
            const extra =
                stage !== "complete"
                    ? ""
                    : unknown
                      ? `${evidence}. Whether the task was completed is not known: submit the same report and note again to retry safely (it cannot complete twice), or refresh to see the task's state.`
                      : `${evidence} but the task was not completed; it will be reused if you submit the same report again.`;
            setKept(extra.trim());
            onConflict(err, extra);
        } finally {
            setBusy(false);
        }
    }
    return (
        <Card padding={4}>
            <form onSubmit={submit} aria-labelledby="task-complete-title">
                <VStack gap={2}>
                    <Heading level={2} id="task-complete-title">Complete with a report</Heading>
                    <Text as="p" size="sm" color="secondary">
                        Your report is stored as your own evidence and cited by the completion; the outcome is observed separately. Applies to revision {t.revision}.
                    </Text>
                    <TextArea label="Report (what you inspected and found)" isRequired status={reportTooLong} value={report} onChange={(v) => setReport(v)} rows={5} />
                    <TextArea label="Completion note" isRequired status={noteTooLong} value={note} onChange={(v) => setNote(v)} rows={2} />
                    <Problem error={error} />
                    {kept && (
                        <div role="status">
                            <Banner status="warning" title={kept} />
                        </div>
                    )}
                    <div>
                        <Button type="submit" variant="primary" label={busy ? "Submitting…" : "Complete task"} isDisabled={busy || !online || !report.trim() || !note.trim() || !!reportTooLong || !!noteTooLong} />
                    </div>
                </VStack>
            </form>
        </Card>
    );
}
