// Recorded product walkthrough against the real backend and the WIS-520 demo seed: a shift lead,
// two operators on tablets and a second supervisor, each in their own browser context and
// Keycloak session. It is also an ordinary e2e test: every step asserts what the person sees.
//
// WALKTHROUGH_DIR: where step screenshots and per-person videos go (default: test output dir).
// WALKTHROUGH_PACE: pause in ms between actions so a viewer can follow (default 900; 0 = fast).
// The pauses are pacing only; every wait for the app is a web-first assertion.
import { test, expect, type Browser, type BrowserContext, type Page } from "@playwright/test";
import { openSeedShift, openSite, section, signIn } from "./session";

const env = (globalThis as typeof globalThis & { process?: { env?: Record<string, string | undefined> } }).process?.env ?? {};
const PACE = Number(env.WALKTHROUGH_PACE ?? 900);
const TABLET = { width: 1180, height: 820 };
const DESKTOP = { width: 1440, height: 900 };

test("walkthrough: shift lead, two operators and an approver run one shift", async ({ browser }, info) => {
    test.setTimeout(10 * 60_000);
    const dir = env.WALKTHROUGH_DIR || info.outputPath();
    const pause = (page: Page, factor = 1) => page.waitForTimeout(PACE * factor);
    const shot = (page: Page, name: string) => page.screenshot({ path: `${dir}/${name}.png`, fullPage: true });

    // One person on one device: own context, own IdP session, own video.
    async function person(b: Browser, username: string, viewport: typeof TABLET) {
        const context = await b.newContext({ viewport, recordVideo: { dir: `${dir}/raw`, size: viewport } });
        const page = await context.newPage();
        await signIn(page, username);
        await pause(page);
        await openSite(page);
        await pause(page);
        return { context, page };
    }
    async function done({ context, page }: { context: BrowserContext; page: Page }, video: string) {
        await pause(page, 1.5);
        const v = page.video()!;
        await context.close();
        await v.saveAs(`${dir}/${video}.webm`);
        await v.delete();
    }
    async function openCurrentShift(page: Page) {
        // The seed plan for the current shift is approved; it is found by its line.
        await openSeedShift(page);
        await pause(page);
    }
    async function openStation(page: Page, name: string) {
        await section(page, "Workstations");
        await pause(page);
        await page.getByRole("link", { name }).click();
        await expect(page.getByRole("heading", { name })).toBeVisible();
        await expect(page.getByText("Current job order")).toBeVisible();
    }
    // Acknowledge and Start until the station runs (the seed already started press-01).
    async function ensureRunning(page: Page) {
        const toolbar = page.getByRole("toolbar", { name: "Record" });
        await expect(page.getByRole("button", { name: /History \(\d+\)/ })).toBeVisible();
        for (const label of ["Acknowledge", "Start"]) {
            const button = toolbar.getByRole("button", { name: label, exact: true });
            if (await button.isVisible()) {
                await button.click();
                await expect(page.getByRole("status").filter({ hasText: /Recorded: (Acknowledged|Started)/ })).toBeVisible();
                await pause(page);
            }
        }
        await expect(page.getByRole("status").filter({ hasText: "Running" })).toBeVisible();
    }
    const remainingTile = (page: Page) => page.locator(".kpi").filter({ hasText: "Remaining" }).locator("strong");
    const receipt = (page: Page, kind: string) => page.getByRole("status").filter({ hasText: `Recorded: ${kind}` });

    // 1. Shift lead opens today's shift: stations and their targets.
    const lead = await person(browser, "shift-lead", DESKTOP);
    await expect(lead.page.getByRole("banner").getByText("Rahul Deshmukh", { exact: true })).toBeVisible();
    await openCurrentShift(lead.page);
    await expect(lead.page.getByText(/Approved · rev \d+/).first()).toBeVisible();
    for (const station of ["press-01", "weld-01"]) {
        const row = lead.page.getByRole("row").filter({ has: lead.page.getByRole("link", { name: station }) });
        await expect(row.getByRole("cell").nth(2)).toHaveText(/^\d+$/); // Planned target
    }
    await pause(lead.page, 2);
    await shot(lead.page, "step-1-shift-lead-plan");
    await done(lead, "01-shift-lead");

    // 2. Sanjay Patil on the press tablet records good parts, a cycle time, a reject and a note.
    const op1 = await person(browser, "operator-1", TABLET);
    let p = op1.page;
    await expect(p.getByRole("banner").getByText("Sanjay Patil", { exact: true })).toBeVisible();
    await openStation(p, "press-01");
    await expect(p.getByText("Bracket A-100, op 10 Press")).toBeVisible();
    await ensureRunning(p);
    await expect(remainingTile(p)).toHaveText(/^\d+$/);
    const before = Number(await remainingTile(p).textContent());
    await pause(p);

    await p.getByRole("button", { name: /\+ Good/ }).click();
    await pause(p);
    await p.getByRole("button", { name: "Add 1 good" }).click();
    await pause(p, 0.6);
    await p.getByRole("button", { name: "Add 1 good" }).click();
    await expect(p.getByLabel("Good", { exact: true })).toHaveValue("3");
    await pause(p);
    await p.getByRole("button", { name: /^Record/ }).click();
    await expect(receipt(p, "Quantity")).toBeVisible();
    // Other specs may record on press-01 concurrently, so Remaining drops by at least 3.
    await expect.poll(async () => Number(await remainingTile(p).textContent())).toBeLessThanOrEqual(Math.max(0, before - 3));
    await pause(p);

    await p.getByRole("button", { name: /Cycle time/ }).click();
    await pause(p, 0.6);
    await p.getByLabel("Cycle time (s)").pressSequentially("31.5", { delay: 120 });
    await pause(p);
    await p.getByRole("button", { name: /^Record/ }).click();
    await expect(receipt(p, "Cycle time")).toBeVisible();
    await pause(p);

    await p.getByRole("button", { name: /\+ Reject/ }).click();
    await expect(p.getByLabel("Reject", { exact: true })).toHaveValue("1");
    await pause(p);
    await p.getByRole("button", { name: /^Record/ }).click();
    await expect(receipt(p, "Quantity")).toBeVisible();
    await pause(p);
    // The quantity form has no comment, so the quality reason goes in as a note.
    await p.getByRole("button", { name: /Note/ }).click();
    await p.getByRole("textbox", { name: "Note" }).pressSequentially("Reject: burr on flange edge, part tagged", { delay: 40 });
    await pause(p);
    await p.getByRole("button", { name: /^Record/ }).click();
    await expect(receipt(p, "Note")).toBeVisible();
    await pause(p);

    await p.getByRole("button", { name: /History/ }).click();
    let history = p.getByRole("dialog", { name: "Records this shift" });
    await expect(history.getByText("good 3 · reject 0").first()).toBeVisible();
    await expect(history.getByText("good 0 · reject 1").first()).toBeVisible();
    await expect(history.getByText("“Reject: burr on flange edge, part tagged”").first()).toBeVisible();
    // The signed-in operator sees their own entries as "by you".
    await expect(history.getByText(/^by you/).first()).toBeVisible();
    await pause(p, 2);
    await shot(p, "step-2-operator-one-press-entries");

    // 3. Sanjay Patil corrects the cycle time 31.5 s -> 30.5 s from History.
    const original = history.getByRole("listitem").filter({ hasText: "31.5 s" }).filter({ has: p.getByRole("button", { name: "Correct" }) }).first();
    await original.scrollIntoViewIfNeeded();
    await pause(p);
    await original.getByRole("button", { name: "Correct" }).click();
    const correction = p.getByRole("form", { name: /Correct Cycle time/ });
    await expect(correction.getByLabel("Cycle time (s)")).toHaveValue("31.5");
    await pause(p);
    await correction.getByLabel("Cycle time (s)").fill("");
    await correction.getByLabel("Cycle time (s)").pressSequentially("30.5", { delay: 120 });
    await pause(p);
    await correction.getByRole("button", { name: /Submit correction/ }).click();
    await expect(receipt(p, "Cycle time")).toBeVisible();
    await pause(p);
    await p.getByRole("button", { name: /History/ }).click();
    history = p.getByRole("dialog", { name: "Records this shift" });
    const corrected = history.getByRole("listitem").filter({ hasText: "30.5 s" }).first();
    await expect(corrected).toContainText("● current");
    await expect(corrected).toContainText(/corrects [0-9a-f]{8}/);
    await expect(history.getByRole("listitem").filter({ hasText: "31.5 s" }).filter({ hasText: "✕ superseded" }).first()).toBeVisible();
    await pause(p, 2);
    await shot(p, "step-3-operator-one-correction");
    await history.getByRole("button", { name: "Close" }).click();
    await done(op1, "02-operator-one");

    // 4. Meera Joshi on the weld tablet: an out-of-range temperature is refused before sending.
    const op2 = await person(browser, "operator-2", TABLET);
    p = op2.page;
    let posts = 0;
    p.on("request", (r) => {
        if (r.method() === "POST" && /\/workstations\/[^/]+\/records$/.test(new URL(r.url()).pathname)) posts++;
    });
    await openStation(p, "weld-01");
    await ensureRunning(p);
    await p.getByRole("button", { name: "Quality", exact: true }).click();
    const temp = p.getByLabel("Weld temperature (C)");
    await expect(temp).toBeVisible();
    await pause(p);
    await temp.pressSequentially("520", { delay: 150 });
    await p.getByRole("radio", { name: "accept", exact: true }).check({ force: true });
    await pause(p);
    posts = 0;
    await p.getByRole("button", { name: /^Record/ }).click();
    await expect(p.getByRole("alert").filter({ hasText: /\S/ }).getByRole("link", { name: "Weld temperature: outside 350–450" })).toBeVisible();
    expect(posts).toBe(0);
    await expect(receipt(p, "Quality")).toHaveCount(0);
    await pause(p, 2);
    await shot(p, "step-4a-operator-two-weld-out-of-range");
    await temp.fill("");
    await temp.pressSequentially("412", { delay: 150 });
    await pause(p);
    await p.getByRole("button", { name: /^Record/ }).click();
    await expect(receipt(p, "Quality measurement")).toBeVisible();
    expect(posts).toBe(1);
    await pause(p);
    await p.getByRole("button", { name: /History/ }).click();
    await expect(p.getByRole("dialog", { name: "Records this shift" }).getByText("weld_temp_c 412 C · disposition accept").first()).toBeVisible();
    await pause(p, 2);
    await shot(p, "step-4b-operator-two-weld-recorded");
    await p.getByRole("dialog", { name: "Records this shift" }).getByRole("button", { name: "Close" }).click();
    await done(op2, "03-operator-two");

    // 5. Shift lead reviews: who recorded on press-01, then summary and handover.
    const lead2 = await person(browser, "shift-lead", DESKTOP);
    p = lead2.page;
    await openStation(p, "press-01");
    await p.getByRole("button", { name: /History/ }).click();
    await expect(p.getByRole("dialog", { name: "Records this shift" }).getByText(/by Sanjay Patil · [0-9a-f]{4}/).first()).toBeVisible();
    await pause(p, 2);
    await p.getByRole("dialog", { name: "Records this shift" }).getByRole("button", { name: "Close" }).click();
    await openCurrentShift(p);
    const planUrl = p.url();
    await pause(p);
    await p.getByRole("button", { name: "Generate summary" }).click();
    await expect(p.getByText(/(Generated|Already generated) summary r\d+|Nothing changed since the latest revision/)).toBeVisible();
    await expect(p.getByText(/Summary r\d+ of plan r\d+/)).toBeVisible();
    for (const station of ["press-01", "weld-01"]) {
        const row = p.getByRole("row").filter({ has: p.getByRole("link", { name: station }) });
        await expect(row.getByText(/(Ready|Limited|Not ready|Invalid)$/)).toBeVisible();
    }
    // Recorded Remaining is Planned minus Good for press-01 (not clamped below target).
    const press = p.getByRole("row").filter({ has: p.getByRole("link", { name: "press-01" }) }).getByRole("cell");
    await expect(press.nth(3)).toHaveText(/^\d+$/);
    const [planned, good, remaining] = await Promise.all([2, 3, 5].map(async (i) => Number(await press.nth(i).textContent())));
    expect(remaining).toBe(Math.max(0, planned - good));
    // The cycle mean is shown to 1 decimal; the exact value stays in the tooltip (rule 04).
    await expect(press.nth(6)).toHaveText(/^\d+\.\d$/);
    await expect(press.nth(6).locator("span")).toHaveAttribute("title", /^Exact: \d+(\.\d+)?$/);
    // Meera Joshi's weld measurement is visible in the summary, labelled as recorded (never a release).
    const weldQuality = p.getByRole("row").filter({ has: p.getByRole("link", { name: "weld-01" }) }).getByRole("cell").nth(7);
    await expect(weldQuality).toContainText(/\d+ measurements? · last weld_temp_c 412 C · accept \(as recorded\)/);
    // Station-specific readiness reasons sit on their rows; the header keeps only shift-level ones.
    await expect(p.locator(".summaryline")).not.toContainText(/No quantities yet|No cycle times yet/);
    await expect(p.locator(".summaryline")).toContainText("Shift in progress");
    await pause(p, 2);
    await shot(p, "step-5a-shift-lead-summary");
    await p.getByRole("button", { name: "Handover", exact: true }).click();
    await pause(p);
    await p.getByRole("button", { name: "Generate handover" }).click();
    await expect(p.getByText(/(Generated|Already generated) handover r\d+/)).toBeVisible();
    await expect(p.getByRole("heading", { name: /Handover r\d+ →/ })).toBeVisible();
    await expect(p.getByText("✓ Carried over").first()).toBeVisible();
    // The handover header keeps only shift-level reasons; station reasons sit on the rows.
    const handoverMeta = p.getByRole("heading", { name: /Handover r\d+ →/ }).locator("xpath=../following-sibling::p[1]");
    await expect(handoverMeta).not.toContainText(/No quantities yet|No cycle times yet|Late entries|Corrections present/);
    await p.getByRole("heading", { name: /Handover r\d+ →/ }).scrollIntoViewIfNeeded();
    await pause(p, 2);
    await shot(p, "step-5b-shift-lead-handover");

    // 6. Shift lead revises a target, cannot approve their own revision; another supervisor can.
    await p.getByRole("button", { name: "Revise" }).click();
    const qty = p.getByLabel("Row 1 target quantity");
    const current = Number(await qty.inputValue());
    // Small change that stays within the job order target (JOB_ORDER_MISMATCH) and does not drift.
    await qty.fill(String(current >= 120 ? current - 1 : current + 1));
    await pause(p);
    await p.getByRole("button", { name: "Save draft revision" }).click();
    const saved = p.getByText(/Saved draft r(\d+)/);
    await expect(saved).toBeVisible();
    const revision = /r(\d+)/.exec((await saved.textContent()) ?? "")![1];
    await expect(p.getByText(`Draft · rev ${revision} awaiting approval`)).toBeVisible();
    await pause(p);
    await p.getByRole("button", { name: `Approve r${revision}` }).click();
    const dialog = p.getByRole("dialog", { name: `Approve revision ${revision}?` });
    await expect(dialog.getByText(/approver must not be a submitter/)).toBeVisible();
    await pause(p);
    await dialog.getByRole("button", { name: `Approve r${revision}` }).click();
    const refusal = p.getByRole("alert").filter({ hasText: /\S/ });
    await expect(refusal.getByText("You submitted this revision. Another supervisor must approve it.")).toBeVisible();
    await expect(refusal).not.toContainText("You do not have permission");
    await pause(p);
    await refusal.getByText("Details").click();
    await expect(refusal.getByText("SELF_REVIEW_FORBIDDEN")).toBeVisible();
    await pause(p, 2);
    await shot(p, "step-6a-shift-lead-self-approval-refused");
    await done(lead2, "04-shift-lead-review");

    const approver = await person(browser, "shift-approver", DESKTOP);
    p = approver.page;
    await expect(p.getByRole("banner").getByText("Ananya Kulkarni", { exact: true })).toBeVisible();
    await openCurrentShift(p);
    expect(p.url()).toBe(planUrl);
    await expect(p.getByText(`Draft · rev ${revision} awaiting approval`)).toBeVisible();
    await pause(p);
    await p.getByRole("button", { name: `Approve r${revision}` }).click();
    await pause(p);
    await p.getByRole("dialog").getByRole("button", { name: `Approve r${revision}` }).click();
    await expect(p.getByText(`Approved r${revision}`)).toBeVisible();
    await expect(p.getByText(`Approved · rev ${revision}`).first()).toBeVisible();
    await pause(p, 2);
    await shot(p, "step-6b-shift-approver-approved");
    await done(approver, "05-shift-approver");
});
