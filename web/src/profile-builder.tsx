import React, { useEffect, useMemo, useRef, useState } from "react";
import type { LinePageV1, StationProfileBodyV1, StationProfilePageV1, StationProfilePreviewV1, StationProfileV1 } from "../../contracts/typescript/oee";
import { allPages, AmbiguousError, ApiError, classify, Intents, mutate, request } from "./api";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { EmptyState } from "@astryxdesign/core/EmptyState";
import { Field as FieldBox } from "@astryxdesign/core/Field";
import { Grid } from "@astryxdesign/core/Grid";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextArea } from "@astryxdesign/core/TextArea";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Token } from "@astryxdesign/core/Token";
import { VStack } from "@astryxdesign/core/VStack";
import { Actor, Link, Problem, Time, canSupervise, loadStations, short, useSession, type Station } from "./ui";

// WIS-520 G(1): station-profile form builder. It edits ProfileFieldV1 data only; the server
// validates and stores each save as an immutable revision. There is deliberately no
// expression, condition or script member to edit (proposal §5: editors emit config, never logic).

type Body = StationProfileBodyV1;
type Field = Body["fields"][number];
const kinds = ["text", "decimal", "choice", "boolean", "timestamp"] as const;
const templates = ["production", "quality", "material"] as const;
type Row = { uid: number; auto: boolean; name: string; label: string; kind: string; required: boolean; unit: string; choices: string; min: string; max: string };

const retryable = (e: unknown) => e instanceof AmbiguousError || (e instanceof ApiError && classify(e.problem) === "retry");

let uids = 0;
const toRow = (f: Field): Row => ({
    uid: ++uids,
    auto: false,
    name: f.name,
    label: f.label,
    kind: f.kind,
    required: f.required,
    unit: f.unit ?? "",
    choices: (f.choices ?? []).join("\n"),
    min: f.min ?? "",
    max: f.max ?? "",
});
// toField emits only the members the kind allows, matching the server's omitempty shape so a
// saved profile reloads deep-equal. Values are sent as typed; the server is the validator.
function toField(r: Row): Field {
    const f: { -readonly [K in keyof Field]: Field[K] } = { name: r.name, label: r.label, kind: r.kind, required: r.required };
    if (r.kind === "decimal") {
        if (r.unit) f.unit = r.unit;
        if (r.min) f.min = r.min;
        if (r.max) f.max = r.max;
    }
    if (r.kind === "choice") {
        const choices = r.choices.split("\n").filter((c) => c !== "");
        if (choices.length) f.choices = choices;
    }
    return f;
}
const slug = (label: string) =>
    label.toLowerCase().replace(/[^a-z0-9]+/g, "_").replace(/^_+|_+$/g, "").replace(/^(?=[0-9])/, "f_").slice(0, 64);

type Header = { display_name: string; template: string; line_id: string };
function bodyOf(id: string, h: Header, rows: Row[], refs: Body["instruction_references"]): Body {
    return {
        schema_version: 1,
        workstation_id: id,
        line_id: h.line_id,
        display_name: h.display_name,
        template: h.template,
        fields: rows.map(toField),
        ...(refs?.length ? { instruction_references: refs } : {}),
    };
}
const currentOf = (p: StationProfileV1) => p.revisions[p.current_revision - 1];

// A server path names one member: "fields[2].min", "profile.fields[0].label", "fields[1].*",
// "display_name". Anything else stays a form-level problem.
type Issue = { code: string; row?: number; member?: string; from: "preview" | "save" };
export function issueOf(code: string, path: string | undefined, from: Issue["from"]): Issue | null {
    const p = (path ?? "").replace(/^profile\./, "");
    const m = /^fields\[(\d+)\](?:\.([a-z_]+|\*))?/.exec(p);
    if (m) return { code, row: +m[1], member: m[2] === "*" ? undefined : m[2], from };
    if (p === "display_name" || p === "template" || p === "line_id") return { code, member: p, from };
    return null;
}
const hint: Record<string, string> = {
    name: "Use a unique name: lowercase letters, digits and _, starting with a letter.",
    label: "1–128 characters, no leading or trailing spaces.",
    unit: "A decimal field needs a unit.",
    choices: "A choice field needs 1–64 distinct options, one per line.",
    min: "A canonical decimal (up to 6 places), not above Max.",
    max: "A canonical decimal (up to 6 places).",
    display_name: "1–128 characters.",
    line_id: "Pick an active line.",
};
// target is the control an issue lands on; a row-level path ("fields[2]") lands on the label.
const focusable = ["name", "label", "unit", "min", "max", "choices", "display_name", "line_id"];
const target = (i: Issue) => (i.member && focusable.includes(i.member) ? i.member : "label");
function issueText(i: Issue) {
    if (i.code === "UNSUPPORTED_STATION_CONFIGURATION")
        return "Not allowed: profiles are configuration only (no formulas, templates, scripts or extra members).";
    return (i.member && hint[i.member]) || "Check this field's name, label, unit (decimal) and choices (choice).";
}

// loadLines reads every line page or rejects (allPages: failed page, repeated cursor, or more
// than 50 pages = 5,000 lines). A partial line list is never offered as complete (rule 06).
function loadLines(site: string) {
    return allPages(async (cursor) => {
        const page = await request<LinePageV1>(`/v1/sites/${site}/lines?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
        return { items: page.lines, next_cursor: page.next_cursor };
    });
}

export function ProfileList() {
    const { site, navigate } = useSession();
    const [list, setList] = useState<Station[] | null>(null);
    const [error, setError] = useState<unknown>(null);
    // Read fresh (not the shared cache) so a profile created a moment ago is listed.
    useEffect(() => {
        loadStations((cursor) =>
            request<StationProfilePageV1>(`/v1/sites/${site.id}/workstations?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`),
        ).then(setList, setError);
    }, [site.id]);
    return (
        <VStack gap={2}>
            <HStack justify="between" align="center" gap={2} wrap="wrap">
                <Heading level={1}>Station profiles</Heading>
                {canSupervise(site.role) && (
                    <Button label="New profile" variant="primary" onClick={() => navigate(`/sites/${site.id}/profiles/${crypto.randomUUID()}`)} />
                )}
            </HStack>
            <Problem error={error} />
            {!list && !error && <Skeleton height={160} />}
            {list && !list.length && <EmptyState title="No station profile yet." headingLevel={2} />}
            {list && list.length > 0 && (
                <Table aria-label="Station profiles" density="compact">
                    <TableHeader>
                        <TableRow isHeaderRow>
                            <TableHeaderCell scope="col">Workstation</TableHeaderCell>
                            <TableHeaderCell scope="col">Rev</TableHeaderCell>
                        </TableRow>
                    </TableHeader>
                    <TableBody>
                        {list.map((s) => (
                            <TableRow key={s.id}>
                                <TableCell>
                                    <Link to={`/sites/${site.id}/profiles/${s.id}`}>{s.name}</Link>
                                </TableCell>
                                <TableCell className="n">{s.revision}</TableCell>
                            </TableRow>
                        ))}
                    </TableBody>
                </Table>
            )}
        </VStack>
    );
}

export function ProfileBuilder({ stationId }: { stationId: string }) {
    const { site, online } = useSession();
    const edit = canSupervise(site.role);
    const base = `/v1/sites/${site.id}/workstations/${stationId}`;
    const [loaded, setLoaded] = useState<StationProfileV1 | null>(null);
    const [missing, setMissing] = useState(false);
    const [loadError, setLoadError] = useState<unknown>(null);
    const [revision, setRevision] = useState(0);
    const [saved, setSaved] = useState(""); // JSON of the body at `revision`, for the dirty check
    const [header, setHeader] = useState<Header>({ display_name: "", template: "production", line_id: "" });
    const [rows, setRows] = useState<Row[]>([]);
    const [refs, setRefs] = useState<Body["instruction_references"]>(undefined);
    const [lines, setLines] = useState<LinePageV1["lines"][number][]>([]);
    const [linesError, setLinesError] = useState<unknown>(null);
    const [linesVersion, setLinesVersion] = useState(0);
    const [preview, setPreview] = useState<StationProfilePreviewV1 | null>(null);
    const [previewError, setPreviewError] = useState<unknown>(null);
    const [issue, setIssue] = useState<Issue | null>(null);
    const [saveError, setSaveError] = useState<unknown>(null);
    const [conflict, setConflict] = useState<StationProfileV1 | null>(null);
    const [busy, setBusy] = useState(false);
    const [notice, setNotice] = useState("");
    const [focusId, setFocusId] = useState("");
    const intents = useMemo(() => new Intents(), []);
    const seq = useRef(0);
    const conflictRef = useRef<HTMLHeadingElement>(null);

    function adopt(p: StationProfileV1) {
        const cur = currentOf(p).profile;
        const nextRows = cur.fields.map(toRow);
        const h = { display_name: cur.display_name, template: cur.template, line_id: cur.line_id };
        setLoaded(p);
        setRevision(p.current_revision);
        setHeader(h);
        setRows(nextRows);
        setRefs(cur.instruction_references);
        setSaved(JSON.stringify(bodyOf(stationId, h, nextRows, cur.instruction_references)));
        setConflict(null);
        setIssue(null);
    }
    useEffect(() => {
        request<StationProfileV1>(`${base}/profile`).then(adopt, (e) => {
            // A writer opening an unknown id starts a new profile (If-Match "0").
            if (e instanceof ApiError && e.problem.status === 404 && edit) {
                setMissing(true);
                setRows([{ ...toRow({ name: "", label: "", kind: "text", required: false }), auto: true }]);
            } else setLoadError(e);
        });
    }, [base]);
    useEffect(() => {
        if (!edit) return;
        setLinesError(null);
        loadLines(site.id).then(setLines, (e) => {
            setLines([]);
            setLinesError(e);
        });
    }, [site.id, linesVersion]);

    const draft = bodyOf(stationId, header, rows, refs);
    const draftJSON = JSON.stringify(draft);
    const dirty = draftJSON !== saved;
    const ready = loaded || missing;

    // Issue 2: warn before tab close or browser navigation when there are unsaved edits.
    useEffect(() => {
        if (!dirty) return;
        const handler = (e: BeforeUnloadEvent) => { e.preventDefault(); };
        addEventListener("beforeunload", handler);
        return () => removeEventListener("beforeunload", handler);
    }, [dirty]);

    // Live server preview: debounced, last response wins, never persisted. Its 422 path is the
    // same live validation Save would get, so it lands on the same row.
    useEffect(() => {
        if (!edit || !ready || !header.line_id) return;
        const mine = ++seq.current;
        const timer = setTimeout(() => {
            request<StationProfilePreviewV1>(`${base}/profile-previews`, "POST", { body: { profile: JSON.parse(draftJSON) } }).then(
                (p) => {
                    if (mine !== seq.current) return;
                    setPreview(p);
                    setPreviewError(null);
                    setIssue(null);
                },
                (e) => {
                    if (mine !== seq.current) return;
                    const mapped = e instanceof ApiError && e.problem.status === 422 ? issueOf(e.problem.code, e.problem.path, "preview") : null;
                    setIssue(mapped);
                    setPreviewError(mapped ? null : e);
                },
            );
        }, 350);
        return () => clearTimeout(timer);
    }, [draftJSON, ready, edit]);

    useEffect(() => {
        if (focusId) document.getElementById(focusId)?.focus();
        setFocusId("");
    }, [focusId]);
    useEffect(() => {
        if (conflict) conflictRef.current?.focus();
    }, [conflict]);

    const control = (uid: number, member: string) => `pf-${uid}-${member}`;
    function update(uid: number, patch: Partial<Row>) {
        setRows((rs) =>
            rs.map((r) => {
                if (r.uid !== uid) return r;
                const next = { ...r, ...patch };
                if (patch.label !== undefined && r.auto) next.name = slug(patch.label);
                if (patch.name !== undefined) next.auto = false;
                return next;
            }),
        );
        setNotice("");
    }
    function move(i: number, by: number, focus: "up" | "down" | "label" = by < 0 ? "up" : "down") {
        const j = i + by;
        if (j < 0 || j >= rows.length) return;
        const next = rows.slice();
        [next[i], next[j]] = [next[j], next[i]];
        setRows(next);
        const r = next[j];
        const target = focus === "label" ? "label" : focus === "up" && j === 0 ? "down" : focus === "down" && j === next.length - 1 ? "up" : focus;
        setFocusId(control(r.uid, target));
        setNotice(`Moved ${r.label || "field"} to position ${j + 1}.`);
    }
    function remove(i: number) {
        const next = rows.filter((_, n) => n !== i);
        setRows(next);
        const neighbour = next[i] ?? next[i - 1];
        setFocusId(neighbour ? control(neighbour.uid, "label") : "pf-add");
        setNotice(`Removed ${rows[i].label || "field"}.`);
    }
    function add() {
        const r = { ...toRow({ name: "", label: "", kind: "text", required: false }), auto: true };
        setRows((rs) => [...rs, r]);
        setFocusId(control(r.uid, "label"));
    }

    async function save() {
        if (!dirty || busy) return;
        seq.current++; // a late preview response must not overwrite what Save reports
        setBusy(true);
        setSaveError(null);
        setNotice("");
        try {
            const out = await mutate<StationProfileV1>(intents, `profile:${stationId}`, `${base}/profile-revisions`, draft, revision);
            setMissing(false);
            adopt(out);
            setNotice(`Saved revision ${out.current_revision}.`);
        } catch (e) {
            const p = e instanceof ApiError ? e.problem : null;
            if (p?.status === 412) {
                // Never resubmitted: show what changed and let the person choose.
                try {
                    setConflict(await request<StationProfileV1>(`${base}/profile`));
                } catch (reload) {
                    setSaveError(reload);
                }
            } else {
                const mapped = p?.status === 422 ? issueOf(p.code, p.path, "save") : null;
                setIssue(mapped);
                setSaveError(mapped ? null : e);
                const r = mapped?.row !== undefined ? rows[mapped.row] : undefined;
                if (mapped) setFocusId(r ? control(r.uid, target(mapped)) : `pf-h-${target(mapped)}`);
            }
        } finally {
            setBusy(false);
        }
    }

    if (loadError)
        return (
            <VStack gap={2}>
                <Heading level={1}>Station profile</Heading>
                <Problem error={loadError} />
            </VStack>
        );
    if (!ready)
        return (
            <VStack gap={2}>
                <Text as="p" color="secondary" role="status">
                    Loading profile…
                </Text>
                <Skeleton height={240} />
            </VStack>
        );
    const current = loaded ? currentOf(loaded) : null;
    const lineName = (id: string) => lines.find((l) => l.id === id)?.body.display_name ?? short(id);
    const facts = current && (
        <Text as="p" size="sm" color="secondary" hasTabularNumbers>
            Revision {revision} · saved by <Actor a={current.submitted_by} /> <Time iso={current.created_at} />
        </Text>
    );

    if (!edit)
        return (
            <VStack gap={2}>
                <Heading level={1}>{header.display_name}</Heading>
                {facts}
                <HStack gap={1} wrap="wrap" align="center">
                    <Token label={header.template} />
                    <Token label={`Line ${lineName(header.line_id)}`} />
                    <Token label="Read-only for your role" color="orange" />
                </HStack>
                <Table aria-label="Profile fields" density="compact">
                    <TableHeader>
                        <TableRow isHeaderRow>
                            <TableHeaderCell scope="col">#</TableHeaderCell>
                            <TableHeaderCell scope="col">Label</TableHeaderCell>
                            <TableHeaderCell scope="col">Name</TableHeaderCell>
                            <TableHeaderCell scope="col">Kind</TableHeaderCell>
                            <TableHeaderCell scope="col">Required</TableHeaderCell>
                            <TableHeaderCell scope="col">Constraints</TableHeaderCell>
                        </TableRow>
                    </TableHeader>
                    <TableBody>
                        {rows.map((r, i) => (
                            <TableRow key={r.uid}>
                                <TableCell className="n">{i + 1}</TableCell>
                                <TableCell>{r.label}</TableCell>
                                <TableCell className="identifier">{r.name}</TableCell>
                                <TableCell>{r.kind}</TableCell>
                                <TableCell>{r.required ? "Required" : "Optional"}</TableCell>
                                <TableCell>{constraints(toField(r))}</TableCell>
                            </TableRow>
                        ))}
                    </TableBody>
                </Table>
            </VStack>
        );

    const invalid = (row: number | undefined, member: string) => !!issue && issue.row === row && target(issue) === member;
    // Astryx text inputs derive aria-invalid from status; a native control gets the attribute itself.
    const status = (row: number | undefined, member: string, id: string) =>
        invalid(row, member) ? { status: { type: "error" as const }, "aria-describedby": `${id}-err` } : {};
    const aria = (row: number | undefined, member: string, id: string) =>
        invalid(row, member) ? { "aria-invalid": true as const, "aria-describedby": `${id}-err` } : {};
    const errText = { color: "var(--color-text-error)" };
    const headerErr = (member: string) =>
        invalid(undefined, member) && issue ? (
            <Text id={`pf-h-${member}-err`} size="sm" style={errText}>
                {issueText(issue)}
            </Text>
        ) : null;

    return (
        <form
            onSubmit={(e) => {
                e.preventDefault();
                save();
            }}
            onKeyDown={(e) => {
                if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") {
                    e.preventDefault();
                    save();
                }
            }}
        >
            <VStack gap={2}>
                <Heading level={1}>{missing ? "New station profile" : header.display_name || "Station profile"}</Heading>
                {facts}
                <Grid columns={{ minWidth: 360, max: 2 }} gap={3} align="start">
                    <VStack as="section" aria-label="Profile editor" gap={2}>
                        <Grid columns={{ minWidth: 160, max: 3 }} gap={1} align="start">
                            <VStack gap={0.5}>
                                <TextInput
                                    id="pf-h-display_name"
                                    label="Display name"
                                    value={header.display_name}
                                    width="100%"
                                    {...{ maxLength: 128 }}
                                    onChange={(v) => setHeader({ ...header, display_name: v })}
                                    {...status(undefined, "display_name", "pf-h-display_name")}
                                />
                                {headerErr("display_name")}
                            </VStack>
                            <FieldBox label="Template" inputID="pf-h-template">
                                <select id="pf-h-template" value={header.template} onChange={(e) => setHeader({ ...header, template: e.target.value })}>
                                    {templates.map((t) => (
                                        <option key={t}>{t}</option>
                                    ))}
                                </select>
                            </FieldBox>
                            <VStack gap={0.5}>
                                <FieldBox label="Line" inputID="pf-h-line_id">
                                    <select id="pf-h-line_id" value={header.line_id} onChange={(e) => setHeader({ ...header, line_id: e.target.value })} {...aria(undefined, "line_id", "pf-h-line_id")}>
                                        <option value="">Choose a line…</option>
                                        {header.line_id && !lines.some((l) => l.id === header.line_id) && <option value={header.line_id}>{short(header.line_id)}</option>}
                                        {lines.map((l) => (
                                            <option key={l.id} value={l.id}>
                                                {l.body.display_name}
                                                {l.body.active ? "" : " (retired)"}
                                            </option>
                                        ))}
                                    </select>
                                </FieldBox>
                                {headerErr("line_id")}
                            </VStack>
                        </Grid>
                        {linesError ? (
                            <Card role="alert" variant="red" padding={2}>
                                <HStack gap={1} align="center" wrap="wrap">
                                    <Text>Lines could not be loaded completely, so none are offered.</Text>
                                    <Button label="Reload lines" variant="secondary" size="sm" onClick={() => setLinesVersion((v) => v + 1)} />
                                </HStack>
                            </Card>
                        ) : null}
                        <VStack as="ol" aria-label="Fields" gap={1}>
                            {rows.map((r, i) => {
                                const id = (m: string) => control(r.uid, m);
                                const name = r.label || `field ${i + 1}`;
                                const rowIssue = issue && issue.row === i ? issue : null;
                                return (
                                    <li
                                        key={r.uid}
                                        onKeyDown={(e) => {
                                            if (e.altKey && (e.key === "ArrowUp" || e.key === "ArrowDown")) {
                                                e.preventDefault();
                                                move(i, e.key === "ArrowUp" ? -1 : 1, "label");
                                            }
                                        }}
                                    >
                                        <Card padding={2} variant={rowIssue ? "red" : "default"}>
                                            <VStack gap={1}>
                                                <HStack gap={1} align="center" wrap="wrap">
                                                    <Text color="secondary" hasTabularNumbers aria-hidden="true">
                                                        {i + 1}
                                                    </Text>
                                                    <TextInput id={id("label")} label={`Label, field ${i + 1}`} isLabelHidden placeholder="Label" value={r.label} width={200} {...{ maxLength: 128 }} onChange={(v) => update(r.uid, { label: v })} {...status(i, "label", id("label"))} />
                                                    <TextInput id={id("name")} controlClassName="identifier" label={`Name, field ${i + 1}`} isLabelHidden placeholder="name" value={r.name} width={160} {...{ maxLength: 64 }} onChange={(v) => update(r.uid, { name: v })} {...status(i, "name", id("name"))} />
                                                    <select id={id("kind")} aria-label={`Kind, field ${i + 1}`} value={r.kind} onChange={(e) => update(r.uid, { kind: e.target.value })} {...aria(i, "kind", id("kind"))}>
                                                        {kinds.map((k) => (
                                                            <option key={k}>{k}</option>
                                                        ))}
                                                    </select>
                                                    <CheckboxInput label="Required" value={r.required} onChange={(checked) => update(r.uid, { required: checked })} />
                                                    <HStack gap={0.5}>
                                                        <Button id={id("up")} label={`Move up ${name}`} variant="ghost" size="sm" isDisabled={i === 0} onClick={() => move(i, -1)}>
                                                            ↑
                                                        </Button>
                                                        <Button id={id("down")} label={`Move down ${name}`} variant="ghost" size="sm" isDisabled={i === rows.length - 1} onClick={() => move(i, 1)}>
                                                            ↓
                                                        </Button>
                                                        <Button label={`Remove ${name}`} variant="ghost" size="sm" onClick={() => remove(i)}>
                                                            ✕
                                                        </Button>
                                                    </HStack>
                                                </HStack>
                                                {r.kind === "decimal" && (
                                                    <HStack gap={1} wrap="wrap">
                                                        <TextInput id={id("unit")} label={`Unit, field ${i + 1}`} isLabelHidden placeholder="unit" value={r.unit} width={120} {...{ maxLength: 32 }} onChange={(v) => update(r.uid, { unit: v })} {...status(i, "unit", id("unit"))} />
                                                        <TextInput id={id("min")} label={`Min, field ${i + 1}`} isLabelHidden placeholder="min" inputMode="decimal" value={r.min} width={120} onChange={(v) => update(r.uid, { min: v })} {...status(i, "min", id("min"))} />
                                                        <TextInput id={id("max")} label={`Max, field ${i + 1}`} isLabelHidden placeholder="max" inputMode="decimal" value={r.max} width={120} onChange={(v) => update(r.uid, { max: v })} {...status(i, "max", id("max"))} />
                                                    </HStack>
                                                )}
                                                {r.kind === "choice" && (
                                                    <TextArea id={id("choices")} label={`Choices, one per line, field ${i + 1}`} isLabelHidden rows={Math.min(6, Math.max(2, r.choices.split("\n").length))} value={r.choices} onChange={(v) => update(r.uid, { choices: v })} {...status(i, "choices", id("choices"))} />
                                                )}
                                                {rowIssue && (
                                                    <Text as="p" size="sm" id={`${id(target(rowIssue))}-err`} style={errText} role={rowIssue.from === "save" ? "alert" : undefined}>
                                                        {issueText(rowIssue)} <span className="identifier">{rowIssue.code}</span>
                                                    </Text>
                                                )}
                                            </VStack>
                                        </Card>
                                    </li>
                                );
                            })}
                        </VStack>
                        <HStack gap={1} align="center" wrap="wrap">
                            <Button id="pf-add" label="+ Add field" variant="secondary" isDisabled={rows.length >= 64} onClick={add} />
                            <Text size="sm" color="secondary">
                                Alt+↑/↓ moves the focused field · Ctrl+S saves
                            </Text>
                        </HStack>
                    </VStack>
                    <aside aria-label="Operator preview" aria-live="polite" style={{ position: "sticky", top: 16 }}>
                        <Card padding={3} variant="muted">
                            <VStack gap={1.5}>
                                <HStack justify="between" align="center" gap={1} wrap="wrap">
                                    <Heading level={2}>Operator preview</Heading>
                                    {preview && <Token label={preview.ready ? "✓ ready" : "✕ not ready"} color={preview.ready ? "green" : "orange"} />}
                                </HStack>
                                {!header.line_id && (
                                    <Text size="sm" color="secondary">
                                        Choose a line to preview.
                                    </Text>
                                )}
                                {issue && (
                                    <Text size="sm" color="secondary">
                                        Fix the highlighted field to refresh the preview.
                                    </Text>
                                )}
                                <Problem error={previewError} />
                                {preview && (
                                    <>
                                        <Text size="sm" color="secondary">
                                            {preview.template} · digest <span className="identifier">{preview.profile_digest.slice(0, 12)}</span>
                                        </Text>
                                        <VStack as="ol" gap={1}>
                                            {preview.fields.map(({ field }) => (
                                                <VStack as="li" key={field.name} gap={0.5}>
                                                    <Text weight="semibold">
                                                        {field.label}
                                                        {field.required && <abbr title="required"> *</abbr>}
                                                    </Text>
                                                    <Card padding={1}>
                                                        <Text color="secondary">{constraints(field) || field.kind}</Text>
                                                    </Card>
                                                </VStack>
                                            ))}
                                        </VStack>
                                    </>
                                )}
                            </VStack>
                        </Card>
                    </aside>
                </Grid>
                {conflict && (
                    <Card role="alert" variant="orange" padding={3}>
                        <VStack gap={2}>
                            <Heading level={2} tabIndex={-1} ref={conflictRef}>
                                Changed on the server (revision {conflict.current_revision})
                            </Heading>
                            <Text as="p">Your edits were not saved and nothing was resubmitted. Compare, then choose.</Text>
                            <Grid columns={2} gap={3}>
                                <Compare title="Yours" mine={draft.fields} other={currentOf(conflict).profile.fields} />
                                <Compare title={`Server revision ${conflict.current_revision}`} mine={currentOf(conflict).profile.fields} other={draft.fields} />
                            </Grid>
                            <HStack gap={1} wrap="wrap">
                                <Button label="Use server version" variant="secondary" onClick={() => adopt(conflict)} />
                                <Button
                                    label={`Keep mine on revision ${conflict.current_revision}`}
                                    variant="secondary"
                                    onClick={() => {
                                        setRevision(conflict.current_revision);
                                        setLoaded(conflict);
                                        setSaved(JSON.stringify(bodyOf(stationId, currentOf(conflict).profile, currentOf(conflict).profile.fields.map(toRow), currentOf(conflict).profile.instruction_references)));
                                        setConflict(null);
                                        setFocusId("pf-save");
                                    }}
                                />
                            </HStack>
                        </VStack>
                    </Card>
                )}
                <Problem error={saveError} />
                <HStack
                    justify="end"
                    align="center"
                    gap={1}
                    wrap="wrap"
                    paddingBlock={1}
                    style={{ position: "sticky", bottom: 0, background: "var(--color-background-body)", borderTop: "1px solid var(--color-border)" }}
                >
                    <Text color="secondary" hasTabularNumbers aria-live="polite" style={{ marginRight: "auto" }}>
                        {notice || (dirty ? `Unsaved changes · ${rows.length} fields` : `No changes · ${rows.length} fields`)}
                    </Text>
                    <Button
                        label="Discard"
                        variant="secondary"
                        isDisabled={!dirty || busy}
                        onClick={() => {
                            if (!window.confirm("Discard all unsaved changes?")) return;
                            loaded ? adopt(loaded) : setRows([{ ...toRow({ name: "", label: "", kind: "text", required: false }), auto: true }]);
                        }}
                    />
                    <Button
                        id="pf-save"
                        type="submit"
                        variant="primary"
                        label={busy ? "Saving…" : retryable(saveError) ? "Retry save" : missing ? "Create profile" : `Save revision ${revision + 1}`}
                        isDisabled={!dirty || busy || !online || !!conflict}
                    />
                </HStack>
            </VStack>
        </form>
    );
}

function constraints(f: Field) {
    if (f.kind === "decimal") return [f.unit, f.min || f.max ? `${f.min || "…"} – ${f.max || "…"}` : ""].filter(Boolean).join(" · ");
    if (f.kind === "choice") return (f.choices ?? []).join(" / ");
    return "";
}

function Compare({ title, mine, other }: { title: string; mine: Body["fields"]; other: Body["fields"] }) {
    return (
        <VStack gap={1}>
            <Heading level={3}>{title}</Heading>
            <VStack as="ol" gap={0.5}>
                {mine.map((f) => {
                    const o = other.find((x) => x.name === f.name);
                    const mark = !o ? "only here" : JSON.stringify(o) !== JSON.stringify(f) ? "changed" : "";
                    return (
                        <HStack as="li" key={f.name} gap={1} align="center" wrap="wrap">
                            <Text>{f.label}</Text>
                            <Text size="sm" color="secondary">
                                ({f.kind})
                            </Text>
                            {mark && <Token label={mark} color="orange" size="sm" />}
                        </HStack>
                    );
                })}
            </VStack>
        </VStack>
    );
}
