import React, { useEffect, useId, useMemo, useRef, useState } from "react";
import { ArrowDown, ArrowUp, Plus, X } from "lucide-react";
import { Badge } from "@astryxdesign/core/Badge";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Field } from "@astryxdesign/core/Field";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { VStack } from "@astryxdesign/core/VStack";
import type { ItemPageV1, LinePageV1, RoutingBodyV1, RoutingPageV1, RoutingV1 } from "../../contracts/typescript/oee";
import { allPages, ApiError, Intents, mutate, request, sameBody, fieldOf } from "./api";
import { Actor, Link, Problem, Time, Val, canSupervise, useSession, useStations } from "./ui";

// tick sizes a native checkbox or radio; the legacy input rule gives every input a 44 px touch height.
const tick = { minHeight: 0, width: 18, height: 18, padding: 0, margin: 0 } as const;

type Line = LinePageV1["lines"][number];
type Item = ItemPageV1["items"][number];
type Routing = RoutingPageV1["routings"][number];
type Catalog = { lines: Line[]; items: Item[]; routings: Routing[] };
type Step = RoutingBodyV1["steps"][number];
type FieldTarget = { field: string; step?: number };

const emptyBody = (): RoutingBodyV1 => ({ schema_version: 1, display_name: "", active: true, line_id: "", item_id: "", steps: [] });
const numbered = (ids: readonly string[]): Step[] => ids.map((workstation_id, i) => ({ step_number: i + 1, workstation_id }));

async function catalog(site: string): Promise<Catalog> {
    const base = `/v1/sites/${site}`;
    const query = (cursor?: string) => `?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`;
    const [lines, items, routings] = await Promise.all([
        allPages(async (cursor) => {
            const page = await request<LinePageV1>(`${base}/lines${query(cursor)}`);
            return { items: page.lines, next_cursor: page.next_cursor };
        }),
        allPages(async (cursor) => {
            const page = await request<ItemPageV1>(`${base}/items${query(cursor)}`);
            return { items: page.items, next_cursor: page.next_cursor };
        }),
        allPages(async (cursor) => {
            const page = await request<RoutingPageV1>(`${base}/routings${query(cursor)}`);
            return { items: page.routings, next_cursor: page.next_cursor };
        }),
    ]);
    return { lines, items, routings };
}

// ReadOnly shows a field's value to a viewer: a label and text, no control.
function ReadOnly({ label, children }: { label: string; children: React.ReactNode }) {
    return (
        <VStack gap={0.5}>
            <Text size="sm" color="secondary">{label}</Text>
            <Text weight="semibold">{children}</Text>
        </VStack>
    );
}

function routingBody(value: RoutingV1): RoutingBodyV1 {
    const revision = value.revisions.find((entry) => entry.revision === value.current_revision);
    if (!revision) throw new Error("Current routing revision is missing.");
    return revision.body;
}

export function RoutingList() {
    const { site } = useSession();
    const stations = useStations();
    const canEdit = canSupervise(site.role);
    const [data, setData] = useState<Catalog | null>(null);
    const [error, setError] = useState<unknown>(null);
    const [lineName, setLineName] = useState("");
    const [savingLine, setSavingLine] = useState(false);
    const [lineMessage, setLineMessage] = useState("");
    const lineIntents = useMemo(() => new Intents(), []);
    const load = () => catalog(site.id).then(setData, setError);
    useEffect(() => { void load(); }, [site.id]);

    async function createLine(event: React.KeyboardEvent<HTMLInputElement>) {
        if (event.key !== "Enter") return;
        event.preventDefault();
        const name = lineName.trim();
        if (!canEdit || !name || savingLine) return;
        setSavingLine(true);
        setLineMessage("");
        setError(null);
        try {
            await mutate(lineIntents, "new-line", `/v1/sites/${site.id}/lines`, { schema_version: 1, display_name: name, active: true }, 0);
            setLineName("");
            setLineMessage(`${name} added.`);
            await load();
        } catch (reason) {
            setError(reason);
        } finally {
            setSavingLine(false);
        }
    }

    const lineNames = new Map((data?.lines ?? []).map((line) => [line.id, line.body.display_name]));
    const itemNames = new Map((data?.items ?? []).map((item) => [item.id, item.body.display_name]));
    const stationNames = new Map((stations.list ?? []).map((station) => [station.id, station.name]));
    const groups = new Map<string, Routing[]>();
    for (const routing of data?.routings ?? []) groups.set(routing.body.line_id, [...(groups.get(routing.body.line_id) ?? []), routing]);
    const active = data?.routings.filter((routing) => routing.body.active).length;
    return (
        <section aria-labelledby="routing-list-title">
            <VStack gap={3}>
                <HStack justify="between" align="center" gap={2} wrap="wrap">
                    <Heading level={1} id="routing-list-title">Routings</Heading>
                    {canEdit && <Button label="New routing" variant="primary" href={`/sites/${site.id}/routings/new`} />}
                </HStack>
                <div className="kpis">
                    <Kpi label="Routings" value={data?.routings.length} />
                    <Kpi label="Active" value={active} />
                    <Kpi label="Lines" value={data?.lines.length} />
                </div>
                {canEdit && <HStack className="routing-new-line" gap={2} align="center" wrap="wrap">
                    <label htmlFor="routing-new-line">New line</label>
                    <input id="routing-new-line" aria-label="New line name" value={lineName} maxLength={128} onChange={(e) => setLineName(e.target.value)} onKeyDown={createLine} placeholder="Name, then Enter" />
                    {savingLine && <span role="status">Adding line…</span>}
                    {lineMessage && <span role="status">✓ {lineMessage}</span>}
                </HStack>}
                <Problem error={error} />
                <Problem error={stations.error} />
                {!data && !error && <div role="status" aria-label="Loading routings"><Skeleton height={200} /></div>}
                {data && (data.routings.length ? <Table aria-label="Site routings" density="compact">
                    <TableHeader><TableRow isHeaderRow>{["Line", "Routing", "Item", "Steps", "State", "Revision"].map((h) => <TableHeaderCell key={h} scope="col">{h}</TableHeaderCell>)}</TableRow></TableHeader>
                    {[...groups].sort(([a], [b]) => (lineNames.get(a) ?? a).localeCompare(lineNames.get(b) ?? b)).map(([lineId, routings]) => <TableBody key={lineId}>
                        <TableRow><TableCell scope="rowgroup" colSpan={6}><Text weight="semibold">{lineNames.get(lineId) ?? lineId}</Text> <Text size="sm" color="secondary" hasTabularNumbers>{routings.length}</Text></TableCell></TableRow>
                        {routings.map((routing) => <TableRow key={routing.id}>
                            <TableCell>{lineNames.get(routing.body.line_id) ?? routing.body.line_id}</TableCell>
                            <TableHeaderCell scope="row"><Link to={`/sites/${site.id}/routings/${routing.id}`}>{routing.body.display_name}</Link></TableHeaderCell>
                            <TableCell>{itemNames.get(routing.body.item_id) ?? routing.body.item_id}</TableCell>
                            <TableCell><ol style={{ margin: 0, paddingInlineStart: 18 }}>{[...routing.body.steps].sort((a, b) => a.step_number - b.step_number).map((step) => <li key={step.step_number}>{stationNames.get(step.workstation_id) ?? step.workstation_id}</li>)}</ol></TableCell>
                            <TableCell><Badge variant={routing.body.active ? "green" : "neutral"} label={routing.body.active ? "Active" : "Inactive"} /></TableCell>
                            <TableCell>r{routing.current_revision}</TableCell>
                        </TableRow>)}
                    </TableBody>)}
                </Table> : <Text as="p" color="secondary">No routings have been configured.</Text>)}
            </VStack>
        </section>
    );
}

function Kpi({ label, value }: { label: string; value: number | undefined }) {
    return (
        <Card padding={3} className="kpi">
            <Text size="sm" color="secondary">{label}</Text>
            <strong><Val v={value} /></strong>
        </Card>
    );
}

export function RoutingEditor({ id }: { id?: string }) {
    const { site, navigate, online } = useSession();
    const stations = useStations();
    const canEdit = canSupervise(site.role);
    const base = `/v1/sites/${site.id}`;
    const [data, setData] = useState<Omit<Catalog, "routings"> | null>(null);
    const [draft, setDraft] = useState<RoutingBodyV1>(emptyBody);
    const [saved, setSaved] = useState<RoutingBodyV1 | null>(null);
    const [revision, setRevision] = useState(0);
    const [latest, setLatest] = useState<RoutingV1["revisions"][number] | null>(null);
    const [conflict, setConflict] = useState<{ value: RoutingV1; body: RoutingBodyV1 } | null>(null);
    const [target, setTarget] = useState<FieldTarget | null>(null);
    const [error, setError] = useState<unknown>(null);
    const [busy, setBusy] = useState(false);
    const [stationText, setStationText] = useState("");
    const [stationMessage, setStationMessage] = useState("");
    const [announcement, setAnnouncement] = useState("");
    const intents = useMemo(() => new Intents(), []);
    const nameRef = useRef<HTMLInputElement>(null);
    const lineRef = useRef<HTMLSelectElement>(null);
    const itemRef = useRef<HTMLSelectElement>(null);
    const addRef = useRef<HTMLInputElement>(null);
    const rows = useRef<(HTMLLIElement | null)[]>([]);
    const nextStepKey = useRef(0);
    const [stepKeys, setStepKeys] = useState<number[]>([]);
    const diagramId = useId();
    const dirty = !saved || !sameBody(draft, saved);

    function newStepKeys(count: number) {
        return Array.from({ length: count }, () => nextStepKey.current++);
    }

    useEffect(() => {
        let active = true;
        setData(null); setError(null); setConflict(null);
        const query = (cursor?: string) => `?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`;
        Promise.all([
            allPages(async (cursor) => { const page = await request<LinePageV1>(`${base}/lines${query(cursor)}`); return { items: page.lines, next_cursor: page.next_cursor }; }),
            allPages(async (cursor) => { const page = await request<ItemPageV1>(`${base}/items${query(cursor)}`); return { items: page.items, next_cursor: page.next_cursor }; }),
            id ? request<RoutingV1>(`${base}/routings/${id}`) : Promise.resolve(null),
        ]).then(([lines, items, value]) => {
            if (!active) return;
            setData({ lines, items });
            if (value) {
                const body = routingBody(value);
                setStepKeys(newStepKeys(body.steps.length));
                setDraft(body); setSaved(body); setRevision(value.current_revision);
                setLatest(value.revisions.find((entry) => entry.revision === value.current_revision) ?? null);
            } else {
                const body = emptyBody();
                if (lines[0]) body.line_id = lines[0].id;
                if (items[0]) body.item_id = items[0].id;
                setStepKeys([]);
                setDraft(body); setSaved(null); setRevision(0); setLatest(null);
            }
        }).catch((reason: unknown) => { if (active) setError(reason); });
        return () => { active = false; };
    }, [base, id]);

    useEffect(() => {
        if (!target) return;
        const step = target.step;
        if (step !== undefined) requestAnimationFrame(() => rows.current[step]?.focus());
        else if (target.field === "display_name") nameRef.current?.focus();
        else if (target.field === "line_id") lineRef.current?.focus();
        else if (target.field === "item_id") itemRef.current?.focus();
    }, [target]);

    const stationById = new Map((stations.list ?? []).map((station) => [station.id, station]));
    const stationLabels = useMemo(() => {
        const counts = new Map<string, number>();
        for (const station of stations.list ?? []) counts.set(station.name, (counts.get(station.name) ?? 0) + 1);
        return new Map((stations.list ?? []).map((station) => [station.id, counts.get(station.name)! > 1 ? `${station.name} · ${station.id.slice(0, 8)}` : station.name]));
    }, [stations.list]);
    const stationByLabel = new Map([...(stations.list ?? [])].map((station) => [stationLabels.get(station.id)!, station]));
    const lineNames = new Map((data?.lines ?? []).map((line) => [line.id, line.body.display_name]));
    const itemNames = new Map((data?.items ?? []).map((item) => [item.id, item.body.display_name]));
    const localPath = !draft.display_name.trim() ? "display_name" : !draft.line_id ? "line_id" : !draft.item_id ? "item_id" : draft.steps.length < 1 || draft.steps.length > 256 ? "steps" : "";

    function update(patch: Partial<RoutingBodyV1>) {
        setError(null); setTarget(null); setDraft((current) => ({ ...current, ...patch }));
    }
    function updateSteps(ids: readonly string[], focusIndex?: number, message?: string) {
        update({ steps: numbered(ids) });
        if (focusIndex !== undefined) requestAnimationFrame(() => rows.current[focusIndex]?.focus());
        if (message) setAnnouncement(message);
    }
    function move(index: number, by: number) {
        const next = index + by;
        if (!canEdit || next < 0 || next >= draft.steps.length) return;
        const focused = document.activeElement;
        const refocus = focused === rows.current[index] ? rows.current[index] : focused instanceof HTMLButtonElement && rows.current[index]?.contains(focused) ? focused : null;
        const ids = draft.steps.map((step) => step.workstation_id);
        const keys = [...stepKeys];
        [ids[index], ids[next]] = [ids[next], ids[index]];
        [keys[index], keys[next]] = [keys[next], keys[index]];
        setStepKeys(keys);
        updateSteps(ids, undefined, `Moved ${stationLabels.get(ids[next]) ?? ids[next]} to position ${next + 1}`);
        if (refocus) requestAnimationFrame(() => refocus.focus());
    }
    function remove(index: number) {
        const ids = draft.steps.map((step) => step.workstation_id);
        const keys = [...stepKeys];
        const [removed] = ids.splice(index, 1);
        keys.splice(index, 1);
        setStepKeys(keys);
        updateSteps(ids, ids.length ? Math.min(index, ids.length - 1) : undefined, `Removed ${stationLabels.get(removed) ?? removed}`);
        if (!ids.length) requestAnimationFrame(() => addRef.current?.focus());
    }
    function insertStation(index: number) {
        if (!canEdit || draft.steps.length >= 256) return;
        const station = stationByLabel.get(stationText.trim());
        if (!station) { setStationMessage("Choose a workstation name from the suggestions."); return; }
        if (!data?.lines.some((line) => line.id === draft.line_id)) { setStationMessage("Choose a line before adding steps."); return; }
        const ids = draft.steps.map((step) => step.workstation_id);
        const keys = [...stepKeys];
        ids.splice(index + 1, 0, station.id);
        keys.splice(index + 1, 0, nextStepKey.current++);
        setStepKeys(keys);
        setStationText(""); setStationMessage("");
        updateSteps(ids, index + 1, `Inserted ${stationLabels.get(station.id)} at position ${index + 2}`);
    }
    function addStation(event: React.KeyboardEvent<HTMLInputElement>) {
        if (event.key !== "Enter") return;
        event.preventDefault();
        if (!canEdit || draft.steps.length >= 256) return;
        const station = stationByLabel.get(stationText.trim());
        if (!station) { setStationMessage("Choose a workstation name from the suggestions."); return; }
        if (!data?.lines.some((line) => line.id === draft.line_id)) { setStationMessage("Choose a line before adding steps."); return; }
        const ids = [...draft.steps.map((step) => step.workstation_id), station.id];
        setStepKeys([...stepKeys, nextStepKey.current++]);
        setStationText(""); setStationMessage("");
        updateSteps(ids, ids.length - 1, `Added ${stationLabels.get(station.id)} at position ${ids.length}`);
    }

    async function reloadConflict() {
        if (!id) return;
        const value = await request<RoutingV1>(`${base}/routings/${id}`);
        setConflict({ value, body: routingBody(value) });
    }
    async function save() {
        if (!canEdit || !online || busy || !dirty || localPath || conflict) {
            if (localPath) {
                const target = fieldOf(localPath);
                setTarget(target);
                setError(new ApiError({ type: "about:blank", title: "Invalid routing", status: 422, code: "INVALID_MASTER_DATA", correlation_id: "", path: localPath }));
            }
            return;
        }
        const body: RoutingBodyV1 = { ...draft, steps: numbered(draft.steps.map((step) => step.workstation_id)) };
        setBusy(true); setError(null); setTarget(null);
        try {
            const result = id
                ? await mutate<RoutingV1>(intents, "routing-save", `${base}/routings/${id}/revisions`, body, revision)
                : await mutate<RoutingV1>(intents, "routing-save", `${base}/routings`, body, 0);
            const accepted = routingBody(result);
            setDraft(accepted); setSaved(accepted); setRevision(result.current_revision);
            setLatest(result.revisions.find((entry) => entry.revision === result.current_revision) ?? null);
            if (!id) navigate(`/sites/${site.id}/routings/${result.id}`);
        } catch (reason) {
            if (reason instanceof ApiError && reason.problem.status === 412 && id) {
                try { await reloadConflict(); } catch (reloadError) { setError(reloadError); }
            } else {
                setError(reason);
                if (reason instanceof ApiError && reason.problem.status === 422) {
                    const mapped = fieldOf(reason.problem.path);
                    if (mapped && (mapped.step === undefined || draft.steps[mapped.step])) setTarget(mapped);
                }
            }
        } finally { setBusy(false); }
    }
    function useServer() {
        if (!conflict) return;
        setStepKeys(newStepKeys(conflict.body.steps.length));
        setDraft(conflict.body); setSaved(conflict.body); setRevision(conflict.value.current_revision);
        setLatest(conflict.value.revisions.find((entry) => entry.revision === conflict.value.current_revision) ?? null);
        setConflict(null); setError(null); setTarget(null);
    }
    function keepMine() {
        if (!conflict) return;
        setSaved(conflict.body); setRevision(conflict.value.current_revision);
        setLatest(conflict.value.revisions.find((entry) => entry.revision === conflict.value.current_revision) ?? null);
        setConflict(null); setError(null);
    }
    function discard() {
        if (conflict) { useServer(); return; }
        if (!id) { navigate(`/sites/${site.id}/routings`); return; }
        if (saved) {
            setStepKeys(newStepKeys(saved.steps.length));
            setDraft(saved);
        }
        setError(null); setTarget(null);
    }

    const fieldID = diagramId;
    if (!data && !error) return <div role="status" aria-label="Loading routing"><Skeleton height={320} /></div>;
    if (!data) return <Problem error={error} />;
    const lineName = lineNames.get(draft.line_id) ?? "No line selected";
    const isDirty = dirty;
    const targetFor = (step: number) => target?.step === step;
    const serverBody = conflict?.body;
    const describeSteps = (body: RoutingBodyV1) => body.steps.map((step) => stationLabels.get(step.workstation_id) ?? stationById.get(step.workstation_id)?.name ?? step.workstation_id);

    return (
        <section className="routing-editor" aria-labelledby="routing-title" onKeyDownCapture={(event) => {
            if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "s") { event.preventDefault(); void save(); }
        }}>
            <VStack gap={3}>
            <HStack gap={2} align="center" wrap="wrap">
                <Heading level={1} id="routing-title">{id ? draft.display_name || "Routing" : "New routing"}</Heading>
                {id && <Badge variant="neutral" label={`rev ${revision}`} />}
                <Badge variant={draft.active ? "green" : "neutral"} label={draft.active ? "Active" : "Inactive"} />
            </HStack>
            <Problem error={error} />
            <Problem error={stations.error} />
            <div className="routing-workspace">
                <Card padding={4}>
                    <VStack gap={3}>
                    <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(200px, 1fr))", gap: 12, alignItems: "end" }}>
                        {canEdit ? <>
                            <Field label="Name" inputID={`${fieldID}-name`}><input id={`${fieldID}-name`} ref={nameRef} disabled={busy} aria-invalid={target?.field === "display_name" || undefined} aria-describedby={target?.field === "display_name" ? "routing-field-error" : undefined} value={draft.display_name} maxLength={128} onChange={(e) => update({ display_name: e.target.value })} /></Field>
                            <Field label="Line" inputID={`${fieldID}-line`}><select id={`${fieldID}-line`} ref={lineRef} disabled={busy} aria-invalid={target?.field === "line_id" || undefined} aria-describedby={target?.field === "line_id" ? "routing-field-error" : undefined} value={draft.line_id} onChange={(e) => update({ line_id: e.target.value })}><option value="">Select a line</option>{data.lines.map((line) => <option key={line.id} value={line.id}>{line.body.display_name}</option>)}</select></Field>
                            <Field label="Item" inputID={`${fieldID}-item`}><select id={`${fieldID}-item`} ref={itemRef} disabled={busy} aria-invalid={target?.field === "item_id" || undefined} aria-describedby={target?.field === "item_id" ? "routing-field-error" : undefined} value={draft.item_id} onChange={(e) => update({ item_id: e.target.value })}><option value="">Select an item</option>{data.items.map((item) => <option key={item.id} value={item.id}>{item.body.display_name}</option>)}</select></Field>
                            <label style={{ display: "flex", alignItems: "center", gap: 8 }}><input type="checkbox" style={tick} checked={draft.active} disabled={busy} onChange={(e) => update({ active: e.target.checked })} /> Active</label>
                        </> : <>
                            <ReadOnly label="Name">{draft.display_name}</ReadOnly>
                            <ReadOnly label="Line">{lineName}</ReadOnly>
                            <ReadOnly label="Item">{itemNames.get(draft.item_id) ?? (draft.item_id || "No item selected")}</ReadOnly>
                        </>}
                    </div>
                    <HStack gap={2} align="center">
                        <Heading level={2}>Steps</Heading>
                        <Badge variant="neutral" label={<>{draft.steps.length}</>} />
                    </HStack>
                    {target && <p id="routing-field-error" className="routing-field-error">Invalid {target.field.replaceAll("_", " ")}.</p>}
                    <ol className="routing-steps" aria-label="Routing steps">
                        {draft.steps.map((step, index) => {
                            const station = stationById.get(step.workstation_id);
                            const otherLine = !!station && station.line_id !== draft.line_id;
                            const label = stationLabels.get(step.workstation_id) ?? station?.name ?? step.workstation_id;
                            return <li key={stepKeys[index]} ref={(node) => { rows.current[index] = node; }} className={targetFor(index) ? "routing-step-invalid" : undefined} tabIndex={0} aria-invalid={targetFor(index) || undefined} aria-describedby={targetFor(index) ? "routing-field-error" : undefined} aria-keyshortcuts="Alt+ArrowUp Alt+ArrowDown Delete" onKeyDown={(event) => {
                                if (!canEdit || busy) return;
                                if (event.altKey && event.key === "ArrowUp") { event.preventDefault(); move(index, -1); }
                                else if (event.altKey && event.key === "ArrowDown") { event.preventDefault(); move(index, 1); }
                                else if (event.key === "Delete") { event.preventDefault(); remove(index); }
                            }}>
                                <span className="routing-step-number">{step.step_number}</span><span className="routing-step-name">{label}<small>{otherLine ? "on another line" : station?.line_id ? (lineNames.get(station.line_id) ?? "same line") : "Line unavailable"}</small></span>
                                {canEdit && <HStack gap={0.5}>
                                    <Button label={`Move ${label} up`} isIconOnly icon={<ArrowUp size={16} aria-hidden />} variant="ghost" size="sm" isDisabled={busy || index === 0} onClick={() => move(index, -1)} />
                                    <Button label={`Move ${label} down`} isIconOnly icon={<ArrowDown size={16} aria-hidden />} variant="ghost" size="sm" isDisabled={busy || index === draft.steps.length - 1} onClick={() => move(index, 1)} />
                                    <Button label={`Insert after ${label}`} isIconOnly icon={<Plus size={16} aria-hidden />} variant="ghost" size="sm" isDisabled={busy || !stationText.trim() || draft.steps.length >= 256} onClick={() => insertStation(index)} />
                                    <Button label={`Remove ${label}`} isIconOnly icon={<X size={16} aria-hidden />} variant="ghost" size="sm" isDisabled={busy} onClick={() => remove(index)} />
                                </HStack>}
                            </li>;
                        })}
                    </ol>
                    {canEdit && <Field label="Add station" inputID="routing-station">
                        <input ref={addRef} id="routing-station" list="routing-stations" placeholder="Workstation name, then Enter" value={stationText} onChange={(e) => { setStationText(e.target.value); setStationMessage(""); }} onKeyDown={addStation} disabled={busy || draft.steps.length >= 256} />
                        <datalist id="routing-stations">{(stations.list ?? []).map((station) => <option key={station.id} value={stationLabels.get(station.id)} label={station.line_id === draft.line_id ? lineName : "on another line"} />)}</datalist>
                    </Field>}
                    {canEdit && stationMessage && <div role="alert"><Banner status="error" title={stationMessage} /></div>}
                    </VStack>
                </Card>
                <Card padding={4}>
                    <div style={{ overflowX: "auto" }}>
                    <svg className="routing-diagram" role="img" aria-labelledby={`${diagramId}-title ${diagramId}-desc`} width={Math.max(420, 48 + draft.steps.length * 148)} height="150" viewBox={`0 0 ${Math.max(420, 48 + draft.steps.length * 148)} 150`}>
                        <title id={`${diagramId}-title`}>Routing diagram for {draft.display_name || "new routing"}</title>
                        <desc id={`${diagramId}-desc`}>Routing {draft.display_name || "new routing"}: {describeSteps(draft).join(" then ") || "no steps yet"}.</desc>
                        <text x="24" y="24" className="routing-lane-label">{lineName}</text>
                        <line x1="24" y1="91" x2={Math.max(24, 48 + draft.steps.length * 148 - 20)} y2="91" className="routing-lane" />
                        {draft.steps.map((step, index) => {
                            const station = stationById.get(step.workstation_id);
                            const x = 24 + index * 148;
                            const invalid = targetFor(index);
                            return <g key={`${step.workstation_id}-${index}`} className={`${station && station.line_id !== draft.line_id ? "routing-node-other" : "routing-node"}${invalid ? " routing-node-invalid" : ""}`} tabIndex={-1} onClick={() => rows.current[index]?.focus()}>
                                {index > 0 && <path d={`M ${x - 28} 91 l 8 -5 v 10 z`} className="routing-arrow" />}
                                <rect x={x} y="62" width="120" height="58" rx="6" />
                                <text x={x + 8} y="82" className="routing-node-number">{index + 1}</text>
                                <text x={x + 28} y="96" className="routing-node-label">{(stationLabels.get(step.workstation_id) ?? station?.name ?? step.workstation_id).slice(0, 16)}</text>
                                {invalid && <text x={x + 106} y="76" className="routing-invalid-badge">!</text>}
                            </g>;
                        })}
                    </svg>
                    </div>
                </Card>
            </div>
            {conflict && serverBody && <section role="alert" aria-labelledby="routing-conflict-title">
                <Card padding={4}>
                    <VStack gap={3}>
                        <Heading level={2} id="routing-conflict-title">Changed on the server (rev {conflict.value.current_revision})</Heading>
                        <div className="routing-compare"><div><Heading level={3}>Your draft</Heading><p>{draft.display_name} · {lineNames.get(draft.line_id) ?? draft.line_id} · {itemNames.get(draft.item_id) ?? draft.item_id}</p><ol>{describeSteps(draft).map((name, i) => <li key={`${name}-${i}`}>{name}</li>)}</ol></div><div><Heading level={3}>Server version</Heading><p>{serverBody.display_name} · {lineNames.get(serverBody.line_id) ?? serverBody.line_id} · {itemNames.get(serverBody.item_id) ?? serverBody.item_id}</p><ol>{describeSteps(serverBody).map((name, i) => <li key={`${name}-${i}`}>{name}</li>)}</ol></div></div>
                        <HStack gap={2}>
                            <Button label={`Keep mine on rev ${conflict.value.current_revision}`} variant="secondary" onClick={keepMine} />
                            <Button label="Use server version" variant="primary" onClick={useServer} />
                        </HStack>
                    </VStack>
                </Card>
            </section>}
            <HStack className="routing-footer" gap={2} align="center" wrap="wrap">
                <Text size="sm" color="secondary" hasTabularNumbers><span role="status">{isDirty ? `Unsaved changes · ${draft.steps.length} steps` : `Saved · rev ${revision}`}</span></Text>
                {latest && <Text size="sm" color="secondary">saved by <Actor a={latest.submitted_by} /> · <Time iso={latest.created_at} /></Text>}
                {canEdit && <>
                    <Button label="Discard" variant="secondary" isDisabled={busy || (!isDirty && !conflict)} onClick={discard} />
                    <Button label={busy ? "Saving…" : "Save"} variant="primary" isDisabled={!online || busy || !isDirty || !!localPath || !!conflict} onClick={() => void save()} />
                </>}
            </HStack>
            </VStack>
            <p className="sr-only" aria-live="polite">{announcement}</p>
        </section>
    );
}
