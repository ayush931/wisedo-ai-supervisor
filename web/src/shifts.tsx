import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type {
    JobOrderPageV1,
    ShiftHandoverPageV1,
    ShiftHandoverRequestV1,
    ShiftHandoverSnapshotV1,
    ShiftPlanBodyV1,
    ShiftPlanPageV1,
    ShiftPlanV1,
    ShiftPlanSuggestionV1,
    ShiftRunEndInputV1,
    ShiftRunStartInputV1,
    ShiftRunV1,
    ShiftSummaryPageV1,
    ShiftSummarySnapshotV1,
    StationRecordDetailV1,
} from "../../contracts/typescript/oee";
import { ApiError, allPages, compareInstant, formatIn, formatInterval, Intents, isConflict, mutate, request, toLocalInput, zonedISO } from "./api";
import { Badge } from "@astryxdesign/core/Badge";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { EmptyState } from "@astryxdesign/core/EmptyState";
import { Field } from "@astryxdesign/core/Field";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { VStack } from "@astryxdesign/core/VStack";
import { Dec, Details, Link, Problem, Readiness, Reasons, Refreshed, StationName, Status, Time, Val, Actor, canSupervise, short, useSession, useStations } from "./ui";

type Assignment = ShiftPlanBodyV1["assignments"][number];
type Station = ShiftSummarySnapshotV1["summary"]["stations"][number];
const unplacedText: Record<string, string> = {
 ROUTING_UNLINKED: "Link this job order to a routing step.",
 ROUTING_NOT_ON_LINE: "The routing step belongs to another line.",
 ROUTING_AMBIGUOUS: "The routing conflicts with approved process knowledge. Review the workstation link.",
 ROUTING_PRECEDENCE_UNSUPPORTED: "The selected operations cannot be ordered. Review duplicate steps or circular dependencies.",
 ROUTING_PREDECESSOR_UNPLACED: "A selected earlier operation could not be placed.",
 STATION_ALREADY_ASSIGNED: "Another selected job already uses this workstation for the shift.",
 CYCLE_TIME_MISSING: "Add a cycle time to this job order.",
 INSTRUCTION_UNAVAILABLE: "Confirm the current instruction document before planning this job.",
 STATION_TEMPLATE_MISMATCH: "The job order and workstation use different forms.",
 SHIFT_CAPACITY_EXCEEDED: "This job and its earlier steps do not fit in the shift.",
 INVALID_DURATION: "Review the target quantity and cycle time.",
};
const suggestionWarningText: Record<string,string> = {
 KNOWLEDGE_CROSS_CHECK_UNAVAILABLE: "No current process snapshot is available for a cross-check. Master-data routings were used.",
 EXTERNAL_PRECEDENCE_NOT_SCHEDULED: "Earlier or related operations outside this selection are not scheduled. Confirm they will be complete when needed.",
 ROUTING_FLOW_OVERLAP_ASSUMED: "Routing steps of one order overlap: each part moves on as soon as it is done, with no buffer limit. Check this matches how parts move on the line.",
 APPROVED_CYCLE_TIME_DIFFERS: "Some job orders differ from the cycle times approved during onboarding. The plan uses the job orders' own values until someone revises them.",
};
// WIS-628: how a job order compares with the cycle time approved during onboarding.
const cycleTimeText: Record<string, string> = {
 differs: "differs from the approved value",
 missing_on_job_order: "has no cycle time; an approved value exists",
 approved_conflict: "approved values disagree; resolve them in knowledge review",
};

type JobOrder = JobOrderPageV1["job_orders"][number];

// Numbers sit right-aligned with tabular digits; inline, because Astryx cell styles outrank legacy CSS.
const num: React.CSSProperties = { textAlign: "end", fontVariantNumeric: "tabular-nums" };
// A fieldset only groups controls for disabling; it draws no box of its own.
const bareFieldset: React.CSSProperties = { border: 0, padding: 0, margin: 0, minWidth: 0 };

// Pill is a state Badge with a text glyph (never colour alone); the wrapper keeps the state code as its tooltip.
function Pill({ tone, title, glyph, children }: { tone: "green" | "orange" | "neutral"; title?: string; glyph?: string; children: React.ReactNode }) {
    return (
        <span title={title}>
            <Badge
                variant={tone}
                label={
                    <>
                        {glyph && <span aria-hidden="true">{glyph} </span>}
                        {children}
                    </>
                }
            />
        </span>
    );
}

// Receipt shows success only from a server reply, next to the action that produced it.
function Receipt({ text }: { text: string }) {
    return (
        <div role="status">
            <Banner status="success" title={`✓ ${text}`} />
        </div>
    );
}
type NextShift = ShiftRunEndInputV1["next_shift"];

// loadRun reads a plan's shift run. A failed or malformed read is null: the page then shows no run
// state and no Start/End, never a guessed "Planned" (the server stays the authority).
export function loadRun(base: string): Promise<ShiftRunV1 | null> {
    return request<ShiftRunV1>(`${base}/run`).then(
        (r) => (r && ["planned", "started", "ended"].includes(r.state) ? r : null),
        () => null,
    );
}

// latestSummary reads every summary page of a plan and returns its newest snapshot, or null when
// none has been generated (recorded values are then unknown, never zero).
export async function latestSummary(base: string): Promise<ShiftSummarySnapshotV1 | null> {
    const sums = await allPages(async (cursor) => {
        const page = await request<ShiftSummaryPageV1>(`${base}/summaries?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
        return { items: page.summaries, next_cursor: page.next_cursor };
    });
    const s = Math.max(0, ...sums.map((x) => x.revision));
    return s ? request<ShiftSummarySnapshotV1>(`${base}/summaries/${s}`) : null;
}

export function ShiftList() {
    const { site } = useSession();
    const [page, setPage] = useState<ShiftPlanPageV1 | null>(null);
    const [cursor, setCursor] = useState("");
    const [error, setError] = useState<unknown>(null);
    useEffect(() => {
        request<ShiftPlanPageV1>(`/v1/sites/${site.id}/shift-plans?limit=50${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`).then(
            setPage,
            setError,
        );
    }, [site.id, cursor]);
    return (
        <VStack gap={3}>
            <HStack justify="between" align="center" gap={2} wrap="wrap">
                <Heading level={1}>Shifts</Heading>
                <HStack gap={2} align="center">
                    <Button label="Job orders" variant="ghost" href={`/sites/${site.id}/job-orders`} />
                    {canSupervise(site.role) && <Button label="New plan" variant="primary" href={`/sites/${site.id}/shifts/new`} />}
                </HStack>
            </HStack>
            <Problem error={error} />
            {!page && !error && <Skeleton height={240} />}
            {page && !page.plans.length && <EmptyState title="No shift plans yet" headingLevel={2} />}
            {page && page.plans.length > 0 && (
                <Table aria-label="Shift plans" density="compact">
                    <TableHeader>
                        <TableRow isHeaderRow>
                            <TableHeaderCell scope="col">Shift</TableHeaderCell>
                            <TableHeaderCell scope="col">Line</TableHeaderCell>
                            <TableHeaderCell scope="col">Approval</TableHeaderCell>
                            <TableHeaderCell scope="col" style={num}>
                                Rev
                            </TableHeaderCell>
                        </TableRow>
                    </TableHeader>
                    <TableBody>
                        {page.plans.map((p) => (
                            <TableRow key={p.id}>
                                <TableCell>
                                    <Link to={`/sites/${site.id}/shifts/${p.id}`}>Shift · {formatInterval(p.starts_at, p.ends_at)}</Link>
                                </TableCell>
                                <TableCell>
                                    <Text size="sm" color="secondary">
                                        <span title={p.line_id}>…{p.line_id.slice(-4)}</span>
                                    </Text>
                                </TableCell>
                                <TableCell>
                                    <HStack gap={1} align="center">
                                        {p.approved_revision === null ? <Status code="draft" prefix="" /> : <Status code="approved" prefix="" />}
                                        {p.approved_revision !== null && (
                                            <Text size="sm" color="secondary">
                                                rev {p.approved_revision}
                                            </Text>
                                        )}
                                    </HStack>
                                </TableCell>
                                <TableCell style={num}>{p.current_revision}</TableCell>
                            </TableRow>
                        ))}
                    </TableBody>
                </Table>
            )}
            {page?.next_cursor && (
                <div>
                    <Button label="Next page" onClick={() => setCursor(page.next_cursor!)} />
                </div>
            )}
        </VStack>
    );
}

export function ShiftNew() {
    const { site, navigate } = useSession();
    return (
        <VStack gap={3}>
            <Heading level={1}>New shift plan</Heading>
            <PlanForm
                submit={(intents, body) => mutate<ShiftPlanV1>(intents, "plan-create", `/v1/sites/${site.id}/shift-plans`, body)}
                onDone={(plan) => navigate(`/sites/${site.id}/shifts/${plan.id}`)}
            />
        </VStack>
    );
}

export function ShiftPage({ shiftId }: { shiftId: string }) {
    const { site, me, online } = useSession();
    const base = `/v1/sites/${site.id}/shift-plans/${shiftId}`;
    const supervise = canSupervise(site.role);
    const [plan, setPlan] = useState<ShiftPlanV1 | null>(null);
    const [summary, setSummary] = useState<ShiftSummarySnapshotV1 | null>(null);
    const [handover, setHandover] = useState<ShiftHandoverSnapshotV1 | null>(null);
    const [loadError, setLoadError] = useState<unknown>(null);
    const [error, setError] = useState<unknown>(null);
    const [notice, setNotice] = useState("");
    const [busy, setBusy] = useState("");
    const [refreshedAt, setRefreshedAt] = useState<Date | null>(null);
    const [run, setRunState] = useState<ShiftRunV1 | null>(null);
    // A run read or receipt bumps the sequence; an older poll answering late is discarded.
    const runSeq = useRef(0);
    const setRun = useCallback((r: ShiftRunV1 | null) => {
        runSeq.current++;
        setRunState(r);
    }, []);
    const [panel, setPanel] = useState<"" | "revise" | "handover" | "end">("");
    const [approving, setApproving] = useState(false);
    const [expanded, setExpanded] = useState<string | null>(null);
    const intents = useMemo(() => new Intents(), []);
    const { list: stationList } = useStations();

    const reload = useCallback(async () => {
        // The run is read on its own: a failed read hides the run controls, not the whole plan.
        const seq = ++runSeq.current;
        loadRun(base).then((r) => seq === runSeq.current && setRunState(r));
        try {
            const [p, sum, hands] = await Promise.all([
                request<ShiftPlanV1>(base),
                latestSummary(base),
                allPages(async (cursor) => {
                    const page = await request<ShiftHandoverPageV1>(`${base}/handovers?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
                    return { items: page.handovers, next_cursor: page.next_cursor };
                }),
            ]);
            setPlan(p);
            const h = Math.max(0, ...hands.map((x) => x.revision));
            setSummary(sum);
            setHandover(h ? await request<ShiftHandoverSnapshotV1>(`${base}/handovers/${h}`) : null);
            setLoadError(null);
        } catch (e) {
            setLoadError(e);
        } finally {
            setRefreshedAt(new Date());
        }
    }, [base]);
    useEffect(() => {
        setRun(null); // another plan: never show the previous plan's run
        reload();
    }, [reload, setRun]);

    async function act<T>(name: string, run: () => Promise<T>, done: (v: T) => void) {
        setBusy(name);
        setError(null);
        setNotice("");
        try {
            done(await run());
        } catch (e) {
            setError(e);
            if (isConflict(e)) reload();
        } finally {
            setBusy("");
        }
    }

    if (!plan)
        return loadError ? <Problem error={loadError} /> : <Skeleton height={240} aria-label="Loading shift plan" />;

    const rev = plan.revisions[plan.current_revision - 1];
    const tz = rev.plan.timezone;
    const draft = rev.state === "draft";
    const stations = new Map<string, Station>((summary?.summary.stations ?? []).map((s) => [s.workstation_id, s]));
    // Shift-level reasons are the roll-up minus what any station already explains (shown on its row).
    const stationReasons = new Set([...stations.values()].flatMap((s) => s.reasons));
    const shiftReasons = summary?.summary.reasons.filter((r) => !stationReasons.has(r));
    const stationLabel = (id: string) => stationList?.find((s) => s.id === id)?.name ?? short(id);
    const disabled = !online || !!busy;
    const sending = (name: string) => (busy === name ? "Sending…" : undefined);

    return (
        <VStack gap={3}>
            <Heading level={1}>Shift · {formatInterval(plan.starts_at, plan.ends_at, tz)}</Heading>
            <HStack gap={2} align="center" wrap="wrap">
                {plan.approved_revision !== null && (
                    <Pill tone="green" title="approved" glyph="●">
                        Approved · rev {plan.approved_revision}
                    </Pill>
                )}
                {draft && (
                    <Pill tone="orange" title="draft" glyph="◐">
                        Draft · rev {rev.revision} awaiting approval
                    </Pill>
                )}
                <RunState run={run} tz={tz} />
                <Text size="sm" color="secondary">
                    Line …{plan.line_id.slice(-4)}
                </Text>
            </HStack>
            {/* Numbers first. No summary means the recorded counts are unknown (—), never zero. */}
            <HStack gap={2} wrap="wrap">
                <Card padding={3} className="kpi">
                    <Text size="sm" color="secondary">Stations planned</Text>
                    <strong><Val v={rev.plan.assignments.length} /></strong>
                </Card>
                <Card padding={3} className="kpi">
                    <Text size="sm" color="secondary">Records</Text>
                    <strong><Val v={summary?.summary.source_record_count} /></strong>
                </Card>
                <Card padding={3} className="kpi">
                    <Text size="sm" color="secondary">Operator notes</Text>
                    <strong><Val v={summary?.summary.notes.length} /></strong>
                </Card>
            </HStack>
            <Details
                items={[
                    ["Plan ID", plan.id],
                    ["Line ID", plan.line_id],
                    ["Timezone", tz],
                    ["State revision", plan.state_revision],
                    ["Current revision", `${plan.current_revision} (${rev.state})`],
                    ["Approved revision", plan.approved_revision ?? "none"],
                    ["Revision digest", rev.canonical_payload_digest],
                ]}
            />
            {supervise && (
                <HStack gap={2} wrap="wrap" role="toolbar" aria-label="Shift actions">
                    {draft && <Button label={`Approve r${rev.revision}`} variant="primary" isDisabled={disabled} onClick={() => setApproving(true)} />}
                    {run?.state === "planned" && plan.approved_revision !== null && (
                        <Button
                            label="Start shift"
                            variant="primary"
                            isDisabled={disabled}
                            onClick={() => {
                                const body: ShiftRunStartInputV1 = { schema_version: 1, plan_revision: plan.approved_revision! };
                                act("start", () => mutate<ShiftRunV1>(intents, "start", `${base}/run`, body, plan.state_revision), (r) => {
                                    setRun(r);
                                    setNotice(`${r.replayed ? "Already started" : "Started"} shift on plan r${r.started_plan_revision}`);
                                    // A replay returns the original stored receipt; the run may have ended since. Read
                                    // the authoritative state (as End does).
                                    reload();
                                });
                            }}
                        >
                            {sending("start")}
                        </Button>
                    )}
                    {run?.state === "started" && (
                        <Button label="End shift" variant="primary" aria-expanded={panel === "end"} isDisabled={!online} onClick={() => setPanel(panel === "end" ? "" : "end")} />
                    )}
                    <Button label="Revise" aria-expanded={panel === "revise"} isDisabled={!online} onClick={() => setPanel(panel === "revise" ? "" : "revise")} />
                    <Button
                        label="Generate summary"
                        isDisabled={disabled}
                        onClick={() =>
                            act("summary", () => mutate<ShiftSummarySnapshotV1>(intents, "summary", `${base}/summaries`, { schema_version: 1 }), (s) => {
                                setSummary(s);
                                setNotice(`${s.replayed ? "Already generated" : "Generated"} summary r${s.revision}`);
                            })
                        }
                    >
                        {sending("summary")}
                    </Button>
                    <Button
                        label="Handover"
                        aria-expanded={panel === "handover"}
                        isDisabled={!online || !summary}
                        tooltip={summary ? undefined : "Generate a summary first"}
                        onClick={() => setPanel(panel === "handover" ? "" : "handover")}
                    />
                </HStack>
            )}
            {notice && <Receipt text={notice} />}
            <Problem error={error} />
            {panel === "revise" && (
                <PlanForm
                    initial={rev.plan}
                    submit={(i, body) => mutate<ShiftPlanV1>(i, "plan-revise", `${base}/revisions`, body, plan.state_revision)}
                    onConflict={reload}
                    onDone={(p) => {
                        setPlan(p);
                        setPanel("");
                        setNotice(`${p.replayed ? "Already saved" : "Saved"} draft r${p.current_revision}`);
                    }}
                />
            )}
            {panel === "handover" && summary && (
                <NextShiftForm
                    plan={plan}
                    title={`Next-shift handover from summary r${summary.revision}`}
                    submit="Generate handover"
                    busy={busy === "handover"}
                    disabled={disabled}
                    onSubmit={(next) =>
                        act("handover", () => mutate<ShiftHandoverSnapshotV1>(intents, "handover", `${base}/handovers`, { schema_version: 1, summary_revision: summary.revision, next_shift: next } satisfies ShiftHandoverRequestV1), (h) => {
                            setHandover(h);
                            setPanel("");
                            setNotice(`${h.replayed ? "Already generated" : "Generated"} handover r${h.revision}`);
                        })
                    }
                />
            )}
            {panel === "end" && run?.state === "started" && (
                <NextShiftForm
                    plan={plan}
                    title="End this shift"
                    submit="End shift"
                    busy={busy === "end"}
                    disabled={disabled}
                    consequences={[
                        "Records the end time and you as the person who ended it.",
                        "Generates the final shift summary and a handover draft for the next shift below. The draft creates no plan.",
                        "Operators can no longer record new entries for this shift.",
                    ]}
                    onSubmit={(next) => {
                        const body: ShiftRunEndInputV1 = { schema_version: 1, next_shift: next };
                        act("end", () => mutate<ShiftRunV1>(intents, "end", `${base}/run/end`, body, plan.state_revision), (r) => {
                            setRun(r);
                            setPanel("");
                            setNotice(`${r.replayed ? "Already ended" : "Ended"} shift · summary r${r.summary?.revision} · handover draft r${r.handover_revision}`);
                            // Show the summary and handover the end wrote (or a later revision, flagged on the run pill).
                            reload();
                        });
                    }}
                />
            )}

            <Heading level={2}>Stations</Heading>
            {summary ? (
                <div className="summaryline">
                    <Text as="p" size="sm" color="secondary">
                        <Status code={summary.summary.readiness} /> Summary r{summary.revision} of plan r{summary.summary.plan_revision} · as of{" "}
                        <Time iso={summary.summary.as_of} tz={tz} /> · {summary.summary.shift_ended ? "shift ended" : "shift not ended"} ·{" "}
                        {summary.summary.source_record_count} records
                    </Text>
                    {plan.approved_revision !== null && summary.summary.plan_revision < plan.approved_revision && (
                        <Pill tone="orange">Plan r{plan.approved_revision} approved after this summary · Generate summary to update</Pill>
                    )}
                    <Reasons codes={shiftReasons} />
                </div>
            ) : (
                <Text as="p" size="sm" color="secondary">
                    No summary generated: recorded columns are unknown.
                </Text>
            )}
            <div className="scroll">
                <Table aria-label="Stations" density="compact">
                    <TableHeader>
                        <TableRow isHeaderRow>
                            <TableHeaderCell scope="col">Workstation</TableHeaderCell>
                            <TableHeaderCell scope="col">Item</TableHeaderCell>
                            <TableHeaderCell scope="col" style={num}>Planned</TableHeaderCell>
                            <TableHeaderCell scope="col" style={num}>Good</TableHeaderCell>
                            <TableHeaderCell scope="col" style={num}>Reject</TableHeaderCell>
                            <TableHeaderCell scope="col" style={num}>Remaining</TableHeaderCell>
                            <TableHeaderCell scope="col" style={num}>Cycle s (mean)</TableHeaderCell>
                            <TableHeaderCell scope="col">Quality (as recorded)</TableHeaderCell>
                            <TableHeaderCell scope="col">State</TableHeaderCell>
                        </TableRow>
                    </TableHeader>
                    <TableBody>
                        {rev.plan.assignments.map((a) => {
                            const s = stations.get(a.workstation_id);
                            const open = expanded === a.workstation_id;
                            return (
                                <React.Fragment key={a.workstation_id}>
                                    <TableRow>
                                        <TableCell>
                                            <HStack gap={1} align="center">
                                                <Button
                                                    label={`Details for workstation ${stationLabel(a.workstation_id)}`}
                                                    variant="ghost"
                                                    size="sm"
                                                    aria-expanded={open}
                                                    onClick={() => setExpanded(open ? null : a.workstation_id)}
                                                >
                                                    {open ? "▾" : "▸"}
                                                </Button>
                                                <Link to={`/sites/${site.id}/workstations/${a.workstation_id}`}>
                                                    <StationName id={a.workstation_id} />
                                                </Link>
                                            </HStack>
                                        </TableCell>
                                        {/* Planned comes from the revision the summary was computed against, not a newer draft. */}
                                        <TableCell>{s ? s.planned?.item_code ?? "not planned" : a.item_code}</TableCell>
                                        <TableCell style={num}>{s ? <Val v={s.planned?.target_quantity} /> : a.target_quantity}</TableCell>
                                        <TableCell style={num}><Val v={s?.recorded.good_quantity} /></TableCell>
                                        <TableCell style={num}><Val v={s?.recorded.reject_quantity} /></TableCell>
                                        <TableCell style={num}><Val v={s?.recorded.remaining_quantity} /></TableCell>
                                        <TableCell style={num}><Dec v={s?.recorded.cycle_time?.mean_seconds} /></TableCell>
                                        <TableCell><Quality s={s} /></TableCell>
                                        <TableCell>
                                            {s ? (
                                                <HStack gap={1} align="center" wrap="wrap">
                                                    <Status code={s.recorded.completion_state} /> <Readiness value={s.readiness} reasons={s.reasons} />
                                                </HStack>
                                            ) : (
                                                <Val v={null} />
                                            )}
                                        </TableCell>
                                    </TableRow>
                                    {open && (
                                        <TableRow>
                                            <TableCell colSpan={9}>
                                                <StationDetail a={a} s={s} tz={tz} />
                                            </TableCell>
                                        </TableRow>
                                    )}
                                </React.Fragment>
                            );
                        })}
                        {/* Stations recorded against this shift but missing from the current revision stay visible. */}
                        {[...stations.values()]
                            .filter((s) => !rev.plan.assignments.some((a) => a.workstation_id === s.workstation_id))
                            .map((s) => (
                                <TableRow key={s.workstation_id}>
                                    <TableCell>
                                        <StationName id={s.workstation_id} />
                                    </TableCell>
                                    <TableCell>{s.planned?.item_code ?? "not planned"}</TableCell>
                                    <TableCell style={num}><Val v={s.planned?.target_quantity} /></TableCell>
                                    <TableCell style={num}><Val v={s.recorded.good_quantity} /></TableCell>
                                    <TableCell style={num}><Val v={s.recorded.reject_quantity} /></TableCell>
                                    <TableCell style={num}><Val v={s.recorded.remaining_quantity} /></TableCell>
                                    <TableCell style={num}><Dec v={s.recorded.cycle_time?.mean_seconds} /></TableCell>
                                    <TableCell><Quality s={s} /></TableCell>
                                    <TableCell>
                                        <Readiness value={s.readiness} reasons={s.reasons} />
                                    </TableCell>
                                </TableRow>
                            ))}
                    </TableBody>
                </Table>
            </div>

            {summary && (
                <>
                    <Heading level={2}>Operator notes · not measured data</Heading>
                    {!summary.summary.notes.length ? (
                        <Text as="p" size="sm" color="secondary">
                            No notes recorded.
                        </Text>
                    ) : (
                        <ul className="rows">
                            {summary.summary.notes.map((n) => (
                                <li key={n.record_id}>
                                    “{n.comment}”
                                    <Text size="sm" color="secondary">
                                        {" "}
                                        · <StationName id={n.workstation_id} /> · <Actor a={n.recorded_by} /> · <Time iso={n.event_at} tz={tz} />
                                        {n.late && " · late"}
                                    </Text>
                                </li>
                            ))}
                        </ul>
                    )}
                </>
            )}

            {handover && (
                <Handover
                    h={handover}
                    tz={tz}
                    canAdopt={supervise && !disabled}
                    busy={busy === "adopt"}
                    onAdopt={() =>
                        act(
                            "adopt",
                            () =>
                                mutate<ShiftPlanV1>(intents, "adopt", `${base}/handovers/${handover.revision}/adopt`, {
                                    canonical_payload_digest: handover.canonical_payload_digest,
                                }),
                            (p) => {
                                setNotice(`Draft plan r${p.current_revision} created for the next shift (not approved)`);
                                reload();
                            },
                        )
                    }
                />
            )}

            <details>
                <summary>Revisions ({plan.revisions.length})</summary>
                <Table aria-label="Revisions" density="compact">
                    <TableHeader>
                        <TableRow isHeaderRow>
                            <TableHeaderCell scope="col" style={num}>Rev</TableHeaderCell>
                            <TableHeaderCell scope="col">State</TableHeaderCell>
                            <TableHeaderCell scope="col">Submitted</TableHeaderCell>
                            <TableHeaderCell scope="col">Approval</TableHeaderCell>
                            <TableHeaderCell scope="col">Digest</TableHeaderCell>
                        </TableRow>
                    </TableHeader>
                    <TableBody>
                        {[...plan.revisions].reverse().map((r) => (
                            <TableRow key={r.revision}>
                                <TableCell style={num}>{r.revision}</TableCell>
                                <TableCell>{r.state}</TableCell>
                                <TableCell>
                                    <Actor a={r.submitted_by} /> · <Time iso={r.created_at} tz={tz} />
                                </TableCell>
                                <TableCell>
                                    {r.approval ? (
                                        <>
                                            <Actor a={r.approval.approver} /> · <Time iso={r.approval.approved_at} tz={tz} />
                                        </>
                                    ) : (
                                        "—"
                                    )}
                                </TableCell>
                                <TableCell className="identifier">{r.canonical_payload_digest.slice(0, 12)}</TableCell>
                            </TableRow>
                        ))}
                    </TableBody>
                </Table>
            </details>
            <Refreshed at={refreshedAt} tz={tz} onRefresh={reload} />
            <Problem error={loadError} />

            {approving && (
                <ApproveDialog
                    plan={plan}
                    me={me.subject}
                    busy={busy === "approve"}
                    disabled={disabled}
                    onClose={() => setApproving(false)}
                    onApprove={() =>
                        act(
                            "approve",
                            () =>
                                mutate<ShiftPlanV1>(
                                    intents,
                                    "approve",
                                    `${base}/approvals`,
                                    { canonical_payload_digest: rev.canonical_payload_digest },
                                    plan.state_revision,
                                ),
                            (p) => {
                                setPlan(p);
                                setApproving(false);
                                setNotice(`${p.replayed ? "Already approved" : "Approved"} r${p.approved_revision}`);
                            },
                        ).finally(() => setApproving(false))
                    }
                />
            )}
        </VStack>
    );
}

// Quality as recorded by operators: measurement count, the latest measurement's values and its
// disposition (read from that immutable record), and separately recorded dispositions. This is a
// record of what was entered, never a quality release.
function Quality({ s }: { s?: Station }) {
    const { site } = useSession();
    const q = s?.recorded.quality;
    const lastId = q?.measurement_record_ids.at(-1);
    const [last, setLast] = useState<StationRecordDetailV1 | null | "unavailable">(null);
    useEffect(() => {
        if (!s || !lastId) return;
        let live = true;
        request<StationRecordDetailV1>(`/v1/sites/${site.id}/workstations/${s.workstation_id}/records/${lastId}`).then(
            (r) => live && setLast(r),
            () => live && setLast("unavailable"),
        );
        return () => {
            live = false;
        };
    }, [site.id, s?.workstation_id, lastId]);
    if (!q) return <Val v={null} />;
    if (!q.measurement_count && !q.dispositions_as_recorded.length)
        return (
            <Text size="sm" color="secondary">
                none recorded
            </Text>
        );
    const parts: string[] = [];
    if (q.measurement_count) parts.push(`${q.measurement_count} measurement${q.measurement_count === 1 ? "" : "s"}`);
    if (last === "unavailable") parts.push("latest value unavailable");
    else if (last) {
        // A measurement form may carry its disposition as a value; show it as recorded, not as a result.
        const b = last.record.record;
        const values = (b.values ?? []).filter((v) => v.field !== "disposition");
        const disposition = b.disposition ?? b.values?.find((v) => v.field === "disposition")?.value;
        if (values.length) parts.push(`last ${values.map((v) => `${v.field} ${v.value}${v.unit ? ` ${v.unit}` : ""}`).join(", ")}`);
        if (disposition) parts.push(`${disposition} (as recorded)`);
    }
    const counts: [string, number][] = [
        ["accept", q.accepts_as_recorded],
        ["hold", q.holds_as_recorded],
        ["reject", q.rejects_as_recorded],
    ];
    for (const [k, n] of counts) if (n) parts.push(`${k} ${n} (as recorded)`);
    return <span title="As recorded by operators; not a quality release">{parts.join(" · ")}</span>;
}

function StationDetail({ a, s, tz }: { a: Assignment; s?: Station; tz: string }) {
    return (
        <MetadataList columns={3}>
            <MetadataListItem label="Job order">
                <span className="identifier">
                    {short(a.job_order_id)} r{a.job_order_revision} · order {short(a.order_id)}
                </span>
            </MetadataListItem>
            <MetadataListItem label="Instruction">
                <span className="identifier">
                    {short(a.instruction_document_id)} r{a.instruction_revision}
                </span>
            </MetadataListItem>
            <MetadataListItem label="Target cycle">{a.target_cycle_time_seconds ? <Dec v={a.target_cycle_time_seconds} unit="s" /> : "none set"}</MetadataListItem>
            {s && (
                <>
                    <MetadataListItem label="Readiness">
                        <Readiness value={s.readiness} reasons={s.reasons} />
                    </MetadataListItem>
                    <MetadataListItem label="Downtime closed">
                        {s.recorded.closed_downtime_seconds} s · {s.recorded.downtime.filter((d) => d.open).length} open
                    </MetadataListItem>
                    <MetadataListItem label="Quality (as recorded)">
                        {s.recorded.quality.measurement_count} measurements · accept {s.recorded.quality.accepts_as_recorded} · hold{" "}
                        {s.recorded.quality.holds_as_recorded} · reject {s.recorded.quality.rejects_as_recorded}
                    </MetadataListItem>
                    <MetadataListItem label="Material issues">
                        {s.recorded.material_exceptions.length
                            ? s.recorded.material_exceptions.map((m) => `${m.material_code} ${m.issue}`).join(", ")
                            : "none recorded"}
                    </MetadataListItem>
                    <MetadataListItem label="Last event">{s.recorded.last_event_at ? <Time iso={s.recorded.last_event_at} tz={tz} /> : "none recorded"}</MetadataListItem>
                </>
            )}
        </MetadataList>
    );
}

function Handover({ h, tz, canAdopt, busy, onAdopt }: { h: ShiftHandoverSnapshotV1; tz: string; canAdopt: boolean; busy: boolean; onAdopt: () => void }) {
    const next = h.handover.next_shift;
    return (
        <>
            {/* The meta line stays the heading row's next sibling <p> (walkthrough reads it that way). */}
            <HStack justify="between" align="center" gap={2} wrap="wrap">
                <Heading level={2}>
                    Handover r{h.revision} → {formatInterval(next.starts_at, next.ends_at, next.timezone)}
                </Heading>
                {h.status === "draft" && h.handover.proposed_plan && (
                    <Button label="Adopt into draft plan" isDisabled={!canAdopt} onClick={onAdopt}>
                        {busy ? "Sending…" : undefined}
                    </Button>
                )}
            </HStack>
            <Text as="p" size="sm" color="secondary">
                <Status code={h.status} />{" "}
                {/* Station reasons are on their rows; the header keeps only shift-level ones. */}
                <Readiness
                    value={h.handover.source.readiness}
                    reasons={h.handover.source.reasons.filter((r) => !h.handover.jobs.some((j) => j.station_reasons.includes(r)))}
                />{" "}
                · from summary r{h.handover.summary_revision} ·
                generated <Time iso={h.generated_at} tz={tz} />
                {h.adoption && ` · adopted as draft rev ${h.adoption.plan_revision}`}
                {!h.handover.proposed_plan && " · no plan proposed"}
            </Text>
            <div className="scroll">
                <Table aria-label="Handover jobs" density="compact">
                    <TableHeader>
                        <TableRow isHeaderRow>
                            <TableHeaderCell scope="col">Workstation</TableHeaderCell>
                            <TableHeaderCell scope="col">State</TableHeaderCell>
                            <TableHeaderCell scope="col" style={num}>Good</TableHeaderCell>
                            <TableHeaderCell scope="col" style={num}>Remaining</TableHeaderCell>
                            <TableHeaderCell scope="col">Carry over</TableHeaderCell>
                            <TableHeaderCell scope="col">Open</TableHeaderCell>
                        </TableRow>
                    </TableHeader>
                    <TableBody>
                        {h.handover.jobs.map((j) => (
                            <TableRow key={j.workstation_id + j.job_order_id}>
                                <TableCell>
                                    <StationName id={j.workstation_id} />
                                </TableCell>
                                <TableCell>
                                    <HStack gap={1} align="center" wrap="wrap">
                                        <Status code={j.completion_state} /> <Readiness value={j.station_readiness} reasons={j.station_reasons} />
                                    </HStack>
                                </TableCell>
                                <TableCell style={num}><Val v={j.good_quantity} /></TableCell>
                                <TableCell style={num}><Val v={j.remaining_quantity} /></TableCell>
                                <TableCell>{j.proposed ? "✓ Carried over" : j.not_proposed_reason ? <Status code={j.not_proposed_reason} /> : "Not proposed"}</TableCell>
                                <TableCell>
                                    {j.open_blockers.length} blockers · {j.material_exceptions.length} material · {j.quality_holds_as_recorded.length} holds
                                    {j.instruction.reason && ` · ${j.instruction.reason}`}
                                </TableCell>
                            </TableRow>
                        ))}
                    </TableBody>
                </Table>
            </div>
        </>
    );
}

// NextShiftForm asks for the next shift's window, defaulting to the same length right after this
// plan. The handover needs it, and so does ending a shift (which writes the handover).
function NextShiftForm({ plan, title, submit, consequences, busy, disabled, onSubmit }: { plan: ShiftPlanV1; title: string; submit: string; consequences?: string[]; busy: boolean; disabled: boolean; onSubmit: (next: NextShift) => void }) {
    const tz = plan.revisions[plan.current_revision - 1].plan.timezone;
    const length = Date.parse(plan.ends_at) - Date.parse(plan.starts_at);
    // The inputs hold whole minutes; round the default start up so it is never before this plan's
    // end (the server refuses a next shift that starts before the summarized one ends).
    // Date.parse keeps only milliseconds; compareInstant checks the exact instant, so an end of
    // 12:00:00.000001Z defaults to 12:01, not 12:00.
    let nextStart = Math.ceil(Date.parse(plan.ends_at) / 60_000) * 60_000;
    if (compareInstant(new Date(nextStart).toISOString(), plan.ends_at) < 0) nextStart += 60_000;
    const [start, setStart] = useState(toLocalInput(new Date(nextStart).toISOString(), tz));
    const [end, setEnd] = useState(toLocalInput(new Date(nextStart + length).toISOString(), tz));
    const [err, setErr] = useState("");
    return (
        <Card padding={4}>
            <form
                aria-label={title}
                onSubmit={(e) => {
                    e.preventDefault();
                    const s = zonedISO(start, tz),
                        f = zonedISO(end, tz);
                    if (Date.parse(f) <= Date.parse(s)) return setErr("End must be after start");
                    setErr("");
                    onSubmit({ line_id: plan.line_id, starts_at: s, ends_at: f, timezone: tz });
                }}
            >
                <VStack gap={3}>
                    <Heading level={2}>{title}</Heading>
                    {consequences && (
                        <ul className="consequences">
                            {consequences.map((c) => (
                                <li key={c}>{c}</li>
                            ))}
                        </ul>
                    )}
                    {err && (
                        <div role="alert">
                            <Banner status="error" title={<a href="#ho-end">{err}</a>} />
                        </div>
                    )}
                    <HStack gap={3} wrap="wrap">
                        <Field label={`Next shift starts (${tz})`} inputID="ho-start">
                            <input id="ho-start" type="datetime-local" required value={start} onChange={(e) => setStart(e.target.value)} />
                        </Field>
                        <Field label="Ends" inputID="ho-end">
                            <input id="ho-end" type="datetime-local" required value={end} onChange={(e) => setEnd(e.target.value)} />
                        </Field>
                    </HStack>
                    <div>
                        <Button type="submit" label={submit} variant="primary" isDisabled={disabled}>
                            {busy ? "Sending…" : undefined}
                        </Button>
                    </div>
                </VStack>
            </form>
        </Card>
    );
}

// RunState shows the shift run as the server last reported it; nothing reads as started or
// ended before the server's receipt says so. No run read means no pill.
function RunState({ run, tz }: { run: ShiftRunV1 | null; tz: string }) {
    if (!run) return null;
    if (run.state === "planned")
        return (
            <Pill tone="neutral" title="planned" glyph="○">
                Planned · not started
            </Pill>
        );
    if (run.state === "started")
        return (
            <Pill tone="green" title="started" glyph="▶">
                Running since <Time iso={run.started_at} tz={tz} /> by <Actor a={run.started_by} />
            </Pill>
        );
    return (
        <>
            <Pill tone="neutral" title="ended" glyph="■">
                Ended at <Time iso={run.ended_at} tz={tz} /> by <Actor a={run.ended_by} />
            </Pill>
            {run.summary_revised && (
                <Pill tone="orange" title="A late entry appended a newer summary after the shift ended">
                    ▲ Summary revised after end (r{run.latest_summary_revision})
                </Pill>
            )}
        </>
    );
}

function ApproveDialog({ plan, me, busy, disabled, onClose, onApprove }: { plan: ShiftPlanV1; me: string; busy: boolean; disabled: boolean; onClose: () => void; onApprove: () => void }) {
    const rev = plan.revisions[plan.current_revision - 1];
    const tz = rev.plan.timezone;
    const self = plan.revisions.some((r) => r.submitted_by.subject === me);
    const onOpenChange = (open: boolean) => !open && onClose();
    return (
        <Dialog isOpen onOpenChange={onOpenChange} purpose="form">
            <DialogHeader title={`Approve revision ${rev.revision}?`} onOpenChange={onOpenChange} />
            <VStack gap={3} padding={4}>
                <MetadataList columns={2}>
                    <MetadataListItem label="Line">
                        <span className="identifier">{plan.line_id}</span>
                    </MetadataListItem>
                    <MetadataListItem label="Shift">{formatInterval(rev.plan.starts_at, rev.plan.ends_at, tz)}</MetadataListItem>
                    <MetadataListItem label="Assignments">{rev.plan.assignments.length} workstations</MetadataListItem>
                    <MetadataListItem label="State revision">{plan.state_revision}</MetadataListItem>
                    <MetadataListItem label="Digest">
                        <span className="identifier">{rev.canonical_payload_digest}</span>
                    </MetadataListItem>
                    <MetadataListItem label="Submitted by">
                        <Actor a={rev.submitted_by} />
                    </MetadataListItem>
                </MetadataList>
                <ul className="consequences">
                    <li>Assigned workstations show these job orders; operators record against this revision.</li>
                    <li>{plan.approved_revision ? `Replaces approved r${plan.approved_revision}.` : "First approval for this shift."}</li>
                    <li>The approver must not be a submitter; the server decides.</li>
                </ul>
                {self && (
                    <div role="note">
                        <Banner status="warning" title="▲ You submitted a revision of this plan; the server is expected to refuse." />
                    </div>
                )}
                <HStack gap={2}>
                    <Button label={`Approve r${rev.revision}`} variant="primary" isDisabled={disabled} onClick={onApprove}>
                        {busy ? "Sending…" : undefined}
                    </Button>
                    <Button label="Cancel" variant="secondary" onClick={onClose} />
                </HStack>
            </VStack>
        </Dialog>
    );
}

type Row = { workstation_id: string; job_order_id: string; target_quantity: string; target_cycle: string; pinned?: number };

function PlanForm({
    initial,
    submit,
    onDone,
    onConflict,
}: {
    initial?: ShiftPlanBodyV1;
    submit: (intents: Intents, body: ShiftPlanBodyV1) => Promise<ShiftPlanV1>;
    onDone: (p: ShiftPlanV1) => void;
    onConflict?: () => void;
}) {
    const { site, online } = useSession();
    const stations = useStations();
    const [jobs, setJobs] = useState<Record<string, JobOrder>>({});
    const [jobsError, setJobsError] = useState<unknown>(null);
    const [jobsLoad, setJobsLoad] = useState<{ page: number } | null>({ page: 1 });
    const defaultTz = initial?.timezone ?? Intl.DateTimeFormat().resolvedOptions().timeZone;
    const [line, setLine] = useState(initial?.line_id ?? "");
    const [tz, setTz] = useState(defaultTz);
    const [start, setStart] = useState(initial ? toLocalInput(initial.starts_at, defaultTz) : "");
    const [end, setEnd] = useState(initial ? toLocalInput(initial.ends_at, defaultTz) : "");
    const [rows, setRows] = useState<Row[]>(
        initial?.assignments.map((a) => ({
            workstation_id: a.workstation_id,
            job_order_id: a.job_order_id,
            target_quantity: String(a.target_quantity),
            target_cycle: a.target_cycle_time_seconds ?? "",
            pinned: a.job_order_revision,
        })) ?? [{ workstation_id: "", job_order_id: "", target_quantity: "", target_cycle: "" }],
    );
    const [errors, setErrors] = useState<{ id: string; message: string }[]>([]);
    const [error, setError] = useState<unknown>(null);
    const [busy, setBusy] = useState(false);
    const [suggesting, setSuggesting] = useState(false);
    const [selectedJobs, setSelectedJobs] = useState<string[]>(initial?.assignments.map(a => a.job_order_id) ?? []);
    const [suggestion, setSuggestion] = useState<ShiftPlanSuggestionV1 | null>(null);
    const [suggestionError, setSuggestionError] = useState("");
    const intents = useMemo(() => new Intents(), []);

    useEffect(() => {
        setSuggestion(null);
        setSuggestionError("");
        setError(null);
    }, [line, tz, start, end, selectedJobs]);

    useEffect(() => {
        let active = true;
        setJobs({});
        setJobsError(null);
        setJobsLoad({ page: 1 });
        let pageNumber = 0;
        allPages(async (cursor) => {
            if (active) setJobsLoad({ page: ++pageNumber });
            const page = await request<JobOrderPageV1>(`/v1/sites/${site.id}/job-orders?limit=100&include=current${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
            return { items: page.job_orders, next_cursor: page.next_cursor };
        })
            .then((jobOrders) => {
                if (!active) return;
                setJobs(Object.fromEntries(jobOrders.map((job) => [job.id, job])));
                setJobsLoad(null);
            })
            .catch((reason) => {
                if (!active) return;
                setJobsError(reason);
                setJobsLoad(null);
            });
        return () => {
            active = false;
        };
    }, [site.id]);
    const lines = [...new Set((stations.list ?? []).map((s) => s.line_id))];
    const order = (id: string) => {
        return jobs[id]?.current_order ?? null;
    };
    const set = (i: number, patch: Partial<Row>) => setRows(rows.map((r, n) => (n === i ? { ...r, ...patch } : r)));

    async function suggest() {
        if (busy || suggesting || jobsLoad) return;
        setSuggestionError("");
        setError(null);
        if (!line || !start || !end || selectedJobs.length === 0) {
            setSuggestionError("Choose a line, shift window and at least one job order.");
            return;
        }
        if (rows.some(r => r.workstation_id || r.job_order_id || r.target_quantity || r.target_cycle) &&
            !window.confirm("Replace the current assignment rows with a new editable suggestion? Your row edits will be replaced.")) return;
        setSuggesting(true);
        setSuggestion(null);
        try {
            // Selection only. The server supplies all durations, routes and revision pins.
            const result = await request<ShiftPlanSuggestionV1>(`/v1/sites/${site.id}/shift-plans/suggestions`, "POST", {
                body: { schema_version: 1, line_id: line, starts_at: zonedISO(start, tz), ends_at: zonedISO(end, tz), timezone: tz, job_order_ids: selectedJobs },
            });
            setJobs(current => ({ ...current, ...Object.fromEntries(result.job_orders.map(j => [j.id, j])) }));
            setRows(result.assignments.map(a => ({ workstation_id: a.workstation_id, job_order_id: a.job_order_id,
                target_quantity: String(a.target_quantity), target_cycle: a.target_cycle_time_seconds ?? "", pinned: a.job_order_revision })));
            setErrors([]);
            setSuggestion(result);
        } catch (e) {
            if (e instanceof ApiError && e.problem.code === "NO_FEASIBLE_SCHEDULE")
                setSuggestionError("No feasible plan for this selection. Review the shift length, routing and cycle times, or choose fewer orders.");
            else if (e instanceof ApiError && e.problem.code === "PLANNER_DATA_UNAVAILABLE")
                setSuggestionError("Station profiles or approved planned breaks are unavailable. Review the line’s workstation setup and pending plan reviews.");
            else if (e instanceof RangeError) setSuggestionError("Enter a valid shift window and IANA timezone.");
            else setError(e);
        } finally {
            setSuggesting(false);
        }
    }

    async function onSubmit(e: React.FormEvent) {
        e.preventDefault();
        if (jobsLoad || suggesting || busy) return;
        const errs: { id: string; message: string }[] = [];
        if (!/^[0-9a-f-]{36}$/.test(line)) errs.push({ id: "pf-line", message: "Choose a line" });
        let zoneOK = true;
        try {
            new Intl.DateTimeFormat("en", { timeZone: tz });
        } catch {
            zoneOK = false;
            errs.push({ id: "pf-tz", message: "Timezone must be an IANA name, e.g. Asia/Kolkata" });
        }
        if (!start) errs.push({ id: "pf-start", message: "Start is required" });
        if (!end) errs.push({ id: "pf-end", message: "End is required" });
        let startsAt = "",
            endsAt = "";
        if (zoneOK && start && end) {
            startsAt = zonedISO(start, tz);
            endsAt = zonedISO(end, tz);
            const span = Date.parse(endsAt) - Date.parse(startsAt);
            if (span <= 0) errs.push({ id: "pf-end", message: "End must be after start" });
            else if (span > 864e5) errs.push({ id: "pf-end", message: "A shift is at most 24 hours" });
        }
        if (!rows.length) errs.push({ id: "pf-add", message: "Add at least one assignment" });
        const assignments: Assignment[] = [];
        rows.forEach((r, i) => {
            const o = order(r.job_order_id);
            if (!r.workstation_id) errs.push({ id: `pf-ws-${i}`, message: `Row ${i + 1}: choose a workstation` });
            if (!o)
                errs.push({ id: `pf-jo-${i}`, message: r.job_order_id ? `Row ${i + 1}: job order details are not loaded` : `Row ${i + 1}: choose a job order` });
            if (rows.findIndex((x) => x.workstation_id === r.workstation_id) !== i && r.workstation_id)
                errs.push({ id: `pf-ws-${i}`, message: `Row ${i + 1}: workstation already assigned` });
            if (rows.findIndex((x) => x.job_order_id === r.job_order_id) !== i && r.job_order_id)
                errs.push({ id: `pf-jo-${i}`, message: `Row ${i + 1}: job order already assigned` });
            if (!/^[1-9]\d*$/.test(r.target_quantity)) errs.push({ id: `pf-qty-${i}`, message: `Row ${i + 1}: target is a whole number above 0` });
            else if (o && Number(r.target_quantity) > o.target_quantity)
                errs.push({ id: `pf-qty-${i}`, message: `Row ${i + 1}: target at most the order's ${o.target_quantity}` });
            if (r.target_cycle && (!/^\d+(\.\d{1,6})?$/.test(r.target_cycle) || Number(r.target_cycle) === 0))
                errs.push({ id: `pf-ct-${i}`, message: `Row ${i + 1}: cycle time in seconds above 0` });
            if (!o) return;
            assignments.push({
                workstation_id: r.workstation_id,
                job_order_id: r.job_order_id,
                job_order_revision: jobs[r.job_order_id].current_revision,
                order_id: o.order_id,
                operation_id: o.operation_id,
                item_code: o.item_code,
                instruction_document_id: o.instruction_document_id,
                instruction_revision: o.instruction_revision,
                instruction_sha256: o.instruction_sha256,
                template: o.template,
                fields: o.fields,
                target_quantity: Number(r.target_quantity),
                ...(r.target_cycle ? { target_cycle_time_seconds: r.target_cycle } : {}),
            });
        });
        setErrors(errs);
        if (errs.length) return;
        setBusy(true);
        setError(null);
        try {
            onDone(await submit(intents, { schema_version: 1, line_id: line, timezone: tz, starts_at: startsAt, ends_at: endsAt, assignments }));
        } catch (e) {
            setError(e);
            if (isConflict(e)) onConflict?.();
        } finally {
            setBusy(false);
        }
    }

    return (
        <Card padding={4}>
        <form onSubmit={onSubmit} noValidate aria-label={initial ? "Revise plan" : "Create plan"}>
            <VStack gap={3}>
            {errors.length > 0 && (
                <div role="alert" tabIndex={-1} ref={(el) => el?.focus()}>
                    <Banner status="error" collapsible={false} title={`Fix ${errors.length} field${errors.length === 1 ? "" : "s"}`}>
                        <ul className="consequences">
                            {errors.map((e, i) => (
                                <li key={i}>
                                    <a
                                        href={`#${e.id}`}
                                        onClick={(ev) => {
                                            ev.preventDefault();
                                            document.getElementById(e.id)?.focus();
                                        }}
                                    >
                                        {e.message}
                                    </a>
                                </li>
                            ))}
                        </ul>
                    </Banner>
                </div>
            )}
            <Problem error={error ?? jobsError ?? stations.error} />
            <fieldset disabled={suggesting || busy} style={bareFieldset}>
            <legend className="sr-only">Shift and draft assignments</legend>
            <VStack gap={3}>
            <HStack gap={3} wrap="wrap" align="end">
                <Field label="Line" inputID="pf-line">
                    <select id="pf-line" value={line} onChange={(e) => setLine(e.target.value)}>
                        <option value="">Choose…</option>
                        {[...new Set([line, ...lines])].filter(Boolean).map((l) => (
                            <option key={l} value={l}>
                                {short(l)}
                            </option>
                        ))}
                    </select>
                </Field>
                <Field label="Timezone" inputID="pf-tz">
                    <input id="pf-tz" value={tz} onChange={(e) => setTz(e.target.value)} />
                </Field>
                <Field label="Starts" inputID="pf-start">
                    <input id="pf-start" type="datetime-local" value={start} onChange={(e) => setStart(e.target.value)} />
                </Field>
                <Field label="Ends" inputID="pf-end">
                    <input id="pf-end" type="datetime-local" value={end} onChange={(e) => setEnd(e.target.value)} />
                </Field>
            </HStack>
            <HStack gap={3} wrap="wrap" align="end">
                <Field
                    label="Job orders to suggest"
                    inputID="pf-suggest-jobs"
                    description="Up to 50 orders. Fills an editable draft; nothing is saved until you submit, and another supervisor approves."
                    descriptionID="pf-suggest-help"
                >
                    <select id="pf-suggest-jobs" multiple value={selectedJobs} disabled={jobsLoad !== null}
                        onChange={e => setSelectedJobs(Array.from(e.target.selectedOptions, option => option.value))} aria-describedby="pf-suggest-help">
                        {Object.values(jobs).map(j => <option key={j.id} value={j.id}>{j.current_order?.item_code ?? short(j.id)} · {short(j.id)}</option>)}
                    </select>
                </Field>
                <Button label="Suggest assignments" isDisabled={!online || jobsLoad !== null || !!jobsError} onClick={suggest} />
            </HStack>
            </VStack>
            </fieldset>
            <p role="status" aria-live="polite">
                {suggesting ? "Suggesting assignments…" : suggestion ? `${suggestion.method === "ortools_cp_sat" ? (suggestion.is_optimized ? "Optimised (CP-SAT)" : "CP-SAT feasible (optimality not proven)") : "Heuristic"} · Editable draft filled; not saved. Review before submitting.` : ""}
            </p>
            {suggestionError && (
                <div role="alert">
                    <Banner status="error" title={suggestionError} />
                </div>
            )}
            {suggestion && suggestion.warnings.filter(code => suggestionWarningText[code]).map(code => <Text key={code} as="p" size="sm" color="secondary">{suggestionWarningText[code]}</Text>)}
            {suggestion && (suggestion.approved_cycle_times ?? []).some(c => c.status !== "matches") && <div role="status" aria-label="Approved cycle times">
                <Banner status="warning" collapsible={false} title="Cycle times approved during onboarding (advice only; edit the job order to use one):">
                <ul className="consequences">{(suggestion.approved_cycle_times ?? []).filter(c => c.status !== "matches").map(c => <li key={c.job_order_id}>
                    {jobs[c.job_order_id]?.current_order?.item_code ?? short(c.job_order_id)} · {cycleTimeText[c.status] ?? "review this cycle time"}
                    {c.approved_seconds ? ` · approved ${c.approved_seconds} s` : ""}{c.job_order_seconds ? ` · job order ${c.job_order_seconds} s` : ""}
                    {` · cited in ${c.source_span_ids.length} approved passage${c.source_span_ids.length === 1 ? "" : "s"}`}
                </li>)}</ul>
                </Banner>
            </div>}
            {suggestion && suggestion.unplaced_orders.length > 0 && <div role="status">
                <Banner status="warning" collapsible={false} title="Orders not placed:">
                <ul className="consequences">{suggestion.unplaced_orders.map(j => <li key={j.job_order_id}>{jobs[j.job_order_id]?.current_order?.item_code ?? short(j.job_order_id)} · {unplacedText[j.reason_code] ?? "Review this job order before planning it."}</li>)}</ul>
                </Banner>
            </div>}
            <fieldset disabled={suggesting || busy} style={bareFieldset}>
            <legend className="sr-only">Editable assignment rows</legend>
            <VStack gap={3}>
            <Table aria-label="Assignments" density="compact">
                <TableHeader>
                    <TableRow isHeaderRow>
                        <TableHeaderCell scope="col">Workstation</TableHeaderCell>
                        <TableHeaderCell scope="col">Job order</TableHeaderCell>
                        <TableHeaderCell scope="col">Target qty</TableHeaderCell>
                        <TableHeaderCell scope="col">Cycle s</TableHeaderCell>
                        <TableHeaderCell scope="col">
                            <span className="sr-only">Remove</span>
                        </TableHeaderCell>
                    </TableRow>
                </TableHeader>
                <TableBody>
                    {rows.map((r, i) => {
                        const j = jobs[r.job_order_id];
                        return (
                            <TableRow key={i}>
                                <TableCell>
                                    <select id={`pf-ws-${i}`} aria-label={`Row ${i + 1} workstation`} value={r.workstation_id} onChange={(e) => set(i, { workstation_id: e.target.value })}>
                                        <option value="">Choose…</option>
                                        {(stations.list ?? [])
                                            .filter((s) => !line || s.line_id === line || s.id === r.workstation_id)
                                            .map((s) => (
                                                <option key={s.id} value={s.id}>
                                                    {s.name}
                                                </option>
                                            ))}
                                    </select>
                                </TableCell>
                                <TableCell>
                                    <HStack gap={1} align="center" wrap="wrap">
                                    <select
                                        id={`pf-jo-${i}`}
                                        aria-label={`Row ${i + 1} job order`}
                                        value={r.job_order_id}
                                        onChange={(e) => {
                                            const o = order(e.target.value);
                                            set(i, {
                                                job_order_id: e.target.value,
                                                target_quantity: r.target_quantity || String(o?.target_quantity ?? ""),
                                                target_cycle: r.target_cycle || (o?.target_cycle_time_seconds ?? ""),
                                                pinned: undefined,
                                            });
                                        }}
                                    >
                                        <option value="">Choose…</option>
                                        {[...new Set([r.job_order_id, ...Object.keys(jobs)])].filter(Boolean).map((id) => {
                                            const o = order(id);
                                            return (
                                                <option key={id} value={id}>
                                                    {o ? `${o.item_code} · ${short(o.order_id)}` : short(id)}
                                                </option>
                                            );
                                        })}
                                    </select>
                                    {j && r.pinned && r.pinned !== j.current_revision && (
                                        <Pill tone="orange" glyph="▲">
                                            job order r{r.pinned} → r{j.current_revision}
                                        </Pill>
                                    )}
                                    </HStack>
                                </TableCell>
                                <TableCell>
                                    <input id={`pf-qty-${i}`} aria-label={`Row ${i + 1} target quantity`} inputMode="numeric" value={r.target_quantity} onChange={(e) => set(i, { target_quantity: e.target.value })} />
                                </TableCell>
                                <TableCell>
                                    <input id={`pf-ct-${i}`} aria-label={`Row ${i + 1} target cycle seconds`} inputMode="decimal" value={r.target_cycle} onChange={(e) => set(i, { target_cycle: e.target.value })} />
                                </TableCell>
                                <TableCell>
                                    <Button label={`Remove row ${i + 1}`} variant="ghost" size="sm" onClick={() => setRows(rows.filter((_, n) => n !== i))}>
                                        ✕
                                    </Button>
                                </TableCell>
                            </TableRow>
                        );
                    })}
                </TableBody>
            </Table>
            <HStack gap={2} align="center" wrap="wrap">
                <Button label="+ Assignment" id="pf-add" onClick={() => setRows([...rows, { workstation_id: "", job_order_id: "", target_quantity: "", target_cycle: "" }])} />
                <Button
                    type="submit"
                    label={initial ? "Save draft revision" : "Create draft plan"}
                    variant="primary"
                    isDisabled={busy || !online || jobsLoad !== null}
                    aria-describedby={jobsLoad ? "pf-jobs-load" : undefined}
                >
                    {busy ? "Sending…" : undefined}
                </Button>
                {jobsLoad && (
                    <span id="pf-jobs-load" role="status">
                        Loading job orders (page {jobsLoad.page})…
                    </span>
                )}
            </HStack>
            </VStack>
            </fieldset>
            </VStack>
        </form>
        </Card>
    );
}
