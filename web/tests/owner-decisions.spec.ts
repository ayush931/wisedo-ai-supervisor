import { expect, test, type Page, type Request, type Route } from "@playwright/test";
import { appRegions } from "./regions";

// WIS-641 slice B: the owner marks a document authoritative and confirms it current on Documents,
// and an owner or supervisor publishes a snapshot on Approved process. Every test stubs the API (UI
// behaviour: what is sent, what is shown only from a receipt, how 412/409/428 and a lost connection
// are handled). The server's role checks, revision preconditions and replay are covered by the Go
// suites; the real owner flow is web/tests/onboarding-pilot.spec.ts.

const site = "00000000-0000-4000-8000-000000000641";
const doc = "aaaaaaaa-0000-4000-8000-000000000641";
const decision = "bbbbbbbb-0000-4000-8000-000000000641";
const verification = "cccccccc-0000-4000-8000-000000000641";
const meFor = (role: string) => ({
    csrf_token: "test-csrf",
    display_name: "Stubbed member",
    subject: "member-subject",
    sites: [{ id: site, name: "Stubbed Site", role, tenant_id: "00000000-0000-4000-8000-000000000640", agent_available: true }],
});
const problem = (status: number, code: string) => ({ status, contentType: "application/problem+json", body: JSON.stringify({ code, status, title: code, type: "about:blank" }) });
const source = (state = 1) => ({
    access_epoch: 1, authority: "site", authority_class: "document", byte_length: 10, created_at: "2026-11-05T00:00:00Z", current_revision: 1,
    independence_group_id: "g", kind: "document", revision: 1, rune_length: 10, sha256: "x", source_id: doc, state_revision: state, text: "Press SOP", title: "Press SOP",
});
const authority = (revision: number, verdict = "designate") => ({
    actor_issuer: "i", actor_subject: "member-subject", authenticated_at: "2026-11-05T00:00:00Z", created_at: "2026-11-05T00:00:00Z", current_revision: revision,
    decision_id: decision, reason: "Owner verified", review_evidence_eligible: verdict === "designate", revision, source_access_epoch: 1, source_id: doc,
    source_revision: 1, source_sha256: "x", verdict,
});
const currentness = () => ({
    actor_issuer: "i", actor_subject: "member-subject", authenticated_at: "2026-11-05T00:00:00Z", authority_decision_id: decision, authority_revision: 1,
    created_at: "2026-11-05T00:00:00Z", current_revision: 1, reason: "Checked", revision: 1, schema_version: 1, source_access_epoch: 1, source_id: doc,
    source_revision: 1, source_sha256: "x", verdict: "confirm_current", verification_current: true, verification_id: verification, verified_at: "2026-11-05T00:00:00Z",
});
type Reply = { status?: number; json?: unknown; contentType?: string; body?: string } | "abort";
type Handler = (r: Request) => Reply | undefined | Promise<Reply | undefined>;

// stub answers /v1/me and the given handler; anything else is a non-disclosing 404.
async function stub(page: Page, role: string, handler: Handler) {
    await page.route("**/v1/**", async (route: Route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor(role) });
        const reply = await handler(route.request());
        if (reply === "abort") return route.abort("connectionreset");
        return route.fulfill(reply ?? problem(404, "NOT_FOUND"));
    });
}
const at = (r: Request, suffix: string, method = "GET") => r.method() === method && new URL(r.url()).pathname === `/v1/sites/${site}${suffix}`;

// documents: a document list with one SOP and its authority and currentness heads, as given.
function documents(state: { authority: ReturnType<typeof authority> | null; currentness: ReturnType<typeof currentness> | null; source?: ReturnType<typeof source> }, post: Handler): Handler {
    return (r) => {
        if (at(r, "/sources")) return { json: { sources: [{ ...source(), text: undefined, created_at: undefined, current_revision: undefined }] } };
        if (at(r, `/sources/${doc}`)) return { json: state.source ?? source() };
        if (at(r, `/sources/${doc}/authority`)) return state.authority ? { json: state.authority } : problem(404, "NOT_FOUND");
        if (at(r, `/sources/${doc}/currentness`)) return state.currentness ? { json: state.currentness } : problem(404, "NOT_FOUND");
        if (r.method() === "POST") return post(r);
        return undefined;
    };
}
async function openAuthority(page: Page) {
    await page.goto(`/sites/${site}/documents`);
    const row = page.getByRole("list", { name: "Site documents" }).getByRole("listitem", { name: "Press SOP" });
    await row.getByRole("button", { name: "Authority" }).click();
    const panel = row.getByRole("group", { name: "Authority of Press SOP" });
    await expect(panel).toBeVisible();
    return panel;
}

test("stubbed owner marks a document authoritative, then confirms it current, each shown only from the receipt", async ({ page }) => {
    const state = { authority: null as ReturnType<typeof authority> | null, currentness: null as ReturnType<typeof currentness> | null };
    const posts: Request[] = [];
    await stub(page, "owner", documents(state, (r) => {
        posts.push(r);
        if (at(r, `/sources/${doc}/authority-decisions`, "POST")) {
            state.authority = authority(1);
            return { json: state.authority };
        }
        if (at(r, `/sources/${doc}/currentness-decisions`, "POST")) {
            state.currentness = currentness();
            return { json: state.currentness };
        }
        return undefined;
    }));
    const panel = await openAuthority(page);
    await expect(panel).toContainText("Not authoritative");
    await expect(panel.getByRole("button", { name: "Confirm current" })).toHaveCount(0);
    await panel.getByLabel("Decision reason").fill("Owner verified");
    await panel.getByRole("button", { name: "Mark authoritative" }).click();
    await expect(panel.getByRole("status").filter({ hasText: /\S/ })).toHaveText("Marked authoritative. Decision bbbbbbbb recorded.");
    await expect(panel.getByText("Authoritative", { exact: true })).toBeVisible();
    expect(JSON.parse(posts[0].postData() ?? "{}")).toEqual({ source_revision: 1, expected_source_state_revision: 1, verdict: "designate", reason: "Owner verified" });
    expect(posts[0].headers()["if-match"]).toBe('"0"');

    await panel.getByLabel("Decision reason").fill("Checked against the shop floor copy");
    await panel.getByRole("button", { name: "Confirm current" }).click();
    await expect(panel.getByRole("status").filter({ hasText: /\S/ })).toHaveText("Confirmed current. Decision cccccccc recorded.");
    await expect(panel.getByText("Confirmed current", { exact: true })).toBeVisible();
    await expect(panel.getByRole("button", { name: "Confirm current" })).toHaveCount(0);
    expect(JSON.parse(posts[1].postData() ?? "{}")).toEqual({
        source_revision: 1, expected_source_state_revision: 1, authority_revision: 1, authority_decision_id: decision, verdict: "confirm_current", reason: "Checked against the shop floor copy",
    });
    expect(posts[1].headers()["if-match"]).toBe('"0"');
    expect(posts).toHaveLength(2);
});

test("stubbed 412 reloads the authority head; nothing is resubmitted and the next decision carries the new If-Match", async ({ page }) => {
    const state = { authority: null as ReturnType<typeof authority> | null, currentness: null };
    const posts: Request[] = [];
    await stub(page, "owner", documents(state, (r) => {
        posts.push(r);
        if (posts.length === 1) {
            state.authority = authority(1, "withdraw"); // another owner decided meanwhile
            return problem(412, "REVISION_CONFLICT");
        }
        state.authority = authority(2);
        return { json: state.authority };
    }));
    const panel = await openAuthority(page);
    await panel.getByLabel("Decision reason").fill("Owner verified");
    await panel.getByRole("button", { name: "Mark authoritative" }).click();
    await expect(panel.getByRole("alert").filter({ hasText: /\S/ })).toContainText("This changed on the server.");
    await expect(panel).toContainText("withdrawn");
    expect(posts).toHaveLength(1);
    await panel.getByRole("button", { name: "Mark authoritative" }).click();
    await expect(panel.getByRole("status").filter({ hasText: /\S/ })).toHaveText("Marked authoritative. Decision bbbbbbbb recorded.");
    expect(posts[1].headers()["if-match"]).toBe('"1"');
    expect(posts[1].headers()["idempotency-key"]).not.toBe(posts[0].headers()["idempotency-key"]);
});

test("stubbed 409 SOURCE_CHANGED rereads the source and the next decision binds its new state revision", async ({ page }) => {
    const state = { authority: null as ReturnType<typeof authority> | null, currentness: null, source: source(1) };
    const posts: Request[] = [];
    await stub(page, "owner", documents(state, (r) => {
        posts.push(r);
        if (posts.length === 1) {
            state.source = source(2);
            return problem(409, "SOURCE_CHANGED");
        }
        state.authority = authority(1);
        return { json: state.authority };
    }));
    const panel = await openAuthority(page);
    await panel.getByLabel("Decision reason").fill("Owner verified");
    await panel.getByRole("button", { name: "Mark authoritative" }).click();
    await expect(panel.getByRole("alert").filter({ hasText: /\S/ })).toContainText("The document or its authority changed since it was read.");
    await panel.getByRole("button", { name: "Mark authoritative" }).click();
    await expect(panel.getByRole("status").filter({ hasText: /\S/ })).toHaveText("Marked authoritative. Decision bbbbbbbb recorded.");
    expect(JSON.parse(posts[1].postData() ?? "{}").expected_source_state_revision).toBe(2);
});

test("stubbed 428 asks the owner to confirm identity and records nothing", async ({ page }) => {
    await stub(page, "owner", documents({ authority: null, currentness: null }, () => problem(428, "REAUTHENTICATION_REQUIRED")));
    const panel = await openAuthority(page);
    await panel.getByLabel("Decision reason").fill("Owner verified");
    await panel.getByRole("button", { name: "Mark authoritative" }).click();
    await expect(panel.getByRole("alert").filter({ hasText: /\S/ })).toContainText("Confirm your identity to continue. Nothing was submitted.");
    await expect(panel.getByRole("button", { name: "Confirm identity" })).toBeVisible();
    await expect(appRegions(panel, "status")).toHaveCount(0);
    await expect(panel).toContainText("Not authoritative");
});

for (const role of ["supervisor", "operator", "viewer"]) {
    test(`stubbed ${role} reads a document's authority but gets no owner decisions`, async ({ page }) => {
        await stub(page, role, documents({ authority: authority(1), currentness: null }, () => undefined));
        const panel = await openAuthority(page);
        await expect(panel.getByText("Authoritative", { exact: true })).toBeVisible();
        await expect(panel).toContainText("Only the site owner marks a document authoritative");
        await expect(panel.getByRole("button")).toHaveCount(0);
    });
}

// The Approved process page: the publish form sits above the process picker.
function graph(post: Handler, revisions: number[]): Handler {
    let reads = 0;
    return (r) => {
        if (at(r, "/discovery-inventories")) return { json: { inventories: [] } };
        if (at(r, "/knowledge-inventory")) return { json: { revision: revisions[Math.min(reads++, revisions.length - 1)], schema_version: 1 } };
        if (at(r, "/process-snapshots", "POST")) return post(r);
        return undefined;
    };
}
const snapshot = (id: string, revision: number) => ({
    actor_issuer: "i", actor_subject: "member-subject", authenticated_at: "2026-11-05T00:00:00Z", contents: { claims: [], entities: [] }, created_at: "2026-11-05T00:00:00Z",
    current_inventory_revision: revision, inventory_revision: revision, reason: "Reviewed", schema_version: 1, snapshot_id: id, stale: false,
});
const publishForm = (page: Page) => page.getByRole("form", { name: "Publish approved knowledge" });

test("stubbed supervisor publishes a snapshot against the knowledge revision just read; a lost connection retries with the same key and snapshot ID", async ({ page }) => {
    const posts: Request[] = [];
    await stub(page, "supervisor", graph((r) => {
        posts.push(r);
        if (posts.length === 1) return "abort";
        const body = JSON.parse(r.postData() ?? "{}");
        return { json: snapshot(body.snapshot_id, 7) };
    }, [7, 8]));
    await page.goto(`/sites/${site}/process`);
    const form = publishForm(page);
    await expect(form).toContainText("Knowledge revision 7.");
    await form.getByLabel("Snapshot reason").fill("Reviewed");
    await form.getByRole("button", { name: "Publish snapshot" }).click();
    await expect(form.getByRole("alert").filter({ hasText: /\S/ })).toContainText("Connection lost before the server answered.");
    await expect(appRegions(form, "status")).toHaveCount(0);
    await form.getByRole("button", { name: "Publish snapshot" }).click();
    const id = JSON.parse(posts[0].postData() ?? "{}").snapshot_id as string;
    await expect(form.getByRole("status").filter({ hasText: /\S/ })).toHaveText(`Snapshot ${id.slice(0, 8)} published at knowledge revision 7.`);
    expect(posts).toHaveLength(2);
    expect(posts[1].postData()).toBe(posts[0].postData());
    expect(posts[1].headers()["idempotency-key"]).toBe(posts[0].headers()["idempotency-key"]);
    expect(posts.map((p) => p.headers()["if-match"])).toEqual(['"7"', '"7"']);
    await expect(form).toContainText("Knowledge revision 8.");
});

test("stubbed 412 reloads the knowledge revision; the next publish carries it with a new snapshot ID", async ({ page }) => {
    const posts: Request[] = [];
    await stub(page, "owner", graph((r) => {
        posts.push(r);
        if (posts.length === 1) return problem(412, "REVISION_CONFLICT");
        return { json: snapshot(JSON.parse(r.postData() ?? "{}").snapshot_id, 9) };
    }, [7, 9]));
    await page.goto(`/sites/${site}/process`);
    const form = publishForm(page);
    await expect(form).toContainText("Knowledge revision 7.");
    await form.getByLabel("Snapshot reason").fill("Reviewed");
    await form.getByRole("button", { name: "Publish snapshot" }).click();
    await expect(form.getByRole("alert").filter({ hasText: /\S/ })).toContainText("This changed on the server.");
    await expect(form).toContainText("Knowledge revision 9.");
    await form.getByRole("button", { name: "Publish snapshot" }).click();
    await expect(form.getByRole("status").filter({ hasText: /\S/ })).toContainText("published at knowledge revision 9.");
    expect(posts.map((p) => p.headers()["if-match"])).toEqual(['"7"', '"9"']);
    expect(JSON.parse(posts[1].postData() ?? "{}").snapshot_id).not.toBe(JSON.parse(posts[0].postData() ?? "{}").snapshot_id);
});

test("stubbed 428 on publish asks to confirm identity and shows no snapshot", async ({ page }) => {
    await stub(page, "owner", graph(() => problem(428, "REAUTHENTICATION_REQUIRED"), [7]));
    await page.goto(`/sites/${site}/process`);
    const form = publishForm(page);
    await form.getByLabel("Snapshot reason").fill("Reviewed");
    await form.getByRole("button", { name: "Publish snapshot" }).click();
    await expect(form.getByRole("button", { name: "Confirm identity" })).toBeVisible();
    await expect(appRegions(form, "status")).toHaveCount(0);
});

for (const role of ["operator", "viewer"]) {
    test(`stubbed ${role} gets no publish form`, async ({ page }) => {
        const reads: Request[] = [];
        await stub(page, role, (r) => {
            if (at(r, "/knowledge-inventory")) reads.push(r);
            return graph(() => undefined, [7])(r);
        });
        await page.goto(`/sites/${site}/process`);
        await expect(page.getByText("An owner or supervisor publishes approved knowledge as a snapshot.")).toBeVisible();
        await expect(publishForm(page)).toHaveCount(0);
        expect(reads).toHaveLength(0);
    });
}

// Review of #244 (WIS-641 / WIS-625): since #254 the server refuses to designate a model-extracted
// (OCR) source without confirm_model_extracted, so the plain action could only fail. The panel marks
// such a source and designates it only after the owner ticks an explicit check.
const ocr = () => ({ ...source(), extraction: { method: "ocr", model: "vision-ocr", model_revision: "1", prompt_sha256: "p", run_id: "r" } });

test("stubbed OCR source shows the model-read marker and offers no plain designation", async ({ page }) => {
    const posts: Request[] = [];
    await stub(page, "owner", documents({ authority: null, currentness: null, source: ocr() }, (r) => {
        posts.push(r);
        return problem(409, "MODEL_EXTRACTED_UNCONFIRMED"); // the server's status (httpapi/evidence.go)
    }));
    const panel = await openAuthority(page);
    await expect(panel.getByRole("note")).toContainText("Text read by a model");
    await expect(panel.getByRole("note")).toContainText("lower authority");
    await panel.getByLabel("Decision reason").fill("Owner verified");
    await expect(panel.getByRole("button", { name: "Mark authoritative" })).toBeDisabled();
    await panel.getByLabel("Decision reason").press("Enter");
    await expect(appRegions(panel, "status")).toHaveCount(0);
    expect(posts).toHaveLength(0);
});

test("stubbed OCR source is designated with confirm_model_extracted only after the explicit check, shown from the receipt", async ({ page }) => {
    const state = { authority: null as ReturnType<typeof authority> | null, currentness: null, source: ocr() };
    const posts: Request[] = [];
    let release = () => {};
    const held = new Promise<void>((resolve) => (release = resolve));
    await stub(page, "owner", documents(state, async (r) => {
        posts.push(r);
        await held;
        state.authority = authority(1);
        return { json: state.authority };
    }));
    const panel = await openAuthority(page);
    // The owner can read the model's text, with its revision and model, before confirming the comparison.
    const text = panel.getByLabel("Model-read text of Press SOP");
    await expect(text).toHaveText("Press SOP");
    await expect(panel).toContainText("Text of revision 1 as read by vision-ocr");
    await expect(panel.getByText(/as if it were the original/)).toHaveCount(0);
    await panel.getByLabel("Decision reason").fill("Checked against the scan");
    await panel.getByRole("checkbox", { name: /I compared this text with the original page/ }).check();
    await panel.getByRole("button", { name: "Mark authoritative" }).click();
    await expect.poll(() => posts.length).toBe(1);
    await expect(appRegions(panel, "status")).toHaveCount(0); // nothing shown while the receipt is pending
    release();
    await expect(panel.getByRole("status").filter({ hasText: /\S/ })).toHaveText("Marked authoritative. Decision bbbbbbbb recorded.");
    await expect(panel.getByRole("note")).toContainText("Text read by a model"); // provenance stays after designation
    expect(JSON.parse(posts[0].postData() ?? "{}")).toEqual({
        source_revision: 1, expected_source_state_revision: 1, verdict: "designate", reason: "Checked against the scan", confirm_model_extracted: true,
    });
    expect(posts).toHaveLength(1);
});

// WIS-643: the server refuses a plain designation of model-extracted text with 409
// MODEL_EXTRACTED_UNCONFIRMED, for example when the document was replaced by a scan after the panel
// read it. The panel explains why, shows nothing as decided, and rereads the source, which now asks
// for the explicit comparison.
test("stubbed 409 MODEL_EXTRACTED_UNCONFIRMED is explained and the panel rereads the source as model-read", async ({ page }) => {
    const state = { authority: null as ReturnType<typeof authority> | null, currentness: null, source: source() };
    const posts: Request[] = [];
    await stub(page, "owner", documents(state, (r) => {
        posts.push(r);
        state.source = ocr(); // a scan replaced the typed text after the panel read it
        return problem(409, "MODEL_EXTRACTED_UNCONFIRMED");
    }));
    const panel = await openAuthority(page);
    await expect(panel.getByRole("note")).toHaveCount(0);
    await panel.getByLabel("Decision reason").fill("Owner verified");
    await panel.getByRole("button", { name: "Mark authoritative" }).click();
    await expect(panel.getByRole("alert").filter({ hasText: /\S/ })).toContainText("Check it against the original page, then confirm that you did before marking it authoritative.");
    await expect(appRegions(panel, "status")).toHaveCount(0);
    await expect(panel.getByText("Not authoritative", { exact: true })).toBeVisible();
    // The reread shows the model-read text and the check; nothing is resent without it.
    await expect(panel.getByRole("note")).toContainText("Text read by a model");
    await expect(panel.getByRole("checkbox", { name: /I compared this text with the original page/ })).not.toBeChecked();
    expect(posts).toHaveLength(1);
    expect(JSON.parse(posts[0].postData() ?? "{}")).not.toHaveProperty("confirm_model_extracted");
});

test("stubbed typed source shows no model-read marker or check", async ({ page }) => {
    await stub(page, "owner", documents({ authority: null, currentness: null }, () => undefined));
    const panel = await openAuthority(page);
    await expect(panel.getByRole("note")).toHaveCount(0);
    await expect(panel.getByRole("checkbox")).toHaveCount(0);
    await panel.getByLabel("Decision reason").fill("Owner verified");
    await expect(panel.getByRole("button", { name: "Mark authoritative" })).toBeEnabled();
});

// WIS-625 post-merge review: the owner's check that the model's text matches the page belongs to
// the revision they read. A 409 SOURCE_CHANGED reloads revision 2; before the fix the check and
// reason stayed set, so one more click designated revision 2 without anyone comparing it.
test("stubbed OCR check is cleared when SOURCE_CHANGED reloads a newer revision", async ({ page }) => {
    const state = { authority: null as ReturnType<typeof authority> | null, currentness: null, source: ocr() };
    const posts: Request[] = [];
    await stub(page, "owner", documents(state, (r) => {
        posts.push(r);
        state.source = { ...ocr(), revision: 2, current_revision: 2, state_revision: 2, text: "Press SOP v2" };
        return problem(409, "SOURCE_CHANGED");
    }));
    const panel = await openAuthority(page);
    await panel.getByLabel("Decision reason").fill("Checked against the scan");
    const check = panel.getByRole("checkbox", { name: /I compared this text with the original page/ });
    await check.check();
    await panel.getByRole("button", { name: "Mark authoritative" }).click();
    await expect(panel).toContainText("Text of revision 2 as read by vision-ocr");
    await expect(panel.getByLabel("Model-read text of Press SOP")).toHaveText("Press SOP v2");
    await expect(check).not.toBeChecked();
    await expect(panel.getByRole("button", { name: "Mark authoritative" })).toBeDisabled();
    await panel.getByLabel("Decision reason").press("Enter");
    expect(posts).toHaveLength(1);
});

// WIS-625 post-merge review: a 2xx whose body is lost (the connection drops while it is read)
// committed on the server, so its outcome is unknown, not a fresh start. Before the fix the
// failed read discarded the Idempotency-Key and snapshot ID, and the retry published a second
// snapshot.
test("stubbed publish whose response body is lost retries with the same key and snapshot ID", async ({ page }) => {
    const posts: Request[] = [];
    await stub(page, "supervisor", graph((r) => {
        posts.push(r);
        if (posts.length === 1) return { status: 201, contentType: "application/json", body: "{" };
        return { json: snapshot(JSON.parse(r.postData() ?? "{}").snapshot_id, 7) };
    }, [7, 8]));
    await page.goto(`/sites/${site}/process`);
    const form = publishForm(page);
    await expect(form).toContainText("Knowledge revision 7.");
    await form.getByLabel("Snapshot reason").fill("Reviewed");
    await form.getByRole("button", { name: "Publish snapshot" }).click();
    await expect(form.getByRole("alert").filter({ hasText: /\S/ })).toContainText("Connection lost before the server answered.");
    await form.getByRole("button", { name: "Publish snapshot" }).click();
    await expect(form.getByRole("status").filter({ hasText: /\S/ })).toContainText("published at knowledge revision 7.");
    expect(posts).toHaveLength(2);
    expect(posts[1].postData()).toBe(posts[0].postData());
    expect(posts[1].headers()["idempotency-key"]).toBe(posts[0].headers()["idempotency-key"]);
});
