import { randomUUID } from "node:crypto";
import { expect, test, type Browser, type Page } from "@playwright/test";
import { openSite, section, signIn } from "./session";

// WIS-520 G(3) operational source mapping editor against the real backend. Every test creates
// its own operational source and knowledge entities (fresh UUIDs) and finds them by id, so
// accumulated data and run order do not matter. A second person always uses a fresh browser
// context: app sign-out keeps the Keycloak SSO session.

type Mapping = { external_type: "asset" | "order"; external_key: string; entity_id: string; entity_revision: number };
type Reply = { status: number; body: { code?: string; path?: string; revision?: number; mappings?: Mapping[]; source?: { revision: number; mapping_revision: number } } };

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

// seed creates an operational source and two asset entities as the signed-in owner.
async function seed(page: Page) {
    const site = siteOf(page);
    const suffix = randomUUID().slice(0, 8);
    const evidence = await api(page, "POST", `/v1/sites/${site}/sources`, { source_id: randomUUID(), kind: "manual", title: `G3 identity ${suffix}`, text: "Press identities for mapping tests" }, "0");
    expect(evidence.status, JSON.stringify(evidence.body)).toBe(200);
    const entities: { id: string; name: string; revision: number }[] = [];
    for (const n of [1, 2]) {
        const id = randomUUID();
        const name = `G3 press ${n} ${suffix}`;
        const created = await api(page, "POST", `/v1/sites/${site}/entities`, { entity_id: id, type: "asset", asset_kind: "machine", display_name: name, source_id: (evidence.body as { source_id?: string }).source_id ?? "", source_revision: evidence.body.revision }, "0");
        expect(created.status, JSON.stringify(created.body)).toBe(200);
        entities.push({ id, name, revision: created.body.revision! });
    }
    const source = randomUUID();
    const config = { source_id: source, type: "csv", authority_scope: `g3-${suffix}`, enabled: true, access_policy_revision: 1, freshness_seconds: 300, clock_skew_seconds: 30 };
    const made = await api(page, "POST", `/v1/sites/${site}/operational-sources`, config, "0");
    expect(made.status, JSON.stringify(made.body)).toBe(200);
    return { site, source, entities, path: `/v1/sites/${site}/operational-sources/${source}`, suffix };
}

// openSource clicks through the Sources tab to the source, finding its row by id.
async function openSource(page: Page, site: string, source: string) {
    await section(page, "Sources");
    await page.locator(`a[href="/sites/${site}/sources/${source}"]`).click();
    await expect(page.getByRole("table", { name: "Mappings" })).toBeVisible();
}

function mappingPosts(page: Page, source: string) {
    const seen: { n: number; keys: string[] } = { n: 0, keys: [] };
    page.on("request", (r) => {
        if (r.method() === "POST" && r.url().endsWith(`/operational-sources/${source}/mappings`)) {
            seen.n++;
            seen.keys.push(r.headers()["idempotency-key"] ?? "");
        }
    });
    return seen;
}

// mapRow adds row n (1-based) and maps it by entity id: the editor reads that one entity, so the
// test never depends on the site's whole entity list (which grows on a long-lived database).
async function mapRow(page: Page, n: number, key: string, entity: { id: string; revision: number }) {
    await page.getByRole("button", { name: "+ Add mapping" }).click();
    await expect(page.getByLabel(`External key, row ${n}`)).toBeFocused();
    await page.getByLabel(`External key, row ${n}`).fill(key);
    await page.getByLabel(`Entity ID, row ${n}`).fill(entity.id);
    await expect(page.locator(`tr[data-row="${n - 1}"] td.n`)).toContainText(String(entity.revision));
}

const problem = (status: number, code: string) => ({ status, contentType: "application/problem+json", body: JSON.stringify({ type: "about:blank", title: code, status, code, correlation_id: "test" }) });

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

test("a saved mapping round-trips and the list shows its new revisions", async ({ page }) => {
    const s = await owner(page);
    await openSource(page, s.site, s.source);
    const posts = mappingPosts(page, s.source);
    await mapRow(page, 1, `PLC-${s.suffix}`, s.entities[0]);
    await mapRow(page, 2, `MES-${s.suffix}`, s.entities[1]);
    await expect(page.getByText("saving creates source revision 2")).toBeVisible();
    await page.getByRole("button", { name: "Save mapping revision 2" }).click();
    await expect(page.getByText("Saved mapping revision 2. This created source revision 2")).toBeVisible();
    expect(posts.n).toBe(1);

    const expected: Mapping[] = [
        { external_type: "asset", external_key: `MES-${s.suffix}`, entity_id: s.entities[1].id, entity_revision: s.entities[1].revision },
        { external_type: "asset", external_key: `PLC-${s.suffix}`, entity_id: s.entities[0].id, entity_revision: s.entities[0].revision },
    ];
    const stored = await api(page, "GET", s.path);
    expect(stored.body.source?.mapping_revision).toBe(2);
    expect(stored.body.mappings).toEqual(expected);
    await expect(page.locator(`tr[data-source="${s.source}"]`)).toContainText("2/2");

    // Reload through the UI: the editor shows exactly what was stored, and is clean.
    await section(page, "Master data");
    await openSource(page, s.site, s.source);
    await expect(page.getByLabel("External key, row 1")).toHaveValue(`MES-${s.suffix}`);
    await expect(page.getByLabel("Entity ID, row 1")).toHaveValue(s.entities[1].id);
    await expect(page.getByLabel("External key, row 2")).toHaveValue(`PLC-${s.suffix}`);
    await expect(page.getByRole("button", { name: "Save mapping revision 3" })).toBeDisabled();
});

test("unmapped entities stay visible and can be mapped from the list (rule 06)", async ({ page }) => {
    const s = await owner(page);
    // Scope the browse list to this test's two entities so accumulated data cannot matter.
    const mine = await Promise.all(s.entities.map(async (e) => (await api(page, "GET", `/v1/sites/${s.site}/entities/${e.id}`)).body));
    await page.route(`**/v1/sites/${s.site}/entities?*`, (route) => route.fulfill({ json: { entities: mine } }));
    await openSource(page, s.site, s.source);
    await expect(page.getByText("2 entities have no external key")).toBeVisible();
    await page.getByRole("button", { name: `Map ${s.entities[0].name}` }).click();
    await expect(page.getByLabel("Entity ID, row 1")).toHaveValue(s.entities[0].id);
    await expect(page.getByText("1 entity has no external key")).toBeVisible();
    await expect(page.getByRole("button", { name: `Map ${s.entities[0].name}` })).toHaveCount(0);
    await expect(page.getByRole("button", { name: `Map ${s.entities[1].name}` })).toBeVisible();
});

test("a crafted extra member is refused 422 and nothing is stored", async ({ page }) => {
    const s = await owner(page);
    const row = { external_type: "asset", external_key: `X-${s.suffix}`, entity_id: s.entities[0].id, entity_revision: s.entities[0].revision };
    for (const body of [{ mappings: [{ ...row, x: 120 }] }, { mappings: [row], condition: "state == 'running'" }]) {
        const r = await api(page, "POST", `${s.path}/mappings`, body, "1");
        expect(r.status).toBe(422);
        expect(r.body.code).toBe("INVALID_REQUEST");
    }
    expect((await api(page, "GET", s.path)).body.source?.mapping_revision).toBe(1);
});

test("a server row error lands on its row", async ({ page }) => {
    const s = await owner(page);
    await openSource(page, s.site, s.source);
    await mapRow(page, 1, `A-${s.suffix}`, s.entities[0]);
    await mapRow(page, 2, `B-${s.suffix}`, s.entities[1]);
    // The UI cannot produce an invalid key, so the request is altered on the wire, as a
    // crafted client would: row 2's key gains a leading space.
    await page.route(`**/operational-sources/${s.source}/mappings`, async (route) => {
        const body = JSON.parse(route.request().postData() ?? "{}") as { mappings: Mapping[] };
        body.mappings[1].external_key = ` ${body.mappings[1].external_key}`;
        await route.continue({ postData: JSON.stringify(body) });
    });
    const answer = page.waitForResponse((r) => r.url().endsWith(`/operational-sources/${s.source}/mappings`));
    await page.getByRole("button", { name: "Save mapping revision 2" }).click();
    const reply = (await (await answer).json()) as { code: string; path: string };
    expect(reply).toMatchObject({ code: "INVALID_SOURCE", path: "mappings[1].external_key" });
    await expect(page.getByLabel("External key, row 2")).toHaveAttribute("aria-invalid", "true");
    await expect(page.getByLabel("External key, row 2")).toBeFocused();
    await expect(page.getByLabel("External key, row 1")).not.toHaveAttribute("aria-invalid", "true");
    await expect(page.locator('tr[data-row="1"]').getByRole("alert").filter({ hasText: /\S/ })).toContainText("Invalid or duplicate external key");
    expect((await api(page, "GET", s.path)).body.source?.mapping_revision).toBe(1);
});

test("a stale save shows compare and reload and never resubmits", async ({ page, browser }) => {
    const s = await owner(page);
    await openSource(page, s.site, s.source);
    const posts = mappingPosts(page, s.source);
    await mapRow(page, 1, `MINE-${s.suffix}`, s.entities[0]);

    const other = await person(browser, "shift-lead");
    try {
        const theirs = { mappings: [{ external_type: "asset", external_key: `THEIRS-${s.suffix}`, entity_id: s.entities[1].id, entity_revision: s.entities[1].revision }] };
        const r = await api(other.page, "POST", `${s.path}/mappings`, theirs, "1");
        expect(r.status, JSON.stringify(r.body)).toBe(200);
    } finally {
        await other.context.close();
    }

    await page.getByRole("button", { name: "Save mapping revision 2" }).click();
    const panel = page.getByRole("alert").filter({ hasText: "Changed on the server (mapping revision 2)" });
    await expect(panel).toBeVisible();
    await expect(panel).toContainText(`MINE-${s.suffix}`);
    await expect(panel).toContainText(`THEIRS-${s.suffix}`);
    await expect(page.getByRole("button", { name: "Save mapping revision 2" })).toBeDisabled();
    await page.waitForTimeout(1000);
    expect(posts.n).toBe(1);
    expect((await api(page, "GET", s.path)).body.source?.mapping_revision).toBe(2);

    await panel.getByRole("button", { name: "Keep mine on mapping revision 2" }).click();
    await page.getByRole("button", { name: "Save mapping revision 3" }).click();
    await expect(page.getByText("Saved mapping revision 3.")).toBeVisible();
    expect(posts.n).toBe(2);
    // The save on the new revision is a new intent with its own key.
    expect(posts.keys[1]).not.toBe(posts.keys[0]);
    const stored = await api(page, "GET", s.path);
    expect(stored.body.mappings!.map((m) => m.external_key)).toEqual([`MINE-${s.suffix}`]);
});

test("a retry after a lost connection reuses its Idempotency-Key and saves once", async ({ page }) => {
    const s = await owner(page);
    await openSource(page, s.site, s.source);
    const posts = mappingPosts(page, s.source);
    await mapRow(page, 1, `RETRY-${s.suffix}`, s.entities[0]);
    let dropped = false;
    await page.route(`**/operational-sources/${s.source}/mappings`, (route) => {
        if (dropped) return route.continue();
        dropped = true;
        return route.abort("connectionreset");
    });
    await page.getByRole("button", { name: "Save mapping revision 2" }).click();
    await expect(page.getByText("Connection lost before the server answered.")).toBeVisible();
    await page.getByRole("button", { name: "Retry save" }).click();
    await expect(page.getByText("Saved mapping revision 2.")).toBeVisible();
    expect(posts.n).toBe(2);
    expect(posts.keys[1]).toBe(posts.keys[0]);
    expect((await api(page, "GET", s.path)).body.source?.mapping_revision).toBe(2);
});

test("a 428 asks for a fresh sign-in, and inputs are locked while a save is in flight", async ({ page }) => {
    const s = await owner(page);
    await openSource(page, s.site, s.source);
    await mapRow(page, 1, `AUTH-${s.suffix}`, s.entities[0]);
    let release!: () => void;
    const held = new Promise<void>((r) => (release = r));
    await page.route(`**/operational-sources/${s.source}/mappings`, async (route) => {
        await held;
        await route.fulfill(problem(428, "REAUTHENTICATION_REQUIRED"));
    });
    await page.getByRole("button", { name: "Save mapping revision 2" }).click();
    await expect(page.getByLabel("External key, row 1")).toBeDisabled();
    await expect(page.getByLabel("Entity ID, row 1")).toBeDisabled();
    await expect(page.getByRole("button", { name: "+ Add mapping" })).toBeDisabled();
    release();
    await expect(page.getByText("Mapping changes need a recent sign-in. Confirm your identity, then save again.")).toBeVisible();
    await expect(page.getByRole("button", { name: "Confirm identity" })).toBeVisible();
    await expect(page.getByLabel("External key, row 1")).toBeEnabled();
    expect((await api(page, "GET", s.path)).body.source?.mapping_revision).toBe(1);
});

test("a confirmed save whose re-read fails offers a reload, never a retry", async ({ page }) => {
    const s = await owner(page);
    await openSource(page, s.site, s.source);
    const posts = mappingPosts(page, s.source);
    await mapRow(page, 1, `READ-${s.suffix}`, s.entities[0]);
    let saved = false;
    await page.route(`**/operational-sources/${s.source}/mappings`, async (route) => {
        const response = await route.fetch();
        saved = response.ok();
        await route.fulfill({ response });
    });
    // The detail re-read after the save fails once.
    const detail = (url: URL) => url.pathname === `/v1/sites/${s.site}/operational-sources/${s.source}`;
    await page.route(detail, (route) => (saved ? route.fulfill(problem(503, "STORAGE_UNAVAILABLE")) : route.continue()));
    await page.getByRole("button", { name: "Save mapping revision 2" }).click();
    await expect(page.getByText("Saved mapping revision 2.")).toBeVisible();
    await expect(page.getByText("The save is confirmed, but the latest version could not be reloaded")).toBeVisible();
    await expect(page.getByRole("button", { name: "Retry save" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: /Save mapping revision/ })).toBeDisabled();
    await expect(page.getByLabel("External key, row 1")).toBeDisabled();
    await page.unroute(detail);
    await page.getByRole("button", { name: "Reload source" }).click();
    await expect(page.getByText("could not be reloaded")).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Save mapping revision 3" })).toBeDisabled();
    expect(posts.n).toBe(1);
});

test("a viewer sees mappings read-only and the API refuses its write", async ({ page, browser }) => {
    const s = await owner(page);
    const mapped = { mappings: [{ external_type: "asset", external_key: `VIEW-${s.suffix}`, entity_id: s.entities[0].id, entity_revision: s.entities[0].revision }] };
    expect((await api(page, "POST", `${s.path}/mappings`, mapped, "1")).status).toBe(200);
    const viewer = await person(browser, "read-only-viewer");
    try {
        const v = viewer.page;
        expect(siteOf(v)).toBe(s.site);
        await openSource(v, s.site, s.source);
        const table = v.getByRole("table", { name: "Mappings" });
        await expect(table).toContainText(`VIEW-${s.suffix}`);
        await expect(table).toContainText(s.entities[0].id.slice(0, 8));
        const editor = v.locator(".sm-editor");
        for (const role of ["textbox", "combobox", "checkbox", "spinbutton"] as const) await expect(editor.getByRole(role)).toHaveCount(0);
        await expect(editor.getByRole("button")).toHaveCount(0);

        const write = await api(v, "POST", `${s.path}/mappings`, { mappings: [] }, "2");
        expect(write.status).toBe(403);
        expect(write.body.code).toBe("PERMISSION_DENIED");
    } finally {
        await viewer.context.close();
    }
    expect((await api(page, "GET", s.path)).body.source?.mapping_revision).toBe(2);
});

test("a failed list page hides the whole list instead of showing part of it", async ({ page }) => {
    const s = await owner(page);
    // Force a second page, then fail it: the first page must not be presented as complete.
    await page.route(`**/v1/sites/${s.site}/operational-sources?*`, async (route) => {
        if (new URL(route.request().url()).searchParams.has("cursor")) return route.fulfill({ status: 503, contentType: "application/problem+json", body: JSON.stringify({ type: "about:blank", title: "STORAGE_UNAVAILABLE", status: 503, code: "STORAGE_UNAVAILABLE", correlation_id: "test" }) });
        const first = await route.fetch();
        const body = (await first.json()) as { sources: unknown[] };
        await route.fulfill({ response: first, json: { ...body, next_cursor: randomUUID() } });
    });
    await section(page, "Sources");
    await expect(page.getByText("The source list could not be loaded completely, so none of it is shown.")).toBeVisible();
    await expect(page.locator(`tr[data-source="${s.source}"]`)).toHaveCount(0);
    await expect(page.getByRole("table", { name: "Operational sources" })).toHaveCount(0);
});
