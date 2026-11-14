// WIS-707: customers run with no internet, so the built app must load every top-level screen
// without a request to any host other than its own (no web fonts, CDN scripts or styles).
// The same walk also fails on any Content-Security-Policy violation under the server's real
// header: the Astryx Layer slide rules (a runtime <style>) were blocked by `style-src 'self'`,
// so every tooltip and menu lost its @position-try fallbacks (WIS-709 facility run). A layer
// mounts only on some data-dependent screens, so station.spec.ts holds the deterministic case.
// Sign-in goes through the identity provider on its own origin, so recording starts after it;
// the app is then loaded again from "/" (the backend has no SPA fallback for deep links).
import { test, expect } from "@playwright/test";
import { openSite, signIn } from "./session";

test("every top-level screen loads with no request to another host and no CSP violation", async ({ page, baseURL }) => {
    await signIn(page, "shift-lead");
    // Finish reads on the identity callback page before observing the fresh app origin.
    await page.waitForLoadState("networkidle");
    const origin = new URL(baseURL ?? page.url()).origin;
    const foreign: string[] = [];
    page.on("request", (r) => {
        const url = new URL(r.url());
        if (url.protocol === "data:" || url.protocol === "blob:") return;
        if (url.origin !== origin) foreign.push(r.url());
    });
    const csp: string[] = [];
    page.on("console", (m) => {
        if (m.type() === "error" && /Content Security Policy/i.test(m.text())) csp.push(m.text());
    });

    // Load the app again under observation so its own scripts, styles and fonts are requested.
    await page.goto("/");
    await openSite(page);
    const tabs = page.getByRole("navigation", { name: "Site sections" });
    const tabNames = (await tabs.getByRole("link").allInnerTexts()).map((t) => t.trim());
    expect(tabNames.length).toBeGreaterThan(0);
    let screens = 0;
    async function open(nav: string, name: string) {
        const link = page.getByRole("navigation", { name: nav }).getByRole("link", { name, exact: true });
        const href = await link.getAttribute("href");
        expect(href).toBeTruthy();
        await link.click();
        await expect(page).toHaveURL(new RegExp(`${href}$`));
        await expect(page.getByRole("main")).toBeVisible();
        screens++;
    }
    for (const tab of tabNames) {
        await open("Site sections", tab);
        const sub = page.getByRole("navigation", { name: `${tab} pages` });
        if (!(await sub.isVisible())) continue;
        for (const name of (await sub.getByRole("link").allInnerTexts()).map((t) => t.trim())) await open(`${tab} pages`, name);
    }
    await page.waitForLoadState("networkidle");
    expect(screens).toBeGreaterThan(tabNames.length);
    expect(foreign).toEqual([]);
    expect(csp).toEqual([]);
});
