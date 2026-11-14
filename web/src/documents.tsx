import React, { useCallback, useRef, useState } from "react";
import type { EvidenceSourceV1, SourceDocumentV1, SourceScanBatchV1, SourceScanV1 } from "../../contracts/typescript/oee";
import { AmbiguousError, ApiError, classify, parseProblem, readBody, request } from "./api";
import { SiteDocuments, SourceAuthority } from "./document-authority";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Field } from "@astryxdesign/core/Field";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { VStack } from "@astryxdesign/core/VStack";
import { Problem, Status, short, usePolling, useSession } from "./ui";

// The upload the person is making: one key per chosen file and title, kept while its outcome may be
// unknown, so a retry after a dropped connection replays instead of storing a second source.
type Intent = { file: File; title: string; key: string };

// Words for the refusals the server gives a document, in place of the codes (the code stays in
// the problem details).
// keepsKey: an outcome after which the same file and title keep their Idempotency-Key. An unknown
// outcome replays; a conflict (409) must not become a second, new attempt.
const keepsKey = (problem: Parameters<typeof classify>[0]) => classify(problem) === "retry" || problem.status === 409;

const refusal: Record<string, string> = {
    // Busy or unavailable: the file may be fine, and the same upload is safe to send again.
    EXTRACTION_BUSY: "The server is reading other documents right now. Nothing was stored yet; upload again in a few seconds. Sending the same file again is safe.",
    SCAN_QUEUE_FULL: "Your organisation already has many pages waiting for the vision model. Nothing was stored yet; upload again in a minute. Sending the same file again is safe.",
    EXTRACTION_UNAVAILABLE: "The document reader is not available on this server right now. Nothing was stored; try again later, or ask your administrator. Sending the same file again is safe.",
    // Refused for what the file is: sending it again cannot succeed.
    LIMIT_EXCEEDED: "This file is too large: a document can be at most 20 MiB (a photo of a page 4 MiB), and its text at most 256 KiB. Split it into smaller documents.",
    UNSUPPORTED_MEDIA: "This kind of file cannot be uploaded. Use Word (.docx), Excel (.xlsx), PDF, CSV, plain text, or a photo of a page (PNG or JPEG).",
    UNREADABLE_SOURCE: "This file could not be read safely: it contains macros or external links, or it is damaged. Save a plain copy and try again.",
    PDF_ENCRYPTED: "This PDF is password-protected. Save an unprotected copy and upload that.",
    PDF_ACTIVE_CONTENT: "This PDF contains scripts, attachments or form actions, which are not accepted. Print it to a new PDF (\"Save as PDF\") and upload that.",
    PDF_EXTERNAL_REFERENCE: "This PDF links to websites or other files, which are not accepted (as for Word files with external links). Save a copy without the links and upload that.",
    PDF_NO_TEXT_LAYER: "This PDF has a page with no typed text, such as a scan, or a page shows something neither its text nor the vision model can read, such as an annotation, a stamp, a form or a drawing. The vision model can read the scanned pages: use \u201cRead the scanned pages\u201d below. Typed pages in the same file are stored with them as a document. Otherwise upload a typed version, such as the Word file.",
    PDF_SCAN_HAS_TEXT: "No page of this PDF is a scan for the vision model to read, or a typed page also has pictures. Upload it as a document instead: its typed text is stored and its pictures are read there.",
    PDF_SCAN_IMAGE_UNSUPPORTED: "A picture in this PDF cannot be read: it uses an encoding the vision model does not take (such as JBIG2, JPEG 2000 or an unusual fax setting), or a scanned page has several pictures or none. Save the pictures as JPEG or PNG, or upload a photo of each page.",
    PDF_SCAN_HIDDEN_CONTENT: "This PDF hides part of what it holds: typed text is covered or clipped, or a picture is cropped, rotated, masked, see-through, annotated or has something drawn over it. The stored text or the vision model could then read what a reader cannot see. Save it again without those edits, or upload a photo of each page.",
    PDF_TOO_MANY_IMAGES: "This PDF has more than 20 pictures large enough to read. Split it into smaller documents.",
    PDF_SCAN_TOO_MANY_PAGES: "This scan has more than 20 pages. Split it into smaller PDFs.",
    PDF_TOO_MANY_PAGES: "This PDF has more than 1000 pages. Split it into smaller documents.",
    PDF_PARSE_BUDGET_EXCEEDED: "This PDF needs more memory or time to read than the server allows. Split it, or save it again from the program that made it.",
    PDF_TEXT_UNREADABLE: "The text in this PDF uses an encoding the server cannot read. Save it again from the program that made it, or upload the Word version.",
};

// A photo of a page is read by the site's vision model after upload (WIS-625); these say why a
// page was set aside instead (the job's quarantine reason). No text is kept for such a page.
const quarantined: Record<string, string> = {
    PAGE_ILLEGIBLE: "The vision model found no legible text on this page. Take a sharper, well-lit photo of the whole page and upload it again.",
    PAGE_TRUNCATED: "This page holds more text than one reading allows, so none of it was kept. Photograph it in two halves and upload each.",
    OUTPUT_INVALID: "The vision model's reading did not have the required form, so none of it was kept. Upload the photo again.",
    TOOL_NOT_ALLOWED: "The vision model tried to do something other than read the page, so its reading was discarded. Upload the photo again.",
    EXTRACTION_FAILED: "The vision model could not finish reading this page. Upload the photo again later.",
    UPLOADER_NOT_AUTHORIZED: "The person who uploaded this page can no longer add documents at this site, so its text was not stored.",
    TEXT_TOO_LARGE: "The text read from this page is larger than one document allows. Photograph it in parts.",
    BUDGET_EXHAUSTED: "This page needs more reading than one page is allowed. Photograph it in parts.",
    RUN_CANCELED: "Reading this page was stopped before it finished, so nothing was stored. Upload it again.",
};

const plural = (n: number, one: string) => `${n} ${one}${n === 1 ? "" : "s"}`;

// Passages lists stored text passages as written, one card each.
function Passages({ label, spans }: { label: string; spans: readonly { id: string; text: string }[] }) {
    return (
        <VStack as="ol" gap={1} aria-label={label} style={{ listStyle: "none", padding: 0, margin: 0 }}>
            {spans.map((span) => (
                <li key={span.id}>
                    <Card padding={2}>
                        <pre className="passage">{span.text}</pre>
                    </Card>
                </li>
            ))}
        </VStack>
    );
}

const isPhoto = (f: File) => f.type === "image/png" || f.type === "image/jpeg" || /\.(png|jpe?g)$/i.test(f.name);

// DocumentUpload (WIS-618) sends one document to POST /source-documents. The server checks the bytes,
// extracts the text and stores it as a document source with citable passages; what is shown here is
// only what the server returned.
export function DocumentUpload() {
    const { site, me } = useSession();
    const canUpload = site.role !== "viewer";
    const [file, setFile] = useState<File | null>(null);
    const [title, setTitle] = useState("");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const [result, setResult] = useState<SourceDocumentV1 | null>(null);
    // WIS-645: the typed pages of a PDF that also has scanned pages, stored as one typed document.
    const [typed, setTyped] = useState<NonNullable<SourceScanBatchV1["document"]> | null>(null);
    const [scan, setScan] = useState<SourceScanV1 | null>(null);
    // WIS-625 B2: the pages of a scanned PDF, queued once the person asks for them to be read.
    const [pages, setPages] = useState<readonly SourceScanV1[] | null>(null);
    const pagesIntent = useRef<Intent | null>(null);
    // Pages the vision model has read and stored; each relists the site's documents, where the
    // owner decides a stored page's authority.
    const [stored, setStored] = useState(0);
    const onStored = useCallback(() => setStored((n) => n + 1), []);
    const intent = useRef<Intent | null>(null);

    function keyFor(nextFile: File, nextTitle: string) {
        const current = intent.current;
        if (current && current.file === nextFile && current.title === nextTitle) return current.key;
        intent.current = { file: nextFile, title: nextTitle, key: crypto.randomUUID() };
        return intent.current.key;
    }

    async function upload(e: React.FormEvent) {
        e.preventDefault();
        if (!file || !title.trim()) return;
        setBusy(true);
        setError(null);
        setResult(null);
        setScan(null);
        setPages(null);
        setTyped(null);
        const key = keyFor(file, title.trim());
        const photo = isPhoto(file);
        try {
            let response: Response;
            try {
                response = await fetch(`/v1/sites/${site.id}/${photo ? "source-scans" : "source-documents"}`, {
                    method: "POST",
                    credentials: "same-origin",
                    cache: "no-store",
                    headers: {
                        "X-CSRF-Token": me.csrf_token,
                        "Idempotency-Key": key,
                        "X-Source-Title": encodeURIComponent(title.trim()),
                        ...(file.type ? { "Content-Type": file.type } : {}),
                    },
                    body: file,
                });
            } catch {
                throw new AmbiguousError();
            }
            if (!response.ok) throw new ApiError(await parseProblem(response));
            if (photo) setScan(await readBody<SourceScanV1>(response));
            else setResult(await readBody<SourceDocumentV1>(response));
            intent.current = null;
        } catch (err) {
            // A refusal is final for this file; an unknown outcome (connection lost, 429 or 5xx, which a
            // proxy can return after the server committed) keeps the key so a retry replays. A 409
            // keeps it too: the key already names a stored attempt, and a new key could store this
            // file a second time (WIS-625 Codex round 10).
            if (err instanceof ApiError && !keepsKey(err.problem)) intent.current = null;
            setError(err);
        } finally {
            setBusy(false);
        }
    }

    // readPages sends the same scanned PDF to /source-scan-batches: one job per page, all or none.
    // Its key is kept while the outcome may be unknown, as for an upload.
    async function readPages() {
        if (!file) return;
        const current = pagesIntent.current;
        const key = current && current.file === file && current.title === title.trim() ? current.key : crypto.randomUUID();
        pagesIntent.current = { file, title: title.trim(), key };
        setBusy(true);
        setError(null);
        try {
            let response: Response;
            try {
                response = await fetch(`/v1/sites/${site.id}/source-scan-batches`, {
                    method: "POST",
                    credentials: "same-origin",
                    cache: "no-store",
                    headers: { "X-CSRF-Token": me.csrf_token, "Idempotency-Key": key, "X-Source-Title": encodeURIComponent(title.trim()), "Content-Type": "application/pdf" },
                    body: file,
                });
            } catch {
                throw new AmbiguousError();
            }
            if (!response.ok) throw new ApiError(await parseProblem(response));
            const batch = await readBody<SourceScanBatchV1>(response);
            setPages(batch.jobs);
            setTyped(batch.document ?? null);
            pagesIntent.current = null;
        } catch (err) {
            if (err instanceof ApiError && !keepsKey(err.problem)) pagesIntent.current = null;
            setError(err);
        } finally {
            setBusy(false);
        }
    }

    const code = error instanceof ApiError ? error.problem.code : "";
    const unknown = error instanceof AmbiguousError || (error instanceof ApiError && classify(error.problem) === "retry" && !refusal[code]);
    return (
        <VStack as="section" gap={3} aria-labelledby="documents-title">
            <Heading level={1} id="documents-title">
                Documents
            </Heading>
            {canUpload ? (
                <form onSubmit={upload} aria-label="Upload a document">
                    <HStack gap={2} align="end" wrap="wrap">
                        <TextInput label="Title" isRequired value={title} {...{ maxLength: 200 }} onChange={(v) => setTitle(v)} width={280} />
                        <Field label="File" inputID="document-file">
                            <input
                                id="document-file"
                                type="file"
                                required
                                accept=".docx,.xlsx,.csv,.txt,.md,.pdf,.png,.jpg,.jpeg"
                                onChange={(e) => {
                                    const next = e.currentTarget.files?.[0] ?? null;
                                    setFile(next);
                                    setResult(null);
                                    setScan(null);
                                    setPages(null);
                                    setTyped(null);
                                    setError(null);
                                    if (next && !title) setTitle(next.name.replace(/\.[^.]+$/, ""));
                                }}
                            />
                        </Field>
                        <Button type="submit" variant="primary" label={busy ? "Uploading…" : "Upload"} isDisabled={busy || !file || !title.trim()} />
                    </HStack>
                </form>
            ) : (
                <Text color="secondary">Viewers cannot upload documents. An owner, supervisor or operator uploads them.</Text>
            )}
            {error !== null &&
                (refusal[code] ? (
                    <div role="alert">
                        <Banner status="error" title={refusal[code]} />
                    </div>
                ) : (
                    <Problem error={error} />
                ))}
            {unknown && <Text color="secondary">The upload may or may not have been stored. Upload the same file again: it will not be stored twice.</Text>}
            {/* A batch whose outcome is unknown keeps its key (pagesIntent), so the retry replays it. */}
            {(code === "PDF_NO_TEXT_LAYER" || pagesIntent.current?.file === file) && file && !pages && (
                <div>
                    <Button label="Read the scanned pages" variant="primary" onClick={() => void readPages()} isDisabled={busy} />
                </div>
            )}
            {scan && <ScanProgress key={scan.job_id} initial={scan} onStored={onStored} />}
            {typed && (
                <VStack as="section" gap={2} aria-labelledby="typed-pages">
                    <Heading level={2} id="typed-pages">
                        Typed pages stored: {typed.source.title}
                    </Heading>
                    <Text size="sm" color="secondary" hasTabularNumbers>
                        Typed text, stored as written (not read by a model) · {plural(typed.spans.length, "passage")} · source {typed.source.source_id.slice(0, 8)}
                    </Text>
                    <Passages label="Typed passages" spans={typed.spans} />
                    <SourceAuthority key={typed.source.source_id} sourceID={typed.source.source_id} />
                </VStack>
            )}
            {pages && (
                <VStack as="section" gap={2} aria-label="Scanned pages">
                    {pages.map((p) => (
                        <ScanProgress key={p.job_id} initial={p} onStored={onStored} />
                    ))}
                </VStack>
            )}
            {result && (
                <VStack as="section" gap={2} aria-labelledby="document-result">
                    <Heading level={2} id="document-result">
                        Stored: {result.source.title}
                    </Heading>
                    <HStack gap={2} align="center" wrap="wrap">
                        <Status code={result.kind} />
                        <Text size="sm" color="secondary" hasTabularNumbers>
                            {result.source.byte_length} bytes of text · {plural(result.spans.length, "passage")} · source {result.source.source_id.slice(0, 8)} revision {result.source.revision}
                        </Text>
                    </HStack>
                    <Passages label="Passages" spans={result.spans} />
                    {(result.images_skipped ?? 0) > 0 && (
                        <Text size="sm" color="secondary">
                            {result.images_skipped} small picture{result.images_skipped === 1 ? " was" : "s were"} not read (logos and icons too small to hold text).
                        </Text>
                    )}
                    <SourceAuthority key={result.source.source_id} sourceID={result.source.source_id} />
                    {/* WIS-625: a typed PDF's pictures are read by the vision model into their own sources. */}
                    {result.image_reads && result.image_reads.length > 0 && (
                        <VStack as="section" gap={2} aria-label="Pictures in this document">
                            <Banner
                                status="warning"
                                title={`Only the typed text above is stored so far. ${result.image_reads.length} picture${result.image_reads.length === 1 ? " is" : "s are"} being read by the vision model; each becomes model-extracted text that the owner checks against the page before it supports facts.`}
                            />
                            {result.image_reads.map((p) => (
                                <ScanProgress key={p.job_id} initial={p} onStored={onStored} />
                            ))}
                        </VStack>
                    )}
                </VStack>
            )}
            <SiteDocuments key={result?.source.source_id ?? ""} refresh={stored} />
        </VStack>
    );
}

// ScanProgress follows one photographed page until the vision model has read it (WIS-625). What
// it shows comes from the server: the job's state, then the stored source, labelled as text a
// model extracted, which a person checks against the photo before relying on it.
function ScanProgress({ initial, onStored }: { initial: SourceScanV1; onStored: () => void }) {
    const { site } = useSession();
    const [job, setJob] = useState(initial);
    const [source, setSource] = useState<EvidenceSourceV1 | null>(null);
    const [error, setError] = useState<unknown>(null);
    const running = job.state === "running";
    const refresh = useCallback(async () => {
        try {
            const next = await request<SourceScanV1>(`/v1/sites/${site.id}/source-scans/${initial.job_id}`);
            setJob(next);
            if (next.state === "completed" && next.source_id) {
                setSource(await request<EvidenceSourceV1>(`/v1/sites/${site.id}/sources/${next.source_id}`));
                onStored();
            }
            setError(null);
        } catch (e) {
            setError(e);
        }
    }, [site.id, initial.job_id, onStored]);
    // A completed page keeps polling until its stored text has loaded: a failed read of the source
    // is retried, unless the server refused it for good.
    const final = error instanceof ApiError && classify(error.problem) !== "retry";
    usePolling(refresh, running || (job.state === "completed" && !source && !final) ? 3000 : null);
    return (
        <section aria-labelledby={`scan-${job.job_id}`}>
            <Card padding={3}>
                <VStack gap={2}>
                    <HStack gap={2} align="center" wrap="wrap">
                        <Heading level={2} id={`scan-${job.job_id}`}>
                            Photo: {job.title}
                        </Heading>
                        <Text size="sm" color="secondary" hasTabularNumbers>
                            {job.width} × {job.height} px · scan {short(job.job_id)}
                        </Text>
                    </HStack>
                    <Text as="p" role="status">
                        {running && "Queued for the vision model. The page is being read; this can take a minute."}
                        {job.state === "completed" && "Read by the vision model."}
                        {job.state === "quarantined" && "Not stored: this page was set aside."}
                    </Text>
                    {job.state === "quarantined" && (
                        <div role="alert">
                            <Banner status="error" title={quarantined[job.quarantine_reason ?? ""] ?? `Set aside: ${job.quarantine_reason}.`} />
                        </div>
                    )}
                    {error !== null && <Problem error={error} context="The scan's state could not be read." />}
                    {source && (
                        <>
                            <Banner
                                status="warning"
                                title={`Model-extracted text (${source.extraction?.model}). Check it against the photo before relying on it: nothing in it is treated as approved until a person reviews it.`}
                            />
                            <pre className="passage" aria-label="Extracted text">
                                {source.text}
                            </pre>
                            <Text size="sm" color="secondary">
                                source {short(source.source_id)} revision {source.revision}
                            </Text>
                        </>
                    )}
                </VStack>
            </Card>
        </section>
    );
}
