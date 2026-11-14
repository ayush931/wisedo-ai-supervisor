// Labelled stubbed API responses (WIS-278 operator station): the real station page against fixed
// server replies. Problem buttons, receipts and the procedure guide; not backend evidence.
import { test, expect, type Page, type Route } from "@playwright/test";
import { parseSteps } from "../src/station";

const site = "00000000-0000-4000-8000-000000000411";
const ws = "00000000-0000-4000-8000-000000000412";
const line = "00000000-0000-4000-8000-000000000413";
const job = "00000000-0000-4000-8000-000000000414";
const plan = "00000000-0000-4000-8000-000000000415";
const item = "00000000-0000-4000-8000-000000000416";
const doc = "00000000-0000-4000-8000-000000000417";
const NOW = new Date("2026-10-30T10:00:00Z");
const actor = { issuer: "https://issuer.test", subject: "op" };
const SHA = "e".repeat(64);
const SOP = "# Check the job card and material\nLot on the bin label matches the job card.\n# Load the program\nProgram BD-102-G on the screen.\n# Run and gauge the first part\nRecord it as part 1.";
const problem = (route: Route, status: number, code: string) =>
    route.fulfill({ status, contentType: "application/problem+json", json: { type: "about:blank", title: code, status, code, correlation_id: "stub" } });

const order = { schema_version: 1, order_id: "PO 24-1187", operation_id: job, item_code: "BD-102", item_id: item, step_number: 5, target_quantity: 120, instruction_document_id: doc, instruction_revision: 3, instruction_sha256: SHA, template: "production", fields: [] };
const record = (kind: string, id: string, extra: Record<string, unknown> = {}, planId = plan) => ({
    id, plan_id: planId, plan_revision: 1, profile_revision: 1, state: "current", late: false, replayed: false, source: "manual_tablet", digest_schema_version: 1, canonical_payload_digest: "f".repeat(64),
    event_at: NOW.toISOString(), received_at: NOW.toISOString(), recorded_by: actor,
    record: { schema_version: 1, kind, workstation_id: ws, job_order_id: job, job_order_revision: 1, event_at: NOW.toISOString(), ...extra },
});

async function stub(page: Page, o: { sha?: string; running?: boolean; source?: number; plan?: () => string } = {}) {
    const posts: any[] = [];
    const planOf = o.plan ?? (() => plan);
    await page.clock.setFixedTime(NOW);
    await page.route("**/v1/**", async (route) => {
        const req = route.request();
        const url = new URL(req.url());
        const path = url.pathname.replace(`/v1/sites/${site}`, "");
        if (url.pathname === "/v1/me") return route.fulfill({ json: { csrf_token: "stub", subject: "op", display_name: "Sanjay Patil", sites: [{ id: site, name: "Chakan Plant", role: "operator", tenant_id: site, agent_available: false }] } });
        if (path === "/workstations") return route.fulfill({ json: { workstations: [{ workstation_id: ws, line_id: line, display_name: "Grinder 05", current_revision: 1 }] } });
        if (path === "/members") return route.fulfill({ json: { members: [] } });
        if (path === `/workstations/${ws}/current-job-order`)
            return route.fulfill({ json: { workstation_id: ws, job_order_id: job, line_id: line, plan_id: planOf(), plan_revision: 1, pinned_revision: 1, current_revision: 1, starts_at: "2026-10-30T08:30:00Z", ends_at: "2026-10-30T16:30:00Z", timezone: "Asia/Kolkata", state: "current", target_quantity: 120, order: { revision: 1, state: "current", order, created_at: NOW.toISOString(), canonical_payload_digest: "a".repeat(64), digest_schema_version: 1, submitted_by: actor } } });
        if (path === `/workstations/${ws}/records` && req.method() === "GET")
            return route.fulfill({ json: { records: o.running === false ? [] : [record("acknowledged", "r1", {}, planOf()), record("started", "r2", {}, planOf())] } });
        if (path === `/workstations/${ws}/records`) {
            const body = req.postDataJSON();
            posts.push(body);
            return route.fulfill({ status: 201, json: { ...record(body.kind, `r${posts.length + 10}`, {}, planOf()), record: body } });
        }
        if (path === `/workstations/${ws}/profile`) return route.fulfill({ json: { current_revision: 1, replayed: false, revisions: [{ revision: 1, profile: { display_name: "Grinder 05", fields: [], line_id: line, schema_version: 1, template: "production", workstation_id: ws } }] } });
        if (path === `/workstations/${ws}/profile-previews`) return route.fulfill({ json: { ready: true, fields: [], instructions: [], conflicts: [], profile_digest: "a".repeat(64), template: "production", workstation_id: ws } });
        if (path === `/items/${item}`) return route.fulfill({ json: { id: item, current_revision: 1, replayed: false, revisions: [{ revision: 1, body: { display_name: "Solid brake disc", active: true, schema_version: 1, unit: "count" } }] } });
        if (path === `/lines/${line}`) return route.fulfill({ json: { id: line, current_revision: 1, replayed: false, revisions: [{ revision: 1, body: { display_name: "Brake disc line", active: true, schema_version: 1 } }] } });
        if (path === `/sources/${doc}`) {
            expect(url.searchParams.get("revision")).toBe("3");
            if (o.source) return problem(route, o.source, o.source === 410 ? "SOURCE_REVOKED" : "NOT_FOUND");
            return route.fulfill({ json: { source_id: doc, title: "Grinder procedure", revision: 3, current_revision: 3, sha256: o.sha ?? SHA, text: SOP, kind: "sop", authority: "approved", authority_class: "a", access_epoch: 1, byte_length: SOP.length, rune_length: SOP.length, created_at: NOW.toISOString(), independence_group_id: doc, state_revision: 1 } });
        }
        return problem(route, 404, "NOT_FOUND");
    });
    await page.goto(`/sites/${site}/workstations/${ws}`);
    await expect(page.getByRole("heading", { name: "Grinder 05", level: 1 })).toBeVisible();
    return posts;
}

test("stubbed job header names the item, order, step and good-vs-target", async ({ page }) => {
    await stub(page);
    await expect(page.getByRole("heading", { name: "Solid brake disc" })).toBeVisible();
    await expect(page.getByText(/^Order PO 24-1187 · step 5 · order rev 1/)).toBeVisible();
    await expect(page.getByText(/Brake disc line · Shift .* 14:00–22:00 IST/)).toBeVisible();
    await expect(page.locator(".big")).toHaveText("Good parts recorded: 0 / 120 good parts");
});

const problems: [string, string, Record<string, unknown>][] = [
    ["Machine stopped", "blocked", { comment: "Machine stopped: spindle alarm" }],
    ["Tool wear", "blocked", { comment: "Tool wear: spindle alarm" }],
    ["Safety", "blocked", { comment: "Safety: spindle alarm" }],
    ["Quality issue", "note", { comment: "Quality issue: spindle alarm" }],
    ["Something else", "note", { comment: "Other: spindle alarm" }],
];
for (const [button, kind, expected] of problems) {
    test(`stubbed problem button "${button}" records ${kind} with its category and confirms with the receipt`, async ({ page }) => {
        const posts = await stub(page);
        await expect(page.getByRole("status").filter({ hasText: "Running" })).toBeVisible();
        await page.getByRole("button", { name: button, exact: true }).click();
        const reason = page.getByRole("textbox", { name: kind === "blocked" ? "Reason" : "Note" });
        // The category alone is not a report: Record waits for what happened.
        await expect(page.getByRole("button", { name: /^Record/ })).toBeDisabled();
        await reason.press("End");
        await reason.pressSequentially("spindle alarm");
        await page.getByRole("button", { name: /^Record/ }).click();
        await expect(page.getByRole("status").filter({ hasText: `Recorded: ${kind === "blocked" ? "Blocked" : "Note"}` })).toBeVisible();
        expect(posts).toHaveLength(1);
        expect(posts[0]).toMatchObject({ kind, ...expected });
        await expect(page.getByText(/notified|sent to (your|the) /i)).toHaveCount(0);
    });
}

test("stubbed Machine stopped before the job starts is a note, not a block", async ({ page }) => {
    const posts = await stub(page, { running: false });
    await page.getByRole("button", { name: "Machine stopped", exact: true }).click();
    await expect(page.getByRole("textbox", { name: "Note" })).toHaveValue("Machine stopped: ");
    await expect(page.getByRole("button", { name: /^Record/ })).toBeDisabled();
    await page.getByRole("textbox", { name: "Note" }).press("End");
    await page.getByRole("textbox", { name: "Note" }).pressSequentially("no power at start");
    await page.getByRole("button", { name: /^Record/ }).click();
    await expect(page.getByRole("status").filter({ hasText: "Recorded: Note" })).toBeVisible();
    expect(posts[0]).toMatchObject({ kind: "note", comment: "Machine stopped: no power at start" });
    // A material issue needs a started job, as the Issue action does.
    await expect(page.getByRole("button", { name: "Material missing", exact: true })).toBeDisabled();
});

test("stubbed Material missing opens a material issue preset to shortage and records it", async ({ page }) => {
    const posts = await stub(page);
    await page.getByRole("button", { name: "Material missing", exact: true }).click();
    await expect(page.getByRole("radio", { name: "Shortage" })).toBeChecked();
    await page.getByLabel("Material code").fill("GI-CAST-102");
    await page.getByRole("button", { name: /^Record/ }).click();
    await expect(page.getByRole("status").filter({ hasText: "Recorded: Material issue" })).toBeVisible();
    expect(posts[0]).toMatchObject({ kind: "material_exception", material_issue: "shortage", material_code: "GI-CAST-102" });
});

test("stubbed procedure with a different checksum shows the reason and none of its text", async ({ page }) => {
    await stub(page, { sha: "0".repeat(64) });
    const guide = page.getByRole("region", { name: "How to do this job" });
    await expect(guide.getByRole("alert").filter({ hasText: /\S/ })).toContainText("does not match the revision approved for this job");
    await expect(page.getByText("Check the job card and material")).toHaveCount(0);
    await expect(page.getByText("Lot on the bin label")).toHaveCount(0);
});

test("stubbed procedure steps: Done moves on, Back returns, progress survives a reload", async ({ page }) => {
    await stub(page);
    const guide = page.getByRole("region", { name: "How to do this job" });
    await expect(guide.getByText("From Grinder procedure, rev 3")).toBeVisible();
    await expect(guide.getByText("Step 1 of 3")).toBeVisible();
    await expect(guide.getByRole("heading", { name: "Check the job card and material" })).toBeVisible();
    await expect(guide.getByText("Lot on the bin label matches the job card.")).toBeVisible();
    await guide.getByRole("button", { name: "Done, next step" }).click();
    await expect(guide.getByText("Step 2 of 3")).toBeVisible();
    await expect(guide.getByText("1 of 3 done")).toBeVisible();
    await guide.getByRole("button", { name: "Back" }).click();
    await expect(guide.getByText("Step 1 of 3")).toBeVisible();
    await guide.getByRole("button", { name: /Step 3: Run and gauge/ }).click();
    await expect(guide.getByRole("button", { name: "Done", exact: true })).toBeVisible();
    await page.reload();
    // Done state is kept per job on this device; the guide resumes at the first step not done.
    await expect(guide.getByText("1 of 3 done")).toBeVisible();
    await expect(guide.getByText("Step 2 of 3")).toBeVisible();
    await expect(guide.getByRole("button", { name: /Step 1, done: Check the job card/ })).toBeVisible();
});

// WIS-707 regression: the step buttons carry tooltips, and a tooltip's layer installs the Astryx
// slide rules as a runtime <style>. The page and bundle come from the real server, so its
// Content-Security-Policy applies; `style-src 'self'` alone blocked the sheet.
test("stubbed procedure step tooltips apply the layer slide rules with no CSP violation", async ({ page }) => {
    const csp: string[] = [];
    page.on("console", (m) => {
        if (m.type() === "error" && /Content Security Policy/i.test(m.text())) csp.push(m.text());
    });
    await stub(page);
    const guide = page.getByRole("region", { name: "How to do this job" });
    await guide.getByRole("button", { name: /Step 2: Load the program/ }).hover();
    await expect(page.getByRole("tooltip")).toContainText("Load the program");
    expect(csp).toEqual([]);
    // Positive control: the sheet exists and its rules were parsed (a blocked sheet has none).
    expect(await page.evaluate(() => (document.getElementById("astryx-layer-slide-rules") as HTMLStyleElement | null)?.sheet?.cssRules.length ?? -1)).toBeGreaterThan(0);
});

test("parseSteps splits on headings or lines and never adds words", () => {
    expect(parseSteps(SOP)).toEqual([
        { title: "Check the job card and material", body: ["Lot on the bin label matches the job card."] },
        { title: "Load the program", body: ["Program BD-102-G on the screen."] },
        { title: "Run and gauge the first part", body: ["Record it as part 1."] },
    ]);
    expect(parseSteps("Clean the fixture\n\n  Load the part  \r\nStart the cycle\n")).toEqual([
        { title: "Clean the fixture", body: [] },
        { title: "Load the part", body: [] },
        { title: "Start the cycle", body: [] },
    ]);
    expect(parseSteps("Read first\n## Step A\ndo a")).toEqual([
        { title: "Read first", body: [] },
        { title: "Step A", body: ["do a"] },
    ]);
    expect(parseSteps("   \n")).toEqual([]);
});

for (const status of [404, 410]) {
    test(`stubbed procedure that cannot be read (${status}) says so and shows no text`, async ({ page }) => {
        await stub(page, { source: status });
        const guide = page.getByRole("region", { name: "How to do this job" });
        await expect(guide).toContainText("The procedure could not be loaded, so no steps are shown.");
        await expect(page.getByText("Check the job card and material")).toHaveCount(0);
        await expect(guide.getByRole("button", { name: /Done/ })).toHaveCount(0);
    });
}

test("stubbed procedure progress starts fresh when the same job runs in a later shift plan", async ({ page }) => {
    let planId = plan;
    await stub(page, { plan: () => planId });
    const guide = page.getByRole("region", { name: "How to do this job" });
    await guide.getByRole("button", { name: "Done, next step" }).click();
    await expect(guide.getByText("1 of 3 done")).toBeVisible();
    planId = "00000000-0000-4000-8000-000000000499";
    await page.reload();
    await expect(guide.getByText("0 of 3 done")).toBeVisible();
    await expect(guide.getByText("Step 1 of 3")).toBeVisible();
});
