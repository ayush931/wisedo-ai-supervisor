import { expect, test, type Page, type Request } from "@playwright/test";

// WIS-618: the Documents screen uploads one document to POST /source-documents (WIS-613) and shows
// only what the server returned: the stored source and its citable passages. Every test stubs the
// API (UI behaviour); the server's own checks are covered by its integration tests.

const site = "00000000-0000-4000-8000-000000000921";
const sourceID = "11111111-0000-4000-8000-000000000922";
const meFor = (role: string) => ({
    csrf_token: "test-csrf",
    display_name: "Stubbed member",
    subject: "member-subject",
    sites: [{ id: site, name: "Stubbed Site", role, tenant_id: "00000000-0000-4000-8000-000000000920", agent_available: false }],
});
const passages = ["Receiving SOP\nCoil is checked at Gate 2.", "Step\tCycle (s)\nPress\t42"];
const stored = {
    kind: "docx",
    original_sha256: "a".repeat(64),
    source: {
        access_epoch: 1, authority: "reported", authority_class: "reported", byte_length: 64, created_at: "2026-11-02T00:00:00Z", current_revision: 1,
        independence_group_id: sourceID, kind: "document", revision: 1, rune_length: 64, sha256: "b".repeat(64), source_id: sourceID, state_revision: 1,
        text: passages.join("\n\n"), title: "Receiving SOP",
    },
    spans: passages.map((text, i) => ({ id: `22222222-0000-4000-8000-00000000092${i}`, source_id: sourceID, revision: 1, start_char: 0, end_char: text.length, text, source_sha256: "b".repeat(64), access_epoch: 1 })),
};
const problem = (status: number, code: string) => ({ status, contentType: "application/problem+json", body: JSON.stringify({ code, status, title: code, type: "about:blank" }) });
type Reply = { status?: number; json?: unknown; contentType?: string; body?: string };

async function open(page: Page, role: string, upload: (request: Request) => Reply | Promise<Reply> = () => ({ json: stored })) {
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor(role) });
        if (path === `/v1/sites/${site}/source-documents` && route.request().method() === "POST") return route.fulfill(await upload(route.request()));
        // WIS-641: the page also lists the site's documents and reads the stored one's authority
        // (owner-decisions.spec.ts covers those); here the site has none yet and no decisions.
        if (path === `/v1/sites/${site}/sources`) return route.fulfill({ json: { sources: [] } });
        if (path === `/v1/sites/${site}/sources/${sourceID}`) return route.fulfill({ json: stored.source });
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    await expect(page.getByRole("heading", { name: "Documents", exact: true })).toBeVisible();
}

async function choose(page: Page, name: string, mimeType: string, title = "") {
    await page.getByLabel("File").setInputFiles({ name, mimeType, buffer: Buffer.from("PK\x03\x04 stub bytes") });
    if (title) await page.getByLabel("Title").fill(title);
}

test("stubbed upload sends the raw file with its type, title and keys, then lists the passages the server returned", async ({ page }) => {
    const seen: Request[] = [];
    await open(page, "supervisor", (request) => {
        seen.push(request);
        return { json: stored };
    });
    await choose(page, "Receiving SOP.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document");
    // The title defaults to the file name without its extension.
    await expect(page.getByLabel("Title")).toHaveValue("Receiving SOP");
    await page.getByLabel("Title").fill("Receiving SOP — गेट 2");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByRole("heading", { name: "Stored: Receiving SOP" })).toBeVisible();
    const list = page.getByRole("list", { name: "Passages" });
    await expect(list.getByRole("listitem")).toHaveCount(2);
    await expect(list.getByRole("listitem").nth(1)).toContainText("Press\t42");
    expect(seen).toHaveLength(1);
    const headers = seen[0].headers();
    expect(headers["content-type"]).toBe("application/vnd.openxmlformats-officedocument.wordprocessingml.document");
    expect(headers["x-csrf-token"]).toBe("test-csrf");
    expect(headers["idempotency-key"]).toMatch(/^[0-9a-f-]{36}$/);
    expect(decodeURIComponent(headers["x-source-title"])).toBe("Receiving SOP — गेट 2");
    expect(headers["if-match"]).toBeUndefined();
    expect(seen[0].postDataBuffer()?.toString()).toContain("stub bytes");
});

for (const [code, status, words] of [
    ["EXTRACTION_UNAVAILABLE", 503, "The document reader is not available"],
    ["EXTRACTION_BUSY", 503, "Sending the same file again is safe"],
    ["PDF_ENCRYPTED", 422, "This PDF is password-protected"],
    ["PDF_ACTIVE_CONTENT", 415, "scripts, attachments or form actions"],
    ["PDF_EXTERNAL_REFERENCE", 415, "links to websites or other files"],
    ["PDF_NO_TEXT_LAYER", 422, "The vision model can read the scanned pages"],
    ["PDF_TOO_MANY_PAGES", 413, "more than 1000 pages"],
    ["PDF_PARSE_BUDGET_EXCEEDED", 413, "more memory or time to read"],
    ["PDF_TEXT_UNREADABLE", 422, "an encoding the server cannot read"],
    ["LIMIT_EXCEEDED", 413, "This file is too large"],
    ["UNSUPPORTED_MEDIA", 415, "This kind of file cannot be uploaded"],
    ["UNREADABLE_SOURCE", 422, "This file could not be read safely"],
] as const) {
    test(`stubbed refusal ${code} is explained in plain words and nothing is shown as stored`, async ({ page }) => {
        await open(page, "owner", () => problem(status, code));
        await choose(page, "plan.pdf", "application/pdf", "Plan");
        await page.getByRole("button", { name: "Upload" }).click();
        await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText(words);
        await expect(page.getByRole("heading", { name: /^Stored:/ })).toHaveCount(0);
    });
}

test("stubbed upload whose outcome is unknown is retried with the same key, so it is stored once", async ({ page }) => {
    const keys: string[] = [];
    let first = true;
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor("owner") });
        if (path === `/v1/sites/${site}/source-documents`) {
            keys.push(route.request().headers()["idempotency-key"]);
            if (first) {
                first = false;
                return route.abort("connectionreset");
            }
            return route.fulfill({ json: stored });
        }
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    await choose(page, "sop.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "Receiving SOP");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByText(/may or may not have been stored/)).toBeVisible();
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByRole("heading", { name: "Stored: Receiving SOP" })).toBeVisible();
    expect(keys).toHaveLength(2);
    expect(keys[1]).toBe(keys[0]);
});

test("stubbed viewer sees no upload form", async ({ page }) => {
    await open(page, "viewer");
    await expect(page.getByRole("form", { name: "Upload a document" })).toHaveCount(0);
    await expect(page.getByText("Viewers cannot upload documents.")).toBeVisible();
});

// Review of PR #219: a proxy 502/504 can arrive after the server committed the upload. The retry must
// reuse the key so the server replays the first receipt; only a definite refusal starts a new key.
for (const status of [502, 504]) {
    test(`stubbed upload committed behind a ${status} is retried with the same key, so it is stored once`, async ({ page }) => {
        const keys: string[] = [];
        const stores = new Set<string>();
        await open(page, "owner", (request) => {
            const key = request.headers()["idempotency-key"];
            keys.push(key);
            stores.add(key); // the server commits every key it sees; a new key is a second source
            return keys.length === 1 ? problem(status, "BAD_GATEWAY") : { json: stored };
        });
        await choose(page, "sop.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "Receiving SOP");
        await page.getByRole("button", { name: "Upload" }).click();
        await expect.soft(page.getByText(/may or may not have been stored/)).toBeVisible();
        await page.getByRole("button", { name: "Upload" }).click();
        await expect(page.getByRole("heading", { name: "Stored: Receiving SOP" })).toBeVisible();
        expect(keys).toHaveLength(2);
        expect(stores.size).toBe(1);
    });
}

test("stubbed definite refusal clears the key, so the next upload of the same file is a new intent", async ({ page }) => {
    const keys: string[] = [];
    await open(page, "owner", (request) => {
        keys.push(request.headers()["idempotency-key"]);
        return keys.length === 1 ? problem(422, "UNREADABLE_SOURCE") : { json: stored };
    });
    await choose(page, "sop.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "Receiving SOP");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("This file could not be read safely");
    await expect(page.getByText(/may or may not have been stored/)).toHaveCount(0);
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByRole("heading", { name: "Stored: Receiving SOP" })).toBeVisible();
    expect(keys).toHaveLength(2);
    expect(keys[1]).not.toBe(keys[0]);
});

// WIS-625 Codex round 10: a 409 under the key keeps the key and shows the error, so trying the same
// file again cannot store it a second time under a new key.
test("stubbed idempotency conflict keeps the key and shows the error", async ({ page }) => {
    const keys: string[] = [];
    await open(page, "owner", (request) => {
        keys.push(request.headers()["idempotency-key"]);
        return problem(409, "IDEMPOTENCY_CONFLICT");
    });
    await choose(page, "sop.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "Receiving SOP");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByText("IDEMPOTENCY_CONFLICT", { exact: false })).toBeVisible();
    await page.getByRole("button", { name: "Upload" }).click();
    await expect.poll(() => keys.length).toBe(2);
    expect(keys[1]).toBe(keys[0]);
});

// WIS-624: a busy PDF reader (503 EXTRACTION_BUSY) keeps the key, so the retry replays; a refusal
// decided by the file (422 PDF_ENCRYPTED) ends that attempt, and the next upload uses a new key.
test("stubbed EXTRACTION_BUSY is retried with the same key; a PDF refusal starts a new key", async ({ page }) => {
    const keys: string[] = [];
    await open(page, "owner", (request) => {
        keys.push(request.headers()["idempotency-key"]);
        if (keys.length === 1) return problem(503, "EXTRACTION_BUSY");
        if (keys.length === 2) return problem(422, "PDF_ENCRYPTED");
        return { json: stored };
    });
    await choose(page, "plan.pdf", "application/pdf", "Plan");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("Sending the same file again is safe");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("This PDF is password-protected");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByRole("heading", { name: /^Stored:/ })).toBeVisible();
    expect(keys).toHaveLength(3);
    expect(keys[1]).toBe(keys[0]);
    expect(keys[2]).not.toBe(keys[1]);
});

// WIS-625: a photo of a page goes to POST /source-scans and is read later by the vision model. The
// page follows the job and shows the stored text labelled as model-extracted, or why it was set
// aside; nothing is shown as stored before the server says so.
const scanID = "33333333-0000-4000-8000-000000000625";
const scanSource = "44444444-0000-4000-8000-000000000625";
const scanJob = (state: string, extra: Record<string, unknown> = {}) => ({
    job_id: scanID, run_id: "55555555-0000-4000-8000-000000000625", state, title: "Receiving log", media_type: "image/png", width: 1400, height: 1000,
    original_sha256: "c".repeat(64), created_at: "2026-11-06T00:00:00Z", ...extra,
});
async function openScans(page: Page, post: (r: Request) => Reply | "abort", states: Record<string, unknown>[]) {
    let reads = 0;
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor("operator") });
        // The page also lists the site's documents (WIS-641); the site has none yet.
        if (path === `/v1/sites/${site}/sources`) return route.fulfill({ json: { sources: [] } });
        if (path === `/v1/sites/${site}/source-scans` && route.request().method() === "POST") {
            const reply = post(route.request());
            return reply === "abort" ? route.abort("connectionreset") : route.fulfill(reply);
        }
        if (path === `/v1/sites/${site}/source-scans/${scanID}`) return route.fulfill({ json: states[Math.min(reads++, states.length - 1)] });
        if (path === `/v1/sites/${site}/sources/${scanSource}`)
            return route.fulfill({ json: { ...stored.source, source_id: scanSource, title: "Receiving log", text: "RECEIVING LOG\nCoil 4471\tDeccan Steel",
                extraction: { method: "model_ocr", model: "qwen2.5-vl-7b", model_revision: "sha256:v", prompt_sha256: "d".repeat(64), run_id: "55555555-0000-4000-8000-000000000625" } } });
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    await page.getByLabel("File").setInputFiles({ name: "receiving-log.png", mimeType: "image/png", buffer: Buffer.from("\x89PNG\r\n\x1a\n stub") });
    await page.getByLabel("Title").fill("Receiving log");
}

test("stubbed photo is queued for the vision model and its text is shown only once stored, labelled model-extracted", async ({ page }) => {
    const posts: Request[] = [];
    await openScans(page, (r) => {
        posts.push(r);
        return { status: 202, json: scanJob("running") };
    }, [scanJob("running"), scanJob("completed", { source_id: scanSource, finished_at: "2026-11-06T00:01:00Z" })]);
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByRole("status").filter({ hasText: /\S/ })).toHaveText(/Queued for the vision model/);
    await expect(page.getByLabel("Extracted text")).toHaveCount(0);
    await expect(page.getByRole("status").filter({ hasText: /\S/ })).toHaveText("Read by the vision model.", { timeout: 10_000 });
    await expect(page.getByLabel("Extracted text")).toHaveText("RECEIVING LOG\nCoil 4471\tDeccan Steel");
    await expect(page.getByText("Model-extracted text (qwen2.5-vl-7b)", { exact: false })).toBeVisible();
    expect(posts).toHaveLength(1);
    expect(posts[0].headers()["content-type"]).toBe("image/png");
    expect(posts[0].headers()["idempotency-key"]).toBeTruthy();
    expect(decodeURIComponent(posts[0].headers()["x-source-title"])).toBe("Receiving log");
});

test("stubbed set-aside page says why and shows no text", async ({ page }) => {
    await openScans(page, () => ({ status: 202, json: scanJob("running") }), [scanJob("quarantined", { quarantine_reason: "PAGE_TRUNCATED", finished_at: "2026-11-06T00:01:00Z" })]);
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("Photograph it in two halves", { timeout: 10_000 });
    await expect(page.getByRole("status").filter({ hasText: /\S/ })).toHaveText("Not stored: this page was set aside.");
    await expect(page.getByLabel("Extracted text")).toHaveCount(0);
});

test("stubbed photo whose upload outcome is unknown is retried with the same key", async ({ page }) => {
    const posts: Request[] = [];
    await openScans(page, (r) => {
        posts.push(r);
        return posts.length === 1 ? "abort" : { status: 202, json: scanJob("running") };
    }, [scanJob("running")]);
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByText("may or may not have been stored", { exact: false })).toBeVisible();
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByRole("status").filter({ hasText: /\S/ })).toHaveText(/Queued for the vision model/);
    expect(posts).toHaveLength(2);
    expect(posts[1].headers()["idempotency-key"]).toBe(posts[0].headers()["idempotency-key"]);
});

// WIS-625 B2: a PDF refused as scanned can be read page by page; the same file goes to
// /source-scan-batches and each page is followed like a photo.
test("stubbed scanned PDF is read page by page after the person asks", async ({ page }) => {
    const batches: Request[] = [];
    const job2 = "66666666-0000-4000-8000-000000000625";
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor("operator") });
        // The page also lists the site's documents (WIS-641); the site has none yet.
        if (path === `/v1/sites/${site}/sources`) return route.fulfill({ json: { sources: [] } });
        if (path === `/v1/sites/${site}/source-documents`) return route.fulfill(problem(422, "PDF_NO_TEXT_LAYER"));
        if (path === `/v1/sites/${site}/source-scan-batches`) {
            batches.push(route.request());
            return route.fulfill({ status: 202, json: { jobs: [scanJob("running", { title: "Receiving log (page 1 of 2)" }), scanJob("running", { job_id: job2, title: "Receiving log (page 2 of 2)" })] } });
        }
        if (path === `/v1/sites/${site}/source-scans/${scanID}`) return route.fulfill({ json: scanJob("quarantined", { title: "Receiving log (page 1 of 2)", quarantine_reason: "PAGE_ILLEGIBLE" }) });
        if (path === `/v1/sites/${site}/source-scans/${job2}`) return route.fulfill({ json: scanJob("running", { job_id: job2, title: "Receiving log (page 2 of 2)" }) });
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    await page.getByLabel("File").setInputFiles({ name: "receiving-log.pdf", mimeType: "application/pdf", buffer: Buffer.from("%PDF-1.7 stub") });
    await page.getByLabel("Title").fill("Receiving log");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("The vision model can read the scanned pages");
    await page.getByRole("button", { name: "Read the scanned pages" }).click();
    const pages = page.getByRole("region", { name: "Scanned pages" });
    await expect(pages.getByRole("heading", { name: "Photo: Receiving log (page 1 of 2)" })).toBeVisible();
    await expect(pages.getByRole("heading", { name: "Photo: Receiving log (page 2 of 2)" })).toBeVisible();
    await expect(pages.getByRole("alert").filter({ hasText: /\S/ })).toContainText("no legible text");
    expect(batches).toHaveLength(1);
    expect(batches[0].headers()["content-type"]).toBe("application/pdf");
    expect(decodeURIComponent(batches[0].headers()["x-source-title"])).toBe("Receiving log");
});

// WIS-645 slice 2: a PDF with typed and scanned pages is one upload. After "Read the scanned pages",
// the typed pages are shown as a stored typed document, never labelled model-read, with their
// passages and authority panel, next to the scanned page jobs.
test("stubbed mixed PDF shows its typed pages as a typed document next to its scanned pages", async ({ page }) => {
    const typedSource = "77777777-0000-4000-8000-000000000645";
    const typedDoc = { ...stored.source, source_id: typedSource, independence_group_id: typedSource, title: "Receiving SOP", text: "Receiving: weigh every coil.\n" };
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor("operator") });
        if (path === `/v1/sites/${site}/sources`) return route.fulfill({ json: { sources: [] } });
        if (path === `/v1/sites/${site}/source-documents`) return route.fulfill(problem(422, "PDF_NO_TEXT_LAYER"));
        if (path === `/v1/sites/${site}/source-scan-batches`) {
            return route.fulfill({
                status: 202,
                json: {
                    upload_id: "88888888-0000-4000-8000-000000000645",
                    jobs: [scanJob("running", { title: "Receiving SOP (page 2 of 2)" })],
                    document: {
                        source: typedDoc,
                        spans: [{ id: "99999999-0000-4000-8000-000000000645", source_id: typedSource, revision: 1, start_char: 0, end_char: 28, text: "Receiving: weigh every coil.", source_sha256: "b", access_epoch: 1 }],
                    },
                },
            });
        }
        if (path === `/v1/sites/${site}/source-scans/${scanID}`) return route.fulfill({ json: scanJob("running", { title: "Receiving SOP (page 2 of 2)" }) });
        // The typed document's authority panel reads it; it has no decisions yet.
        if (path === `/v1/sites/${site}/sources/${typedSource}`) return route.fulfill({ json: typedDoc });
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    await page.getByLabel("File").setInputFiles({ name: "receiving.pdf", mimeType: "application/pdf", buffer: Buffer.from("%PDF-1.7 stub") });
    await page.getByLabel("Title").fill("Receiving SOP");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("Typed pages in the same file are stored with them as a document");
    await page.getByRole("button", { name: "Read the scanned pages" }).click();
    const typed = page.getByRole("region", { name: "Typed pages stored: Receiving SOP" });
    await expect(typed).toContainText("Typed text, stored as written (not read by a model)");
    await expect(typed.getByRole("list", { name: "Typed passages" })).toContainText("Receiving: weigh every coil.");
    await expect(typed.getByRole("group", { name: "Authority of Receiving SOP" })).toBeVisible();
    await expect(page.getByRole("region", { name: "Scanned pages" }).getByRole("heading", { name: "Photo: Receiving SOP (page 2 of 2)" })).toBeVisible();
});

// WIS-625 review (#252): a replay can return a page that is already read; a failed first read of its
// text is retried until the text loads, instead of leaving the page with no text and no retry.
test("stubbed completed scan whose text read fails at first keeps trying until the text loads", async ({ page }) => {
    let sourceReads = 0;
    const done = scanJob("completed", { source_id: scanSource, finished_at: "2026-11-06T00:01:00Z" });
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor("operator") });
        if (path === `/v1/sites/${site}/sources`) return route.fulfill({ json: { sources: [] } });
        if (path === `/v1/sites/${site}/source-scans` && route.request().method() === "POST") return route.fulfill({ status: 202, json: done });
        if (path === `/v1/sites/${site}/source-scans/${scanID}`) return route.fulfill({ json: done });
        if (path === `/v1/sites/${site}/sources/${scanSource}`) {
            if (++sourceReads === 1) return route.fulfill(problem(503, "SERVICE_UNAVAILABLE"));
            return route.fulfill({ json: { ...stored.source, source_id: scanSource, title: "Receiving log", text: "RECEIVING LOG",
                extraction: { method: "model_ocr", model: "qwen2.5-vl-7b", model_revision: "sha256:v", prompt_sha256: "d".repeat(64), run_id: "55555555-0000-4000-8000-000000000625" } } });
        }
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    await page.getByLabel("File").setInputFiles({ name: "receiving-log.png", mimeType: "image/png", buffer: Buffer.from("\x89PNG\r\n\x1a\n stub") });
    await page.getByLabel("Title").fill("Receiving log");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByLabel("Extracted text")).toHaveText("RECEIVING LOG", { timeout: 10_000 });
    expect(sourceReads).toBeGreaterThanOrEqual(2);
});

// WIS-625 review (#255): a scanned-PDF batch whose outcome is unknown (connection lost, 503) keeps
// its retry control, and each retry replays the batch with the same key.
test("stubbed scanned-PDF batch that fails transiently can be retried with the same key", async ({ page }) => {
    const batches: Request[] = [];
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor("operator") });
        if (path === `/v1/sites/${site}/sources`) return route.fulfill({ json: { sources: [] } });
        if (path === `/v1/sites/${site}/source-documents`) return route.fulfill(problem(422, "PDF_NO_TEXT_LAYER"));
        if (path === `/v1/sites/${site}/source-scan-batches`) {
            batches.push(route.request());
            if (batches.length === 1) return route.abort("connectionreset");
            if (batches.length === 2) return route.fulfill(problem(503, "SERVICE_UNAVAILABLE"));
            return route.fulfill({ status: 202, json: { jobs: [scanJob("running", { title: "Receiving log (page 1 of 1)" })] } });
        }
        if (path === `/v1/sites/${site}/source-scans/${scanID}`) return route.fulfill({ json: scanJob("running", { title: "Receiving log (page 1 of 1)" }) });
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    await page.getByLabel("File").setInputFiles({ name: "receiving-log.pdf", mimeType: "application/pdf", buffer: Buffer.from("%PDF-1.7 stub") });
    await page.getByLabel("Title").fill("Receiving log");
    await page.getByRole("button", { name: "Upload" }).click();
    const read = page.getByRole("button", { name: "Read the scanned pages" });
    await read.click();
    await expect(page.getByText("may or may not have been stored", { exact: false })).toBeVisible();
    await read.click();
    await expect.poll(() => batches.length).toBe(2);
    await expect(read).toBeEnabled();
    await read.click();
    await expect(page.getByRole("region", { name: "Scanned pages" }).getByRole("heading", { name: "Photo: Receiving log (page 1 of 1)" })).toBeVisible();
    expect(batches).toHaveLength(3);
    const keys = batches.map((b) => b.headers()["idempotency-key"]);
    expect(keys[1]).toBe(keys[0]);
    expect(keys[2]).toBe(keys[0]);
});

// WIS-625 review (#252): the site's document list marks a source whose text a model extracted.
test("stubbed site documents mark model-extracted text", async ({ page }) => {
    const summary = (id: string, title: string, model_extracted: boolean) => ({ access_epoch: 1, authority: "reported", authority_class: "reported", byte_length: 10,
        independence_group_id: id, kind: "document", revision: 1, rune_length: 10, sha256: "b".repeat(64), source_id: id, state_revision: 1, title, model_extracted });
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor("operator") });
        if (path === `/v1/sites/${site}/sources`) return route.fulfill({ json: { sources: [summary(scanSource, "Receiving log (page 1 of 1)", true), summary(sourceID, "Receiving SOP", false)] } });
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    const list = page.getByRole("list", { name: "Site documents" });
    await expect(list.getByRole("listitem", { name: "Receiving log (page 1 of 1)" })).toContainText("model-extracted text (OCR)");
    await expect(list.getByRole("listitem", { name: "Receiving SOP" })).not.toContainText("model-extracted");
});

// WIS-625 post-merge review: once a photographed page is read and stored, it appears in the site's
// document list, where the owner decides its authority. Before the fix the list was reloaded only
// for a typed upload, so the stored page and its next action stayed out of sight until a reload.
test("stubbed completed scan appears in the site's document list", async ({ page }) => {
    let lists = 0;
    const done = scanJob("completed", { source_id: scanSource, finished_at: "2026-11-06T00:01:00Z" });
    const summary = { access_epoch: 1, authority: "reported", authority_class: "reported", byte_length: 13, independence_group_id: scanSource, kind: "document",
        revision: 1, rune_length: 13, sha256: "b".repeat(64), source_id: scanSource, state_revision: 1, title: "Receiving log", model_extracted: true };
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor("owner") });
        if (path === `/v1/sites/${site}/sources`) return route.fulfill({ json: { sources: ++lists === 1 ? [] : [summary] } });
        if (path === `/v1/sites/${site}/source-scans` && route.request().method() === "POST") return route.fulfill({ status: 202, json: scanJob("running") });
        if (path === `/v1/sites/${site}/source-scans/${scanID}`) return route.fulfill({ json: done });
        if (path === `/v1/sites/${site}/sources/${scanSource}`) {
            return route.fulfill({ json: { ...stored.source, source_id: scanSource, title: "Receiving log", text: "RECEIVING LOG",
                extraction: { method: "model_ocr", model: "qwen2.5-vl-7b", model_revision: "sha256:v", prompt_sha256: "d".repeat(64), run_id: "55555555-0000-4000-8000-000000000625" } } });
        }
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    await expect(page.getByText("No documents uploaded at this site yet.")).toBeVisible();
    await page.getByLabel("File").setInputFiles({ name: "receiving-log.png", mimeType: "image/png", buffer: Buffer.from("\x89PNG\r\n\x1a\n stub") });
    await page.getByLabel("Title").fill("Receiving log");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByLabel("Extracted text")).toHaveText("RECEIVING LOG", { timeout: 10_000 });
    const item = page.getByRole("list", { name: "Site documents" }).getByRole("listitem", { name: "Receiving log" });
    await expect(item).toContainText("model-extracted text (OCR)");
    await expect(item.getByRole("button", { name: "Authority" })).toBeVisible();
});

// WIS-625 post-merge decision: a typed PDF's pictures are read by the vision model. The upload shows
// that only the typed text is stored so far, follows each picture's read, and counts the small
// pictures it skipped; a read picture is model-extracted text and joins the document list.
test("stubbed typed PDF shows its pictures being read and the small ones skipped", async ({ page }) => {
    let lists = 0;
    const figure = scanJob("running", { title: "Receiving SOP (page 1, image 1)", width: 400, height: 300, media_type: "image/jpeg" });
    const done = { ...figure, state: "completed", source_id: scanSource, finished_at: "2026-11-06T00:01:00Z" };
    const summary = { access_epoch: 1, authority: "reported", authority_class: "reported", byte_length: 13, independence_group_id: scanSource, kind: "document",
        revision: 1, rune_length: 13, sha256: "b".repeat(64), source_id: scanSource, state_revision: 1, title: "Receiving SOP (page 1, image 1)", model_extracted: true };
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor("supervisor") });
        if (path === `/v1/sites/${site}/sources`) return route.fulfill({ json: { sources: ++lists === 1 ? [] : [summary] } });
        if (path === `/v1/sites/${site}/source-documents`) return route.fulfill({ json: { ...stored, kind: "pdf", image_reads: [figure], images_skipped: 1 } });
        if (path === `/v1/sites/${site}/sources/${sourceID}`) return route.fulfill({ json: stored.source });
        if (path === `/v1/sites/${site}/source-scans/${scanID}`) return route.fulfill({ json: done });
        if (path === `/v1/sites/${site}/sources/${scanSource}`) {
            return route.fulfill({ json: { ...stored.source, source_id: scanSource, title: "Receiving SOP (page 1, image 1)", text: "FIGURE 1 PRESS 45 s",
                extraction: { method: "model_ocr", model: "qwen2.5-vl-7b", model_revision: "sha256:v", prompt_sha256: "d".repeat(64), run_id: "55555555-0000-4000-8000-000000000625" } } });
        }
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    await page.getByLabel("File").setInputFiles({ name: "Receiving SOP.pdf", mimeType: "application/pdf", buffer: Buffer.from("%PDF-1.7 stub") });
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(page.getByRole("heading", { name: "Stored: Receiving SOP" })).toBeVisible();
    await expect(page.getByText("1 small picture was not read", { exact: false })).toBeVisible();
    const pictures = page.getByRole("region", { name: "Pictures in this document" });
    await expect(pictures).toContainText("Only the typed text above is stored so far. 1 picture is being read by the vision model");
    await expect(pictures.getByRole("heading", { name: "Photo: Receiving SOP (page 1, image 1)" })).toBeVisible();
    await expect(pictures.getByLabel("Extracted text")).toHaveText("FIGURE 1 PRESS 45 s", { timeout: 10_000 });
    await expect(pictures.getByText("Model-extracted text (qwen2.5-vl-7b)", { exact: false })).toBeVisible();
    await expect(page.getByRole("list", { name: "Site documents" }).getByRole("listitem", { name: "Receiving SOP (page 1, image 1)" })).toContainText("model-extracted text (OCR)");
});

// Codex review (WIS-625 post-merge): a stored scan reloads the site's document list in place. Before
// the fix the list was remounted, closing an open authority panel and discarding the owner's reason.
test("stubbed scan completion keeps an open authority review", async ({ page }) => {
    let lists = 0;
    const done = scanJob("completed", { source_id: scanSource, finished_at: "2026-11-06T00:01:00Z" });
    const summary = (id: string, title: string, model_extracted: boolean) => ({ access_epoch: 1, authority: "reported", authority_class: "reported", byte_length: 10,
        independence_group_id: id, kind: "document", revision: 1, rune_length: 10, sha256: "b".repeat(64), source_id: id, state_revision: 1, title, model_extracted });
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor("owner") });
        if (path === `/v1/sites/${site}/sources`) {
            const docs = [summary(sourceID, "Receiving SOP", false)];
            return route.fulfill({ json: { sources: ++lists === 1 ? docs : [...docs, summary(scanSource, "Receiving log", true)] } });
        }
        if (path === `/v1/sites/${site}/sources/${sourceID}`) return route.fulfill({ json: stored.source });
        if (path === `/v1/sites/${site}/source-scans` && route.request().method() === "POST") return route.fulfill({ status: 202, json: scanJob("running") });
        if (path === `/v1/sites/${site}/source-scans/${scanID}`) return route.fulfill({ json: done });
        if (path === `/v1/sites/${site}/sources/${scanSource}`) {
            return route.fulfill({ json: { ...stored.source, source_id: scanSource, title: "Receiving log", text: "RECEIVING LOG",
                extraction: { method: "model_ocr", model: "qwen2.5-vl-7b", model_revision: "sha256:v", prompt_sha256: "d".repeat(64), run_id: "55555555-0000-4000-8000-000000000625" } } });
        }
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    const list = page.getByRole("list", { name: "Site documents" });
    const sop = list.getByRole("listitem", { name: "Receiving SOP" });
    await sop.getByRole("button", { name: "Authority" }).click();
    const panel = sop.getByRole("group", { name: "Authority of Receiving SOP" });
    await panel.getByLabel("Decision reason").fill("Checked against the binder copy");
    await page.getByLabel("File").setInputFiles({ name: "receiving-log.png", mimeType: "image/png", buffer: Buffer.from("\x89PNG\r\n\x1a\n stub") });
    await page.getByLabel("Title").fill("Receiving log");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(list.getByRole("listitem", { name: "Receiving log" })).toBeVisible({ timeout: 10_000 });
    expect(lists).toBeGreaterThanOrEqual(2);
    await expect(panel).toBeVisible();
    await expect(panel.getByLabel("Decision reason")).toHaveValue("Checked against the binder copy");
});

// Codex round 2 (WIS-625 post-merge): a refresh reloads every page of the list loaded so far. Before
// the fix it reloaded only the first, so an authority panel open on page 2 disappeared.
test("stubbed scan completion keeps an open authority review on page 2", async ({ page }) => {
    const second = "66666666-0000-4000-8000-000000000625";
    let firstPages = 0;
    const done = scanJob("completed", { source_id: scanSource, finished_at: "2026-11-06T00:01:00Z" });
    const summary = (id: string, title: string, model_extracted = false) => ({ access_epoch: 1, authority: "reported", authority_class: "reported", byte_length: 10,
        independence_group_id: id, kind: "document", revision: 1, rune_length: 10, sha256: "b".repeat(64), source_id: id, state_revision: 1, title, model_extracted });
    await page.route("**/v1/**", async (route) => {
        const url = new URL(route.request().url());
        const path = url.pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor("owner") });
        if (path === `/v1/sites/${site}/sources`) {
            if (url.searchParams.get("cursor") === "page-2") {
                const rows = [summary(second, "Coil inspection SOP")];
                return route.fulfill({ json: { sources: firstPages > 1 ? [...rows, summary(scanSource, "Receiving log", true)] : rows } });
            }
            firstPages++;
            return route.fulfill({ json: { sources: [summary(sourceID, "Receiving SOP")], next_cursor: "page-2" } });
        }
        if (path === `/v1/sites/${site}/sources/${second}`) return route.fulfill({ json: { ...stored.source, source_id: second, title: "Coil inspection SOP" } });
        if (path === `/v1/sites/${site}/source-scans` && route.request().method() === "POST") return route.fulfill({ status: 202, json: scanJob("running") });
        if (path === `/v1/sites/${site}/source-scans/${scanID}`) return route.fulfill({ json: done });
        if (path === `/v1/sites/${site}/sources/${scanSource}`) {
            return route.fulfill({ json: { ...stored.source, source_id: scanSource, title: "Receiving log", text: "RECEIVING LOG",
                extraction: { method: "model_ocr", model: "qwen2.5-vl-7b", model_revision: "sha256:v", prompt_sha256: "d".repeat(64), run_id: "55555555-0000-4000-8000-000000000625" } } });
        }
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    const list = page.getByRole("list", { name: "Site documents" });
    await page.getByRole("button", { name: "Load more documents" }).click();
    const coil = list.getByRole("listitem", { name: "Coil inspection SOP" });
    await coil.getByRole("button", { name: "Authority" }).click();
    const panel = coil.getByRole("group", { name: "Authority of Coil inspection SOP" });
    await panel.getByLabel("Decision reason").fill("Matches the controlled copy");
    await page.getByLabel("File").setInputFiles({ name: "receiving-log.png", mimeType: "image/png", buffer: Buffer.from("\x89PNG\r\n\x1a\n stub") });
    await page.getByLabel("Title").fill("Receiving log");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(list.getByRole("listitem", { name: "Receiving log" })).toBeVisible({ timeout: 10_000 });
    await expect(list.getByRole("listitem", { name: "Receiving SOP" })).toBeVisible();
    await expect(panel).toBeVisible();
    await expect(panel.getByLabel("Decision reason")).toHaveValue("Matches the controlled copy");
});

// Codex round 3 (WIS-625 post-merge): rows are in source order, so a new document can push a shown
// row past the pages a refresh reloads. Before the fix that row, with its open panel, disappeared.
test("stubbed scan completion keeps a row pushed past the loaded pages", async ({ page }) => {
    const second = "66666666-0000-4000-8000-000000000625";
    let firstPages = 0;
    const done = scanJob("completed", { source_id: scanSource, finished_at: "2026-11-06T00:01:00Z" });
    const summary = (id: string, title: string, model_extracted = false) => ({ access_epoch: 1, authority: "reported", authority_class: "reported", byte_length: 10,
        independence_group_id: id, kind: "document", revision: 1, rune_length: 10, sha256: "b".repeat(64), source_id: id, state_revision: 1, title, model_extracted });
    await page.route("**/v1/**", async (route) => {
        const url = new URL(route.request().url());
        const path = url.pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor("owner") });
        if (path === `/v1/sites/${site}/sources`) {
            if (url.searchParams.get("cursor") === "page-2") {
                // After the scan is stored, the coil SOP has moved to a third page.
                return route.fulfill({ json: { sources: firstPages > 1 ? [summary(sourceID, "Receiving SOP")] : [summary(second, "Coil inspection SOP")], next_cursor: firstPages > 1 ? "page-3" : undefined } });
            }
            firstPages++;
            return route.fulfill({ json: { sources: firstPages > 1 ? [summary(scanSource, "Receiving log", true)] : [summary(sourceID, "Receiving SOP")], next_cursor: "page-2" } });
        }
        if (path === `/v1/sites/${site}/sources/${second}`) return route.fulfill({ json: { ...stored.source, source_id: second, title: "Coil inspection SOP" } });
        if (path === `/v1/sites/${site}/source-scans` && route.request().method() === "POST") return route.fulfill({ status: 202, json: scanJob("running") });
        if (path === `/v1/sites/${site}/source-scans/${scanID}`) return route.fulfill({ json: done });
        if (path === `/v1/sites/${site}/sources/${scanSource}`) {
            return route.fulfill({ json: { ...stored.source, source_id: scanSource, title: "Receiving log", text: "RECEIVING LOG",
                extraction: { method: "model_ocr", model: "qwen2.5-vl-7b", model_revision: "sha256:v", prompt_sha256: "d".repeat(64), run_id: "55555555-0000-4000-8000-000000000625" } } });
        }
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    const list = page.getByRole("list", { name: "Site documents" });
    await page.getByRole("button", { name: "Load more documents" }).click();
    const coil = list.getByRole("listitem", { name: "Coil inspection SOP" });
    await coil.getByRole("button", { name: "Authority" }).click();
    const panel = coil.getByRole("group", { name: "Authority of Coil inspection SOP" });
    await panel.getByLabel("Decision reason").fill("Matches the controlled copy");
    await page.getByLabel("File").setInputFiles({ name: "receiving-log.png", mimeType: "image/png", buffer: Buffer.from("\x89PNG\r\n\x1a\n stub") });
    await page.getByLabel("Title").fill("Receiving log");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(list.getByRole("listitem", { name: "Receiving log" })).toBeVisible({ timeout: 10_000 });
    await expect(panel).toBeVisible();
    await expect(panel.getByLabel("Decision reason")).toHaveValue("Matches the controlled copy");
});

// Codex round 4 (WIS-625 post-merge): a row a refresh no longer finds is read again by itself and
// kept only while it is still readable. Before the fix a removed or revoked document stayed, with
// its open panel and its text.
test("stubbed scan completion drops a row that is gone, and closes its panel", async ({ page }) => {
    const second = "66666666-0000-4000-8000-000000000625";
    let firstPages = 0;
    let revoked = false;
    const done = scanJob("completed", { source_id: scanSource, finished_at: "2026-11-06T00:01:00Z" });
    const summary = (id: string, title: string, model_extracted = false) => ({ access_epoch: 1, authority: "reported", authority_class: "reported", byte_length: 10,
        independence_group_id: id, kind: "document", revision: 1, rune_length: 10, sha256: "b".repeat(64), source_id: id, state_revision: 1, title, model_extracted });
    await page.route("**/v1/**", async (route) => {
        const url = new URL(route.request().url());
        const path = url.pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor("owner") });
        if (path === `/v1/sites/${site}/sources`) {
            if (url.searchParams.get("cursor") === "page-2") return route.fulfill({ json: { sources: firstPages > 1 ? [] : [summary(second, "Coil inspection SOP")] } });
            firstPages++;
            return route.fulfill({ json: { sources: firstPages > 1 ? [summary(scanSource, "Receiving log", true), summary(sourceID, "Receiving SOP")] : [summary(sourceID, "Receiving SOP")], next_cursor: "page-2" } });
        }
        if (path === `/v1/sites/${site}/sources/${second}`) {
            if (revoked) return route.fulfill(problem(404, "NOT_FOUND"));
            return route.fulfill({ json: { ...stored.source, source_id: second, title: "Coil inspection SOP" } });
        }
        if (path === `/v1/sites/${site}/source-scans` && route.request().method() === "POST") return route.fulfill({ status: 202, json: scanJob("running") });
        if (path === `/v1/sites/${site}/source-scans/${scanID}`) {
            revoked = true; // the coil SOP is revoked while the page is read
            return route.fulfill({ json: done });
        }
        if (path === `/v1/sites/${site}/sources/${scanSource}`) {
            return route.fulfill({ json: { ...stored.source, source_id: scanSource, title: "Receiving log", text: "RECEIVING LOG",
                extraction: { method: "model_ocr", model: "qwen2.5-vl-7b", model_revision: "sha256:v", prompt_sha256: "d".repeat(64), run_id: "55555555-0000-4000-8000-000000000625" } } });
        }
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    const list = page.getByRole("list", { name: "Site documents" });
    await page.getByRole("button", { name: "Load more documents" }).click();
    const coil = list.getByRole("listitem", { name: "Coil inspection SOP" });
    await coil.getByRole("button", { name: "Authority" }).click();
    await expect(coil.getByRole("group", { name: "Authority of Coil inspection SOP" })).toBeVisible();
    await page.getByLabel("File").setInputFiles({ name: "receiving-log.png", mimeType: "image/png", buffer: Buffer.from("\x89PNG\r\n\x1a\n stub") });
    await page.getByLabel("Title").fill("Receiving log");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(list.getByRole("listitem", { name: "Receiving log" })).toBeVisible({ timeout: 10_000 });
    await expect(list.getByRole("listitem", { name: "Coil inspection SOP" })).toHaveCount(0);
    await expect(page.getByRole("group", { name: "Authority of Coil inspection SOP" })).toHaveCount(0);
});

// Codex round 5 (WIS-625 post-merge): a failed re-read that is not a 404 or 403 is not a removal.
// Before the fix a 503 dropped the row, its open panel and the owner's draft.
test("stubbed scan completion keeps a row whose re-read fails for now", async ({ page }) => {
    const second = "66666666-0000-4000-8000-000000000625";
    let firstPages = 0;
    let busy = false;
    const done = scanJob("completed", { source_id: scanSource, finished_at: "2026-11-06T00:01:00Z" });
    const summary = (id: string, title: string, model_extracted = false) => ({ access_epoch: 1, authority: "reported", authority_class: "reported", byte_length: 10,
        independence_group_id: id, kind: "document", revision: 1, rune_length: 10, sha256: "b".repeat(64), source_id: id, state_revision: 1, title, model_extracted });
    await page.route("**/v1/**", async (route) => {
        const url = new URL(route.request().url());
        const path = url.pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor("owner") });
        if (path === `/v1/sites/${site}/sources`) {
            if (url.searchParams.get("cursor") === "page-2") return route.fulfill({ json: { sources: firstPages > 1 ? [] : [summary(second, "Coil inspection SOP")] } });
            firstPages++;
            return route.fulfill({ json: { sources: firstPages > 1 ? [summary(scanSource, "Receiving log", true), summary(sourceID, "Receiving SOP")] : [summary(sourceID, "Receiving SOP")], next_cursor: "page-2" } });
        }
        if (path === `/v1/sites/${site}/sources/${second}`) {
            if (busy) return route.fulfill(problem(503, "SERVICE_UNAVAILABLE"));
            return route.fulfill({ json: { ...stored.source, source_id: second, title: "Coil inspection SOP" } });
        }
        if (path === `/v1/sites/${site}/source-scans` && route.request().method() === "POST") return route.fulfill({ status: 202, json: scanJob("running") });
        if (path === `/v1/sites/${site}/source-scans/${scanID}`) {
            busy = true;
            return route.fulfill({ json: done });
        }
        if (path === `/v1/sites/${site}/sources/${scanSource}`) {
            return route.fulfill({ json: { ...stored.source, source_id: scanSource, title: "Receiving log", text: "RECEIVING LOG",
                extraction: { method: "model_ocr", model: "qwen2.5-vl-7b", model_revision: "sha256:v", prompt_sha256: "d".repeat(64), run_id: "55555555-0000-4000-8000-000000000625" } } });
        }
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/documents`);
    const list = page.getByRole("list", { name: "Site documents" });
    await page.getByRole("button", { name: "Load more documents" }).click();
    const coil = list.getByRole("listitem", { name: "Coil inspection SOP" });
    await coil.getByRole("button", { name: "Authority" }).click();
    const panel = coil.getByRole("group", { name: "Authority of Coil inspection SOP" });
    await panel.getByLabel("Decision reason").fill("Matches the controlled copy");
    await page.getByLabel("File").setInputFiles({ name: "receiving-log.png", mimeType: "image/png", buffer: Buffer.from("\x89PNG\r\n\x1a\n stub") });
    await page.getByLabel("Title").fill("Receiving log");
    await page.getByRole("button", { name: "Upload" }).click();
    await expect(list.getByRole("listitem", { name: "Receiving log" })).toBeVisible({ timeout: 10_000 });
    await expect(coil.getByText("could not be checked just now", { exact: false })).toBeVisible();
    await expect(panel).toBeVisible();
    await expect(panel.getByLabel("Decision reason")).toHaveValue("Matches the controlled copy");
});
