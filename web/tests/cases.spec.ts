import { expect, test, type Browser, type Page } from "@playwright/test";
import { openSite, section, signIn } from "./session";

// WIS-273 case inbox against the real backend. supervisor-demo-seed opens one real two-window
// shortfall case on "Bracket line 1" through plan approval, telemetry intake, the fold and the
// case evaluator. Setup steps use the public API; every assertion reads the UI. The spec reruns
// on a used site: it works from whichever episode of the demo line is newest.

type Reply = { status: number; body: any };

async function call(page: Page, method: string, path: string, body?: unknown, ifMatch?: number): Promise<Reply> {
    return page.evaluate(async ({ method, path, body, ifMatch }) => {
        const me = await (await fetch("/v1/me")).json();
        const headers: Record<string, string> = {};
        if (method !== "GET") {
            headers["Content-Type"] = "application/json";
            headers["X-CSRF-Token"] = me.csrf_token;
            headers["Idempotency-Key"] = crypto.randomUUID();
            if (ifMatch !== undefined) headers["If-Match"] = `"${ifMatch}"`;
        }
        const r = await fetch(`/v1/sites/${me.sites[0].id}${path}`, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
        return { status: r.status, body: await r.json().catch(() => null) };
    }, { method, path, body, ifMatch });
}

async function ok(reply: Promise<Reply>, status: number, what: string) {
    const r = await reply;
    if (r.status !== status) throw new Error(`${what}: expected ${status}, got ${r.status} ${r.body?.code ?? ""}`);
    return r.body;
}

async function as(browser: Browser, user: string) {
    const page = await (await browser.newContext()).newPage();
    await signIn(page, user);
    await openSite(page);
    return page;
}

const terminal = (state: string) => state === "resolved" || state === "canceled";

type DemoCase = { id: string; state: string; revision: number; episode: number; line_id: string };

// demoCases returns every episode on the demo line, oldest first, walking every page (cases are
// ordered by ID, so episodes can be on any page). A missing demo case is BLOCKED, never a skip.
async function demoCases(page: Page): Promise<DemoCase[]> {
    const named = new Map<string, string>();
    const found: DemoCase[] = [];
    let cursor = "";
    for (let n = 0; n < 1000; n++) {
        const list = await ok(call(page, "GET", `/cases?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`), 200, "list cases");
        for (const c of list.items) {
            if (!named.has(c.line_id)) named.set(c.line_id, (await ok(call(page, "GET", `/entities/${c.line_id}`), 200, "read line")).display_name);
            if (named.get(c.line_id) === "Bracket line 1") found.push(c);
        }
        if (!list.next_cursor) break;
        cursor = list.next_cursor;
    }
    if (!found.length) throw new Error("BLOCKED: no demo case on Bracket line 1 (run supervisor-demo-seed)");
    return found.sort((a, b) => a.episode - b.episode);
}
const demoCase = async (page: Page) => (await demoCases(page)).at(-1)!;

// activeDemoCase makes sure the newest demo episode is active, reopening it through the API if an
// earlier run left it canceled.
async function activeDemoCase(page: Page) {
    const c = await demoCase(page);
    if (!terminal(c.state)) return c;
    const reopened = await ok(call(page, "POST", `/cases/${c.id}/reopen`, { expected_revision: c.revision, reason: "E2E setup: start a fresh episode" }, c.revision), 201, "reopen");
    return { ...c, id: reopened.id, state: reopened.state, revision: reopened.revision, episode: reopened.episode };
}

async function openCase(page: Page, short: string) {
    await section(page, "Cases");
    await expect(page.getByRole("heading", { name: "Cases", exact: true })).toBeVisible();
    const link = page.getByRole("table", { name: "Cases" }).getByRole("link", { name: `Case ${short}` });
    await expect(page.getByRole("table", { name: "Cases" })).toBeVisible();
    // The inbox shows 50 cases a page in ID order; on a reused site the case may be further on.
    while (!(await link.count())) {
        const more = page.getByRole("button", { name: "Load more cases" });
        await expect(more, `Case ${short} is not in the inbox`).toBeVisible();
        await more.click();
        await expect(more).toBeEnabled();
    }
    await expect(link).toBeVisible();
    await link.focus();
    await page.keyboard.press("Enter");
    await expect(page.getByRole("heading", { name: `Case ${short}` })).toBeVisible();
}

test("a supervisor finds the demo shortfall in the inbox and reads its timeline", async ({ browser }) => {
    const page = await as(browser, "shift-lead");
    // Episode 1 was opened by the case evaluator; later episodes by a supervisor's reopen.
    const c = (await demoCases(page))[0];
    await openCase(page, c.id.slice(0, 8));
    // Names come from the knowledge graph; the state and revision from the server's case.
    await expect(page.getByText("Bracket line 1").first()).toBeVisible();
    await expect(page.getByText(`episode ${c.episode} · revision`)).toBeVisible();
    expect(c.episode).toBe(1);
    const timeline = page.getByRole("list", { name: "Case timeline" });
    await expect(timeline.getByRole("listitem").first()).toContainText("Opened as");
    // The evaluator is a service, never shown as a site member.
    await expect(timeline.getByRole("listitem").first()).toContainText("Shortfall evaluator (system)");
});

test("a viewer reads a case but is offered no case action", async ({ browser }) => {
    const lead = await as(browser, "shift-lead");
    const c = await activeDemoCase(lead);
    const viewer = await as(browser, "read-only-viewer");
    await openCase(viewer, c.id.slice(0, 8));
    await expect(viewer.getByRole("list", { name: "Case timeline" })).toBeVisible();
    for (const name of ["Cancel case", "Reopen case", "Record clarification"]) await expect(viewer.getByRole("button", { name })).toHaveCount(0);
    // The server, not the hidden button, is the boundary: a viewer's and an operator's cancel are
    // both refused for missing authority.
    const operator = await as(browser, "operator-1");
    for (const [who, page] of [["viewer", viewer], ["operator", operator]] as const) {
        const refused = await call(page, "POST", `/cases/${c.id}/cancel`, { expected_revision: c.revision, reason: `${who} attempt` }, c.revision);
        expect(refused.status, who).toBe(403);
        expect(refused.body?.code, who).toBe("PERMISSION_DENIED");
    }
    await openCase(operator, c.id.slice(0, 8));
    await expect(operator.getByRole("button", { name: "Cancel case" })).toHaveCount(0);
});

test("a stale cancel is refused, reloaded for review, and the case reopens as a new episode", async ({ browser }) => {
    const page = await as(browser, "shift-lead");
    const c = await activeDemoCase(page);
    await openCase(page, c.id.slice(0, 8));
    await expect(page.getByRole("button", { name: "Cancel case" })).toBeVisible();
    // Another supervisor action lands first, so the page's revision is stale.
    await ok(call(page, "POST", `/cases/${c.id}/cancel`, { expected_revision: c.revision, reason: "Canceled elsewhere" }, c.revision), 200, "concurrent cancel");
    await page.getByLabel("Reason").fill("Line restarted; shortfall explained");
    await page.getByRole("button", { name: "Cancel case" }).click();
    await expect(page.getByRole("status").filter({ hasText: "This changed on the server" })).toContainText("Nothing was submitted.");
    // The reload shows the server's state; nothing was resubmitted.
    await expect(page.getByTitle("canceled").first()).toBeVisible();
    await expect(page.getByRole("list", { name: "Case timeline" })).toContainText("Canceled elsewhere");
    await expect(page.getByRole("list", { name: "Case timeline" })).not.toContainText("Line restarted");

    await page.getByLabel("Reason").fill("Shortfall recurred after restart");
    await page.getByRole("button", { name: "Reopen case" }).click();
    await expect(page.getByText(`episode ${c.episode + 1} · revision`)).toBeVisible();
    // Reopening the older episode again is refused for its own reason, not shown as a change.
    await page.getByRole("link", { name: `Case ${c.id.slice(0, 8)}` }).click(); // previous episode
    await expect(page.getByRole("heading", { name: `Case ${c.id.slice(0, 8)}` })).toBeVisible();
    await page.getByLabel("Reason").fill("Second reopen of an old episode");
    await page.getByRole("button", { name: "Reopen case" }).click();
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("A newer episode of this case exists");
    await expect(page.getByRole("status").filter({ hasText: "This changed on the server" })).toHaveCount(0);
});

test("a supervisor cancels an active case with a reason and sees the server's result", async ({ browser }) => {
    const page = await as(browser, "shift-lead");
    const c = await activeDemoCase(page);
    await openCase(page, c.id.slice(0, 8));
    const cancel = page.getByRole("button", { name: "Cancel case" });
    await expect(cancel).toBeDisabled(); // a reason is required
    await page.getByLabel("Reason").fill("E2E: shortfall explained by a planned changeover");
    await cancel.click();
    await expect(page.getByRole("status").filter({ hasText: "The case was canceled." })).toBeVisible();
    await expect(page.getByRole("list", { name: "Case timeline" })).toContainText("E2E: shortfall explained by a planned changeover");
    await expect(page.getByRole("button", { name: "Reopen case" })).toBeVisible();
});
