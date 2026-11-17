import React, { useEffect, useId, useMemo, useState } from "react";
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
import { VisuallyHidden } from "@astryxdesign/core/VisuallyHidden";
import { VStack } from "@astryxdesign/core/VStack";
import type { ItemPageV1, JobOrderBodyV1, JobOrderPageV1, JobOrderV1, RoutingPageV1 } from "../../contracts/typescript/oee";
import { allPages, ApiError, Intents, mutate, request } from "./api";
import { Announce, canSupervise, Problem, short, useSession, useStations, Val } from "./ui";

// tick sizes a native checkbox or radio; the legacy input rule gives every input a 44 px touch height.
const tick = { minHeight: 0, width: 18, height: 18, padding: 0, margin: 0 } as const;

type Job = JobOrderPageV1["job_orders"][number];
type Catalog = { jobs: Job[]; routes: RoutingPageV1["routings"]; items: ItemPageV1["items"] };

export function JobOrdersPage() {
    const { site } = useSession();
    const [catalog, setCatalog] = useState<Catalog | null>(null);
    const [error, setError] = useState<unknown>(null);
    const [editor, setEditor] = useState<Job | "new" | null>(null);
    const [version, setVersion] = useState(0);
    const [status, setStatus] = useState("");
    useEffect(() => {
        let active = true;
        setCatalog(null); setError(null);
        const base = `/v1/sites/${site.id}`;
        const query = (cursor?: string) => `?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`;
        Promise.all([
            allPages(async c => { const p = await request<JobOrderPageV1>(`${base}/job-orders${query(c)}&include=current`); return { items: p.job_orders, next_cursor: p.next_cursor }; }),
            allPages(async c => { const p = await request<RoutingPageV1>(`${base}/routings${query(c)}`); return { items: p.routings, next_cursor: p.next_cursor }; }),
            allPages(async c => { const p = await request<ItemPageV1>(`${base}/items${query(c)}`); return { items: p.items, next_cursor: p.next_cursor }; }),
        ]).then(([jobs, routes, items]) => { if (active) setCatalog({ jobs, routes, items }); }).catch(e => { if (active) setError(e); });
        return () => { active = false; };
    }, [site.id, version]);
    const linked = catalog?.jobs.filter((job) => job.current_order?.routing_id).length;
    return <section aria-labelledby="jobs-title">
        <VStack gap={3}>
            <HStack justify="between" align="center" gap={2} wrap="wrap">
                <Heading level={1} id="jobs-title">Job orders</Heading>
                <HStack gap={2} align="center">
                    <Button label="Shift plans" variant="ghost" href={`/sites/${site.id}/shifts`} />
                    {canSupervise(site.role) && <Button label="New job order" variant="primary" isDisabled={!catalog} onClick={() => { setEditor("new"); setStatus(""); }} />}
                </HStack>
            </HStack>
            {catalog && <div className="kpis">
                <Kpi label="Job orders" value={catalog.jobs.length} />
                <Kpi label="Linked to a routing step" value={linked} />
                <Kpi label="Unlinked" value={linked === undefined ? undefined : catalog.jobs.length - linked} />
            </div>}
            <Problem error={error} />
            <div role="status">
                {status && <Banner status="success" title={status} />}
                <Announce text={!status && !catalog && !error ? "Loading job orders and routings…" : ""} />
            </div>
            {!catalog && !error && <Skeleton height={160} />}
            {catalog && editor && canSupervise(site.role) ? <JobOrderForm key={editor === "new" ? "new" : `${editor.id}/${editor.current_revision}`} catalog={catalog} job={editor === "new" ? undefined : editor}
                cancel={() => { setEditor(null); setVersion(v => v + 1); }} saved={order => { setEditor(null); setStatus(`Job order saved at revision ${order.current_revision}. Replan any shift that used its earlier revision.`); setVersion(v => v + 1); }} />
            : catalog && (catalog.jobs.length ? <Table aria-label="Job orders" density="compact">
                <TableHeader><TableRow isHeaderRow>
                    <TableHeaderCell scope="col">Item</TableHeaderCell><TableHeaderCell scope="col">Routing step</TableHeaderCell><TableHeaderCell scope="col">Revision</TableHeaderCell><TableHeaderCell scope="col"><VisuallyHidden>Action</VisuallyHidden></TableHeaderCell>
                </TableRow></TableHeader>
                <TableBody>{catalog.jobs.map(job => <TableRow key={job.id}>
                    <TableCell>{job.current_order?.item_code ?? short(job.id)}</TableCell>
                    <TableCell>{job.current_order?.routing_id ? `${catalog.routes.find(r => r.id === job.current_order?.routing_id)?.body.display_name ?? short(job.current_order.routing_id)} · step ${job.current_order.step_number}` : <Badge variant="orange" label="Unlinked" />}</TableCell>
                    <TableCell>r{job.current_revision}</TableCell>
                    <TableCell>{canSupervise(site.role) && <Button label={`Revise ${short(job.id)}`} size="sm" variant="secondary" isDisabled={!job.current_order} onClick={() => { setEditor(job); setStatus(""); }}>Revise</Button>}</TableCell>
                </TableRow>)}</TableBody>
            </Table> : <Text as="p" color="secondary">No job orders yet.</Text>)}
        </VStack>
    </section>;
}

function Kpi({ label, value }: { label: string; value: number | undefined }) {
    return <Card padding={3} className="kpi">
        <Text size="sm" color="secondary">{label}</Text>
        <strong><Val v={value} /></strong>
    </Card>;
}

function JobOrderForm({ catalog, job, saved, cancel }: { catalog: Catalog; job?: Job; saved: (j: JobOrderV1) => void; cancel: () => void }) {
    const { site, online } = useSession();
    const stations = useStations();
    const [body, setBody] = useState<JobOrderBodyV1>(() => job?.current_order ? structuredClone(job.current_order) : {
        schema_version: 1, order_id: crypto.randomUUID(), operation_id: crypto.randomUUID(), item_code: "", target_quantity: 1,
        instruction_document_id: "", instruction_revision: 1, instruction_sha256: "", template: "production",
        fields: [{ name: "good_units", label: "Good units", kind: "decimal", required: true, unit: "count" }],
    });
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const [validation, setValidation] = useState("");
    const [conflictRevision, setConflictRevision] = useState<number | null>(null);
    const intents = useMemo(() => new Intents(), []);
    const route = catalog.routes.find(r => r.id === body.routing_id);
    const patch = (value: Partial<JobOrderBodyV1>) => setBody(b => ({ ...b, ...value }));
    const field = (index: number, value: Partial<JobOrderBodyV1["fields"][number]>) => patch({ fields: body.fields.map((f, i) => i === index ? { ...f, ...value } : f) });
    async function submit(e: React.FormEvent) {
        e.preventDefault(); if (busy || !online) return;
        setError(null); setValidation("");
        if (body.routing_id && !body.step_number) { setValidation("Choose a routing step."); return; }
        setBusy(true);
        try {
            const payload = { ...body };
            if (!payload.routing_id) { delete payload.item_id; delete payload.routing_id; delete payload.step_number; }
            if (!payload.target_cycle_time_seconds) delete payload.target_cycle_time_seconds;
            if (!payload.due_at) delete payload.due_at;
            if (!payload.priority) delete payload.priority;
            const url = `/v1/sites/${site.id}/job-orders${job ? `/${job.id}/revisions` : ""}`;
            saved(await mutate<JobOrderV1>(intents, job ? "revise-job" : "create-job", url, payload, job?.current_revision));
        } catch (e) {
            setError(e);
            // Issue 3: on a 409 conflict, fetch the latest revision in the background so the user
            // can compare and re-apply; keep all typed values (body is unchanged).
            if (e instanceof ApiError && e.problem.code === "REVISION_CONFLICT" && job) {
                request<JobOrderV1>(`/v1/sites/${site.id}/job-orders/${job.id}`)
                    .then((latest) => setConflictRevision(latest.current_revision))
                    .catch(() => {});
            }
        } finally { setBusy(false); }
    }
    const id = useId();
    return <form className="composer" onSubmit={submit}>
        <VStack gap={3}>
        <Heading level={2}>{job ? "Revise job order" : "Create job order"}</Heading>
        <Problem error={error instanceof ApiError && error.problem.code === "REVISION_CONFLICT" ? null : error} />
        {error instanceof ApiError && error.problem.code === "REVISION_CONFLICT" && <div role="alert"><Banner status="warning" title={conflictRevision !== null ? `This job order was updated to revision ${conflictRevision} while you were editing. Your values are kept — review and save again to apply them on the latest revision.` : "This job order changed while you were editing. Your values are kept — save again to apply them."} /></div>}
        {validation && <div role="alert"><Banner status="error" title={validation} /></div>}
        <fieldset disabled={busy || !online} className="plan-fields"><legend>Operation and routing</legend>
            <HStack gap={3} wrap="wrap" align="end">
                <Field label="Production order ID" inputID={`${id}-order`} description="Shared by operations that follow one routing."><input id={`${id}-order`} required value={body.order_id} onChange={e => patch({ order_id: e.target.value })} /></Field>
                <Field label="Operation ID" inputID={`${id}-operation`}><input id={`${id}-operation`} required value={body.operation_id} onChange={e => patch({ operation_id: e.target.value })} /></Field>
            </HStack>
            <HStack gap={3} wrap="wrap" align="end">
                <Field label="Routing (optional)" inputID={`${id}-routing`}><select id={`${id}-routing`} value={body.routing_id ?? ""} onChange={e => {
                    const r = catalog.routes.find(r => r.id === e.target.value);
                    const item = catalog.items.find(i => i.id === r?.body.item_id);
                    patch({ routing_id: r?.id, step_number: undefined, item_id: r?.body.item_id,
                        item_code: r && (!body.item_code || (body.item_id && body.item_id !== r.body.item_id)) ? item?.body.display_name ?? body.item_code : body.item_code });
                }}>
                    <option value="">No explicit link</option>
                    {catalog.routes.filter(r => r.body.active || r.id === body.routing_id).map(r => <option key={r.id} value={r.id} disabled={!r.body.active}>{r.body.display_name}{r.body.active ? "" : " (inactive)"}</option>)}
                </select></Field>
                <Field label="Routing step" inputID={`${id}-step`}><select id={`${id}-step`} required={!!body.routing_id} disabled={!body.routing_id} value={body.step_number ?? ""} onChange={e => patch({ step_number: e.target.value ? Number(e.target.value) : undefined })}>
                    <option value="">Choose step…</option>
                    {route?.body.steps.map(s => <option key={s.step_number} value={s.step_number}>{s.step_number} · {stations.list?.find(w => w.id === s.workstation_id)?.name ?? short(s.workstation_id)}</option>)}
                </select></Field>
                <Field label="Item" inputID={`${id}-item`}><select id={`${id}-item`} value={body.item_id ?? ""} disabled={!!body.routing_id} onChange={e => { const item = catalog.items.find(i => i.id === e.target.value); patch({ item_id: item?.id, item_code: item?.body.display_name ?? body.item_code }); }}>
                    <option value="">Choose item or enter a code</option>
                    {catalog.items.filter(i => i.body.active || i.id === body.item_id).map(i => <option key={i.id} value={i.id}>{i.body.display_name}</option>)}
                </select></Field>
                <Field label="Item code" inputID={`${id}-code`}><input id={`${id}-code`} required value={body.item_code} onChange={e => patch({ item_code: e.target.value })} /></Field>
            </HStack>
            <HStack gap={3} wrap="wrap" align="end">
                <Field label="Target quantity" inputID={`${id}-qty`}><input id={`${id}-qty`} type="number" required min={1} step={1} value={body.target_quantity} onChange={e => patch({ target_quantity: Number(e.target.value) })} /></Field>
                <Field label="Cycle time (seconds)" inputID={`${id}-cycle`}><input id={`${id}-cycle`} value={body.target_cycle_time_seconds ?? ""} onChange={e => patch({ target_cycle_time_seconds: e.target.value })} /></Field>
                <Field label="Priority (1 highest)" inputID={`${id}-priority`}><input id={`${id}-priority`} type="number" min={1} max={9} value={body.priority || ""} onChange={e => patch({ priority: e.target.value ? Number(e.target.value) : undefined })} /></Field>
                <Field label="Due time (with timezone)" inputID={`${id}-due`}><input id={`${id}-due`} placeholder="2026-10-30T16:00:00Z" value={body.due_at ?? ""} onChange={e => patch({ due_at: e.target.value })} /></Field>
            </HStack>
        </fieldset>
        <fieldset disabled={busy || !online} className="plan-fields"><legend>Approved instruction document</legend>
            <HStack gap={3} wrap="wrap" align="end">
                <Field label="Instruction document ID" inputID={`${id}-doc`}><input id={`${id}-doc`} required value={body.instruction_document_id} onChange={e => patch({ instruction_document_id: e.target.value })} /></Field>
                <Field label="Instruction revision" inputID={`${id}-docrev`}><input id={`${id}-docrev`} required type="number" min={1} value={body.instruction_revision} onChange={e => patch({ instruction_revision: Number(e.target.value) })} /></Field>
                <Field label="Instruction SHA-256" inputID={`${id}-sha`}><input id={`${id}-sha`} required pattern="[a-f0-9]{64}" value={body.instruction_sha256} onChange={e => patch({ instruction_sha256: e.target.value })} /></Field>
            </HStack>
        </fieldset>
        <fieldset disabled={busy || !online} className="plan-fields"><legend>Operator form</legend>
            <VStack gap={2}>
            <Field label="Template" inputID={`${id}-template`}><select id={`${id}-template`} value={body.template} onChange={e => patch({ template: e.target.value })}><option>production</option><option>quality</option><option>material</option></select></Field>
            {body.fields.map((f, i) => <HStack gap={3} wrap="wrap" align="end" key={i}>
                <Field label={`Field ${i + 1} name`} inputID={`${id}-f${i}-name`}><input id={`${id}-f${i}-name`} required value={f.name} onChange={e => field(i, { name: e.target.value })} /></Field>
                <Field label={`Field ${i + 1} label`} inputID={`${id}-f${i}-label`}><input id={`${id}-f${i}-label`} required value={f.label} onChange={e => field(i, { label: e.target.value })} /></Field>
                <Field label={`Field ${i + 1} kind`} inputID={`${id}-f${i}-kind`}><select id={`${id}-f${i}-kind`} value={f.kind} onChange={e => field(i, { kind: e.target.value, unit: e.target.value === "decimal" ? (f.unit || "count") : undefined, choices: e.target.value === "choice" ? ["pass", "fail"] : undefined })}><option>decimal</option><option>timestamp</option><option>text</option><option>boolean</option><option>choice</option></select></Field>
                {f.kind === "decimal" && <Field label={`Field ${i + 1} unit`} inputID={`${id}-f${i}-unit`}><input id={`${id}-f${i}-unit`} required value={f.unit ?? ""} onChange={e => field(i, { unit: e.target.value || undefined })} /></Field>}
                {f.kind === "choice" && <Field label={`Field ${i + 1} choices (comma separated)`} inputID={`${id}-f${i}-choices`}><input id={`${id}-f${i}-choices`} required value={f.choices?.join(",") ?? ""} onChange={e => field(i, { choices: e.target.value.split(",").map(v => v.trim()) })} /></Field>}
                <label style={{ display: "flex", alignItems: "center", gap: 8 }}><input type="checkbox" style={tick} checked={f.required} onChange={e => field(i, { required: e.target.checked })} /> Field {i + 1} required</label>
                <Button label={`Remove field ${i + 1}`} variant="ghost" size="sm" isDisabled={body.fields.length === 1} onClick={() => patch({ fields: body.fields.filter((_, n) => n !== i) })} />
            </HStack>)}
            <div><Button label="Add field" variant="secondary" size="sm" onClick={() => patch({ fields: [...body.fields, { name: "", label: "", kind: "text", required: false }] })} /></div>
            </VStack>
        </fieldset>
        <HStack gap={2}>
            <Button type="submit" variant="primary" label={job ? "Save job revision" : "Create job order"} isDisabled={busy || !online} />
            <Button label="Cancel" variant="secondary" isDisabled={busy} onClick={cancel} />
        </HStack>
        {busy && <p role="status">Saving job order…</p>}
        </VStack>
    </form>;
}
