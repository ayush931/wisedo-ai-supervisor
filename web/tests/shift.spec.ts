// Supervisor shift page against the real backend and the WIS-520 demo seed
// (plan submitted by shift-lead, approved by shift-approver).
import { test, expect } from "@playwright/test";
import { openSeedShift as openCurrentShift, openSite, signIn, signOut } from "./session";

test("summary is generated with readiness and notes kept apart from measured data", async ({ page }) => {
    await signIn(page, "shift-lead");
    await openSite(page);
    await openCurrentShift(page);
    await page.getByRole("button", { name: "Generate summary" }).click();
    await expect(page.getByText(/(Generated|Already generated) summary r\d+|Nothing changed since the latest revision/)).toBeVisible();
    await expect(page.getByText(/Summary r\d+ of plan r\d+/)).toBeVisible();
    await expect(page.getByText(/(Ready|Limited|Not ready|Invalid)$/).first()).toBeVisible();
    await expect(page.getByRole("heading", { name: "Operator notes · not measured data" })).toBeVisible();
});

test("a submitter cannot approve their own revision; a different supervisor can", async ({ page, browser }) => {
    await signIn(page, "shift-lead");
    await openSite(page);
    await openCurrentShift(page);
    await page.getByRole("button", { name: "Generate summary" }).click();
    await expect(page.getByText(/Summary r\d+ of plan r\d+/)).toBeVisible();
    await page.getByRole("button", { name: "Revise" }).click();
    const qty = page.getByLabel("Row 1 target quantity");
    const before = await qty.inputValue();
    // A shift target may not exceed the job order target (JOB_ORDER_MISMATCH), so lower it.
    await qty.fill(String(Math.max(1, Number(before) - 1)));
    await page.getByRole("button", { name: "Save draft revision" }).click();
    await expect(page.getByText(/Saved draft r(\d+)/)).toBeVisible();
    // Regression: the summarized row kept showing the draft's planned quantity next to the
    // summary's remaining. Planned must stay the value of the revision that was summarized.
    const stations = page.getByRole("table").first();
    await expect(stations.getByRole("row").nth(1).getByRole("cell").nth(2)).toHaveText(before);

    await page.getByRole("button", { name: /^Approve r\d+/ }).click();
    const dialog = page.getByRole("dialog", { name: /Approve revision \d+/ });
    await expect(dialog.getByText("State revision")).toBeVisible();
    await expect(dialog.getByText(/approver must not be a submitter/)).toBeVisible();
    await dialog.getByRole("button", { name: /^Approve r\d+/ }).click();
    await expect(page.getByRole("alert").getByText("You submitted this revision. Another supervisor must approve it.")).toBeVisible();
    await page.getByRole("alert").getByText("Details").click();
    await expect(page.getByRole("alert").getByText("SELF_REVIEW_FORBIDDEN")).toBeVisible();
    const plan = page.url();
    await signOut(page);

    // The approver is a different person on their own device (own browser and IdP session).
    const other = await (await browser.newContext()).newPage();
    await signIn(other, "shift-approver");
    await other.goto(plan); // deep link to the same plan
    await other.getByRole("button", { name: /^Approve r\d+/ }).click();
    await other.getByRole("dialog").getByRole("button", { name: /^Approve r\d+/ }).click();
    await expect(other.getByText(/Approved r\d+/)).toBeVisible();
    await expect(other.getByText(/Approved · rev \d+/).first()).toBeVisible();
    // The summary on screen was computed from the earlier revision; say so until it is regenerated.
    const stale = other.getByText(/Plan r\d+ approved after this summary/);
    await expect(stale).toBeVisible();
    await other.getByRole("button", { name: "Generate summary" }).click();
    await expect(stale).toBeHidden();
});
