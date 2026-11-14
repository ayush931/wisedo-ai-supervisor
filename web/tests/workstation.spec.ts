// Operator tablet against the real backend and the WIS-520 demo seed (press-01, operator-1).
import { test, expect } from "@playwright/test";
import { clientNavigate, openSite, section, signIn } from "./session";

test("operator records a good quantity with a durable receipt and sees it in history", async ({ page }) => {
    let stationListRequests = 0;
    let stationProfileReads = 0;
    page.on("request", (request) => {
        const path = new URL(request.url()).pathname;
        if (request.method() === "GET" && /^\/v1\/sites\/[^/]+\/workstations$/.test(path)) stationListRequests++;
        if (request.method() === "GET" && /^\/v1\/sites\/[^/]+\/workstations\/[^/]+\/profile$/.test(path)) stationProfileReads++;
    });
    await signIn(page, "operator-1");
    await openSite(page);
    // WIS-290: the header names the signed-in person (IdP display name), not a subject.
    await expect(page.getByRole("banner").getByText("Sanjay Patil", { exact: true })).toBeVisible();
    await section(page, "Workstations");
    await expect(page.getByRole("link", { name: "press-01" })).toBeVisible();
    await expect.poll(() => stationListRequests).toBe(1);
    expect(stationProfileReads).toBe(0);
    await page.getByRole("link", { name: "press-01" }).click();
    await expect(page.getByRole("heading", { name: "press-01" })).toBeVisible();
    await expect(page.getByText("Current job order")).toBeVisible();
    await expect(page.getByText(/^Refreshed \d{2}:\d{2} \S+/)).toBeVisible();

    await page.getByRole("button", { name: /\+ Good/ }).click();
    await page.getByRole("button", { name: "Add 1 good" }).click(); // 1 + 1
    await expect(page.getByLabel("Good", { exact: true })).toHaveValue("2");
    await page.getByLabel("Good", { exact: true }).press("Enter");
    const receipt = page.getByRole("status").filter({ hasText: /Recorded: Quantity/ });
    await expect(receipt).toBeVisible();

    await page.getByRole("button", { name: /History/ }).click();
    const history = page.getByRole("dialog", { name: "Records this shift" });
    await expect(history.getByText("good 2 · reject 0").first()).toBeVisible();
    await expect(history.getByText(/by you/).first()).toBeVisible();
    await history.getByRole("button", { name: "Close" }).click();
});

test("WIS-290 a supervisor sees the operator's name in station record history", async ({ page, browser }) => {
    // The directory learns a name at login; the operator signs in on their own device first.
    const operator = await browser.newContext();
    await signIn(await operator.newPage(), "operator-1");
    await operator.close();
    await signIn(page, "shift-lead");
    await openSite(page);
    await expect(page.getByRole("banner").getByText("Rahul Deshmukh", { exact: true })).toBeVisible();
    await section(page, "Workstations");
    await page.getByRole("link", { name: "press-01" }).click();
    await page.getByRole("button", { name: /History/ }).click();
    const history = page.getByRole("dialog", { name: "Records this shift" });
    await expect(history.getByText(/by Sanjay Patil · [0-9a-f]{4}/).first()).toBeVisible();
    await expect(history.getByText(/by member [0-9a-f]{8}/)).toHaveCount(0);
});

// DESIGN-E Q1: the CSV import needs workstation UUIDs; the ID shown must be the real one, in full, and copy exactly.
test("workstation ID is shown in full and copies to the clipboard from the list and the station page", async ({ page, context }) => {
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    await signIn(page, "operator-1");
    await openSite(page);
    await section(page, "Workstations");
    const site = new URL(page.url()).pathname.split("/")[2];
    const res = await page.request.get(`/v1/sites/${site}/workstations?limit=100`);
    expect(res.status()).toBe(200);
    const body = (await res.json()) as { workstations: { workstation_id: string; display_name?: string }[] };
    const id = body.workstations.find((w) => w.display_name === "press-01")?.workstation_id;
    expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
    const readClipboard = () => page.evaluate(() => navigator.clipboard.readText());

    const row = page.getByRole("row").filter({ has: page.getByRole("link", { name: "press-01" }) });
    await expect(row.getByTitle("Workstation ID of press-01")).toHaveText(id!);
    await page.evaluate(() => navigator.clipboard.writeText(""));
    await row.getByRole("button", { name: "Copy workstation ID of press-01" }).click();
    await expect(row.getByText("Copied", { exact: true })).toBeVisible();
    expect(await readClipboard()).toBe(id);

    await page.getByRole("link", { name: "press-01" }).click();
    await expect(page.getByRole("heading", { name: "press-01" })).toBeVisible();
    expect(new URL(page.url()).pathname.endsWith(`/workstations/${id}`)).toBe(true);
    await expect(page.getByTitle("Workstation ID of press-01")).toHaveText(id!);
    await page.evaluate(() => navigator.clipboard.writeText(""));
    await page.getByRole("button", { name: "Copy workstation ID of press-01" }).click();
    await expect(page.getByText("Copied", { exact: true })).toBeVisible();
    expect(await readClipboard()).toBe(id);
});

test("client validation names the field and sends nothing", async ({ page }) => {
    await signIn(page, "operator-1");
    await openSite(page);
    await section(page, "Workstations");
    await page.getByRole("link", { name: "press-01" }).click();
    await page.getByRole("button", { name: /Cycle time/ }).click();
    await page.getByLabel("Cycle time (s)").fill("0");
    await page.getByRole("button", { name: /^Record/ }).click();
    await expect(page.getByRole("alert").filter({ hasText: /\S/ }).getByRole("link", { name: /Cycle time: seconds above 0/ })).toBeVisible();
    await expect(page.getByText(/Recorded: Cycle time/)).toHaveCount(0);
});

test("offline disables entry and keeps the draft; reconnect re-enables", async ({ page, context }) => {
    await signIn(page, "operator-1");
    await openSite(page);
    await section(page, "Workstations");
    await page.getByRole("link", { name: "press-01" }).click();
    await page.getByRole("button", { name: /Note/ }).click();
    await page.getByRole("textbox", { name: "Note" }).fill("draft kept offline");
    await context.setOffline(true);
    await expect(page.getByText(/Offline · entry disabled, draft kept/)).toBeVisible();
    await expect(page.getByRole("button", { name: /^Record/ })).toBeDisabled();
    await expect(page.getByRole("textbox", { name: "Note" })).toHaveValue("draft kept offline");
    await context.setOffline(false);
    await expect(page.getByRole("button", { name: /^Record/ })).toBeEnabled();
});

test("a site outside the session is non-disclosing; an expired session signs out to sign-in", async ({ page, context }) => {
    await signIn(page, "operator-1");
    await clientNavigate(page, "/sites/00000000-0000-4000-8000-999999999999/workstations");
    await expect(page.getByRole("heading", { name: "Not found" })).toBeVisible();
    await openSiteFromSites(page);
    await section(page, "Workstations");
    await expect(page.getByRole("link", { name: "press-01" })).toBeVisible();
    // The session ends while the list is open; the next protected read is a 401.
    await context.clearCookies();
    await page.getByRole("link", { name: "press-01" }).click();
    await expect(page.getByRole("link", { name: "Sign in", exact: true })).toBeVisible();
    await expect(page.getByText(/Your session ended/)).toBeVisible();
});

async function openSiteFromSites(page: import("@playwright/test").Page) {
    await page.getByRole("link", { name: "Back to your sites" }).click();
    await openSite(page);
}

// WIS-709: the server requires authorizes_release on a disposition and refuses true; the UI sent
// neither, so every disposition was refused with 422. Each one must send false and get a receipt.
for (const [disposition, label] of [["accept", "Accept"], ["hold", "Hold"]] as const) {
    test(`WIS-709 operator records a ${disposition} disposition with authorizes_release false`, async ({ page }) => {
        await signIn(page, "operator-1");
        await openSite(page);
        await section(page, "Workstations");
        await page.getByRole("link", { name: "press-01" }).click();
        await expect(page.getByText("Current job order")).toBeVisible();
        await page.getByRole("button", { name: "Quality", exact: true }).click();
        await page.getByRole("radio", { name: "Disposition", exact: true }).check({ force: true });
        await page.getByRole("radio", { name: label, exact: true }).check({ force: true });
        const posted = page.waitForRequest((r) => r.method() === "POST" && /\/workstations\/[^/]+\/records$/.test(new URL(r.url()).pathname));
        await page.getByRole("button", { name: /^Record/ }).click();
        const body = (await posted).postDataJSON() as { kind: string; disposition: string; authorizes_release?: boolean };
        expect(body).toMatchObject({ kind: "quality_disposition_recorded", disposition, authorizes_release: false });
        await expect(page.getByRole("status").filter({ hasText: "Recorded: Quality disposition (as recorded)" })).toBeVisible();
    });
}
