import { expect, test, type Browser, type Page } from "@playwright/test";
import { openSite, section, signIn } from "./session";

// Proposal F configuration drafts, slice 4 UI, against the real backend. Fixture users (demo
// seed): shift-lead and shift-approver are supervisors, foundation-reviewer is the owner,
// read-only-viewer is a viewer. Every test seeds its own drafts.

const env = (globalThis as typeof globalThis & { process?: { env?: Record<string, string | undefined> } }).process?.env ?? {};

type Reply = { status: number; body: any };

// call performs one same-origin API call as the page's signed-in user (site from /v1/me).
async function call(page: Page, method: string, path: string, body?: unknown, ifMatch?: number, key?: string): Promise<Reply> {
    return page.evaluate(async ({ method, path, body, ifMatch, key }) => {
        const me = await (await fetch("/v1/me")).json();
        const headers: Record<string, string> = {};
        if (method !== "GET") {
            headers["Content-Type"] = "application/json";
            headers["X-CSRF-Token"] = me.csrf_token;
            headers["Idempotency-Key"] = key ?? crypto.randomUUID();
            if (ifMatch !== undefined) headers["If-Match"] = `"${ifMatch}"`;
        }
        const r = await fetch(`/v1/sites/${me.sites[0].id}${path}`, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
        return { status: r.status, body: await r.json().catch(() => null) };
    }, { method, path, body, ifMatch, key });
}

async function ok(reply: Promise<Reply>, status: number, what: string) {
    const r = await reply;
    if (r.status !== status) throw new Error(`${what}: expected ${status}, got ${r.status} ${r.body?.code ?? ""}`);
    return r.body;
}

// cite creates an evidence source and a span over `quote` inside it; returns ids for citations.
async function cite(page: Page, quote: string) {
    const sourceID = crypto.randomUUID();
    const text = `Interview note. ${quote}. End of note.`;
    const source = await ok(call(page, "POST", "/sources", { source_id: sourceID, kind: "manual", title: "E2E draft evidence", text }, 0), 200, "create source");
    const start = text.indexOf(quote);
    const span = await ok(call(page, "POST", `/sources/${sourceID}/spans`, { revision: source.revision, start_char: start, end_char: start + quote.length }, source.state_revision), 200, "create span");
    return { sourceID, spanID: span.id as string };
}

async function lineDraft(page: Page, name: string, spans: { path: string; span_id: string }[] = [], target?: { id: string; revision: number }) {
    return ok(call(page, "POST", "/configuration-drafts", {
        kind: "line",
        ...(target ? { target_object_id: target.id, target_expected_revision: target.revision } : {}),
        body: { schema_version: 1, display_name: name, active: true },
        field_spans: spans,
    }), 201, "create draft") as Promise<{ draft_id: string; current_revision: number }>;
}

async function as(browser: Browser, user: string) {
    const context = await browser.newContext();
    const page = await context.newPage();
    await signIn(page, user);
    await openSite(page);
    return page;
}

async function openDraft(page: Page, id: string) {
    await section(page, "Master data");
    await page.getByRole("button", { name: /^Drafts/ }).click();
    await page.getByRole("button", { name: `Open draft ${id.slice(0, 8)}` }).click();
    await expect(page.getByRole("heading", { name: new RegExp(`draft ${id.slice(0, 8)}`) })).toBeVisible();
}

// tabTo presses Tab until the focused element's accessible text matches; keyboard only.
async function tabTo(page: Page, name: RegExp) {
    for (let i = 0; i < 200; i++) {
        const text = await page.evaluate(() => {
            const el = document.activeElement as HTMLElement | null;
            return el ? (el.getAttribute("aria-label") ?? el.innerText ?? "").trim() : "";
        });
        if (name.test(text)) return;
        await page.keyboard.press("Tab");
    }
    throw new Error(`Keyboard focus never reached ${name}`);
}

const suffix = () => `${Date.now()}-${Math.floor(Math.random() * 1e6)}`;

test("a second supervisor promotes a cited draft with the keyboard only", async ({ browser }) => {
    const lead = await as(browser, "shift-lead");
    const name = `E2E draft line ${suffix()}`;
    const { spanID } = await cite(lead, name);
    const draft = await lineDraft(lead, name, [{ path: "/display_name", span_id: spanID }]);

    const page = await as(browser, "shift-approver");
    await tabTo(page, /^Setup$/);
    await page.keyboard.press("Enter");
    await tabTo(page, /Master data$/);
    await page.keyboard.press("Enter");
    await tabTo(page, /^Drafts/);
    await page.keyboard.press("Enter");
    await tabTo(page, new RegExp(`^Open draft ${draft.draft_id.slice(0, 8)}$`));
    await page.keyboard.press("Enter");
    await expect(page.getByText(/Cited · source/)).toBeVisible();
    await tabTo(page, /^Promote r1$/);
    await page.keyboard.press("Enter");
    const dialog = page.getByRole("dialog", { name: "Promote line draft r1?" });
    await expect(dialog).toBeVisible();
    expect(await page.evaluate(() => !!document.activeElement?.closest("dialog"))).toBe(true);
    // Negative control: Escape closes the dialog and nothing is promoted.
    await page.keyboard.press("Escape");
    await expect(dialog).toBeHidden();
    await expect(page.getByRole("button", { name: /^Promote r1/ })).toBeFocused();
    expect((await ok(call(page, "GET", `/configuration-drafts/${draft.draft_id}`), 200, "read draft")).status).toBe("draft");

    await tabTo(page, /^Promote r1$/);
    await page.keyboard.press("Enter");
    await expect(dialog).toBeVisible();
    await tabTo(page, /^Confirm promotion$/);
    await page.keyboard.press("Enter");
    await expect(page.getByRole("status").filter({ hasText: "Promoted as line r1" })).toBeVisible();
    const promoted = await ok(call(page, "GET", `/configuration-drafts/${draft.draft_id}`), 200, "read draft");
    expect(promoted.status).toBe("promoted");
    const line = await ok(call(page, "GET", `/lines/${promoted.promotion.object_id}`), 200, "read line");
    expect(line.current_revision).toBe(1);
    expect(line.revisions[0].body.display_name).toBe(name);
    await tabTo(page, /^Open line$/);
    await page.keyboard.press("Enter");
    await expect(page.getByRole("table", { name: "Lines master data" }).getByText(name)).toBeVisible();
});

test("an editor sees why promotion is disabled; dropped and revoked citations are shown", async ({ browser }) => {
    const lead = await as(browser, "shift-lead");
    const name = `E2E cited line ${suffix()}`;
    const shown = await cite(lead, name);
    const activeSource = await cite(lead, "The line is active");
    const draft = await lineDraft(lead, name, [
        { path: "/display_name", span_id: shown.spanID },
        { path: "/active", span_id: activeSource.spanID },
    ]);

    await openDraft(lead, draft.draft_id);
    await expect(lead.getByRole("button", { name: /^Promote r1/ })).toBeDisabled();
    await expect(lead.getByRole("button", { name: /^Promote r1/ })).toContainText("You created this draft; another supervisor must promote it.");
    // Editing the cited name drops its citation (FIELD_CHANGED); the /active citation carries over.
    await lead.getByLabel("display name").fill(`${name} edited`);
    await lead.getByRole("button", { name: "Save r2" }).click();
    await expect(lead.getByRole("status").filter({ hasText: "Saved r2" })).toBeVisible();
    await expect(lead.getByRole("note", { name: "Dropped citations" })).toContainText("display name: Field changed; citation dropped");
    // The server, not the button, is the boundary: the author is refused.
    const refused = await call(lead, "POST", `/configuration-drafts/${draft.draft_id}/promotions`, {}, 2);
    expect([refused.status, refused.body?.code]).toEqual([403, "PROMOTER_IS_EDITOR"]);

    const owner = await as(browser, "foundation-reviewer");
    const head = await ok(call(owner, "GET", `/sources/${activeSource.sourceID}`), 200, "read source");
    await ok(call(owner, "POST", `/sources/${activeSource.sourceID}/revocations`, { reason: "e2e revoke" }, head.state_revision), 200, "revoke source");

    const approver = await as(browser, "shift-approver");
    await openDraft(approver, draft.draft_id);
    await expect(approver.getByText(/Source revoked · source/)).toBeVisible();
    await expect(approver.getByText("1 unavailable").first()).toBeVisible();
    const promote = approver.getByRole("button", { name: /^Promote r2/ });
    await expect(promote).toBeDisabled();
    await expect(promote).toContainText("1 citation unavailable.");
    const unavailable = await call(approver, "POST", `/configuration-drafts/${draft.draft_id}/promotions`, {}, 2);
    expect([unavailable.status, unavailable.body?.code]).toEqual([409, "DRAFT_SOURCE_UNAVAILABLE"]);
});

test("a stale draft revision shows compare and reload", async ({ browser }) => {
    const lead = await as(browser, "shift-lead");
    const name = `E2E stale line ${suffix()}`;
    const draft = await lineDraft(lead, name);
    const page = await as(browser, "shift-approver");
    await openDraft(page, draft.draft_id);
    // Someone else saves r2 while this page still shows r1.
    await ok(call(lead, "POST", `/configuration-drafts/${draft.draft_id}/revisions`, { body: { schema_version: 1, display_name: `${name} server`, active: true }, field_spans: [] }, 1), 201, "revise draft");

    await page.getByLabel("display name").fill(`${name} mine`);
    await page.getByRole("button", { name: "Save r2" }).click();
    const compare = page.getByRole("table", { name: "Your edits compared with the server revision" });
    await expect(compare).toBeVisible();
    await expect(compare.getByRole("row", { name: /display name/ })).toContainText(`${name} mine`);
    await expect(compare.getByRole("row", { name: /display name/ })).toContainText(`${name} server`);
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("REVISION_CONFLICT");
    await page.getByRole("button", { name: "Reload r2" }).click();
    await expect(page.getByLabel("display name")).toHaveValue(`${name} server`);
    await expect(compare).toBeHidden();
    // After reload the next save is accepted against r2.
    await page.getByLabel("display name").fill(`${name} mine`);
    await page.getByRole("button", { name: "Save r3" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Saved r3" })).toBeVisible();
    const saved = await ok(call(page, "GET", `/configuration-drafts/${draft.draft_id}`), 200, "read draft");
    expect(saved.revisions[2].body.display_name).toBe(`${name} mine`);
});

test("a draft whose live object moved tells the user to discard and recreate it", async ({ browser }) => {
    const lead = await as(browser, "shift-lead");
    const name = `E2E target line ${suffix()}`;
    const line = await ok(call(lead, "POST", "/lines", { schema_version: 1, display_name: name, active: true }, 0), 201, "create line");
    const draft = await lineDraft(lead, `${name} drafted`, [], { id: line.id, revision: 1 });
    await ok(call(lead, "POST", `/lines/${line.id}/revisions`, { schema_version: 1, display_name: `${name} moved`, active: true }, 1), 201, "revise line");

    const page = await as(browser, "shift-approver");
    await openDraft(page, draft.draft_id);
    await page.getByRole("button", { name: "Promote r1" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText("The live object is now r2");
    await dialog.getByRole("button", { name: "Confirm promotion" }).click();
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("The live configuration changed after this draft was written. Discard this draft and create a new one.");
    const after = await ok(call(page, "GET", `/lines/${line.id}`), 200, "read line");
    expect(after.current_revision).toBe(2);
    expect((await ok(call(page, "GET", `/configuration-drafts/${draft.draft_id}`), 200, "read draft")).status).toBe("draft");
});

// The server's recent-authentication window (default 15 min) cannot be aged inside one browser
// test, so this case needs the API started with SUPERVISOR_SESSION_RECENT_AUTH equal to
// SUPERVISOR_E2E_RECENT_AUTH_SECONDS (at most 60s). Without it the case is BLOCKED, never skipped.
test("promotion after the recent-authentication window asks for identity, then succeeds @recent-auth", async ({ browser }) => {
    const window = Number(env.SUPERVISOR_E2E_RECENT_AUTH_SECONDS);
    if (!(window > 0 && window <= 60))
        throw new Error("BLOCKED: set SUPERVISOR_E2E_RECENT_AUTH_SECONDS (<=60) and start the API with SUPERVISOR_SESSION_RECENT_AUTH=<same>s");
    test.setTimeout((window * 2 + 90) * 1000);
    const password = env.SUPERVISOR_LOCAL_USER_PASSWORD;
    if (!password) throw new Error("BLOCKED: SUPERVISOR_LOCAL_USER_PASSWORD required");
    const lead = await as(browser, "shift-lead");
    const draft = await lineDraft(lead, `E2E reauth line ${suffix()}`);
    const page = await as(browser, "shift-approver");
    await openDraft(page, draft.draft_id);
    await page.waitForTimeout((window + 2) * 1000);

    await page.getByRole("button", { name: "Promote r1" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Confirm promotion" }).click();
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("Confirm your identity to continue. Nothing was submitted.");
    expect((await ok(call(page, "GET", `/configuration-drafts/${draft.draft_id}`), 200, "read draft")).status).toBe("draft");

    await page.getByRole("button", { name: "Confirm identity", exact: true }).click();
    await page.getByLabel("Password", { exact: true }).fill(password);
    await page.getByRole("button", { name: "Sign In", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Master data" })).toBeVisible();
    await page.getByRole("button", { name: /^Drafts/ }).click();
    await page.getByRole("button", { name: `Open draft ${draft.draft_id.slice(0, 8)}` }).click();
    await page.getByRole("button", { name: "Promote r1" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Confirm promotion" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Promoted as line r1" })).toBeVisible();
    expect((await ok(call(page, "GET", `/configuration-drafts/${draft.draft_id}`), 200, "read draft")).status).toBe("promoted");
});

test("a viewer has no Drafts tab and the API refuses them", async ({ browser }) => {
    const owner = await as(browser, "foundation-reviewer");
    await section(owner, "Master data");
    await expect(owner.getByRole("button", { name: /^Drafts/ })).toBeVisible();
    expect((await call(owner, "GET", "/configuration-drafts")).status).toBe(200);

    const viewer = await as(browser, "read-only-viewer");
    await section(viewer, "Master data");
    await expect(viewer.getByRole("heading", { name: "Master data" })).toBeVisible();
    await expect(viewer.getByRole("button", { name: /^Drafts/ })).toHaveCount(0);
    const denied = await call(viewer, "GET", "/configuration-drafts");
    expect([denied.status, denied.body?.code]).toEqual([403, "PERMISSION_DENIED"]);
});

test("discard asks in an in-app dialog; a closed draft answers DRAFT_NOT_OPEN and a same-key replay", async ({ browser }) => {
    const lead = await as(browser, "shift-lead");
    const page = await as(browser, "shift-approver");

    const kept = await lineDraft(lead, `E2E discard line ${suffix()}`);
    await openDraft(page, kept.draft_id);
    await page.getByRole("button", { name: "Discard", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: `Discard line draft ${kept.draft_id.slice(0, 8)}?` });
    await expect(dialog).toBeVisible();
    // Negative control: Cancel returns focus and discards nothing.
    await dialog.getByRole("button", { name: "Cancel" }).click();
    await expect(dialog).toBeHidden();
    await expect(page.getByRole("button", { name: "Discard", exact: true })).toBeFocused();
    expect((await ok(call(page, "GET", `/configuration-drafts/${kept.draft_id}`), 200, "read draft")).status).toBe("draft");
    await page.getByRole("button", { name: "Discard", exact: true }).click();
    await dialog.getByRole("button", { name: "Confirm discard" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Draft discarded" })).toBeVisible();
    await expect(page.getByRole("button", { name: /^Promote/ })).toHaveCount(0);
    expect((await ok(call(page, "GET", `/configuration-drafts/${kept.draft_id}`), 200, "read draft")).status).toBe("discarded");

    // A page still showing an open draft that someone else discarded: promotion is refused and the
    // page reloads the closed draft.
    const stale = await lineDraft(lead, `E2E closed line ${suffix()}`);
    await openDraft(page, stale.draft_id);
    const key = crypto.randomUUID();
    const first = await call(lead, "POST", `/configuration-drafts/${stale.draft_id}/discards`, {}, 1, key);
    const replay = await call(lead, "POST", `/configuration-drafts/${stale.draft_id}/discards`, {}, 1, key);
    expect([first.status, first.body?.replayed, replay.status, replay.body?.replayed]).toEqual([200, false, 200, true]);
    await page.getByRole("button", { name: "Promote r1" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Confirm promotion" }).click();
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("This draft is already promoted or discarded. It has been reloaded.");
    await expect(page.getByRole("button", { name: /^Promote/ })).toHaveCount(0);
    const again = await call(page, "POST", `/configuration-drafts/${stale.draft_id}/discards`, {}, 1);
    expect([again.status, again.body?.code]).toEqual([409, "DRAFT_NOT_OPEN"]);
});
