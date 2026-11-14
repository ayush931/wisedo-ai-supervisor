import { randomUUID } from "node:crypto";
import { expect, test, type Browser, type Page } from "@playwright/test";
import { openSite, section, signIn } from "./session";

// WIS-520 G(1) station-profile builder against the real backend. Every test creates its own
// line and workstation profile (fresh UUIDs) and finds it by id, so accumulated data and run
// order do not matter. A second person always uses a fresh browser context: app sign-out keeps
// the Keycloak SSO session.

type Field = { name: string; label: string; kind: string; required: boolean; unit?: string; choices?: string[]; min?: string; max?: string };
type Profile = { schema_version: 1; workstation_id: string; line_id: string; display_name: string; template: string; fields: Field[] };
type Reply = { status: number; body: { code?: string; path?: string; current_revision?: number; revisions?: { profile: Profile }[] } };

const startFields: Field[] = [
    { name: "good_count", label: "Good count", kind: "decimal", required: true, unit: "pcs", min: "0" },
    { name: "shift_ok", label: "Shift OK", kind: "boolean", required: false },
];

function siteOf(page: Page) {
    const m = /^\/sites\/([^/]+)(?:\/|$)/.exec(new URL(page.url()).pathname);
    if (!m) throw new Error(`Not inside a site: ${page.url()}`);
    return m[1];
}

// api performs one in-page call with the session's CSRF token, exactly as the app does.
async function api(page: Page, method: string, path: string, body?: unknown, ifMatch?: string): Promise<Reply> {
    return page.evaluate(
        async ({ method, path, body, ifMatch }) => {
            const me = (await (await fetch("/v1/me")).json()) as { csrf_token: string };
            const headers: Record<string, string> = {};
            if (method !== "GET") {
                headers["Content-Type"] = "application/json";
                headers["X-CSRF-Token"] = me.csrf_token;
                headers["Idempotency-Key"] = crypto.randomUUID();
            }
            if (ifMatch !== undefined) headers["If-Match"] = `"${ifMatch}"`;
            const r = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
            return { status: r.status, body: await r.json().catch(() => ({})) };
        },
        { method, path, body, ifMatch },
    );
}

// seed creates an active line and revision 1 of a new workstation profile as the signed-in owner.
async function seed(page: Page) {
    const site = siteOf(page);
    const suffix = randomUUID().slice(0, 8);
    const line = await api(page, "POST", `/v1/sites/${site}/lines`, { schema_version: 1, display_name: `PB line ${suffix}`, active: true }, "0");
    expect(line.status, JSON.stringify(line.body)).toBe(201);
    const station = randomUUID();
    const profile: Profile = {
        schema_version: 1,
        workstation_id: station,
        line_id: (line.body as { id?: string }).id!,
        display_name: `PB station ${suffix}`,
        template: "production",
        fields: startFields,
    };
    const created = await api(page, "POST", `/v1/sites/${site}/workstations/${station}/profile-revisions`, profile, "0");
    expect(created.status, JSON.stringify(created.body)).toBe(201);
    return { site, station, profile, lineName: `PB line ${suffix}`, path: `/v1/sites/${site}/workstations/${station}` };
}

async function openProfile(page: Page, site: string, station: string, heading: string) {
    await section(page, "Profiles");
    await page.locator(`a[href="/sites/${site}/profiles/${station}"]`).click();
    await expect(page.getByRole("heading", { level: 1, name: heading })).toBeVisible();
}

function countPosts(page: Page, station: string) {
    const seen: { n: number; keys: string[] } = { n: 0, keys: [] };
    page.on("request", (r) => {
        if (r.method() === "POST" && r.url().includes(`/workstations/${station}/profile-revisions`)) {
            seen.n++;
            seen.keys.push(r.headers()["idempotency-key"] ?? "");
        }
    });
    return seen;
}

async function owner(page: Page) {
    await signIn(page, "foundation-reviewer");
    await openSite(page);
    return seed(page);
}

async function person(browser: Browser, username: string) {
    const context = await browser.newContext();
    const page = await context.newPage();
    await signIn(page, username);
    await openSite(page);
    return { context, page };
}

const rows = (page: Page) => page.getByRole("list", { name: "Fields" }).getByRole("listitem");
const preview = (page: Page) => page.getByRole("complementary", { name: "Operator preview" });

test("saved profile reloads equal, and the preview follows each edit", async ({ page }) => {
    const s = await owner(page);
    await openProfile(page, s.site, s.station, s.profile.display_name);
    const posts = countPosts(page, s.station);
    await expect(preview(page)).toContainText("Good count");
    await expect(preview(page)).not.toContainText("Scrap reason");

    await page.getByRole("button", { name: "+ Add field" }).click();
    await expect(page.getByLabel("Label, field 3")).toBeFocused();
    await page.getByLabel("Label, field 3").fill("Scrap reason");
    await expect(page.getByLabel("Name, field 3")).toHaveValue("scrap_reason");
    await page.getByLabel("Kind, field 3").selectOption("choice");
    await page.getByLabel("Choices, one per line, field 3").fill("dent\nscratch");
    await rows(page).nth(2).getByLabel("Required").check();
    // Preview reflects the edit: the server resolved the new field and its choices.
    await expect(preview(page)).toContainText("Scrap reason");
    await expect(preview(page)).toContainText("dent / scratch");

    await page.getByRole("button", { name: "Move up Scrap reason" }).click();
    await expect(page.getByRole("button", { name: "Move up Scrap reason" })).toBeFocused();
    await page.getByLabel("Max, field 1").fill("500");
    await expect(preview(page)).toContainText("0 – 500");
    await expect(preview(page).locator("li")).toHaveText([/Good count/, /Scrap reason/, /Shift OK/]);

    await page.getByRole("button", { name: "Save revision 2" }).click();
    await expect(page.getByText("Saved revision 2.")).toBeVisible();
    expect(posts.n).toBe(1);

    const expected: Field[] = [
        { name: "good_count", label: "Good count", kind: "decimal", required: true, unit: "pcs", min: "0", max: "500" },
        { name: "scrap_reason", label: "Scrap reason", kind: "choice", required: true, choices: ["dent", "scratch"] },
        { name: "shift_ok", label: "Shift OK", kind: "boolean", required: false },
    ];
    const stored = await api(page, "GET", `${s.path}/profile`);
    expect(stored.body.current_revision).toBe(2);
    expect(stored.body.revisions![1].profile).toEqual({ ...s.profile, fields: expected });

    // Reload through the UI: the editor shows exactly what was stored.
    await section(page, "Workstations");
    await openProfile(page, s.site, s.station, s.profile.display_name);
    await expect(rows(page)).toHaveCount(3);
    for (const [i, f] of expected.entries()) await expect(page.getByLabel(`Label, field ${i + 1}`)).toHaveValue(f.label);
    await expect(page.getByLabel("Max, field 1")).toHaveValue("500");
    await expect(page.getByLabel("Choices, one per line, field 2")).toHaveValue("dent\nscratch");
    await expect(page.getByRole("button", { name: "Save revision 3" })).toBeDisabled();

    // The reloaded body is canonically identical: re-posting it is refused as unchanged.
    const again = await api(page, "POST", `${s.path}/profile-revisions`, stored.body.revisions![1].profile, "2");
    expect(again.status).toBe(409);
    expect(again.body.code).toBe("STATION_PROFILE_UNCHANGED");
});

test("a crafted extra member is refused 422 and nothing is stored", async ({ page }) => {
    const s = await owner(page);
    const crafted: [unknown, string, string][] = [
        [{ ...s.profile, fields: [{ ...startFields[0], script: "return 1" }, startFields[1]] }, "profile-revisions", "fields[0].*"],
        [{ ...s.profile, condition: "good_count > 5" }, "profile-revisions", "*"],
        [{ profile: { ...s.profile, fields: [{ ...startFields[0], on_change: "x" }, startFields[1]] } }, "profile-previews", "profile.fields[0].*"],
    ];
    for (const [body, endpoint, path] of crafted) {
        const r = await api(page, "POST", `${s.path}/${endpoint}`, body, endpoint === "profile-revisions" ? "1" : undefined);
        expect(r.status, endpoint).toBe(422);
        expect(r.body.code).toBe("UNSUPPORTED_STATION_CONFIGURATION");
        expect(r.body.path).toBe(path);
    }
    expect((await api(page, "GET", `${s.path}/profile`)).body.current_revision).toBe(1);
});

test("a server field error lands on its row and takes focus", async ({ page }) => {
    const s = await owner(page);
    await openProfile(page, s.site, s.station, s.profile.display_name);
    const posts = countPosts(page, s.station);
    await page.getByLabel("Label, field 2").fill("{{ total }}");
    // The live preview already reports it on row 2 only.
    await expect(page.getByLabel("Label, field 2")).toHaveAttribute("aria-invalid", "true");
    await expect(rows(page).nth(1)).toContainText("UNSUPPORTED_STATION_CONFIGURATION");
    await expect(page.getByLabel("Label, field 1")).not.toHaveAttribute("aria-invalid", "true");

    await page.getByRole("button", { name: "Save revision 2" }).click();
    await expect(page.getByLabel("Label, field 2")).toBeFocused();
    await expect(rows(page).nth(1).getByRole("alert").filter({ hasText: /\S/ })).toContainText("configuration only");
    expect(posts.n).toBe(1);
    expect((await api(page, "GET", `${s.path}/profile`)).body.current_revision).toBe(1);

    // Range error: min above max maps to that row's Min control.
    await page.getByLabel("Label, field 2").fill("Shift OK");
    await page.getByLabel("Min, field 1").fill("10");
    await page.getByLabel("Max, field 1").fill("5");
    await expect(page.getByLabel("Min, field 1")).toHaveAttribute("aria-invalid", "true");
    await expect(page.getByLabel("Label, field 2")).not.toHaveAttribute("aria-invalid", "true");
});

test("a stale save gets 412 and a compare panel, and is never resubmitted", async ({ page, browser }) => {
    const s = await owner(page);
    await openProfile(page, s.site, s.station, s.profile.display_name);
    const posts = countPosts(page, s.station);
    await page.getByLabel("Label, field 1").fill("Good parts");

    const other = await person(browser, "shift-lead");
    try {
        const changed = { ...s.profile, fields: [startFields[0], { ...startFields[1], label: "Shift checked" }] };
        const r = await api(other.page, "POST", `${s.path}/profile-revisions`, changed, "1");
        expect(r.status, JSON.stringify(r.body)).toBe(201);
    } finally {
        await other.context.close();
    }

    await page.getByRole("button", { name: "Save revision 2" }).click();
    const panel = page.getByRole("alert").filter({ hasText: "Changed on the server (revision 2)" });
    await expect(panel).toBeVisible();
    await expect(panel).toContainText("Shift checked");
    await expect(panel).toContainText("Good parts");
    await expect(page.getByRole("button", { name: "Save revision 2" })).toBeDisabled();
    await page.waitForTimeout(1000);
    expect(posts.n).toBe(1);
    expect((await api(page, "GET", `${s.path}/profile`)).body.current_revision).toBe(2);

    await panel.getByRole("button", { name: "Keep mine on revision 2" }).click();
    await page.getByRole("button", { name: "Save revision 3" }).click();
    await expect(page.getByText("Saved revision 3.")).toBeVisible();
    expect(posts.n).toBe(2);
    // "Keep mine" is a new intent on a new revision: it must not reuse the refused key.
    expect(posts.keys[1]).not.toBe(posts.keys[0]);
    const stored = await api(page, "GET", `${s.path}/profile`);
    expect(stored.body.revisions![2].profile.fields.map((f) => f.label)).toEqual(["Good parts", "Shift OK"]);
});

test("a viewer gets a read-only profile and the API refuses its write", async ({ page, browser }) => {
    const s = await owner(page);
    const viewer = await person(browser, "read-only-viewer");
    try {
        const v = viewer.page;
        expect(siteOf(v)).toBe(s.site);
        await section(v, "Profiles");
        await expect(v.getByRole("button", { name: "New profile" })).toHaveCount(0);
        await openProfile(v, s.site, s.station, s.profile.display_name);
        await expect(v.getByRole("table", { name: "Profile fields" })).toContainText("Good count");
        await expect(v.getByRole("table", { name: "Profile fields" })).toContainText("pcs · 0 – …");
        const editor = v.locator(".page");
        for (const role of ["textbox", "combobox", "checkbox", "spinbutton"] as const) await expect(editor.getByRole(role)).toHaveCount(0);
        await expect(editor.getByRole("button", { name: /save|add|remove|move|discard|keep|new/i })).toHaveCount(0);

        const write = await api(v, "POST", `${s.path}/profile-revisions`, { ...s.profile, display_name: "viewer edit" }, "1");
        expect(write.status).toBe(403);
        expect(write.body.code).toBe("PERMISSION_DENIED");
    } finally {
        await viewer.context.close();
    }
    expect((await api(page, "GET", `${s.path}/profile`)).body.current_revision).toBe(1);
});

test("a retry after a lost connection reuses its Idempotency-Key and saves once", async ({ page }) => {
    const s = await owner(page);
    await openProfile(page, s.site, s.station, s.profile.display_name);
    const posts = countPosts(page, s.station);
    await page.getByLabel("Label, field 1").fill("Good pieces");
    let dropped = false;
    await page.route(`**/workstations/${s.station}/profile-revisions`, (route) => {
        if (dropped) return route.continue();
        dropped = true;
        return route.abort("connectionreset");
    });
    await page.getByRole("button", { name: "Save revision 2" }).click();
    await expect(page.getByText("Connection lost before the server answered.")).toBeVisible();
    await page.getByRole("button", { name: "Retry save" }).click();
    await expect(page.getByText("Saved revision 2.")).toBeVisible();
    expect(posts.n).toBe(2);
    expect(posts.keys[1]).toBe(posts.keys[0]);
    expect((await api(page, "GET", `${s.path}/profile`)).body.current_revision).toBe(2);
});

test("a failed line page offers no lines and says so (rule 06)", async ({ page }) => {
    const s = await owner(page);
    let fail = true;
    // Force a second line page and fail it: the first page must not be offered as complete.
    await page.route(`**/v1/sites/${s.site}/lines?*`, async (route) => {
        if (!fail) return route.continue();
        if (new URL(route.request().url()).searchParams.has("cursor")) return route.fulfill({ status: 503, contentType: "application/problem+json", body: JSON.stringify({ type: "about:blank", title: "STORAGE_UNAVAILABLE", status: 503, code: "STORAGE_UNAVAILABLE", correlation_id: "test" }) });
        const first = await route.fetch();
        await route.fulfill({ response: first, json: { ...(await first.json()), next_cursor: randomUUID() } });
    });
    await openProfile(page, s.site, s.station, s.profile.display_name);
    await expect(page.getByRole("alert").filter({ hasText: "Lines could not be loaded completely, so none are offered." })).toBeVisible();
    const line = page.locator("#pf-h-line_id");
    // Only the placeholder and the profile's own line (as its short id) remain.
    await expect(line.locator("option")).toHaveCount(2);
    fail = false;
    await page.getByRole("button", { name: "Reload lines" }).click();
    await expect(page.getByText("Lines could not be loaded completely")).toHaveCount(0);
    await expect(line.locator("option", { hasText: s.lineName })).toHaveCount(1);
});
