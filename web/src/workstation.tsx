import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type {
    CurrentJobOrderV1,
    ShiftRunV1,
    StationProfilePreviewInputV1,
    StationProfilePreviewV1,
    StationProfileV1,
    StationRecordBodyV1,
    StationRecordPageV1,
    StationRecordV1,
} from "../../contracts/typescript/oee";
import { Badge, type BadgeProps } from "@astryxdesign/core/Badge";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { Drawer, DrawerHeader } from "@astryxdesign/core/Drawer";
import { EmptyState } from "@astryxdesign/core/EmptyState";
import { Field } from "@astryxdesign/core/Field";
import { Grid } from "@astryxdesign/core/Grid";
import { Heading } from "@astryxdesign/core/Heading";
import { Kbd } from "@astryxdesign/core/Kbd";
import { List, ListItem } from "@astryxdesign/core/List";
import { ProgressBar } from "@astryxdesign/core/ProgressBar";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { VisuallyHidden } from "@astryxdesign/core/VisuallyHidden";
import { AmbiguousError, ApiError, classify, compareInstant, formatInterval, Intents, request, zonedISO } from "./api";
import { Details, ItemName, Link, Name, Problem, Refreshed, Status, Time, Val, Actor, short, usePolling, useSession, useStations } from "./ui";
import { Procedure } from "./station";
import { loadRun } from "./shifts";
import { shiftRunText } from "./labels";

type Kind = StationRecordBodyV1["kind"];
type FieldDef = StationProfilePreviewV1["fields"][number]["field"];
type Rec = StationRecordV1;
type ExpandedHistory = { jobOrderId: string; planId: string; page: StationRecordPageV1 };

const LIFECYCLE: readonly Kind[] = ["acknowledged", "started", "paused", "blocked", "resumed", "completion_evidence"];
const CORRECTABLE = (k: Kind) => !["acknowledged", "started", "paused", "blocked", "resumed"].includes(k);
const KIND_LABEL: Record<Kind, string> = {
    acknowledged: "Acknowledged",
    started: "Started",
    paused: "Paused",
    blocked: "Blocked",
    resumed: "Resumed",
    quantity: "Quantity",
    cycle_time: "Cycle time",
    quality_measurement: "Quality measurement",
    quality_disposition_recorded: "Quality disposition (as recorded)",
    material_exception: "Material issue",
    completion_evidence: "Completion evidence",
    note: "Note",
};
const DECIMAL = /^\d+(\.\d{1,6})?$/;

// One-tap problem reports map onto existing record kinds only; the category goes into the
// required comment. A stop while running is a block; otherwise it is a note. [label, composer, needs a started job]
const stop = (label: string) => (running: boolean): Composer => ({ kind: "comment", record: running ? "blocked" : "note", prefill: `${label}: ` });
const PROBLEMS: [string, (running: boolean) => Composer, boolean][] = [
    ["Material missing", () => ({ kind: "issue", issue: "shortage" }), true],
    ["Machine stopped", stop("Machine stopped"), false],
    ["Tool wear", stop("Tool wear"), false],
    ["Safety", stop("Safety"), false],
    ["Quality issue", () => ({ kind: "comment", record: "note", prefill: "Quality issue: " }), false],
    ["Something else", () => ({ kind: "comment", record: "note", prefill: "Other: " }), false],
];

// CopyId shows the full workstation ID (needed for routing steps in the master-data CSV import)
// with a copy button. Without a clipboard (non-secure origin, denied permission) it selects the
// text so the user can copy by hand.
function CopyId({ id, name }: { id: string; name: string }) {
    const ref = useRef<HTMLElement>(null);
    const [said, setSaid] = useState("");
    const copy = async () => {
        try {
            await navigator.clipboard.writeText(id);
            setSaid("Copied");
        } catch {
            if (ref.current) getSelection()?.selectAllChildren(ref.current);
            setSaid("Selected; press Ctrl+C to copy");
        }
        setTimeout(() => setSaid(""), 2500);
    };
    return (
        <HStack as="span" gap={1} align="center" wrap="wrap">
            <Text size="sm" color="secondary" hasTabularNumbers>
                <code ref={ref} title={`Workstation ID of ${name}`} style={{ userSelect: "all" }}>
                    {id}
                </code>
            </Text>
            <Button label={`Copy workstation ID of ${name}`} variant="ghost" size="sm" onClick={copy}>
                Copy
            </Button>
            <Text size="sm" color="secondary" aria-live="polite">
                {said}
            </Text>
        </HStack>
    );
}

export function WorkstationList() {
    const { site } = useSession();
    const { list, error } = useStations();
    return (
        <VStack gap={4}>
            <HStack justify="between" align="center" gap={2} wrap="wrap">
                <Heading level={1}>Workstations</Heading>
                {list && <Badge label={`${list.length} station${list.length === 1 ? "" : "s"}`} />}
            </HStack>
            <Problem error={error} />
            {!list && !error && <Skeleton height={160} />}
            {list && list.length > 0 && (
                <Table aria-label="Workstations" density="compact">
                    <TableHeader>
                        <TableRow isHeaderRow>
                            <TableHeaderCell scope="col">Workstation</TableHeaderCell>
                            <TableHeaderCell scope="col">Workstation ID</TableHeaderCell>
                            <TableHeaderCell scope="col">Line</TableHeaderCell>
                            <TableHeaderCell scope="col">Profile rev</TableHeaderCell>
                        </TableRow>
                    </TableHeader>
                    <TableBody>
                        {list.map((w) => (
                            <TableRow key={w.id}>
                                <TableCell>
                                    <Link to={`/sites/${site.id}/workstations/${w.id}`}>{w.name}</Link>
                                </TableCell>
                                <TableCell>
                                    <CopyId id={w.id} name={w.name} />
                                </TableCell>
                                <TableCell>
                                    <Text size="sm" color="secondary">
                                        <span title={w.line_id}>…{w.line_id.slice(-4)}</span>
                                    </Text>
                                </TableCell>
                                <TableCell>
                                    <Text hasTabularNumbers>{w.revision}</Text>
                                </TableCell>
                            </TableRow>
                        ))}
                    </TableBody>
                </Table>
            )}
            {list && !list.length && <EmptyState title="No workstation has a station profile yet" headingLevel={2} />}
        </VStack>
    );
}

type Composer =
    | { kind: "comment"; record: "paused" | "blocked" | "note"; prefill?: string }
    | { kind: "good" | "reject" }
    | { kind: "cycle" }
    | { kind: "quality" }
    | { kind: "issue"; issue?: string }
    | { kind: "complete" }
    | { kind: "correct"; of: Rec };

export function WorkstationPage({ stationId }: { stationId: string }) {
    const { site, me, online } = useSession();
    const base = `/v1/sites/${site.id}/workstations/${stationId}`;
    const [current, setCurrent] = useState<CurrentJobOrderV1 | null>(null);
    const [run, setRun] = useState<ShiftRunV1 | null>(null);
    const refreshSeq = useRef(0);
    const [currentError, setCurrentError] = useState<unknown>(null);
    const [records, setRecords] = useState<StationRecordPageV1 | null>(null);
    const [historyPage, setHistoryPage] = useState<ExpandedHistory | null>(null);
    const [profile, setProfile] = useState<StationProfileV1 | null>(null);
    const [preview, setPreview] = useState<StationProfilePreviewV1 | null>(null);
    const [previewError, setPreviewError] = useState<unknown>(null);
    const [refreshedAt, setRefreshedAt] = useState<Date | null>(null);
    const [composer, setComposer] = useState<Composer | null>(null);
    const [composerSeq, setComposerSeq] = useState(0);
    const [lifecycleConfirm, setLifecycleConfirm] = useState<{ kind: Kind; label: string } | null>(null);
    const [busy, setBusy] = useState<string | null>(null);
    const [error, setError] = useState<unknown>(null);
    const [lastBody, setLastBody] = useState<StationRecordBodyV1 | null>(null);
    const [unresolved, setUnresolved] = useState<StationRecordBodyV1 | null>(null);
    const [receipt, setReceipt] = useState<Rec | null>(null);
    const [historyOpen, setHistoryOpen] = useState(false);
    const [historyLoading, setHistoryLoading] = useState(false);
    const [historyError, setHistoryError] = useState<unknown>(null);
    const historyOpenRef = useRef(false);
    const currentBindingRef = useRef<{ jobOrderId: string; planId: string } | null>(null);
    const sending = useRef(false);
    const intents = useMemo(() => new Intents(), []);

    const openHistory = () => {
        historyOpenRef.current = true;
        setHistoryPage(null);
        setHistoryError(null);
        setHistoryOpen(true);
    };
    const closeHistory = () => {
        historyOpenRef.current = false;
        setHistoryPage(null);
        setHistoryOpen(false);
    };

    const refresh = useCallback(async () => {
        // One number per refresh, taken before its first request: a refresh that a newer one has
        // overtaken applies nothing (binding, records, run or error).
        const seq = ++refreshSeq.current;
        const live = () => seq === refreshSeq.current;
        try {
            const c = await request<CurrentJobOrderV1>(`${base}/current-job-order`);
            if (!live()) return;
            const priorBinding = currentBindingRef.current;
            currentBindingRef.current = { jobOrderId: c.job_order_id, planId: c.plan_id };
            if (priorBinding?.jobOrderId !== c.job_order_id || priorBinding.planId !== c.plan_id) setRecords(null);
            if (priorBinding?.planId !== c.plan_id) setRun(null);
            setCurrent(c);
            setHistoryPage((prior) => prior?.jobOrderId === c.job_order_id && prior.planId === c.plan_id ? prior : null);
            setCurrentError(null);
            // A failed run read shows no shift state and leaves entry to the server's decision.
            loadRun(`/v1/sites/${site.id}/shift-plans/${c.plan_id}`).then((r) => {
                // A refusal in this refresh may already have cleared the binding: apply only to it.
                if (live() && currentBindingRef.current?.planId === c.plan_id) setRun(r?.plan_id === c.plan_id ? r : null);
            });
            const page = await request<StationRecordPageV1>(
                `${base}/records?job_order_id=${encodeURIComponent(c.job_order_id)}&plan_id=${encodeURIComponent(c.plan_id)}&limit=100`,
            );
            if (live()) setRecords(page);
        } catch (e) {
            if (!live()) return;
            setCurrentError(e);
            if (e instanceof ApiError && classify(e.problem) !== "retry") {
                currentBindingRef.current = null;
                setCurrent(null);
                setRun(null);
                setRecords(null);
                setHistoryPage(null);
            }
        } finally {
            if (live()) setRefreshedAt(new Date());
        }
    }, [base, site.id]);
    usePolling(refresh, 10000);
    async function loadMoreHistory() {
        const page = current && historyPage?.jobOrderId === current.job_order_id && historyPage.planId === current.plan_id ? historyPage.page : records;
        if (!current || !page?.next_cursor || historyLoading) return;
        const cursor = page.next_cursor;
        const jobOrderId = current.job_order_id;
        const planId = current.plan_id;
        setHistoryLoading(true);
        setHistoryError(null);
        try {
            const nextPage = await request<StationRecordPageV1>(
                `${base}/records?job_order_id=${encodeURIComponent(jobOrderId)}&plan_id=${encodeURIComponent(planId)}&limit=100&cursor=${encodeURIComponent(cursor)}`,
            );
            setHistoryPage((prior) => {
                const priorPage = prior?.jobOrderId === jobOrderId && prior.planId === planId ? prior.page : page;
                const binding = currentBindingRef.current;
                if (!historyOpenRef.current || binding?.jobOrderId !== jobOrderId || binding.planId !== planId || priorPage?.next_cursor !== cursor) return prior;
                return { jobOrderId, planId, page: { ...priorPage, records: [...priorPage.records, ...nextPage.records], next_cursor: nextPage.next_cursor } };
            });
        } catch (e) {
            setHistoryError(e);
        } finally {
            setHistoryLoading(false);
        }
    }
    useEffect(() => {
        request<StationProfileV1>(`${base}/profile`).then(setProfile, setPreviewError);
    }, [base]);
    const orderRevision = current?.order?.revision;
    useEffect(() => {
        if (!profile || !current || current.state !== "current" || !orderRevision) return;
        const body: StationProfilePreviewInputV1 = {
            profile: profile.revisions[profile.current_revision - 1].profile,
            job_order_id: current.job_order_id,
            job_order_revision: orderRevision,
        };
        // The server resolves the form (profile + job order), so the tablet never re-derives it.
        request<StationProfilePreviewV1>(`${base}/profile-previews`, "POST", { body }).then(
            (p) => {
                setPreview(p);
                setPreviewError(null);
            },
            setPreviewError,
        );
    }, [base, profile, current?.job_order_id, current?.state, orderRevision]);

    // Records of this shift's binding only; a job carried into another shift starts afresh.
    const shiftRecords = useMemo(
        () => (records && current ? records.records.filter((r) => r.plan_id === current.plan_id) : []),
        [records, current?.plan_id],
    );
    const historyRecordsPage = current && historyPage?.jobOrderId === current.job_order_id && historyPage.planId === current.plan_id ? historyPage.page : records;
    const historyRecords = useMemo(
        () => (historyRecordsPage && current ? historyRecordsPage.records.filter((r) => r.plan_id === current.plan_id) : []),
        [historyRecordsPage, current?.plan_id],
    );
    const complete = records !== null && !records.next_cursor;
    // The server total covers the whole binding, so it is known even when the first page ends
    // with a cursor. Older replies without totals keep the page-by-page fallback.
    const serverTotals = records?.totals;
    const totalsKnown = complete || !!serverTotals;
    const tally = useMemo(() => {
        if (serverTotals) return { good: serverTotals.good_quantity, reject: serverTotals.reject_quantity, n: shiftRecords.length };
        let good = 0,
            reject = 0,
            n = 0;
        for (const r of shiftRecords)
            if (r.state === "current" && r.record.kind === "quantity") {
                good += r.record.good_quantity ?? 0;
                reject += r.record.reject_quantity ?? 0;
                n++;
            }
        return { good, reject, n };
    }, [serverTotals, shiftRecords]);
    const last = useMemo(() => {
        const life = shiftRecords
            .filter((r) => !r.record.corrects_record_id && LIFECYCLE.includes(r.record.kind))
            .sort((a, b) => compareInstant(a.event_at, b.event_at) || compareInstant(a.received_at, b.received_at));
        return life.length ? life[life.length - 1].record.kind : "";
    }, [shiftRecords]);

    const isCurrent = current?.state === "current" && current.order !== null;
    const running = last === "started" || last === "resumed";
    const halted = last === "paused" || last === "blocked";
    const done = last === "completion_evidence";
    // New entries need a started shift run; the server refuses them otherwise (SHIFT_NOT_STARTED, SHIFT_RUN_ENDED).
    const shiftClosed = run?.state === "planned" || run?.state === "ended";
    // Corrections are not gated by the run: the server accepts some after the end (late data) and
    // on plans without a run, and shows its own refusal otherwise.
    const canCorrect = online && isCurrent && !busy && !unresolved;
    const canSend = canCorrect && !shiftClosed;

    async function send(body: StationRecordBodyV1) {
        if (sending.current || !online) return;
        sending.current = true;
        setBusy(body.kind);
        setError(null);
        setReceipt(null);
        setLastBody(body);
        try {
            const rec = await request<Rec>(`${base}/records`, "POST", { key: intents.key("record", body), body });
            intents.done("record");
            setUnresolved(null);
            setReceipt(rec);
            setComposer(null);
            refresh();
        } catch (e) {
            setError(e);
            const retryable = e instanceof AmbiguousError || (e instanceof ApiError && classify(e.problem) === "retry");
            // An unknown outcome keeps the same key and body; only resending it is allowed.
            if (retryable) setUnresolved(body);
            else {
                intents.done("record");
                setUnresolved(null);
                if (e instanceof ApiError && classify(e.problem) === "conflict") refresh();
            }
        } finally {
            sending.current = false;
            setBusy(null);
        }
    }
    const make = (kind: Kind, extra: Partial<StationRecordBodyV1> = {}): StationRecordBodyV1 => ({
        schema_version: 1,
        kind,
        workstation_id: stationId,
        job_order_id: current!.job_order_id,
        job_order_revision: current!.order!.revision,
        event_at: new Date().toISOString(),
        ...extra,
    });
    const quick = (kind: Kind) => canSend && send(make(kind));
    const askLifecycle = (kind: Kind, label: string) => canSend && setLifecycleConfirm({ kind, label });
    const open = (c: Composer) => {
        setError(null);
        setComposerSeq((n) => n + 1);
        setComposer(c);
    };

    // Keyboard shortcuts for the tablet screen; ignored while typing.
    const shortcuts = useRef<Record<string, () => void>>({});
    shortcuts.current = {
        s: () => (last === "" || last === "acknowledged" || halted) && askLifecycle(startKind, startLabel),
        p: () => running && canSend && open({ kind: "comment", record: "paused" }),
        b: () => running && canSend && open({ kind: "comment", record: "blocked" }),
        g: () => canSend && open({ kind: "good" }),
        r: () => canSend && open({ kind: "reject" }),
        t: () => canSend && open({ kind: "cycle" }),
        q: () => canSend && open({ kind: "quality" }),
        i: () => canSend && open({ kind: "issue" }),
        c: () => running && canSend && open({ kind: "complete" }),
        n: () => canSend && open({ kind: "comment", record: "note" }),
        h: openHistory,
    };
    useEffect(() => {
        const onKey = (e: KeyboardEvent) => {
            const t = e.target as HTMLElement;
            if (e.repeat || e.ctrlKey || e.metaKey || e.altKey || /INPUT|TEXTAREA|SELECT/.test(t.tagName) || t.closest("dialog, [role=radiogroup]"))
                return;
            const action = shortcuts.current[e.key.toLowerCase()];
            if (action) {
                e.preventDefault();
                action();
            }
        };
        addEventListener("keydown", onKey);
        return () => removeEventListener("keydown", onKey);
    }, []);

    const order = current?.order?.order;
    const name = profile?.revisions[profile.current_revision - 1].profile.display_name ?? short(stationId);
    const tz = current?.timezone;
    const remaining = totalsKnown && current ? current.target_quantity - tally.good : null;
    const pct = totalsKnown && current ? Math.min(100, Math.round((tally.good / current.target_quantity) * 100)) : 0;
    const startLabel = last === "" ? "Acknowledge" : last === "acknowledged" ? "Start" : halted ? "Resume" : "Running";
    const startKind: Kind = last === "" ? "acknowledged" : last === "acknowledged" ? "started" : "resumed";
    const latest = shiftRecords.reduce<Rec | null>((a, r) => (a && compareInstant(a.received_at, r.received_at) > 0 ? a : r), null);
    const step = done
        ? ["✓", "Completed", "ok"]
        : running
          ? ["▶", "Running", "ok"]
          : last === "paused"
            ? ["❚❚", "Paused", "warn"]
            : last === "blocked"
              ? ["■", "Blocked", "bad"]
              : last === "acknowledged"
                ? ["●", "Acknowledged", "muted"]
                : ["○", "Not started", "muted"];
    const tone: Record<string, BadgeProps["variant"]> = { ok: "green", warn: "orange", bad: "red", muted: "neutral" };

    const Action = (p: { label: string; hint: string; onClick: () => void; enabled: boolean; primary?: boolean; kind?: string }) => (
        <Button
            label={p.label}
            variant={p.primary ? "primary" : "secondary"}
            size="lg"
            isDisabled={!p.enabled}
            aria-keyshortcuts={p.hint}
            onClick={p.onClick}
            endContent={
                <span aria-hidden="true">
                    <Kbd keys={p.hint} />
                </span>
            }
        >
            {busy && busy === p.kind ? "Sending…" : undefined}
        </Button>
    );

    const recordCard = (
        <Card padding={4} role="region" aria-label="Record entries">
            <VStack gap={3}>
                <HStack gap={2} wrap="wrap" role="toolbar" aria-label="Record">
                    {!running && !done && <Action
                        label={startLabel}
                        hint="S"
                        kind={startKind}
                        primary
                        enabled={canSend && !done && (last === "" || last === "acknowledged" || halted)}
                        onClick={() => askLifecycle(startKind, startLabel)}
                    />}
                    <Action label="Pause" hint="P" enabled={canSend && running} onClick={() => open({ kind: "comment", record: "paused" })} />
                    <Action label="Block" hint="B" enabled={canSend && running} onClick={() => open({ kind: "comment", record: "blocked" })} />
                    <Action label="+ Good" hint="G" enabled={canSend && (running || halted)} onClick={() => open({ kind: "good" })} />
                    <Action label="+ Reject" hint="R" enabled={canSend && (running || halted)} onClick={() => open({ kind: "reject" })} />
                    <Action label="Cycle time" hint="T" enabled={canSend && (running || halted)} onClick={() => open({ kind: "cycle" })} />
                    <Action label="Quality" hint="Q" enabled={canSend && (running || halted)} onClick={() => open({ kind: "quality" })} />
                    <Action label="Issue" hint="I" enabled={canSend && (running || halted)} onClick={() => open({ kind: "issue" })} />
                    <Action label="Complete" hint="C" enabled={canSend && running} onClick={() => open({ kind: "complete" })} />
                    <Action label="Note" hint="N" enabled={canSend} onClick={() => open({ kind: "comment", record: "note" })} />
                </HStack>
                {!online && <Banner status="warning" title="Offline · entry disabled, draft kept" />}
                {shiftClosed && (
                    <Banner
                        status="warning"
                        role="note"
                        title={`${run.state === "planned" ? shiftRunText.planned : shiftRunText.ended} New entries are disabled; corrections stay open.`}
                    />
                )}
                {unresolved && (
                    <div role="alert">
                        <Banner
                            status="warning"
                            title={
                                <>
                                    <strong>Not confirmed:</strong> {KIND_LABEL[unresolved.kind]} may or may not be saved.
                                </>
                            }
                            endContent={
                                <Button label="Check by resending the same entry" size="sm" isDisabled={!online || !!busy} onClick={() => send(unresolved)} />
                            }
                        />
                    </div>
                )}
                {receipt && (
                    <div role="status">
                        <Banner
                            status="success"
                            title={
                                <>
                                    ✓ {receipt.replayed ? "Already recorded" : "Recorded"}: {KIND_LABEL[receipt.record.kind]} ·{" "}
                                    <Time iso={receipt.received_at} tz={tz} />
                                    {receipt.late && " · late entry"}
                                </>
                            }
                        />
                    </div>
                )}
                <Problem error={error} />
                {error instanceof ApiError && lastBody && <FieldPath path={error.problem.path} body={lastBody} form={preview} />}
                {composer && current && isCurrent && (
                    <ComposerPanel
                        key={composerSeq}
                        composer={composer}
                        form={preview}
                        formError={previewError}
                        timeZone={current.timezone}
                        disabled={composer.kind === "correct" ? !canCorrect : !canSend}
                        busy={!!busy}
                        make={make}
                        onSubmit={send}
                        onCancel={() => setComposer(null)}
                    />
                )}
                {lifecycleConfirm && (
                    <Dialog
                        isOpen
                        purpose="form"
                        onOpenChange={(isOpen) => {
                            if (!isOpen && !busy) setLifecycleConfirm(null);
                        }}
                    >
                        <DialogHeader title={`${lifecycleConfirm.label} this job?`} />
                        <VStack gap={3} padding={4}>
                            <Text as="p">
                                This records “{lifecycleConfirm.label}” with your name and the time. It cannot be corrected later.
                            </Text>
                            <HStack gap={2}>
                                <Button
                                    label={`Confirm ${lifecycleConfirm.label}`}
                                    variant="primary"
                                    isDisabled={!canSend || !!busy}
                                    onClick={() => {
                                        const confirmed = lifecycleConfirm;
                                        setLifecycleConfirm(null);
                                        quick(confirmed.kind);
                                    }}
                                />
                                <Button label="Cancel" variant="secondary" isDisabled={!!busy} onClick={() => setLifecycleConfirm(null)} />
                            </HStack>
                        </VStack>
                    </Dialog>
                )}
            </VStack>
        </Card>
    );
    return (
        <VStack gap={4}>
            <VStack gap={1}>
                <HStack justify="between" align="center" gap={2} wrap="wrap">
                    <Heading level={1}>{name}</Heading>
                    <Button
                        label={`History (${records ? shiftRecords.length : "…"})`}
                        onClick={openHistory}
                        aria-keyshortcuts="H"
                        endContent={
                            <span aria-hidden="true">
                                <Kbd keys="H" />
                            </span>
                        }
                    />
                </HStack>
                {current && (
                    <Text as="p" size="sm" color="secondary">
                        <Name id={current.line_id} kind="line" /> · Shift {formatInterval(current.starts_at, current.ends_at, tz)}
                    </Text>
                )}
                <HStack gap={1} align="center" wrap="wrap">
                    <Text size="sm" color="secondary">
                        ID
                    </Text>
                    <CopyId id={stationId} name={name} />
                </HStack>
            </VStack>
            <HStack gap={2} align="center" wrap="wrap">
                <StatusLine current={current} error={currentError} />
                <ShiftRun run={run} tz={tz} />
                {order && records && (
                    <span role="status">
                        <Badge
                            variant={tone[step[2]]}
                            label={
                                <>
                                    <span aria-hidden="true">{step[0]}</span> {step[1]}
                                </>
                            }
                        />
                    </span>
                )}
            </HStack>
            {order && current ? (
                <>
                    <Grid columns={{ minWidth: 340, repeat: "fit" }} gap={4} align="start">
                        <VStack gap={4}>
                            <Card padding={4} role="region" aria-labelledby="job-title">
                                <VStack gap={3}>
                                    <VStack gap={1}>
                                        <Text size="sm" color="secondary">
                                            This job
                                        </Text>
                                        <Heading level={2} id="job-title">
                                            {order.item_id ? <ItemName id={order.item_id} fallback={order.item_code} /> : order.item_code}
                                        </Heading>
                                        <Text as="p" size="sm" color="secondary">
                                            <Name id={order.order_id} kind="order" prefixed />
                                            {order.step_number !== undefined && ` · step ${order.step_number}`} · order rev {current.order!.revision} · instruction rev{" "}
                                            {order.instruction_revision}
                                        </Text>
                                    </VStack>
                                    <Text as="p" size="3xl" weight="bold" hasTabularNumbers className="big">
                                        <VisuallyHidden>Good parts recorded: </VisuallyHidden>
                                        <Val v={totalsKnown ? tally.good : null} />
                                        <Text size="base" weight="normal" color="secondary">
                                            {" "}
                                            / {current.target_quantity} good parts
                                        </Text>
                                    </Text>
                                    {totalsKnown && (
                                        <ProgressBar
                                            label="Good against target"
                                            isLabelHidden
                                            value={pct}
                                            max={100}
                                            variant={remaining !== null && remaining <= 0 ? "success" : "accent"}
                                        />
                                    )}
                                    <Grid columns={{ minWidth: 110 }} gap={2} role="group" aria-label="Recorded this shift">
                                        <Kpi label="Remaining" value={remaining === null ? null : Math.max(0, remaining)} lead />
                                        <Kpi label="Good" value={totalsKnown ? tally.good : null} />
                                        <Kpi label="Reject" value={totalsKnown ? tally.reject : null} />
                                        <Kpi label="Target" value={current.target_quantity} />
                                        <Kpi label="Target cycle (s)" value={current.target_cycle_time_seconds ?? null} />
                                    </Grid>
                                    {!totalsKnown && records && (
                                        <Text as="p" size="sm" color="secondary">
                                            Totals unknown: more records are available; open History to load them.
                                        </Text>
                                    )}
                                    {remaining !== null && remaining < 0 && (
                                        <Banner status="warning" title={`▲ Over target by ${-remaining} (as recorded)`} />
                                    )}
                                </VStack>
                            </Card>
                            {recordCard}
                        </VStack>
                        <Card padding={4} role="region" aria-labelledby="problem-title">
                            <VStack gap={3}>
                                <Heading level={2} id="problem-title">
                                    Report a problem
                                </Heading>
                                <Grid columns={{ minWidth: 140 }} gap={2}>
                                    {PROBLEMS.map(([label, make, needsRun]) => (
                                        <Button
                                            key={label}
                                            label={label}
                                            size="lg"
                                            width="100%"
                                            isDisabled={!canSend || (needsRun && !(running || halted))}
                                            onClick={() => open(make(running))}
                                        />
                                    ))}
                                </Grid>
                                <Text as="p" size="sm" color="secondary">
                                    Saved as a record on this job with your name. It is not sent to anyone.
                                </Text>
                            </VStack>
                        </Card>
                    </Grid>
                    {current.state === "current" && (
                        <Procedure
                            key={`${current.plan_id}:${current.job_order_id}:${order.instruction_document_id}:${order.instruction_revision}`}
                            jobKey={`${current.plan_id}.${current.job_order_id}`}
                            documentId={order.instruction_document_id}
                            revision={order.instruction_revision}
                            sha256={order.instruction_sha256}
                        />
                    )}
                </>
            ) : (
                recordCard
            )}
            {preview && (preview.instructions.some((i) => !i.available) || !preview.ready) && (
                <Banner
                    status="warning"
                    title={
                        <>
                            Form not ready{preview.conflicts.length ? `: conflicts ${preview.conflicts.join(", ")}` : ""}
                            {preview.instructions
                                .filter((i) => !i.available)
                                .map((i) => ` · instruction ${short(i.instruction.document_id)} r${i.instruction.revision} ${i.reason ?? "unavailable"}`)}
                        </>
                    }
                />
            )}
            <VStack gap={1}>
                <Refreshed at={refreshedAt} tz={tz} onRefresh={refresh} />
                <Text as="p" size="sm" color="secondary">
                    {latest ? (
                        <>
                            Last entry <Time iso={latest.received_at} tz={tz} /> by <Actor a={latest.recorded_by} />
                        </>
                    ) : records ? (
                        "No entries this shift"
                    ) : null}
                </Text>
            </VStack>
            {current && (
                <Details
                    items={[
                        ["Workstation ID", stationId],
                        ["Job order ID", current.job_order_id],
                        ["Order ID", order?.order_id],
                        ["Operation ID", order?.operation_id],
                        ["Instruction document", order?.instruction_document_id],
                        ["Plan ID", current.plan_id],
                        ["Plan revision", current.plan_revision],
                        ["Pinned job order revision", current.pinned_revision],
                        ["State", current.state],
                        ["Timezone", current.timezone],
                    ]}
                />
            )}
            {historyOpen && (
                <History
                    records={historyRecords}
                    loaded={historyRecordsPage !== null}
                    more={Boolean(historyRecordsPage?.next_cursor)}
                    loadingMore={historyLoading}
                    loadError={historyError}
                    timeZone={tz}
                    canCorrect={canCorrect}
                    onLoadMore={loadMoreHistory}
                    onCorrect={(r) => {
                        closeHistory();
                        open({ kind: "correct", of: r });
                    }}
                    onClose={closeHistory}
                />
            )}
        </VStack>
    );
}

function Kpi({ label, value, lead }: { label: string; value: number | string | null; lead?: boolean }) {
    return (
        <Card padding={3} variant={lead ? "blue" : "default"} className="kpi">
            <Text size="sm" color="secondary">
                {label}
            </Text>
            <strong>
                <Text size="2xl" weight="bold" hasTabularNumbers>
                    <Val v={value} />
                </Text>
            </strong>
        </Card>
    );
}

function StatusLine({ current, error }: { current: CurrentJobOrderV1 | null; error: unknown }) {
    if (error instanceof ApiError && error.problem.code === "NO_CURRENT_JOB_ORDER") return <Status code="NO_CURRENT_JOB_ORDER" />;
    if (error) return <Problem error={error} />;
    if (!current) return <Skeleton width={140} height={22} aria-label="Loading" />;
    if (current.state === "current") return <Status code="current" />;
    return (
        <HStack as="span" gap={1} align="center" wrap="wrap" role="status">
            <Status code={current.state} />
            {current.instruction_reason && <Status code={current.instruction_reason} />}
            <Text size="sm" color="secondary">
                Entry disabled
            </Text>
        </HStack>
    );
}

// ShiftRun shows whether the supervisor has started or ended this shift, as last read from the server.
// Its wording avoids "Running", which is the job's own state pill next to it.
function ShiftRun({ run, tz }: { run: ShiftRunV1 | null; tz?: string }) {
    if (!run) return null;
    if (run.state === "planned")
        return (
            <Badge
                variant="neutral"
                label={
                    <>
                        <span aria-hidden="true">○</span> Shift not started
                    </>
                }
            />
        );
    if (run.state === "started")
        return (
            <Badge
                variant="green"
                label={
                    <>
                        <span aria-hidden="true">●</span> Shift started <Time iso={run.started_at} tz={tz} />
                    </>
                }
            />
        );
    return (
        <Badge
            variant="neutral"
            label={
                <>
                    <span aria-hidden="true">■</span> Shift ended <Time iso={run.ended_at} tz={tz} />
                </>
            }
        />
    );
}

// FieldPath names the form field behind a server path such as values[2] or fields[1].
function FieldPath({ path, body, form }: { path?: string; body: StationRecordBodyV1; form: StationProfilePreviewV1 | null }) {
    const m = path ? /^(values|fields)\[(\d+)\]/.exec(path) : null;
    if (!m) return null;
    const name = m[1] === "values" ? body.values?.[+m[2]]?.field : form?.fields[+m[2]]?.field.name;
    const label = form?.fields.find((f) => f.field.name === name)?.field.label ?? name;
    return label ? (
        <Text as="p" size="sm" color="secondary">
            Field: {label}
        </Text>
    ) : null;
}

function ComposerPanel(p: {
    composer: Composer;
    form: StationProfilePreviewV1 | null;
    formError: unknown;
    timeZone: string;
    disabled: boolean;
    busy: boolean;
    make: (kind: Kind, extra?: Partial<StationRecordBodyV1>) => StationRecordBodyV1;
    onSubmit: (b: StationRecordBodyV1) => void;
    onCancel: () => void;
}) {
    const c = p.composer;
    const of = c.kind === "correct" ? c.of : null;
    const kind: Kind =
        c.kind === "comment"
            ? c.record
            : c.kind === "good" || c.kind === "reject"
              ? "quantity"
              : c.kind === "cycle"
                ? "cycle_time"
                : c.kind === "issue"
                  ? "material_exception"
                  : c.kind === "complete"
                    ? "completion_evidence"
                    : c.kind === "quality"
                      ? "quality_measurement"
                      : of!.record.kind;
    const [qualityKind, setQualityKind] = useState<Kind>(kind);
    const effective = c.kind === "quality" ? qualityKind : kind;
    const prior = of?.record;
    const [good, setGood] = useState(String(prior?.good_quantity ?? (c.kind === "good" ? 1 : 0)));
    const [reject, setReject] = useState(String(prior?.reject_quantity ?? (c.kind === "reject" ? 1 : 0)));
    const [cycle, setCycle] = useState(prior?.cycle_time_seconds ?? "");
    const [comment, setComment] = useState(prior?.comment ?? (c.kind === "comment" ? (c.prefill ?? "") : ""));
    const [disposition, setDisposition] = useState(prior?.disposition ?? "");
    const [material, setMaterial] = useState(prior?.material_code ?? "");
    const [issue, setIssue] = useState<string>(prior?.material_issue ?? (c.kind === "issue" ? (c.issue ?? "") : ""));
    const [values, setValues] = useState<Record<string, string>>(
        Object.fromEntries((prior?.values ?? []).map((v) => [v.field, v.value])),
    );
    const [errors, setErrors] = useState<{ id: string; message: string }[]>([]);
    const fields = p.form?.ready ? p.form.fields.map((f) => f.field) : null;
    const needsForm = effective === "quality_measurement" || effective === "completion_evidence";

    function build(): StationRecordBodyV1 | null {
        const errs: { id: string; message: string }[] = [];
        const extra: Partial<StationRecordBodyV1> = {};
        const int = (v: string, id: string, label: string) => {
            if (!/^\d+$/.test(v)) errs.push({ id, message: `${label}: whole number 0 or more` });
            return +v;
        };
        if (effective === "quantity") {
            extra.good_quantity = int(good, "rec-good", "Good");
            extra.reject_quantity = int(reject, "rec-reject", "Reject");
            if (!errs.length && extra.good_quantity === 0 && extra.reject_quantity === 0)
                errs.push({ id: "rec-good", message: "Enter a good or reject quantity" });
        } else if (effective === "cycle_time") {
            if (!DECIMAL.test(cycle) || Number(cycle) === 0)
                errs.push({ id: "rec-cycle", message: "Cycle time: seconds above 0, up to 6 decimals" });
            extra.cycle_time_seconds = cycle;
        } else if (effective === "paused" || effective === "blocked" || effective === "note") {
            if (!comment.trim()) errs.push({ id: "rec-comment", message: "Reason is required" });
            extra.comment = comment.trim();
        } else if (effective === "quality_disposition_recorded") {
            if (!disposition) errs.push({ id: "rec-disp-accept", message: "Choose a disposition" });
            extra.disposition = disposition as StationRecordBodyV1["disposition"];
            extra.authorizes_release = false; // WIS-709: required by the server; an operator record never releases product
            if (comment.trim()) extra.comment = comment.trim();
        } else if (effective === "material_exception") {
            if (!material.trim()) errs.push({ id: "rec-material", message: "Material code is required" });
            if (!issue) errs.push({ id: "rec-issue-shortage", message: "Choose the issue" });
            extra.material_code = material.trim();
            extra.material_issue = issue as StationRecordBodyV1["material_issue"];
            if (comment.trim()) extra.comment = comment.trim();
        } else if (needsForm) {
            if (!fields) errs.push({ id: "rec-form", message: "The station form is not available" });
            const out: { field: string; value: string; unit?: string }[] = [];
            fields?.forEach((f, i) => {
                const raw = (values[f.name] ?? "").trim();
                const id = `rec-f-${i}`;
                if (!raw) {
                    if (f.required && effective === "completion_evidence")
                        errs.push({ id, message: `${f.label} is required` });
                    return;
                }
                let value = raw;
                if (f.kind === "decimal") {
                    if (!/^-?\d+(\.\d{1,6})?$/.test(raw)) errs.push({ id, message: `${f.label}: number, up to 6 decimals` });
                    else if ((f.min && Number(raw) < Number(f.min)) || (f.max && Number(raw) > Number(f.max)))
                        errs.push({ id, message: `${f.label}: outside ${f.min ?? "…"}–${f.max ?? "…"}` });
                }
                if (f.kind === "timestamp") value = zonedISO(raw, p.timeZone);
                out.push(f.kind === "decimal" && f.unit ? { field: f.name, value, unit: f.unit } : { field: f.name, value });
            });
            if (!out.length && !errs.length) errs.push({ id: "rec-f-0", message: "Enter at least one value" });
            extra.values = out;
        }
        setErrors(errs);
        if (errs.length) return null;
        if (of)
            return {
                ...p.make(effective, extra),
                job_order_id: of.record.job_order_id,
                job_order_revision: of.record.job_order_revision,
                event_at: of.event_at,
                corrects_record_id: of.id,
            };
        return p.make(effective, extra);
    }
    const submit = (e: React.FormEvent) => {
        e.preventDefault();
        if (p.disabled || p.busy) return;
        const body = build();
        if (body) p.onSubmit(body);
    };
    const title = of ? `Correct ${KIND_LABEL[effective]} ${short(of.id)}` : KIND_LABEL[effective];
    // A one-tap problem prefills its category; the operator still has to say what happened.
    const bare = c.kind === "comment" && !!c.prefill && comment.trim() === c.prefill.trim();


    return (
        <Card padding={4} variant="muted">
            <form onSubmit={submit} aria-label={title} noValidate>
                <VStack gap={3}>
                    <HStack justify="between" align="center" gap={2}>
                        <Heading level={2}>{title}</Heading>
                        <Button label="Cancel" variant="ghost" onClick={p.onCancel} />
                    </HStack>
                    <ErrorList errors={errors} />
                    {c.kind === "quality" && (
                        <Chips
                            legend="Quality record"
                            name="quality-kind"
                            value={qualityKind}
                            options={[
                                ["quality_measurement", "Measurement"],
                                ["quality_disposition_recorded", "Disposition"],
                            ]}
                            onChange={(v) => setQualityKind(v as Kind)}
                        />
                    )}
                    {effective === "quantity" && (
                        <HStack gap={4} wrap="wrap">
                            <Stepper id="rec-good" label="Good" value={good} onChange={setGood} autoFocus={c.kind !== "reject"} />
                            <Stepper id="rec-reject" label="Reject" value={reject} onChange={setReject} autoFocus={c.kind === "reject"} />
                        </HStack>
                    )}
                    {effective === "cycle_time" && (
                        <TextInput id="rec-cycle" label="Cycle time (s)" inputMode="decimal" hasAutoFocus value={cycle} onChange={setCycle} width={200} />
                    )}
                    {effective === "quality_disposition_recorded" && (
                        <Chips
                            legend="Disposition as recorded (does not release product)"
                            name="rec-disp"
                            value={disposition}
                            options={[
                                ["accept", "Accept"],
                                ["hold", "Hold"],
                                ["reject", "Reject"],
                            ]}
                            onChange={setDisposition}
                        />
                    )}
                    {effective === "material_exception" && (
                        <>
                            <TextInput id="rec-material" label="Material code" hasAutoFocus value={material} onChange={setMaterial} {...{ maxLength: 128 }} />
                            <Chips
                                legend="Issue"
                                name="rec-issue"
                                value={issue}
                                options={[
                                    ["shortage", "Shortage"],
                                    ["wrong_material", "Wrong material"],
                                    ["damaged", "Damaged"],
                                    ["other", "Other"],
                                ]}
                                onChange={setIssue}
                            />
                        </>
                    )}
                    {needsForm && !fields && (
                        <div id="rec-form">
                            {p.formError ? (
                                <Problem error={p.formError} />
                            ) : (
                                <Text as="p" size="sm" color="secondary">
                                    Loading station form…
                                </Text>
                            )}
                        </div>
                    )}
                    {needsForm &&
                        fields?.map((f, i) => (
                            <FormField key={f.name} id={`rec-f-${i}`} field={f} value={values[f.name] ?? ""} onChange={(v) => setValues({ ...values, [f.name]: v })} />
                        ))}
                    {needsForm && fields && !fields.length && (
                        <Text as="p" size="sm" color="secondary">
                            This station form has no fields.
                        </Text>
                    )}
                    {["paused", "blocked", "note", "quality_disposition_recorded", "material_exception"].includes(effective) && (
                        <TextInput
                            id="rec-comment"
                            label={effective === "paused" || effective === "blocked" ? "Reason" : effective === "note" ? "Note" : "Comment (optional)"}
                            aria-describedby={bare ? "rec-comment-hint" : undefined}
                            hasAutoFocus={effective === "paused" || effective === "blocked" || effective === "note"}
                            value={comment}
                            onChange={setComment}
                            {...{ maxLength: 1024 }}
                        />
                    )}
                    {bare && (
                        <Text as="p" size="sm" color="secondary" id="rec-comment-hint">
                            Add what happened after “{c.kind === "comment" && c.prefill?.trim()}”.
                        </Text>
                    )}
                    <HStack>
                        <Button
                            type="submit"
                            label={of ? "Submit correction" : "Record"}
                            variant="primary"
                            size="lg"
                            isDisabled={p.disabled || bare}
                            endContent={
                                <span aria-hidden="true">
                                    <Kbd keys="Enter" />
                                </span>
                            }
                        >
                            {p.busy ? "Sending…" : undefined}
                        </Button>
                    </HStack>
                </VStack>
            </form>
        </Card>
    );
}

function ErrorList({ errors }: { errors: { id: string; message: string }[] }) {
    if (!errors.length) return null;
    return (
        <div role="alert">
            <Card padding={3} variant="red">
                <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
                    {errors.map((e) => (
                        <li key={e.id + e.message}>
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
            </Card>
        </div>
    );
}

function Stepper({ id, label, value, onChange, autoFocus }: { id: string; label: string; value: string; onChange: (v: string) => void; autoFocus?: boolean }) {
    const add = (n: number) => onChange(String(Math.max(0, (parseInt(value, 10) || 0) + n)));
    return (
        <HStack gap={1} align="end">
            <TextInput id={id} label={label} inputMode="numeric" hasAutoFocus={autoFocus} value={value} onChange={onChange} width={96} />
            {[1, 5, 10].map((n) => (
                <Button key={n} label={`Add ${n} ${label.toLowerCase()}`} size="lg" onClick={() => add(n)}>
                    +{n}
                </Button>
            ))}
        </HStack>
    );
}

// Chips is a segmented choice with a visible label; SegmentedControl's own label is aria-only.
// Each segment keeps the id `${name}-${value}` so ErrorList can focus it.
function Chips({ legend, name, value, options, onChange }: { legend: string; name: string; value: string; options: [string, string][]; onChange: (v: string) => void }) {
    return (
        <Field label={legend} inputID={name} labelID={`${name}-legend`} isGroupLabel>
            <SegmentedControl id={name} label={legend} value={value} onChange={onChange}>
                {options.map(([v, label]) => (
                    <SegmentedControlItem key={v} id={`${name}-${v}`} value={v} label={label} />
                ))}
            </SegmentedControl>
        </Field>
    );
}

function FormField({ id, field, value, onChange }: { id: string; field: FieldDef; value: string; onChange: (v: string) => void }) {
    const label = `${field.label}${field.unit ? ` (${field.unit})` : ""}${field.required ? "" : " · optional"}`;
    if (field.kind === "choice" || field.kind === "boolean")
        return (
            <Chips
                legend={label}
                name={id}
                value={value}
                options={field.kind === "boolean" ? [["true", "Yes"], ["false", "No"]] : (field.choices ?? []).map((c) => [c, c])}
                onChange={onChange}
            />
        );
    if (field.kind === "timestamp")
        return (
            <Field label={label} inputID={id}>
                <input id={id} type="datetime-local" value={value} onChange={(e) => onChange(e.target.value)} />
            </Field>
        );
    return <TextInput id={id} label={label} inputMode={field.kind === "decimal" ? "decimal" : undefined} value={value} onChange={onChange} />;
}

function History(p: {
    records: Rec[];
    loaded: boolean;
    more: boolean;
    loadingMore: boolean;
    loadError: unknown;
    timeZone?: string;
    canCorrect: boolean;
    onLoadMore: () => void;
    onCorrect: (r: Rec) => void;
    onClose: () => void;
}) {
    const sorted = [...p.records].sort((a, b) => compareInstant(b.event_at, a.event_at));
    return (
        <Drawer isOpen label="Records this shift" onOpenChange={(o) => !o && p.onClose()} width={520} padding={4}>
            <VStack gap={3}>
                <DrawerHeader title="Records this shift" onOpenChange={(o) => !o && p.onClose()} />
                {!p.loaded && <Skeleton height={120} />}
                {p.loaded && !sorted.length && <EmptyState title="No records yet" headingLevel={3} isCompact />}
                {p.more && (
                    <Text as="p" size="sm" color="secondary">
                        Showing {p.records.length} record{p.records.length === 1 ? "" : "s"}; more are available.
                    </Text>
                )}
                <List hasDividers>
                    {sorted.map((r) => (
                        <ListItem
                            key={r.id}
                            label={
                                <HStack as="span" gap={2} align="center" wrap="wrap">
                                    <Text weight="semibold" hasStrikethrough={r.state === "superseded"}>
                                        {KIND_LABEL[r.record.kind]}
                                    </Text>
                                    <Badge
                                        variant={r.state === "superseded" ? "neutral" : "green"}
                                        label={
                                            <>
                                                {r.state === "superseded" ? "✕ superseded" : "● current"}
                                                {r.late ? " · late" : ""}
                                            </>
                                        }
                                    />
                                </HStack>
                            }
                            description={
                                <VStack as="span" gap={1}>
                                    <Text>{describe(r)}</Text>
                                    <Text size="sm" color="secondary">
                                        by <Actor a={r.recorded_by} /> · event <Time iso={r.event_at} tz={p.timeZone} /> · received{" "}
                                        <Time iso={r.received_at} tz={p.timeZone} /> · {r.source === "manual_tablet" ? "tablet" : r.source} · order rev {r.record.job_order_revision} · plan rev{" "}
                                        {r.plan_revision}
                                        {r.record.corrects_record_id && ` · corrects ${short(r.record.corrects_record_id)}`}
                                    </Text>
                                </VStack>
                            }
                            endContent={
                                r.state === "current" && CORRECTABLE(r.record.kind) ? (
                                    <Button label="Correct" size="sm" isDisabled={!p.canCorrect} onClick={() => p.onCorrect(r)} />
                                ) : undefined
                            }
                        />
                    ))}
                </List>
                {p.more && <Button label="Load more records" isDisabled={p.loadingMore} onClick={p.onLoadMore}>{p.loadingMore ? "Loading…" : undefined}</Button>}
                <Problem error={p.loadError} />
            </VStack>
        </Drawer>
    );
}

function describe(r: Rec): string {
    const b = r.record;
    const parts: string[] = [];
    if (b.good_quantity !== undefined) parts.push(`good ${b.good_quantity}`);
    if (b.reject_quantity !== undefined) parts.push(`reject ${b.reject_quantity}`);
    if (b.cycle_time_seconds) parts.push(`${b.cycle_time_seconds} s`);
    if (b.disposition) parts.push(`disposition ${b.disposition} (as recorded)`);
    if (b.material_code) parts.push(`${b.material_code} · ${b.material_issue}`);
    for (const v of b.values ?? []) parts.push(`${v.field} ${v.value}${v.unit ? ` ${v.unit}` : ""}`);
    if (b.comment) parts.push(`“${b.comment}”`);
    return parts.join(" · ") || "—";
}
