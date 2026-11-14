import { readFile } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { expect, test, type Page } from "@playwright/test";
import { openSite, section, signIn } from "./session";

const header = "kind,ref,id,revision,display_name,active,unit,line,item,steps,time_zone,shifts";
const lineRow = (ref: string, name: string, active = true) => `line,${ref},,,${name},${active},,,,,,`;
const csvFile = (rows: string[]) => ({
    name: "configuration.csv",
    mimeType: "text/csv",
    buffer: Buffer.from([header, ...rows].join("\n")),
});

async function currentSiteID(page: Page) {
    const match = /^\/sites\/([^/]+)(?:\/|$)/.exec(new URL(page.url()).pathname);
    if (!match) throw new Error(`Current page is not inside a site: ${page.url()}`);
    return match[1];
}

async function createLine(page: Page, siteID: string, displayName: string, active = true) {
    return page.evaluate(async ({ siteID, displayName, active }) => {
        const sessionResponse = await fetch("/v1/me");
        if (!sessionResponse.ok) throw new Error(`Session read failed (${sessionResponse.status}).`);
        const me = await sessionResponse.json() as { csrf_token: string; sites: { id: string }[] };
        if (!me.sites.some((site) => site.id === siteID)) throw new Error("Selected UI site is not authorized in the session.");
        const response = await fetch(`/v1/sites/${siteID}/lines`, {
            method: "POST",
            credentials: "same-origin",
            headers: {
                "Content-Type": "application/json",
                "X-CSRF-Token": me.csrf_token,
                "Idempotency-Key": crypto.randomUUID(),
                "If-Match": '"0"',
            },
            body: JSON.stringify({ schema_version: 1, display_name: displayName, active }),
        });
        if (!response.ok) {
            const problem = await response.json().catch(() => ({})) as { code?: string };
            throw new Error(`Line seed failed (${response.status} ${problem.code ?? "request failed"}).`);
        }
    }, { siteID, displayName, active });
}

async function openConfigurationImport(page: Page) {
    await signIn(page, "foundation-reviewer");
    await openSite(page);
    await section(page, "Master data");
    await page.getByRole("button", { name: "Import CSV", exact: true }).click();
    return currentSiteID(page);
}

test("owner reviews a dry run, applies clean rows, and rechecks stale data", async ({ page }) => {
    const suffix = randomUUID();
    const existing = `Existing ${suffix}`;
    const imported = `Imported ${suffix}`;
    await signIn(page, "foundation-reviewer");
    await openSite(page);
    await section(page, "Master data");
    await page.getByRole("button", { name: "Import CSV", exact: true }).click();
    const siteID = await currentSiteID(page);
    await createLine(page, siteID, existing);

    const input = page.getByLabel("CSV file");
    await input.setInputFiles(csvFile([
        lineRow("new_line", imported),
        lineRow("existing_line", existing),
        lineRow("invalid_line", "=SUM(A1)"),
    ]));
    await expect(page.getByText("1 accepted", { exact: true })).toBeVisible();
    await expect(page.getByText("1 duplicate", { exact: true })).toBeVisible();
    await expect(page.getByText("1 rejected", { exact: true })).toBeVisible();
    await expect(page.getByLabel("Rejected only")).toBeChecked();
    const report = page.getByRole("table", { name: "CSV import report" });
    const rejected = report.getByRole("row").filter({ hasText: "INVALID_MASTER_DATA" });
    await expect(rejected).toContainText("Invalid master data");
    await expect(rejected).toContainText("display_name");
    await expect(rejected.getByRole("rowheader")).toHaveText("3");
    await expect(page.getByRole("button", { name: "Apply 1 change" })).toBeDisabled();
    await page.getByLabel("Rejected only").uncheck();
    await expect(report.getByRole("row")).toHaveCount(4);
    await expect(report.getByRole("row").filter({ hasText: "Accepted" })).toBeVisible();
    await expect(report.getByRole("row").filter({ hasText: "Duplicate" })).toBeVisible();
    const duplicateRow = report.getByRole("row").filter({ hasText: "Duplicate" });
    const objectLink = duplicateRow.getByRole("link", { name: "Open object" });
    const objectHref = await objectLink.getAttribute("href");
    if (!objectHref) throw new Error("Duplicate import row did not link to its existing object.");
    await objectLink.click();
    await expect(page.getByRole("table", { name: "Lines master data" }).getByText(existing)).toBeVisible();
    await expect(page.locator(objectHref)).toBeFocused();
    await page.getByRole("button", { name: "Import CSV", exact: true }).click();
    await page.getByLabel("CSV file").setInputFiles(csvFile([
        lineRow("new_line", imported),
        lineRow("existing_line", existing),
        lineRow("invalid_line", "=SUM(A1)"),
    ]));
    await expect(page.getByText("1 accepted", { exact: true })).toBeVisible();
    await expect(page.getByText("1 duplicate", { exact: true })).toBeVisible();
    await expect(page.getByText("1 rejected", { exact: true })).toBeVisible();
    await expect(page.getByLabel("Rejected only")).toBeChecked();
    await expect(page.getByRole("button", { name: "Apply 1 change" })).toBeDisabled();
    const downloadPromise = page.waitForEvent("download");
    await page.getByRole("button", { name: "Download errors.csv" }).click();
    const download = await downloadPromise;
    expect(download.suggestedFilename()).toBe("configuration-import-errors.csv");
    const downloadPath = await download.path();
    if (!downloadPath) throw new Error("Playwright did not retain the errors CSV download.");
    const errorsCSV = await readFile(downloadPath, "utf8");
    expect(errorsCSV).toContain("INVALID_MASTER_DATA");
    expect(errorsCSV).toContain("display_name");
    expect(errorsCSV).not.toContain("=SUM(A1)");

    const cleanLine = `Clean ${suffix}`;
    await input.setInputFiles(csvFile([lineRow("clean_line", cleanLine)]));
    await expect(page.getByText("1 accepted", { exact: true })).toBeVisible();
    await expect(page.getByText("0 rejected", { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Apply 1 change" }).click();
    await expect(page.getByText(/Import applied\. Report:/)).toBeVisible();
    await expect(page.getByRole("button", { name: "Apply 1 change" })).toBeDisabled();
    await page.getByRole("button", { name: /^Lines/ }).click();
    await expect(page.getByRole("table", { name: "Lines master data" }).getByText(cleanLine)).toBeVisible();

    await page.getByRole("button", { name: "Import CSV", exact: true }).click();
    const staleName = `Stale ${suffix}`;
    await input.setInputFiles(csvFile([lineRow("stale_line", staleName)]));
    await expect(page.getByRole("button", { name: "Apply 1 change" })).toBeEnabled();
    await createLine(page, siteID, staleName, false);
    await page.getByRole("button", { name: "Apply 1 change" }).click();
    await expect(page.getByText(/Site data changed since the check, re-checked/)).toBeVisible();
    await expect(page.getByText(/row 1: accepted → rejected \(DISPLAY_NAME_EXISTS\)/)).toBeVisible();
    await expect(page.getByText("1 rejected", { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Apply 0 changes" })).toBeDisabled();
});

test("ambiguous apply retry reuses its key and a new file gets a new key", async ({ page }) => {
    const suffix = randomUUID();
    const firstName = `Retry ${suffix}`;
    const secondName = `Next ${suffix}`;
    const siteID = await openConfigurationImport(page);
    const keys: string[] = [];
    let abortNext = true;
    await page.route("**/configuration-imports", async (route) => {
        if (!new URL(route.request().url()).pathname.endsWith("/configuration-imports")) return route.continue();
        keys.push(route.request().headers()["idempotency-key"] ?? "");
        if (abortNext) {
            abortNext = false;
            return route.abort("failed");
        }
        return route.continue();
    });

    const input = page.getByLabel("CSV file");
    await input.setInputFiles(csvFile([lineRow("retry_line", firstName)]));
    await expect(page.getByText("1 accepted", { exact: true })).toBeVisible();
    const apply = page.getByRole("button", { name: "Apply 1 change" });
    await apply.click();
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("Connection lost before the server answered.");
    await expect(apply).toBeEnabled();
    await apply.click();
    await expect(page.getByText(/Import applied\. Report:/)).toBeVisible();
    await expect(apply).toBeDisabled();
    expect(keys).toHaveLength(2);
    expect(keys[0]).toBeTruthy();
    expect(keys[1]).toBe(keys[0]);

    await page.getByRole("button", { name: /^Lines/ }).click();
    const lines = page.getByRole("table", { name: "Lines master data" });
    await expect(lines.getByRole("row").filter({ hasText: firstName })).toHaveCount(1);
    await page.getByRole("button", { name: "Import CSV", exact: true }).click();
    await input.setInputFiles(csvFile([lineRow("next_line", secondName)]));
    await expect(page.getByText("1 accepted", { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Apply 1 change" }).click();
    await expect(page.getByText(/Import applied\. Report:/)).toBeVisible();
    expect(keys).toHaveLength(3);
    expect(keys[2]).toBeTruthy();
    expect(keys[2]).not.toBe(keys[1]);
    await page.getByRole("button", { name: /^Lines/ }).click();
    await expect(lines.getByRole("row").filter({ hasText: firstName })).toHaveCount(1);
    await expect(lines.getByRole("row").filter({ hasText: secondName })).toHaveCount(1);
});

test("refreshes the report after apply discovers rejected rows", async ({ page }) => {
    const suffix = randomUUID();
    const displayName = `Concurrent ${suffix}`;
    const siteID = await openConfigurationImport(page);
    let injected = false;
    await page.route("**/configuration-imports", async (route) => {
        if (!new URL(route.request().url()).pathname.endsWith("/configuration-imports") || injected) return route.continue();
        injected = true;
        await createLine(page, siteID, displayName, false);
        await route.fulfill({
            status: 422,
            headers: { "content-type": "application/problem+json" },
            body: JSON.stringify({
                type: "about:blank",
                title: "Import contains rejected rows",
                status: 422,
                code: "IMPORT_HAS_REJECTED_ROWS",
                correlation_id: "configuration-import-test",
            }),
        });
    });
    const input = page.getByLabel("CSV file");
    await input.setInputFiles(csvFile([lineRow("concurrent_line", displayName)]));
    await expect(page.getByText("1 accepted", { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Apply 1 change" }).click();
    await expect(page.getByText(/Apply found rejected rows; the check was refreshed\./)).toBeVisible();
    await expect(page.getByText("1 rejected", { exact: true })).toBeVisible();
    const report = page.getByRole("table", { name: "CSV import report" });
    await expect(report.getByRole("row").filter({ hasText: "DISPLAY_NAME_EXISTS" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Apply 0 changes" })).toBeDisabled();
});

test("keeps file selection busy during an errors download", async ({ page }) => {
    await openConfigurationImport(page);
    const input = page.getByLabel("CSV file");
    await input.setInputFiles(csvFile([lineRow("invalid_line", "=SUM(A1)")]));
    await expect(page.getByText("1 rejected", { exact: true })).toBeVisible();

    let release!: () => void;
    let signalStarted!: () => void;
    const waiting = new Promise<void>((resolve) => { release = resolve; });
    const started = new Promise<void>((resolve) => { signalStarted = resolve; });
    await page.route("**/configuration-imports/dry-runs", async (route) => {
        if (!(route.request().headers().accept ?? "").includes("text/csv")) return route.continue();
        signalStarted();
        await waiting;
        await route.fulfill({
            status: 200,
            headers: { "content-type": "text/csv", etag: '"changed-during-download"' },
            body: "row,reason,path\n1,INVALID_MASTER_DATA,display_name\n",
        });
    });

    const download = page.waitForEvent("download");
    await page.getByRole("button", { name: "Download errors.csv" }).click();
    await started;
    try {
        const region = page.getByRole("region", { name: "Import CSV" });
        await expect(region).toHaveAttribute("aria-busy", "true");
        await expect(input).toBeDisabled();
        await page.locator(".configuration-import-drop").evaluate((element, contents) => {
            const data = new DataTransfer();
            data.items.add(new File([contents], "replacement.csv", { type: "text/csv" }));
            element.dispatchEvent(new DragEvent("drop", { bubbles: true, cancelable: true, dataTransfer: data }));
        }, [header, lineRow("replacement_line", "Replacement")].join("\n"));
        await expect(page.locator(".configuration-import-drop")).not.toContainText("replacement.csv");
    } finally {
        release();
    }
    await download;
    await expect(page.getByRole("region", { name: "Import CSV" })).toHaveAttribute("aria-busy", "false");
    await expect(page.locator(".configuration-import-drop")).not.toContainText("replacement.csv");
});

test("viewer has no configuration import control in a fresh browser context", async ({ browser }) => {
    const viewerContext = await browser.newContext({ baseURL: process.env.SUPERVISOR_PUBLIC_ORIGIN ?? "http://127.0.0.1:58176" });
    try {
        const viewer = await viewerContext.newPage();
        await signIn(viewer, "read-only-viewer");
        await openSite(viewer);
        await section(viewer, "Master data");
        await expect(viewer.getByRole("heading", { name: "Master data" })).toBeVisible();
        await expect(viewer.getByRole("button", { name: "Import CSV", exact: true })).toHaveCount(0);
        await expect(viewer.getByLabel("CSV file")).toHaveCount(0);
    } finally {
        await viewerContext.close();
    }
});
