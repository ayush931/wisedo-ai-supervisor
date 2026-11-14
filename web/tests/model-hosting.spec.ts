import { expect, test, type Page, type Request } from "@playwright/test";

// WIS-623: the Model hosting page shows the site's hosted-inference switch and lets only an owner
// change it. The API is stubbed (UI behaviour); the server's owner-only, recent-sign-in,
// idempotency and revision checks are covered by TestModelHostingSwitchHTTPPostgres.

const site = "00000000-0000-4000-8000-000000000931";
const meFor = (role: string) => ({
    csrf_token: "test-csrf",
    display_name: "Stubbed member",
    subject: "member-subject",
    sites: [{ id: site, name: "Stubbed Site", role, tenant_id: "00000000-0000-4000-8000-000000000930", agent_available: true }],
});
const off = { schema_version: 1, enabled: false, dpa_reference: null, revision: 0, decisions: [], replayed: false };
const onDecision = { revision: 1, enabled: true, dpa_reference: "DPA-7", reason: "Pilot", actor: { issuer: "i", subject: "s" }, created_at: "2026-11-05T09:00:00Z" };
const on = { schema_version: 1, enabled: true, dpa_reference: "DPA-7", revision: 1, decisions: [onDecision], replayed: false };
const problem = (status: number, code: string) => ({ status, contentType: "application/problem+json", body: JSON.stringify({ code, status, title: code, type: "about:blank" }) });
type Reply = { status?: number; json?: unknown; contentType?: string; body?: string };

async function open(page: Page, role: string, current: () => unknown, decide: (r: Request) => Reply = () => ({ json: on })) {
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor(role) });
        if (path === `/v1/sites/${site}/model-hosting` && route.request().method() === "GET") return route.fulfill({ json: current() });
        if (path === `/v1/sites/${site}/model-hosting/decisions`) return route.fulfill(decide(route.request()));
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/model-hosting`);
    await expect(page.getByRole("heading", { name: "Model hosting", exact: true })).toBeVisible();
}

test("stubbed owner turns the hosted model on with a DPA reference and sees the server's state", async ({ page }) => {
    const sent: Request[] = [];
    await open(page, "owner", () => off, (r) => {
        sent.push(r);
        return { json: on };
    });
    await expect(page.getByRole("status", { name: "Current setting" })).toContainText("Off");
    const button = page.getByRole("button", { name: "Turn hosted model on" });
    await expect(button).toBeDisabled(); // a DPA reference and a reason are required
    await page.getByLabel("Reason").fill("Pilot");
    await expect(button).toBeDisabled();
    await page.getByLabel("Data processing agreement reference").fill("DPA-7");
    await button.click();
    await expect(page.getByText("Hosted model inference is on for this site.")).toBeVisible();
    await expect(page.getByRole("list", { name: "Model hosting decisions" })).toContainText("Turned on");
    expect(sent).toHaveLength(1);
    expect(JSON.parse(sent[0].postData() ?? "{}")).toEqual({ enabled: true, dpa_reference: "DPA-7", reason: "Pilot" });
    expect(sent[0].headers()["if-match"]).toBe('"0"');
    expect(sent[0].headers()["idempotency-key"]).toMatch(/^[0-9a-f-]{36}$/);
});

test("stubbed stale decision is refused and the page reloads the server's state", async ({ page }) => {
    let current: unknown = off;
    await open(page, "owner", () => current, () => {
        current = on; // another owner turned it on first
        return problem(412, "REVISION_CONFLICT");
    });
    await page.getByLabel("Data processing agreement reference").fill("DPA-8");
    await page.getByLabel("Reason").fill("Second owner");
    await page.getByRole("button", { name: "Turn hosted model on" }).click();
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("This changed on the server");
    await expect(page.getByRole("button", { name: "Turn hosted model off" })).toBeVisible();
});

// WIS-623 post-merge review (P2): an idempotent replay returns the receipt its key recorded (here
// "off" at revision 2), even when another owner has since turned hosting back on (revision 3). The
// page used to install that receipt as the current state and show "Off".
test("stubbed replayed decision does not replace the current state", async ({ page }) => {
    const offDecision = { ...onDecision, revision: 2, enabled: false, dpa_reference: null, reason: "Pause" };
    const receipt = { ...off, revision: 2, decisions: [offDecision, onDecision], replayed: true };
    const current = { ...on, revision: 3, decisions: [{ ...onDecision, revision: 3 }, offDecision, onDecision] };
    let state: unknown = on;
    await open(page, "owner", () => state, () => {
        state = current;
        return { json: receipt };
    });
    await page.getByLabel("Reason").fill("Pause");
    await page.getByRole("button", { name: "Turn hosted model off" }).click();
    await expect(page.getByText("This decision was already recorded at revision 2.", { exact: false })).toBeVisible();
    await expect(page.getByRole("status", { name: "Current setting" })).toContainText("On");
    await expect(page.getByRole("status", { name: "Current setting" })).toContainText("revision 3");
    await expect(page.getByRole("button", { name: "Turn hosted model off" })).toBeVisible();
});

test("stubbed supervisor reads the switch but cannot change it", async ({ page }) => {
    await open(page, "supervisor", () => on);
    await expect(page.getByRole("status", { name: "Current setting" })).toContainText("data processing agreement DPA-7");
    await expect(page.getByText("Only an owner of this site can change this.")).toBeVisible();
    await expect(page.getByRole("button", { name: /Turn hosted model/ })).toHaveCount(0);
});

// WIS-707 review (PR #302): an over-limit reason is kept as typed, marked invalid with its length,
// and cannot be submitted. The limit counts code points as the server does
// (utf8.RuneCountInString), so an emoji (two UTF-16 units) at the boundary counts once.
test("stubbed over-limit reason keeps the text, shows the length error and blocks submit", async ({ page }) => {
    const sent: Request[] = [];
    await open(page, "owner", () => off, (r) => {
        sent.push(r);
        return { json: on };
    });
    const button = page.getByRole("button", { name: "Turn hosted model on" });
    const reason = page.getByLabel("Reason");
    await page.getByLabel("Data processing agreement reference").fill("DPA-7");
    await reason.focus();
    await page.keyboard.insertText("a".repeat(2001)); // pasted in one insertion
    await expect(reason).toHaveValue("a".repeat(2001)); // not cut to the limit
    await expect(reason).toHaveAttribute("aria-invalid", "true");
    await expect(reason).toHaveAccessibleDescription(/2001 characters; the limit is 2000\. Shorten it to submit\./);
    await expect(button).toBeDisabled();
    await page.getByLabel("Data processing agreement reference").press("Enter"); // the form submits on Enter
    await page.waitForTimeout(300);
    expect(sent).toHaveLength(0);

    // 1,999 letters and an emoji are 2,000 code points (2,001 UTF-16 units): within the limit.
    const atLimit = "a".repeat(1999) + "😀";
    await reason.fill(atLimit);
    await expect(reason).not.toHaveAttribute("aria-invalid", "true");
    await expect(button).toBeEnabled();
    await reason.press("End");
    await page.keyboard.insertText("😀"); // one more code point
    await expect(reason).toHaveAttribute("aria-invalid", "true");
    await expect(reason).toHaveAccessibleDescription(/2001 characters; the limit is 2000\./);
    await expect(button).toBeDisabled();
    await page.keyboard.press("Backspace"); // removes the whole emoji
    await expect(reason).toHaveValue(atLimit);
    await button.click();
    await expect(page.getByText("Hosted model inference is on for this site.")).toBeVisible();
    expect(sent).toHaveLength(1);
    expect(JSON.parse(sent[0].postData() ?? "{}").reason).toBe(atLimit);
});
