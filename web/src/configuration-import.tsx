import React, { useRef, useState } from "react";
import type { ConfigurationImportReportV1 } from "../../contracts/typescript/oee";
import { AmbiguousError, ApiError, parseProblem } from "./api";
import { label } from "./labels";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { Token } from "@astryxdesign/core/Token";
import { VStack } from "@astryxdesign/core/VStack";
import { Problem, Status, useSession } from "./ui";

type ImportReport = ConfigurationImportReportV1;
type StoredApply = { file: File; etag: string; key: string };

export function ConfigurationImport({ onApplied, onOpenObject }: { onApplied: () => void; onOpenObject: (kind: string, id: string) => void }) {
    const { site, me } = useSession();
    const [file, setFile] = useState<File | null>(null);
    const [report, setReport] = useState<ImportReport | null>(null);
    const [etag, setEtag] = useState("");
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const [rejectedOnly, setRejectedOnly] = useState(false);
    const [changeNotice, setChangeNotice] = useState("");
    const [appliedLocation, setAppliedLocation] = useState("");
    const [applied, setApplied] = useState(false);
    const applyIntent = useRef<StoredApply | null>(null);
    const fileInput = useRef<HTMLInputElement | null>(null);
    const dryRunNumber = useRef(0);

    async function send(nextFile: File, path: string, options: { accept?: string; ifMatch?: string; key?: string } = {}) {
        const body = new FormData();
        body.append("file", nextFile);
        const headers: Record<string, string> = { "X-CSRF-Token": me.csrf_token };
        if (options.accept) headers.Accept = options.accept;
        if (options.ifMatch) headers["If-Match"] = options.ifMatch;
        if (options.key) headers["Idempotency-Key"] = options.key;
        let response: Response;
        try {
            response = await fetch(`/v1/sites/${site.id}/${path}`, {
                method: "POST",
                headers,
                body,
                credentials: "same-origin",
                cache: "no-store",
            });
        } catch {
            throw new AmbiguousError();
        }
        if (!response.ok) throw new ApiError(await parseProblem(response));
        return response;
    }

    async function dryRun(nextFile: File, previous?: ImportReport, reason?: "stale" | "rejected") {
        const requestNumber = ++dryRunNumber.current;
        setLoading(true);
        setError(null);
        setReport(null);
        setEtag("");
        setAppliedLocation("");
        setApplied(false);
        try {
            const response = await send(nextFile, "configuration-imports/dry-runs");
            const next = (await response.json()) as ImportReport;
            const nextEtag = response.headers.get("ETag") ?? "";
            if (requestNumber !== dryRunNumber.current) return;
            if (nextEtag !== `"${next.report_sha256}"`) throw new Error("The server returned an incomplete import report.");
            setReport(next);
            setEtag(nextEtag);
            setRejectedOnly(next.counts.rejected > 0);
            setChangeNotice(previous ? changedRows(previous, next, reason) : "");
        } catch (e) {
            if (requestNumber === dryRunNumber.current) setError(e);
        } finally {
            if (requestNumber === dryRunNumber.current) setLoading(false);
        }
    }

    function chooseFile(nextFile?: File) {
        if (!nextFile) return;
        setFile(nextFile);
        setChangeNotice("");
        applyIntent.current = null;
        void dryRun(nextFile);
    }

    async function apply() {
        if (!file || !report || !etag || report.counts.rejected || !report.counts.accepted || loading || applied) return;
        const intent = applyIntent.current?.file === file && applyIntent.current.etag === etag
            ? applyIntent.current
            : { file, etag, key: crypto.randomUUID() };
        applyIntent.current = intent;
        setLoading(true);
        setError(null);
        try {
            const response = await send(file, "configuration-imports", { ifMatch: etag, key: intent.key });
            const applied = (await response.json()) as ImportReport;
            setReport(applied);
            setEtag(response.headers.get("ETag") ?? etag);
            setAppliedLocation(response.headers.get("Location") ?? "");
            setApplied(true);
            applyIntent.current = null;
            onApplied();
        } catch (e) {
            if (e instanceof ApiError && e.problem.status === 412) {
                applyIntent.current = null;
                await dryRun(file, report, "stale");
            } else if (e instanceof ApiError && e.problem.status === 422 && e.problem.code === "IMPORT_HAS_REJECTED_ROWS") {
                applyIntent.current = null;
                await dryRun(file, report, "rejected");
            } else {
                setError(e);
            }
        } finally {
            setLoading(false);
        }
    }

    async function downloadErrors() {
        if (!file || !report || loading) return;
        setLoading(true);
        setError(null);
        try {
            const response = await send(file, "configuration-imports/dry-runs", { accept: "text/csv" });
            const currentEtag = response.headers.get("ETag");
            const blob = await response.blob();
            const url = URL.createObjectURL(blob);
            const anchor = document.createElement("a");
            anchor.href = url;
            anchor.download = "configuration-import-errors.csv";
            anchor.click();
            window.setTimeout(() => URL.revokeObjectURL(url), 0);
            if (currentEtag && currentEtag !== etag) await dryRun(file, report, "stale");
        } catch (e) {
            setError(e);
        } finally {
            setLoading(false);
        }
    }

    const visibleRows = report?.rows.filter((row) => !rejectedOnly || row.status === "rejected") ?? [];
    const canApply = !!report && !!etag && !loading && !applied && report.counts.rejected === 0 && report.counts.accepted > 0;
    const applyLabel = `Apply ${report?.counts.accepted ?? 0} ${report?.counts.accepted === 1 ? "change" : "changes"}`;

    return (
        <VStack as="section" aria-labelledby="configuration-import-title" aria-busy={loading} gap={2}>
            <Heading level={2} id="configuration-import-title">
                Import CSV
            </Heading>
            <Card
                className="configuration-import-drop"
                variant="muted"
                padding={3}
                onDragOver={(event) => event.preventDefault()}
                onDrop={(event) => {
                    event.preventDefault();
                    if (!loading) chooseFile(event.dataTransfer.files[0]);
                }}
            >
                <HStack gap={2} align="center" wrap="wrap">
                    <Button label="Choose CSV" variant="secondary" onClick={() => fileInput.current?.click()} isDisabled={loading} />
                    <input
                        id="configuration-import-file"
                        aria-label="CSV file"
                        ref={fileInput}
                        type="file"
                        accept=".csv,text/csv"
                        disabled={loading}
                        hidden
                        onChange={(event) => {
                            chooseFile(event.currentTarget.files?.[0]);
                            event.currentTarget.value = "";
                        }}
                    />
                    <Text size="sm" color="secondary">
                        or drop a CSV here{file ? ` · ${file.name}` : ""}
                    </Text>
                    <a href="/templates/configuration-import-v1.csv" download>
                        Template
                    </a>
                    <Text size="sm" color="secondary">
                        Maximum 2,000 rows per file.
                    </Text>
                </HStack>
            </Card>
            <Problem error={error} />
            {changeNotice && (
                <Text as="p" role="status">
                    {changeNotice}
                </Text>
            )}
            {loading && (
                <Text as="p" color="secondary" role="status">
                    Checking configuration…
                </Text>
            )}
            {report && (
                <>
                    <HStack justify="between" align="center" gap={2} wrap="wrap">
                        <HStack role="group" aria-label="Import counts" gap={1} align="center" wrap="wrap">
                            <Token label={`${report.counts.accepted} accepted`} color="green" />
                            <Token label={`${report.counts.duplicate} duplicate`} />
                            <Token label={`${report.counts.rejected} rejected`} color={report.counts.rejected ? "red" : "default"} />
                        </HStack>
                        <HStack gap={1} align="center" wrap="wrap">
                            <CheckboxInput label="Rejected only" value={rejectedOnly} onChange={setRejectedOnly} />
                            <Button label="Download errors.csv" variant="secondary" onClick={downloadErrors} isDisabled={loading || !file} />
                            <Button label={applyLabel} variant="primary" onClick={apply} isDisabled={!canApply} />
                        </HStack>
                    </HStack>
                    {appliedLocation && (
                        <div role="status">
                            <Banner
                                status="success"
                                title={
                                    <>
                                        Import applied. Report: <code>{appliedLocation}</code>
                                    </>
                                }
                            />
                        </div>
                    )}
                        <Table aria-label="CSV import report" density="compact" verticalAlign="top">
                            <TableHeader>
                                <TableRow isHeaderRow>
                                    <TableHeaderCell scope="col">Row</TableHeaderCell>
                                    <TableHeaderCell scope="col">Kind</TableHeaderCell>
                                    <TableHeaderCell scope="col">Ref</TableHeaderCell>
                                    <TableHeaderCell scope="col">Name / object</TableHeaderCell>
                                    <TableHeaderCell scope="col">Status</TableHeaderCell>
                                    <TableHeaderCell scope="col">Reason</TableHeaderCell>
                                    <TableHeaderCell scope="col">Path</TableHeaderCell>
                                </TableRow>
                            </TableHeader>
                            <TableBody>
                                {visibleRows.map((row) => {
                                    const reason = row.reason ? label(row.reason) : null;
                                    const objectID = row.object_id;
                                    const canOpenObject = !!objectID && (row.status === "duplicate" || (row.status === "accepted" && (row.expected_revision ?? 0) > 0));
                                    return (
                                        <TableRow key={row.row}>
                                            <TableHeaderCell scope="row">{row.row}</TableHeaderCell>
                                            <TableCell>{row.kind}</TableCell>
                                            <TableCell>{row.ref || "—"}</TableCell>
                                            <TableCell>
                                                {row.display_name || "—"}
                                                {objectID && (
                                                    <Text as="div" size="sm" color="secondary">
                                                        <span title="Existing master-data object">
                                                            {canOpenObject && (
                                                                <a
                                                                    href={`#master-object-${objectID}`}
                                                                    onClick={(event) => {
                                                                        event.preventDefault();
                                                                        onOpenObject(row.kind, objectID);
                                                                    }}
                                                                >
                                                                    Open object
                                                                </a>
                                                            )}
                                                            <br />
                                                            <span className="identifier">{objectID}</span>
                                                        </span>
                                                    </Text>
                                                )}
                                            </TableCell>
                                            <TableCell>
                                                <Status code={row.status} />
                                            </TableCell>
                                            <TableCell>
                                                {reason && (
                                                    <VStack gap={0.5}>
                                                        <Text>{reason.text}</Text>
                                                        <code>{row.reason}</code>
                                                    </VStack>
                                                )}
                                            </TableCell>
                                            <TableCell>{row.path && <code>{row.path}</code>}</TableCell>
                                        </TableRow>
                                    );
                                })}
                            </TableBody>
                        </Table>
                        {!visibleRows.length && (
                            <Text as="p" size="sm" color="secondary">
                                No rows match this filter.
                            </Text>
                        )}
                </>
            )}
        </VStack>
    );
}

function changedRows(before: ImportReport, after: ImportReport, reason: "stale" | "rejected" = "stale") {
    const prior = new Map(before.rows.map((row) => [row.row, row]));
    const changes = after.rows.flatMap((row) => {
        const old = prior.get(row.row);
        if (!old || old.status === row.status && old.reason === row.reason && old.path === row.path && old.object_id === row.object_id && old.expected_revision === row.expected_revision) return [];
        return [`row ${row.row}: ${old.status}${old.reason ? ` (${old.reason})` : ""} → ${row.status}${row.reason ? ` (${row.reason})` : ""}`];
    });
    const prefix = reason === "rejected" ? "Apply found rejected rows; the check was refreshed." : "Site data changed since the check, re-checked.";
    const details = changes.slice(0, 5);
    if (changes.length > details.length) details.push(`${changes.length - details.length} more rows changed`);
    return `${prefix} ${details.length ? details.join("; ") : "Row outcomes are unchanged."}`;
}
