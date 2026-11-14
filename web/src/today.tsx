import { CorrectOutput, CorrectionCount } from "./corrections";
import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type {
    CasePageV1,
    ChatConversationV1,
    ChatMessagePageV1,
    ChatPostResultV1,
    ProposalPageV1,
    ProposalV1,
    ShiftPlanPageV1,
    ShiftPlanV1,
    ShiftSummarySnapshotV1,
} from "../../contracts/typescript/oee";
import { Badge } from "@astryxdesign/core/Badge";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Grid } from "@astryxdesign/core/Grid";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { ProgressBar } from "@astryxdesign/core/ProgressBar";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Token } from "@astryxdesign/core/Token";
import { VStack } from "@astryxdesign/core/VStack";
import { allPages, formatInterval, Intents, isConflict, loadShiftPlans, mutate, request } from "./api";
import { CaseState } from "./cases";
import { label } from "./labels";
import { latestSummary } from "./shifts";
import { Announce, Link, Name, Problem, Readiness, Time, Val, canSupervise, short, usePolling, useSession } from "./ui";

// WIS-278 Today: what needs the signed-in person now, from server data only (rule 06). Every
// section loads and fails on its own; a failed section never reads as "nothing to do".

type Plan = ShiftPlanPageV1["plans"][number];
type Loaded<T> = { items: T[] | null; error: unknown };
const empty = <T,>(): Loaded<T> => ({ items: null, error: null });
const terminalCase = (state: string) => state === "resolved" || state === "canceled";
export const awaitingApproval = (p: Plan) => p.approved_revision === null || p.approved_revision !== p.current_revision;
const isLive = (p: Plan, now: number) => Date.parse(p.starts_at) <= now && now < Date.parse(p.ends_at);

async function settle<T>(run: () => Promise<T[]>): Promise<Loaded<T>> {
    try {
        return { items: await run(), error: null };
    } catch (error) {
        return { items: null, error };
    }
}
function loadPendingProposals(site: string) {
    return allPages(async (cursor) => {
        const page = await request<ProposalPageV1>(`/v1/sites/${site}/proposals?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
        return { items: page.items, next_cursor: page.next_cursor ?? undefined };
    }).then((items) => items.filter((p) => p.state === "pending"));
}
function loadOpenCases(site: string) {
    return allPages(async (cursor) => {
        const page = await request<CasePageV1>(`/v1/sites/${site}/cases?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
        return { items: page.items, next_cursor: page.next_cursor ?? undefined };
    }).then((items) => items.filter((c) => !terminalCase(c.state)));
}

// useNow re-renders every intervalMs so countdowns stay honest without a reload.
function useNow(intervalMs: number) {
    const [now, setNow] = useState(() => Date.now());
    useEffect(() => {
        const t = setInterval(() => setNow(Date.now()), intervalMs);
        return () => clearInterval(t);
    }, [intervalMs]);
    return now;
}

function greeting(name: string | null) {
    const h = new Date().getHours();
    const part = h < 12 ? "Good morning" : h < 17 ? "Good afternoon" : "Good evening";
    const first = name?.trim().split(/\s+/)[0];
    return first ? `${part}, ${first}.` : `${part}.`;
}

// Only a site supervisor may decide a proposal (approvals PolicyRevision 1); an owner may not.
export const canDecide = (role: string) => role === "supervisor";
type Pending = ProposalPageV1["items"][number];
const expired = (p: Pending, now: number) => Date.parse(p.expires_at) <= now;
const tzOf = (d?: ShiftPlanV1 | "unavailable") => (d && d !== "unavailable" ? d.revisions[d.current_revision - 1].plan.timezone : undefined);

// usePlanDetails reads each listed plan's detail once per revision: the list carries no
// timezone, and times are shown in the plan's own zone, never the browser's.
function usePlanDetails(site: string, plans: Plan[]) {
    const [details, setDetails] = useState<Record<string, ShiftPlanV1 | "unavailable">>({});
    const key = plans.map((p) => `${p.id}:${p.current_revision}`).join(",");
    useEffect(() => {
        let active = true;
        for (const p of plans)
            request<ShiftPlanV1>(`/v1/sites/${site}/shift-plans/${p.id}`).then(
                (d) => active && setDetails((m) => ({ ...m, [p.id]: d })),
                () => active && setDetails((m) => ({ ...m, [p.id]: "unavailable" })),
            );
        return () => {
            active = false;
        };
    }, [site, key]); // plans is derived from key
    return details;
}

// useDecided keeps the server-confirmed decisions of this visit and moves focus to the newest.
function useDecided() {
    const [decided, setDecided] = useState<string[]>([]);
    const last = useRef<HTMLDivElement>(null);
    useEffect(() => {
        if (decided.length) last.current?.focus();
    }, [decided.length]);
    const list = decided.map((d, i) => (
        <div key={i} role="status" tabIndex={-1} ref={i === decided.length - 1 ? last : undefined}>
            <Banner status="success" title={`✓ ${d}`} />
        </div>
    ));
    return { list, add: (text: string) => setDecided((d) => [...d, text]) };
}

// Decisions: proposals the viewer can act on, then (Today only) open cases, then plans awaiting
// approval, then proposals that expired undecided. Shared by Today and Approvals.
function useDecisions(withCases: boolean) {
    const { site } = useSession();
    const [plans, setPlans] = useState<Loaded<Plan>>(empty);
    const [proposals, setProposals] = useState<Loaded<Pending>>(empty);
    const [cases, setCases] = useState<Loaded<CasePageV1["items"][number]>>(empty);
    const generation = useRef(0);
    const refresh = useCallback(async () => {
        const mine = ++generation.current;
        const [pl, pr, cs] = await Promise.all([
            settle(() => loadShiftPlans(site.id)),
            settle(() => loadPendingProposals(site.id)),
            withCases ? settle(() => loadOpenCases(site.id)) : Promise.resolve({ items: [], error: null }),
        ]);
        if (mine !== generation.current) return;
        setPlans(pl);
        setProposals(pr);
        setCases(cs);
    }, [site.id, withCases]);
    usePolling(useCallback(() => void refresh(), [refresh]), 30000);
    const now = useNow(15000);
    const planRows = plans.items?.filter((p) => awaitingApproval(p)) ?? [];
    const open = proposals.items?.filter((p) => !expired(p, now)) ?? [];
    const lapsed = proposals.items?.filter((p) => expired(p, now)) ?? [];
    const details = usePlanDetails(site.id, [...(plans.items?.filter((p) => isLive(p, now)) ?? []), ...planRows]);
    // Count only what this person can act on: a proposal needs a supervisor, a plan approval
    // an owner or supervisor; open cases are counted for everyone.
    const loaded = plans.items && proposals.items && cases.items;
    const count = loaded ? (canDecide(site.role) ? open.length : 0) + cases.items!.length + (canSupervise(site.role) ? planRows.length : 0) : null;
    return { plans, proposals, cases, planRows, open, lapsed, details, count, loaded, refresh, now };
}

function DecisionList({ d, decided, empty: emptyText }: { d: ReturnType<typeof useDecisions>; decided: ReturnType<typeof useDecided>; empty: string }) {
    const { site } = useSession();
    const onDecided = (text: string) => {
        decided.add(text);
        void d.refresh();
    };
    return (
        <>
            {decided.list}
            {!canDecide(site.role) && d.open.length > 0 && (
                <Text as="p" size="sm" color="secondary">
                    Only a site supervisor can approve or reject proposals; they are shown here for information.
                </Text>
            )}
            <Problem error={d.proposals.error} context="Proposals could not be loaded." />
            {d.open.map((p) => (
                <ProposalCard key={p.id} id={p.id} onDecided={onDecided} />
            ))}
            <Problem error={d.cases.error} context="Cases could not be loaded." />
            {d.cases.items?.map((c) => (
                <article key={c.id} aria-label={`Case ${short(c.id)}`}>
                    <Card padding={3}>
                        <VStack gap={2}>
                            <CardTop>
                                <CaseState state={c.state} />
                                <Text weight="semibold">
                                    <Name id={c.line_id} kind="line" />
                                </Text>
                                <span className="push">
                                    <Text size="sm" color="secondary">
                                    Detected <Time iso={c.detected_at} />
                                </Text>
                                </span>
                            </CardTop>
                            <Heading level={3}>Production shortfall detected</Heading>
                            <Text as="p" size="sm" color="secondary">
                                <Name id={c.order_id} kind="order" prefixed /> · two consecutive short windows, latest ending <Time iso={c.latest_window_end} />
                            </Text>
                            <HStack justify="end">
                                <Button label="Open case" size="sm" href={`/sites/${site.id}/cases/${c.id}`} />
                            </HStack>
                        </VStack>
                    </Card>
                </article>
            ))}
            <Problem error={d.plans.error} context="Shift plans could not be loaded." />
            {d.planRows.map((p) => (
                <PlanApprovalCard key={p.id} plan={p} detail={d.details[p.id]} />
            ))}
            {d.count === 0 && !d.open.length && !d.planRows.length && (
                <Card variant="muted" padding={3}>
                    <Text as="p" color="secondary">
                        {emptyText}
                    </Text>
                </Card>
            )}
            <div role="status">
                <Announce text={!d.loaded && !d.proposals.error && !d.cases.error && !d.plans.error ? "Loading…" : ""} />
            </div>
            {!d.loaded && !d.proposals.error && !d.cases.error && !d.plans.error && <Skeleton height={120} />}
            {d.lapsed.length > 0 && (
                <VStack as="section" aria-labelledby="lapsed-title" gap={2}>
                    <Heading level={3} id="lapsed-title">
                        Expired without a decision
                    </Heading>
                    {d.lapsed.map((p) => (
                        <ProposalCard key={p.id} id={p.id} onDecided={onDecided} />
                    ))}
                </VStack>
            )}
        </>
    );
}

// CardTop is a card's first row: status pills and names on the left, a time pushed right.
function CardTop({ children }: { children: React.ReactNode }) {
    return (
        <HStack align="center" gap={2} wrap="wrap">
            {children}
        </HStack>
    );
}

// Kpi is one number tile. Unknown (a section that failed or is still loading) is "—", never 0.
function Kpi({ label: text, n }: { label: string; n: number | null }) {
    return (
        <Card padding={3} className="kpi">
            <Text size="sm" color="secondary">
                {text}
            </Text>
            <strong>
                <Val v={n} />
            </strong>
        </Card>
    );
}

export function Today() {
    const { site, me } = useSession();
    const d = useDecisions(true);
    const decided = useDecided();
    const live = d.plans.items?.filter((p) => isLive(p, d.now)) ?? [];
    const windows = [...new Set(live.flatMap((p) => (tzOf(d.details[p.id]) ? [formatInterval(p.starts_at, p.ends_at, tzOf(d.details[p.id]))] : [])))];
    return (
        <VStack gap={5} maxWidth={880}>
            <VStack gap={1}>
                <Text size="sm" color="secondary">
                    {site.name}
                    {windows.length ? ` · ${windows.join(" · ")}` : d.plans.items && !live.length ? " · No shift in progress" : ""}
                </Text>
                <Heading level={1}>
                    {greeting(me.display_name)}{" "}
                    {d.count === null ? "" : d.count === 0 ? "Nothing needs you right now." : `${d.count} ${d.count === 1 ? "thing needs" : "things need"} you.`}
                </Heading>
            </VStack>

            <Grid columns={{ minWidth: 150 }} gap={2}>
                <Kpi label="Proposals" n={d.proposals.items ? d.open.length : null} />
                <Kpi label="Open cases" n={d.cases.items ? d.cases.items.length : null} />
                <Kpi label="Plans to approve" n={d.plans.items ? d.planRows.length : null} />
                <Kpi label="Lines live" n={d.plans.items ? live.length : null} />
            </Grid>

            <CorrectionCount />
            {(canSupervise(site.role) || site.role === "operator") && <Ask />}

            <VStack as="section" aria-labelledby="needs-title" gap={2}>
                <Heading level={2} id="needs-title">
                    Needs you
                </Heading>
                <DecisionList
                    d={d}
                    decided={decided}
                    empty="Nothing waiting. Proposals that need your decision, open shortfall cases and shift plans awaiting approval will appear here."
                />
            </VStack>

            <VStack as="section" aria-labelledby="lines-title" gap={2}>
                <Heading level={2} id="lines-title">
                    Lines on shift
                </Heading>
                {d.plans.items && !live.length && (
                    <Card variant="muted" padding={3}>
                        <Text as="p" color="secondary">
                            No shift plan covers the current time. Lines on shift will appear here with their latest summary.
                        </Text>
                    </Card>
                )}
                <Grid columns={{ minWidth: 260 }} gap={2}>
                    {live.map((p) => (
                        <LiveLine key={p.id} plan={p} detail={d.details[p.id]} />
                    ))}
                </Grid>
            </VStack>
            <Text size="sm">
                <Link to={`/sites/${site.id}/connection`}>Connection check</Link>
            </Text>
        </VStack>
    );
}

// ApprovalsPage: the decisions waiting on the signed-in person, nothing else.
export function ApprovalsPage() {
    const d = useDecisions(false);
    const decided = useDecided();
    return (
        <VStack gap={4} maxWidth={880}>
            <Heading level={1}>{d.count === null ? "Approvals" : d.count ? `${d.count} waiting on you` : "Nothing waiting on you"}</Heading>
            <VStack as="section" gap={2} aria-label="Waiting for a decision">
                <DecisionList d={d} decided={decided} empty="Proposals waiting for a decision and shift plans awaiting approval will appear here." />
            </VStack>
        </VStack>
    );
}

// A shift plan awaiting approval links to its page, where approval (and the self-approval
// refusal) already lives.
function PlanApprovalCard({ plan, detail }: { plan: Plan; detail?: ShiftPlanV1 | "unavailable" }) {
    const { site } = useSession();
    const tz = tzOf(detail);
    return (
        <article aria-label={`Shift plan ${short(plan.id)} awaiting approval`}>
            <Card padding={3}>
                <VStack gap={2}>
                    <CardTop>
                        <Badge
                            variant="warning"
                            label={
                                <>
                                    <span aria-hidden="true">◐</span> Plan awaiting approval
                                </>
                            }
                        />
                        <Text weight="semibold">
                            <Name id={plan.line_id} kind="line" />
                        </Text>
                        <span className="push">
                            <Text size="sm" color="secondary">
                            rev {plan.current_revision}
                        </Text>
                        </span>
                    </CardTop>
                    <Heading level={3}>Shift · {tz ? formatInterval(plan.starts_at, plan.ends_at, tz) : detail === "unavailable" ? "window unavailable" : "…"}</Heading>
                    <Text as="p" size="sm" color="secondary">
                        {plan.approved_revision === null ? "Not approved yet." : `Revision ${plan.approved_revision} is approved; revision ${plan.current_revision} is not.`}
                    </Text>
                    <HStack justify="end">
                        <Button label="Review" size="sm" variant="primary" href={`/sites/${site.id}/shifts/${plan.id}`} />
                    </HStack>
                </VStack>
            </Card>
        </article>
    );
}

type Payload = { action_kind?: string; template?: string; instructions?: string; line_id?: string; order_id?: string; due_at?: string };
// The canonical payload is the server's envelope {schema, schema_version, tenant_id, site_id,
// body}. Anything else is not shown as a decidable action (rule 06).
function actionOf(raw: unknown): Payload | null {
    const env = raw as { schema?: unknown; schema_version?: unknown; body?: unknown } | null;
    if (!env || env.schema !== "ApprovedActionV1" || env.schema_version !== 1 || !env.body || typeof env.body !== "object") return null;
    return env.body as Payload;
}

// ProposalCard shows one pending recovery proposal and takes a human decision. The server
// checks eligibility (approver is not the submitter, recent sign-in) and decides; success is
// shown only from its response.
function ProposalCard({ id, onDecided }: { id: string; onDecided: (text: string) => void }) {
    const { site, online } = useSession();
    const path = `/v1/sites/${site.id}/proposals/${id}`;
    const [p, setP] = useState<ProposalV1 | null>(null);
    const [loadError, setLoadError] = useState<unknown>(null);
    const [reason, setReason] = useState("");
    const [missing, setMissing] = useState(false);
    const [busy, setBusy] = useState("");
    const [error, setError] = useState<unknown>(null);
    const intents = useMemo(() => new Intents(), []);
    const now = useNow(15000);
    const load = useCallback(() => {
        request<ProposalV1>(path).then(
            (v) => {
                setP(v);
                setLoadError(null);
            },
            setLoadError,
        );
    }, [path]);
    useEffect(load, [load]);
    if (!p) return loadError ? <Problem error={loadError} context="A proposal could not be loaded." /> : <Skeleton height={120} aria-label="Loading proposal" />;
    const action = actionOf(p.canonical_payload);
    const payload = action ?? {};
    const left = Date.parse(p.expires_at) - now;
    const minutes = Math.ceil(left / 60000);
    async function decide(decision: "approve" | "reject") {
        if (!reason.trim()) {
            setMissing(true);
            document.getElementById(`reason-${id}`)?.focus();
            return;
        }
        setMissing(false);
        setBusy(decision);
        setError(null);
        try {
            const v = await mutate<ProposalV1>(intents, "decide", `${path}/decisions`, { decision, expected_revision: p!.revision, reason: reason.trim() }, p!.revision);
            setP(v);
            onDecided(`${decision === "approve" ? "Approved" : "Rejected"}: ${payload.instructions ?? `proposal ${short(id)}`} · server state ${label(v.state).text.toLowerCase()}`);
        } catch (e) {
            setError(e);
            if (isConflict(e)) load();
        } finally {
            setBusy("");
        }
    }
    const title = payload.action_kind === "maintenance_investigation.create" ? "Proposed inspection task (report only)" : `Proposed ${payload.action_kind ?? "action"}`;
    return (
        <article aria-labelledby={`proposal-${id}`}>
            <Card padding={3}>
                <VStack gap={2}>
                    <CardTop>
                        <Badge
                            variant="warning"
                            label={
                                <>
                                    <span aria-hidden="true">◆</span> {title}
                                </>
                            }
                        />
                        {payload.line_id && (
                            <Text weight="semibold">
                                <Name id={payload.line_id} kind="line" />
                            </Text>
                        )}
                        <span className="push" title={p.expires_at}>
                            <Text size="sm" color="secondary" hasTabularNumbers>
                                {left > 0 ? `Expires in ${minutes} min` : "Expired"}
                            </Text>
                        </span>
                    </CardTop>
                    <Heading level={3} id={`proposal-${id}`}>
                        {action ? (payload.instructions ?? "No instructions in the proposal.") : `Proposal ${short(id)}`}
                    </Heading>
                    {!action && (
                        <div role="alert">
                            <Banner status="warning" title="This proposal's content is not a recognised approved action, so it cannot be decided here. Open the case to review it." />
                        </div>
                    )}
                    <Text as="p" size="sm" color="secondary">
                        {payload.order_id && (
                            <>
                                <Name id={payload.order_id} kind="order" prefixed /> ·{" "}
                            </>
                        )}
                        {payload.due_at && (
                            <>
                                Due <Time iso={payload.due_at} /> ·{" "}
                            </>
                        )}
                        <Link to={`/sites/${site.id}/cases/${p.case_id}`}>Open case</Link>
                    </Text>
                    {action && p.state === "pending" && left > 0 && !canDecide(site.role) && (
                        <Text as="p" size="sm" color="secondary">
                            Only a site supervisor can approve or reject this proposal.
                        </Text>
                    )}
                    {action && canDecide(site.role) && p.state === "pending" && left > 0 && (
                        <VStack gap={1}>
                            <TextInput
                                id={`reason-${id}`}
                                label="Reason for your decision"
                                value={reason}
                                {...{ maxLength: 2000 }}
                                status={missing ? { type: "error" } : undefined}
                                aria-describedby={missing ? `reason-${id}-error` : undefined}
                                onChange={(v) => setReason(v)}
                                isDisabled={!!busy}
                            />
                            {missing && (
                                <p id={`reason-${id}-error`} role="alert" className="field-error">
                                    Enter a reason before you approve or reject.
                                </p>
                            )}
                            <HStack justify="between" align="center" gap={2} wrap="wrap">
                                <Text size="sm" color="secondary">
                                    The server refuses an approval from whoever raised this proposal.
                                </Text>
                                <HStack gap={2}>
                                <Button label="Reject" isDisabled={!online || !!busy} onClick={() => decide("reject")}>
                                    {busy === "reject" ? "Rejecting…" : "Reject"}
                                </Button>
                                <Button label="Approve" variant="primary" isDisabled={!online || !!busy} onClick={() => decide("approve")}>
                                    {busy === "approve" ? "Approving…" : "Approve"}
                                </Button>
                                </HStack>
                            </HStack>
                        </VStack>
                    )}
                    <Problem error={error} />
                </VStack>
            </Card>
        </article>
    );
}

// LiveLine: one in-progress shift plan and its latest summary as generated by the server.
function LiveLine({ plan, detail }: { plan: Plan; detail?: ShiftPlanV1 | "unavailable" }) {
    const { site, online } = useSession();
    const base = `/v1/sites/${site.id}/shift-plans/${plan.id}`;
    const [summary, setSummary] = useState<ShiftSummarySnapshotV1 | null | undefined>(undefined);
    const [error, setError] = useState<unknown>(null);
    const [busy, setBusy] = useState(false);
    const [notice, setNotice] = useState("");
    const intents = useMemo(() => new Intents(), []);
    useEffect(() => {
        let live = true;
        latestSummary(base).then(
            (s) => live && setSummary(s),
            (e) => live && setError(e),
        );
        return () => {
            live = false;
        };
    }, [base]);
    const tz = tzOf(detail);
    async function update() {
        setBusy(true);
        setError(null);
        setNotice("");
        try {
            const s = await mutate<ShiftSummarySnapshotV1>(intents, "summary", `${base}/summaries`, { schema_version: 1 });
            setSummary(s);
            setNotice(`${s.replayed ? "Already generated" : "Generated"} summary r${s.revision}`);
        } catch (e) {
            setError(e);
        } finally {
            setBusy(false);
        }
    }
    const totals = summary ? lineTotals(summary.summary.stations) : null;
    const pct = totals && totals.good !== null && totals.planned ? Math.min(100, Math.round((totals.good / totals.planned) * 100)) : null;
    return (
        <article aria-label={`Line on shift ${short(plan.id)}`}>
            <Card padding={3}>
                <VStack gap={2}>
                    <CardTop>
                        <Text weight="semibold">
                            <Name id={plan.line_id} kind="line" />
                        </Text>
                        {awaitingApproval(plan) && (
                            <span title="This plan revision is not approved">
                                <Badge
                                    variant="warning"
                                    label={
                                        <>
                                            <span aria-hidden="true">◐</span> Not approved
                                        </>
                                    }
                                />
                            </span>
                        )}
                        {summary && <Readiness value={summary.summary.readiness} reasons={summary.summary.reasons} />}
                        <span className="push">
                            <Text size="sm" color="secondary">
                            {tz ? formatInterval(plan.starts_at, plan.ends_at, tz) : detail === "unavailable" ? "Shift window unavailable" : "…"}
                        </Text>
                        </span>
                    </CardTop>
                    <div role="status">
                        <Announce text={summary === undefined && !error ? "Loading summary…" : ""} />
                    </div>
                    {summary === undefined && !error && <Skeleton height={64} />}
                    {summary === null && (
                        <Text as="p" color="secondary">
                            No summary yet. Recorded output stays unknown until one is generated.
                        </Text>
                    )}
                    {summary && totals && (
                        <>
                            <p className="big-line">
                                <strong>
                                    <Val v={totals.good} />
                                </strong>{" "}
                                <Text color="secondary">
                                    of <Val v={totals.planned} /> planned
                                    {totals.good === null && " · good not reported for every station"}
                                </Text>
                            </p>
                            <Text as="p" size="sm" color="secondary">
                                Sum of good parts recorded at each station, not line output.
                            </Text>
                            {pct !== null && <ProgressBar label="Good against planned" isLabelHidden value={pct} max={100} />}
                            <Text as="p" size="sm" color="secondary">
                                Jobs: {summary.summary.jobs.completed} of {summary.summary.jobs.planned} completed · {summary.summary.jobs.in_progress} in progress
                                {summary.summary.jobs.unknown > 0 && ` · ${summary.summary.jobs.unknown} not reported`} · as of <Time iso={summary.summary.as_of} tz={tz} />
                            </Text>
                        </>
                    )}
                    {notice && (
                        <div role="status">
                            <Banner status="success" title={`✓ ${notice}`} />
                        </div>
                    )}
                    <Problem error={error} />
                    <HStack justify="between" align="center" gap={2}>
                        <Button label="Open shift" size="sm" variant="ghost" href={`/sites/${site.id}/shifts/${plan.id}`} />
                        {canSupervise(site.role) && (
                            <Button label="Update summary" size="sm" isDisabled={!online || busy} onClick={update}>
                                {busy ? "Updating…" : "Update summary"}
                            </Button>
                        )}
                    </HStack>
                </VStack>
            </Card>
        </article>
    );
}

// lineTotals sums a summary's stations. One station with an unknown value makes the total
// unknown (null): a missing count is never added as zero (rule 06).
export function lineTotals(stations: ShiftSummarySnapshotV1["summary"]["stations"]): { good: number | null; planned: number | null } {
    if (!stations.length) return { good: null, planned: null };
    let good: number | null = 0;
    let planned: number | null = 0;
    for (const s of stations) {
        good = good === null || s.recorded.good_quantity === null ? null : good + s.recorded.good_quantity;
        planned = planned === null || s.planned === null ? null : planned + s.planned.target_quantity;
    }
    return { good, planned };
}

// Chat replies that are not answers say why, plainly. Unknown codes show their label.
const chatReason: Record<string, string> = {
    ASSISTANT_DISABLED: "The assistant is turned off for this site.",
    ASSISTANT_NOT_QUALIFIED: "The assistant has not passed qualification yet, so it does not answer.",
    MODE_DISABLED: "Answers are turned off for this site.",
    NO_APPROVED_PROCESS: "There is no approved process snapshot to answer from yet.",
    ANSWERS_BUSY: "Two answers are already being prepared at this site. Ask again once one of them has finished.",
    CAPTURE_LIMIT: "The approved evidence for this question is too large to read in one answer.",
    BUDGET_EXHAUSTED: "The answer budget for this site is used up.",
    DEADLINE_EXCEEDED: "The answer ran out of time before it was finished. Ask again.",
    MEMBERSHIP_REVOKED: "Your access changed while the answer was prepared.",
};
type Reply = ChatPostResultV1["reply"];
const waiting = (state: string) => state === "pending" || state === "received";

// Ask: approved process questions for owners, supervisors and operators (WIS-703).
function Ask() {
    const { site, online } = useSession();
    const base = `/v1/sites/${site.id}/chat-conversations`;
    const conversation = useRef<ChatConversationV1 | null>(null);
    const unresolved = useRef<{ text: string; request_id: string } | null>(null);
    const intents = useMemo(() => new Intents(), []);
    const [text, setText] = useState("");
    const [asked, setAsked] = useState("");
    const [reply, setReply] = useState<Reply | null>(null);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const [gaveUp, setGaveUp] = useState(false);

    // While the reply is pending, re-read the conversation until the reply settles (bounded).
    useEffect(() => {
        if (!reply || !waiting(reply.state) || !conversation.current) return;
        const id = conversation.current.conversation_id;
        let tries = 0;
        const timer = setInterval(async () => {
            if (++tries > 30) {
                clearInterval(timer);
                setGaveUp(true);
                return;
            }
            try {
                const messages = await allPages(async (cursor) => {
                    const page = await request<ChatMessagePageV1>(`${base}/${id}/messages?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
                    return { items: page.messages, next_cursor: page.next_cursor };
                });
                const next = messages.find((m) => m.message_id === reply.message_id);
                if (next && !waiting(next.state)) {
                    clearInterval(timer);
                    setReply(next as Reply);
                }
            } catch (e) {
                clearInterval(timer);
                setError(e);
            }
        }, 2000);
        return () => clearInterval(timer);
    }, [reply?.message_id, reply?.state, base]);

    async function ask(e: React.FormEvent) {
        e.preventDefault();
        const question = text.trim();
        // Astryx disables with aria-disabled, so Enter still submits: keep the disabled button's guard.
        if (!question || busy || !online) return;
        setBusy(true);
        setError(null);
        setGaveUp(false);
        setAsked(question);
        setReply(null);
        try {
            conversation.current ??= await mutate<ChatConversationV1>(intents, "chat-create", base, { title: "Today" });
            // The same unchanged question keeps its request_id, so a resend after a lost
            // connection is the same intent and cannot post a duplicate.
            if (unresolved.current?.text !== question) unresolved.current = { text: question, request_id: crypto.randomUUID() };
            const body = { request_id: unresolved.current.request_id, text: question };
            const out = await mutate<ChatPostResultV1>(intents, "chat-post", `${base}/${conversation.current.conversation_id}/messages`, body, conversation.current.revision);
            unresolved.current = null;
            conversation.current = out.conversation;
            setReply(out.reply);
            setText("");
        } catch (err) {
            setError(err);
            if (isConflict(err) && conversation.current)
                conversation.current = await request<ChatConversationV1>(`${base}/${conversation.current.conversation_id}`).catch(() => null);
        } finally {
            setBusy(false);
        }
    }
    return (
        <Card padding={4}>
            <VStack as="section" aria-labelledby="ask-title" gap={2}>
                <Heading level={2} id="ask-title">
                    Ask about how the plant runs
                </Heading>
                <Text as="p" size="sm" color="secondary">
                    Answers come from the approved process snapshot, not from live shift data.
                </Text>
                <HStack as="form" gap={2} align="end" onSubmit={ask}>
                    <TextInput
                        label="Your question"
                        isLabelHidden
                        value={text}
                        {...{ maxLength: 4000 }}
                        placeholder="How should a changeover on line 2 be done?"
                        onChange={(v) => setText(v)}
                        isDisabled={busy}
                        containerStyle={{ flex: 1, minWidth: 0 }}
                    />
                    <Button type="submit" label="Ask" variant="primary" isDisabled={busy || !online || !text.trim()}>
                        {busy ? "Asking…" : "Ask"}
                    </Button>
                </HStack>
                <Problem error={error} />
                {asked && reply && (
                    <Card variant="muted" padding={3}>
                        <VStack gap={2} aria-live="polite">
                            <Text size="sm" color="secondary">
                                {asked}
                            </Text>
                            {waiting(reply.state) ? (
                                <Text as="p" role="status">
                                    {gaveUp ? "Still working on it. Ask again later; nothing was lost." : "Working on an answer…"}
                                </Text>
                            ) : reply.state === "answered" ? (
                                <>
                                    <Text as="p">{reply.body}</Text>
                                    {reply.citations.length > 0 && (
                                        <HStack gap={1} wrap="wrap" aria-label="Citations">
                                            {reply.citations.map((c) => (
                                                <span key={`${c.kind}:${c.id}:${c.revision}`} title={`${c.kind} ${c.id} revision ${c.revision}`}>
                                                    <Token size="sm" label={`${c.kind} ${short(c.id)} · rev ${c.revision}`} />
                                                </span>
                                            ))}
                                        </HStack>
                                    )}
                                    {reply.quantities && reply.quantities.length > 0 && (
                                        // WIS-640: the values the server matched to approved claims, shown with the claim's own unit.
                                        <HStack gap={1} wrap="wrap" aria-label="Approved values">
                                            {reply.quantities.map((q) => (
                                                <span key={`${q.statement}:${q.claim_id}:${q.span_id}`} title={`Approved claim ${q.claim_id}, span ${q.span_id}`}>
                                                    <Token size="sm" color="green" label={`${q.value}${q.unit ? ` ${q.unit}` : ""} · ${q.predicate.replaceAll("_", " ")}`} />
                                                </span>
                                            ))}
                                        </HStack>
                                    )}
                                    {reply.corrections?.map((c) => (
                                        <Text as="p" size="sm" key={`${c.claim_id}:${c.revision}`}>
                                            updated after a correction by {c.reviewer} on <Time iso={c.reviewed_at} />
                                        </Text>
                                    ))}
                                    {reply.snapshot_stale && <Text as="p" size="sm" color="secondary">The process snapshot behind this answer is out of date.</Text>}
                                    {reply.retrieval === "word_fallback" && <Text as="p" size="sm" color="secondary">Passages were found by word match only: meaning-based search was not available for this question.</Text>}
                                    {reply.retrieval === "hybrid_unreranked" && <Text as="p" size="sm" color="secondary">Passages were found by meaning-based search, but the reranking step did not answer, so they were not reordered.</Text>}
                                </>
                            ) : (
                                <div role="status" title={reply.reason_code}>
                                    <Text as="p">
                                        No answer: {(reply.reason_code && chatReason[reply.reason_code]) ?? (reply.reason_code ? label(reply.reason_code).text : label(reply.state).text)}
                                    </Text>
                                </div>
                            )}
                            {(reply.state === "answered" || reply.state === "needs_input") && conversation.current && <CorrectOutput key={reply.message_id} subject={{ kind: "answer", conversation_id: conversation.current.conversation_id, message_id: reply.message_id }} question={asked} />}
                        </VStack>
                    </Card>
                )}
            </VStack>
        </Card>
    );
}
