// WIS-520 G(3): operational source mapping editor. A mapping is identity configuration only:
// (asset|order, external key) -> one exact knowledge entity revision. The editor emits that
// closed set and nothing else (proposal §5: no expressions, conditions or scripts); Go validates
// and stores every revision.
import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type {
    KnowledgeEntityPageV1,
    OperationalSourceDetailV1,
    OperationalSourceMappingV1,
    OperationalSourcePageV1,
    OperationalSourceV1,
} from "../../contracts/typescript/oee";
import { allPages, AmbiguousError, ApiError, classify, Intents, request } from "./api";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { EmptyState } from "@astryxdesign/core/EmptyState";
import { Grid } from "@astryxdesign/core/Grid";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Token, type TokenColor } from "@astryxdesign/core/Token";
import { VStack } from "@astryxdesign/core/VStack";
import { Actor, canSupervise, clearDraft, Link, loadDraft, Problem, saveDraft, short, Time, useSession } from "./ui";

type Mapping = OperationalSourceMappingV1;
type Kind = Mapping["external_type"];
type Summary = OperationalSourcePageV1["sources"][number];
type Entity = KnowledgeEntityPageV1["entities"][number];
type RowError = { field: keyof Mapping; text: string };

const kinds: Kind[] = ["asset", "order"];
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
// Browsing is capped by allPages at 50 pages × 100 = 5,000 entities or sources. ponytail: a site
// past that gets a visible "could not be loaded completely" error (never a partial list); mapping
// by entity ID still works. A server-side search endpoint is the upgrade path.
const q = (cursor?: string) => `?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`;

// rowPath reads a server member path such as "mappings[3].external_key".
export function rowPath(path: string | undefined): { row: number; field: keyof Mapping } | null {
    const m = /^mappings\[(\d+)\]\.(external_type|external_key|entity_id|entity_revision)$/.exec(path ?? "");
    return m ? { row: Number(m[1]), field: m[2] as keyof Mapping } : null;
}

// localErrors mirrors the server's row rules so most mistakes never leave the browser.
function localErrors(rows: Mapping[]): Map<number, RowError> {
    const errors = new Map<number, RowError>();
    const keys = new Set<string>();
    rows.forEach((r, i) => {
        const k = `${r.external_type}\u0000${r.external_key}`;
        if (!r.external_key) errors.set(i, { field: "external_key", text: "Enter the external key." });
        else if (r.external_key.trim() !== r.external_key) errors.set(i, { field: "external_key", text: "Remove leading or trailing spaces." });
        else if (r.external_key.length > 512) errors.set(i, { field: "external_key", text: "At most 512 characters." });
        else if (keys.has(k)) errors.set(i, { field: "external_key", text: "This key is already mapped above." });
        else if (!uuid.test(r.entity_id)) errors.set(i, { field: "entity_id", text: "Enter or choose an entity ID." });
        else if (r.entity_revision < 1) errors.set(i, { field: "entity_id", text: "This entity was not found at this site." });
        keys.add(k);
    });
    return errors;
}

function serverRowText(status: number, code: string, field: keyof Mapping): string {
    if (status === 404) return "This entity is not available at this site.";
    if (status === 412) return "The entity changed since it was chosen. Use its current revision.";
    if (code === "UNKNOWN_MAPPING") return "This entity was merged. Map the surviving entity instead.";
    return field === "external_type" ? "Choose asset or order." : field === "external_key" ? "Invalid or duplicate external key." : "Invalid entity.";
}

const sameRows = (a: readonly Mapping[], b: readonly Mapping[]) => JSON.stringify(a) === JSON.stringify(b);
const rowKey = (r: Mapping) => `${r.external_type}:${r.external_key}`;

const approvalColor = (text: string): TokenColor => (text === "Approved" ? "green" : text.startsWith("Rejected") ? "red" : text.startsWith("Approval stale") ? "orange" : "default");
const errText = { color: "var(--color-text-error)" };

function approval(s: { review_verdict?: string; review_source_revision?: number; revision: number }) {
    if (!s.review_verdict) return "Not reviewed";
    if (s.review_verdict === "rejected") return `Rejected (rev ${s.review_source_revision})`;
    return s.review_source_revision === s.revision ? "Approved" : `Approval stale (rev ${s.review_source_revision})`;
}

export function SourceMappingPage({ sourceId }: { sourceId?: string }) {
    const { site } = useSession();
    const [sources, setSources] = useState<Summary[] | null>(null);
    const [error, setError] = useState<unknown>(null);
    const [version, setVersion] = useState(0);
    useEffect(() => {
        let live = true;
        setError(null);
        allPages(async (cursor) => {
            const page = await request<OperationalSourcePageV1>(`/v1/sites/${site.id}/operational-sources${q(cursor)}`);
            return { items: page.sources, next_cursor: page.next_cursor };
        }).then(
            (all) => live && setSources(all),
            (e) => {
                if (!live) return;
                setSources(null);
                setError(e);
            },
        );
        return () => {
            live = false;
        };
    }, [site.id, version]);
    const reload = useCallback(() => setVersion((v) => v + 1), []);
    return (
        <VStack gap={2}>
            <Heading level={1}>Source mappings</Heading>
            <Grid columns={sourceId ? { minWidth: 360, max: 2 } : 1} gap={3} align="start">
                <VStack as="section" aria-labelledby="sm-sources" gap={1.5}>
                    <HStack gap={1} align="center">
                        <Heading level={2} id="sm-sources">
                            Operational sources
                        </Heading>
                        {sources && <Token label={String(sources.length)} size="sm" />}
                    </HStack>
                    {error ? (
                        <>
                            <Text as="p" weight="semibold" style={errText}>
                                The source list could not be loaded completely, so none of it is shown.
                            </Text>
                            <Problem error={error} />
                            <HStack>
                                <Button label="Reload sources" variant="secondary" onClick={reload} />
                            </HStack>
                        </>
                    ) : !sources ? (
                        <Skeleton height={160} />
                    ) : sources.length === 0 ? (
                        <EmptyState title="No operational sources at this site yet." headingLevel={3} isCompact />
                    ) : (
                        <Table aria-label="Operational sources" density="compact">
                            <TableHeader>
                                <TableRow isHeaderRow>
                                    <TableHeaderCell scope="col">Source</TableHeaderCell>
                                    <TableHeaderCell scope="col">Type</TableHeaderCell>
                                    <TableHeaderCell scope="col">Scope</TableHeaderCell>
                                    <TableHeaderCell scope="col">Rows</TableHeaderCell>
                                    <TableHeaderCell scope="col">Review</TableHeaderCell>
                                    <TableHeaderCell scope="col">Rev</TableHeaderCell>
                                </TableRow>
                            </TableHeader>
                            <TableBody>
                                {sources.map((s) => {
                                    const review = approval(s);
                                    return (
                                        <TableRow key={s.source_id} data-source={s.source_id} aria-current={s.source_id === sourceId ? "true" : undefined}>
                                            <TableCell>
                                                <Link to={`/sites/${site.id}/sources/${s.source_id}`} current={s.source_id === sourceId}>
                                                    <span className="identifier">{short(s.source_id)}</span>
                                                </Link>
                                                {!s.enabled && (
                                                    <Text size="sm" color="secondary">
                                                        {" "}
                                                        · disabled
                                                    </Text>
                                                )}
                                            </TableCell>
                                            <TableCell>{s.type}</TableCell>
                                            <TableCell>{s.authority_scope}</TableCell>
                                            <TableCell className="n">{s.mapping_count}</TableCell>
                                            <TableCell>
                                                <Token label={review} color={approvalColor(review)} size="sm" />
                                            </TableCell>
                                            <TableCell className="n">
                                                <span title={`Source revision ${s.revision}, mapping revision ${s.mapping_revision}`}>
                                                    {s.revision}/{s.mapping_revision}
                                                </span>
                                            </TableCell>
                                        </TableRow>
                                    );
                                })}
                            </TableBody>
                        </Table>
                    )}
                </VStack>
                {sourceId && <MappingEditor key={sourceId} sourceId={sourceId} onSaved={reload} />}
            </Grid>
        </VStack>
    );
}

function MappingEditor({ sourceId, onSaved }: { sourceId: string; onSaved: () => void }) {
    const { site, online } = useSession();
    const writer = canSupervise(site.role);
    const path = `/v1/sites/${site.id}/operational-sources/${sourceId}`;
    const [detail, setDetail] = useState<OperationalSourceDetailV1 | null>(null);
    const [entities, setEntities] = useState<Map<string, Entity> | null>(null);
    const [loadError, setLoadError] = useState<unknown>(null);
    const [entityError, setEntityError] = useState<unknown>(null);
    const [rows, setRows] = useState<Mapping[]>([]);
    const [base, setBase] = useState<{ mappingRevision: number; rows: Mapping[] } | null>(null);
    const [errors, setErrors] = useState<Map<number, RowError>>(new Map());
    const [saveError, setSaveError] = useState<unknown>(null);
    const [conflict, setConflict] = useState<OperationalSourceDetailV1 | null>(null);
    const [saved, setSaved] = useState<OperationalSourceV1 | null>(null);
    const [busy, setBusy] = useState(false);
    const [known, setKnown] = useState<Map<string, Entity>>(new Map()); // entities read by id
    const [reloadError, setReloadError] = useState<unknown>(null); // saved, but the re-read failed
    const intents = useMemo(() => new Intents(), []);
    const focusRef = useRef<string>("");
    const conflictRef = useRef<HTMLHeadingElement>(null);
    const draftKey = `source-mapping:${site.id}:${sourceId}`;

    const adopt = useCallback((d: OperationalSourceDetailV1) => {
        setDetail(d);
        const baseRows = [...d.mappings];
        setBase({ mappingRevision: d.source.mapping_revision, rows: baseRows });
        const draft = loadDraft<Mapping[]>(draftKey);
        if (draft && draft.length > 0 && !sameRows(draft, baseRows)) {
            setRows(draft);
        } else {
            setRows(baseRows);
        }
        setErrors(new Map());
        setConflict(null);
    }, [draftKey]);

    useEffect(() => {
        if (!base) return;
        if (!sameRows(rows, base.rows)) {
            saveDraft(draftKey, rows);
        } else {
            clearDraft(draftKey);
        }
    }, [rows, base, draftKey]);
    useEffect(() => {
        request<OperationalSourceDetailV1>(path).then(adopt, setLoadError);
    }, [path, adopt]);
    // Entities load separately so a refetch (after a stale entity revision) keeps the draft.
    useEffect(() => {
        allPages(async (cursor) => {
            const page = await request<KnowledgeEntityPageV1>(`/v1/sites/${site.id}/entities${q(cursor)}`);
            return { items: page.entities, next_cursor: page.next_cursor };
        }).then((all) => setEntities(new Map(all.map((e) => [e.entity_id, e]))), setEntityError);
    }, [site.id]);
    useEffect(() => {
        if (conflict) conflictRef.current?.focus();
    }, [conflict]);
    useEffect(() => {
        if (!focusRef.current) return;
        document.getElementById(focusRef.current)?.focus();
        focusRef.current = "";
    });

    if (loadError) return <Problem error={loadError} />;
    if (!detail || !base)
        return (
            <Text as="p" color="secondary">
                Loading source…
            </Text>
        );
    const source = detail.source;
    const dirty = !sameRows(rows, base.rows);

    // Issue 2: warn before tab close or browser navigation when there are unsaved edits.
    useEffect(() => {
        if (!dirty) return;
        const handler = (e: BeforeUnloadEvent) => { e.preventDefault(); };
        addEventListener("beforeunload", handler);
        return () => removeEventListener("beforeunload", handler);
    }, [dirty]);
    const approved = source.review?.verdict === "approved" && source.review_eligible;
    const retryable = saveError instanceof AmbiguousError || (saveError instanceof ApiError && classify(saveError.problem) === "retry");
    const entityOf = (id: string) => known.get(id) ?? entities?.get(id);
    const entityName = (id: string) => entityOf(id)?.display_name;
    const mapped = new Set(rows.map((r) => r.entity_id));
    const unmapped = entities ? [...entities.values()].filter((e) => (e.type === "asset" || e.type === "order") && !mapped.has(e.entity_id)) : [];

    const edit = (i: number, patch: Partial<Mapping>) => {
        setRows((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));
        setErrors((es) => {
            if (!es.has(i)) return es;
            const next = new Map(es);
            next.delete(i);
            return next;
        });
        setSaved(null);
    };
    // resolve reads one entity by id (no full list needed) and pins its current revision.
    const resolve = (i: number, id: string) => {
        edit(i, { entity_id: id, entity_revision: entityOf(id)?.current_revision ?? 0 });
        if (!uuid.test(id) || entityOf(id)) return;
        request<Entity>(`/v1/sites/${site.id}/entities/${id}`).then(
            (e) => {
                setKnown((k) => new Map(k).set(id, e));
                setRows((rs) => rs.map((r, j) => (j === i && r.entity_id === id ? { ...r, entity_revision: e.current_revision } : r)));
            },
            () => setErrors((es) => new Map(es).set(i, { field: "entity_id", text: "This entity was not found at this site." })),
        );
    };
    const refresh = (id: string) =>
        request<Entity>(`/v1/sites/${site.id}/entities/${id}`).then((e) => setKnown((k) => new Map(k).set(id, e)), () => {});
    const add = (seed?: Entity) => {
        const kind: Kind = seed?.type === "order" ? "order" : "asset";
        focusRef.current = `sm-key-${rows.length}`;
        setRows((rs) => [...rs, { external_type: kind, external_key: "", entity_id: seed?.entity_id ?? "", entity_revision: seed?.current_revision ?? 0 }]);
        setSaved(null);
    };
    const remove = (i: number) => {
        focusRef.current = rows.length > 1 ? `sm-key-${Math.max(0, i - 1)}` : "sm-add";
        setRows((rs) => rs.filter((_, j) => j !== i));
        setErrors(new Map());
        setSaved(null);
    };

    const save = async (e: React.FormEvent) => {
        e.preventDefault();
        const local = localErrors(rows);
        setErrors(local);
        if (local.size) {
            const [i, err] = [...local][0];
            focusRef.current = `sm-${err.field === "entity_id" ? "entity" : "key"}-${i}`;
            return;
        }
        setBusy(true);
        setSaveError(null);
        const body = { mappings: rows };
        const ifMatch = base.mappingRevision;
        try {
            // One key per (body, mapping revision): an ambiguous retry reuses it; a "keep mine"
            // after 412 has a new If-Match, so it is a new intent with a new key.
            const key = intents.key(`mappings:${sourceId}`, { body, ifMatch });
            const out = await request<OperationalSourceV1>(`${path}/mappings`, "POST", { key, body, ifMatch });
            intents.done(`mappings:${sourceId}`);
            clearDraft(draftKey);
            setSaved(out);
            onSaved();
            // The save is committed. A failed re-read must never turn into a retry offer.
            await reload();
            return;
        } catch (err) {
            if (!(err instanceof AmbiguousError || (err instanceof ApiError && classify(err.problem) === "retry"))) intents.done(`mappings:${sourceId}`);
            const p = err instanceof ApiError ? err.problem : null;
            const at = rowPath(p?.path);
            if (p && at && at.row < rows.length) {
                setErrors(new Map([[at.row, { field: at.field, text: serverRowText(p.status, p.code, at.field) }]]));
                if (p.status === 412 || p.status === 409) refresh(rows[at.row].entity_id); // show the entity's newer revision
                // Focus after the busy lock lifts (a disabled control cannot take focus).
                focusRef.current = `sm-${at.field === "external_key" ? "key" : at.field === "external_type" ? "type" : "entity"}-${at.row}`;
            } else if (p?.status === 412 && p.code === "REVISION_CONFLICT") {
                // Show both versions; never resubmit on our own.
                request<OperationalSourceDetailV1>(path).then(setConflict, setSaveError);
            } else setSaveError(err);
        } finally {
            setBusy(false);
        }
    };

    async function reload() {
        try {
            adopt(await request<OperationalSourceDetailV1>(path));
            setReloadError(null);
        } catch (err) {
            setReloadError(err);
        }
    }

    const facts: [string, React.ReactNode][] = [
        ["Type", source.type],
        ["Scope", source.authority_scope],
        ["State", source.enabled ? "Enabled" : "Disabled"],
        ["Source revision", source.revision],
        ["Mapping revision", source.mapping_revision],
        ["Review", source.review ? `${source.review.verdict} at source revision ${source.review.source_revision}${source.review_eligible ? "" : " (no longer current)"}` : "Not reviewed"],
        ["Targets current", source.mapping_targets_current ? "Yes" : "No: some entities changed since they were mapped"],
    ];

    return (
        <VStack as="section" aria-labelledby="sm-title" className="sm-editor" gap={2}>
            <Heading level={2} id="sm-title">
                Source <span className="identifier">{short(source.source_id)}</span>
            </Heading>
            <MetadataList columns={2}>
                {facts.map(([k, v]) => (
                    <MetadataListItem key={k} label={k}>
                        {v}
                    </MetadataListItem>
                ))}
                <MetadataListItem label="Last change">
                    <Actor a={{ subject: source.actor_subject }} /> · <Time iso={source.created_at} />
                </MetadataListItem>
            </MetadataList>
            {entityError ? <Problem error={entityError} context="Entity names are unavailable; mapped rows show their IDs." /> : null}
            <form onSubmit={save} noValidate>
                <VStack gap={2}>
                    <fieldset disabled={busy || !!reloadError} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }}>
                        <VStack gap={2}>
                            <datalist id="sm-entities-asset">{[...(entities?.values() ?? [])].filter((e) => e.type === "asset").map((e) => <option key={e.entity_id} value={e.entity_id} label={e.display_name} />)}</datalist>
                            <datalist id="sm-entities-order">{[...(entities?.values() ?? [])].filter((e) => e.type === "order").map((e) => <option key={e.entity_id} value={e.entity_id} label={e.display_name} />)}</datalist>
                            <Table aria-label="Mappings" density="compact" verticalAlign="top">
                                <TableHeader>
                                    <TableRow isHeaderRow>
                                        <TableHeaderCell scope="col">#</TableHeaderCell>
                                        <TableHeaderCell scope="col">Type</TableHeaderCell>
                                        <TableHeaderCell scope="col">External key</TableHeaderCell>
                                        <TableHeaderCell scope="col">Entity</TableHeaderCell>
                                        <TableHeaderCell scope="col">Entity rev</TableHeaderCell>
                                        {writer && (
                                            <TableHeaderCell scope="col">
                                                <span className="sr-only">Actions</span>
                                            </TableHeaderCell>
                                        )}
                                    </TableRow>
                                </TableHeader>
                                <TableBody>
                                    {rows.length === 0 && (
                                        <TableRow>
                                            <TableCell colSpan={writer ? 6 : 5}>No external keys are mapped. Events from this source stay quarantined as UNKNOWN_MAPPING.</TableCell>
                                        </TableRow>
                                    )}
                                    {rows.map((r, i) => {
                                        const err = errors.get(i);
                                        const entity = entityOf(r.entity_id);
                                        const stale = entity && r.entity_revision !== entity.current_revision;
                                        const errId = `sm-err-${i}`;
                                        const isBad = (f: keyof Mapping) => err?.field === f || (f === "entity_id" && err?.field === "entity_revision");
                                        // Astryx text inputs derive aria-invalid from status; the native select gets it directly.
                                        const status = (f: keyof Mapping) => (isBad(f) ? { status: { type: "error" as const }, "aria-describedby": errId } : {});
                                        const label = r.external_key || `row ${i + 1}`;
                                        const message = (cell: "type" | "key" | "entity") =>
                                            err && cell === (err.field === "external_type" ? "type" : err.field === "external_key" ? "key" : "entity") ? (
                                                <Text as="div" size="sm" id={errId} role="alert" style={errText}>
                                                    {err.text}
                                                </Text>
                                            ) : null;
                                        return (
                                            <TableRow key={i} data-row={String(i)} style={err ? { background: "var(--color-background-red)" } : undefined}>
                                                <TableCell>{i + 1}</TableCell>
                                                <TableCell>
                                                    {writer ? (
                                                        <select id={`sm-type-${i}`} aria-label={`Type, row ${i + 1}`} value={r.external_type} onChange={(e) => edit(i, { external_type: e.target.value as Kind, entity_id: "", entity_revision: 0 })} {...(isBad("external_type") ? { "aria-invalid": true, "aria-describedby": errId } : {})}>
                                                            {kinds.map((k) => (
                                                                <option key={k}>{k}</option>
                                                            ))}
                                                        </select>
                                                    ) : (
                                                        r.external_type
                                                    )}
                                                    {message("type")}
                                                </TableCell>
                                                <TableCell>
                                                    {writer ? (
                                                        <TextInput id={`sm-key-${i}`} label={`External key, row ${i + 1}`} isLabelHidden value={r.external_key} width="100%" hasSpellCheck={false} {...{ maxLength: 512 }} onChange={(v) => edit(i, { external_key: v })} {...status("external_key")} />
                                                    ) : (
                                                        <span className="identifier">{r.external_key}</span>
                                                    )}
                                                    {message("key")}
                                                </TableCell>
                                                <TableCell>
                                                    {writer ? (
                                                        <TextInput
                                                            id={`sm-entity-${i}`}
                                                            label={`Entity ID, row ${i + 1}`}
                                                            isLabelHidden
                                                            {...{ list: `sm-entities-${r.external_type}` }}
                                                            value={r.entity_id}
                                                            width="100%"
                                                            hasSpellCheck={false}
                                                            placeholder="Entity ID or pick"
                                                            onChange={(v) => resolve(i, v.trim())}
                                                            {...status("entity_id")}
                                                        />
                                                    ) : null}
                                                    {r.entity_id && (
                                                        <Text size="sm" color="secondary" display="block">
                                                            <span title={r.entity_id}>
                                                                {entityName(r.entity_id) ? `${entityName(r.entity_id)} · ` : entities ? "Unknown entity " : ""}
                                                                <span className="identifier">{short(r.entity_id)}</span>
                                                            </span>
                                                        </Text>
                                                    )}
                                                    {message("entity")}
                                                </TableCell>
                                                <TableCell className="n">
                                                    {r.entity_revision || "—"}
                                                    {stale && (
                                                        <Text size="sm" style={{ color: "var(--color-text-warning)", whiteSpace: "nowrap" }}>
                                                            {" "}
                                                            ⚠ now {entity.current_revision}
                                                            {writer && (
                                                                <>
                                                                    {" "}
                                                                    <Button label={`Use entity revision ${entity.current_revision}, row ${i + 1}`} variant="ghost" size="sm" onClick={() => edit(i, { entity_revision: entity.current_revision })}>
                                                                        Use {entity.current_revision}
                                                                    </Button>
                                                                </>
                                                            )}
                                                        </Text>
                                                    )}
                                                </TableCell>
                                                {writer && (
                                                    <TableCell>
                                                        <Button label={`Remove ${label}`} variant="ghost" size="sm" onClick={() => remove(i)}>
                                                            Remove
                                                        </Button>
                                                    </TableCell>
                                                )}
                                            </TableRow>
                                        );
                                    })}
                                </TableBody>
                            </Table>
                            {writer && (
                                <HStack>
                                    <Button id="sm-add" label="+ Add mapping" variant="secondary" onClick={() => add()} />
                                </HStack>
                            )}

                            {unmapped.length > 0 && (
                                <VStack as="section" aria-labelledby="sm-unmapped" gap={1}>
                                    <Heading level={3} id="sm-unmapped">
                                        {unmapped.length} {unmapped.length === 1 ? "entity has" : "entities have"} no external key
                                    </Heading>
                                    <VStack as="ul" gap={0.5} style={{ maxHeight: "16rem", overflowY: "auto" }}>
                                        {unmapped.map((e) => (
                                            <HStack as="li" key={e.entity_id} gap={1} align="center" wrap="wrap">
                                                <Token label={e.type} size="sm" />
                                                <Text>{e.display_name}</Text>
                                                <Text size="sm" color="secondary">
                                                    <span className="identifier">{short(e.entity_id)}</span>
                                                </Text>
                                                {writer && (
                                                    <Button label={`Map ${e.display_name}`} variant="ghost" size="sm" onClick={() => add(e)}>
                                                        Map
                                                    </Button>
                                                )}
                                            </HStack>
                                        ))}
                                    </VStack>
                                </VStack>
                            )}
                        </VStack>
                    </fieldset>

                    {conflict && (
                        <Card role="alert" variant="orange" padding={3}>
                            <VStack gap={2}>
                                <Heading level={3} tabIndex={-1} ref={conflictRef}>
                                    Changed on the server (mapping revision {conflict.source.mapping_revision})
                                </Heading>
                                <Text as="p">Nothing was saved. Compare, then choose one; nothing is resubmitted for you.</Text>
                                <Grid columns={2} gap={2}>
                                    <Compare title="Yours" rows={rows} other={conflict.mappings} name={entityName} />
                                    <Compare title={`Server mapping revision ${conflict.source.mapping_revision}`} rows={conflict.mappings} other={rows} name={entityName} />
                                </Grid>
                                <HStack gap={1} wrap="wrap">
                                    <Button label="Use server version" variant="secondary" onClick={() => { clearDraft(draftKey); adopt(conflict); }} />
                                    <Button
                                        label={`Keep mine on mapping revision ${conflict.source.mapping_revision}`}
                                        variant="secondary"
                                        onClick={() => {
                                            setDetail(conflict);
                                            setBase({ mappingRevision: conflict.source.mapping_revision, rows: [...conflict.mappings] });
                                            setConflict(null);
                                        }}
                                    />
                                </HStack>
                            </VStack>
                        </Card>
                    )}
                    {saveError instanceof ApiError && saveError.problem.status === 428 && (
                        <Text as="p" weight="semibold" style={errText}>
                            Mapping changes need a recent sign-in. Confirm your identity, then save again.
                        </Text>
                    )}
                    <Problem error={saveError} />
                    {saved && (
                        <div role="status">
                            <Banner status="success" title={`✓ Saved mapping revision ${saved.mapping_revision}. This created source revision ${saved.revision}; the source needs a new review before intake trusts it.`} />
                        </div>
                    )}
                    {reloadError ? (
                        <Card role="alert" variant="red" padding={2}>
                            <HStack gap={1} align="center" wrap="wrap">
                                <Text>The save is confirmed, but the latest version could not be reloaded, so editing is paused.</Text>
                                <Button label="Reload source" variant="secondary" size="sm" onClick={reload} />
                            </HStack>
                        </Card>
                    ) : null}
                    {writer && (
                        <HStack
                            justify="end"
                            align="center"
                            gap={1}
                            wrap="wrap"
                            paddingBlock={1}
                            style={{ position: "sticky", bottom: 0, background: "var(--color-background-body)", borderTop: "1px solid var(--color-border)" }}
                        >
                            <Text size="sm" color="secondary" hasTabularNumbers style={{ marginRight: "auto" }}>
                                {dirty ? `Unsaved changes · ${rows.length} rows` : `${rows.length} rows`} · saving creates source revision {source.current_revision + 1}
                                {approved ? " and voids the current approval" : ""}.
                            </Text>
                            <Button label="Discard" variant="secondary" isDisabled={!dirty || busy || !!reloadError} onClick={() => adopt(detail)} />
                            <Button
                                type="submit"
                                variant="primary"
                                label={busy ? "Saving…" : retryable ? "Retry save" : `Save mapping revision ${base.mappingRevision + 1}`}
                                isDisabled={!dirty || busy || !online || !!conflict || !!reloadError}
                            />
                        </HStack>
                    )}
                </VStack>
            </form>
        </VStack>
    );
}

function Compare({ title, rows, other, name }: { title: string; rows: readonly Mapping[]; other: readonly Mapping[]; name: (id: string) => string | undefined }) {
    const theirs = new Map(other.map((r) => [rowKey(r), r]));
    return (
        <VStack gap={1}>
            <Heading level={4}>{title}</Heading>
            <VStack as="ol" gap={0.5}>
                {rows.map((r) => {
                    const o = theirs.get(rowKey(r));
                    const mark = !o ? "only here" : o.entity_id !== r.entity_id || o.entity_revision !== r.entity_revision ? "different" : "";
                    return (
                        <HStack as="li" key={rowKey(r)} gap={1} align="center" wrap="wrap">
                            <Text>
                                {r.external_type} <span className="identifier">{r.external_key}</span> → {name(r.entity_id) ?? short(r.entity_id)} rev {r.entity_revision}
                            </Text>
                            {mark && <Token label={mark} color="orange" size="sm" />}
                        </HStack>
                    );
                })}
                {rows.length === 0 && <li>No mappings</li>}
            </VStack>
        </VStack>
    );
}
