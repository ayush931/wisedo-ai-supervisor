import { expect, test, type Page } from "@playwright/test";
import { appRegions } from "./regions";
import { clientNavigate, openSite, section, signIn, signOut } from "./session";

type Seed = { siteId: string; lineId: string; itemId: string; routingId: string; lineName: string; itemName: string; routingName: string; stationNames: string[]; stationIds: string[]; body: Record<string, unknown> };

async function seed(page: Page, suffix: string, names = ["press-01", "weld-01"]): Promise<Seed> {
    return page.evaluate(async ({ suffix, names }) => {
        const meResponse = await fetch("/v1/me");
        if (!meResponse.ok) throw new Error(`Could not read session (${meResponse.status}).`);
        const me = await meResponse.json() as { csrf_token: string; sites: { id: string }[] };
        const siteId = me.sites[0]?.id;
        if (!siteId) throw new Error("The demo user has no site.");
        const stations: { workstation_id: string; display_name?: string | null }[] = [];
        let cursor = "";
        let complete = false;
        for (let pageNumber = 0; pageNumber < 50; pageNumber++) {
            const url = new URL(`/v1/sites/${siteId}/workstations`, location.origin);
            url.searchParams.set("limit", "100");
            if (cursor) url.searchParams.set("cursor", cursor);
            const response = await fetch(url);
            if (!response.ok) throw new Error(`Could not read stations (${response.status}).`);
            const page = await response.json() as { workstations: { workstation_id: string; display_name?: string | null }[]; next_cursor?: string | null };
            stations.push(...page.workstations);
            if (!page.next_cursor) { complete = true; break; }
            cursor = page.next_cursor;
        }
        if (!complete) throw new Error("Workstation pagination exceeded 50 pages.");
        const stationIds = names.map((name) => {
            const station = stations.find((entry) => entry.display_name === name);
            if (!station) throw new Error(`Demo fixture requires ${name}.`);
            return station.workstation_id;
        });
        const lineName = `Routing line ${suffix}`;
        const itemName = `Routing item ${suffix}`;
        const routingName = `Routing ${suffix}`;
        async function create<T extends { id: string }>(resource: string, body: unknown): Promise<T> {
            const response = await fetch(`/v1/sites/${siteId}/${resource}`, {
                method: "POST",
                headers: { "Content-Type": "application/json", "X-CSRF-Token": me.csrf_token, "Idempotency-Key": crypto.randomUUID(), "If-Match": '"0"' },
                body: JSON.stringify(body),
            });
            if (!response.ok) {
                const problem = await response.json().catch(() => ({})) as { code?: string };
                throw new Error(`Could not seed ${resource} (${response.status} ${problem.code ?? "request failed"}).`);
            }
            return await response.json() as T;
        }
        const line = await create<{ id: string }>("lines", { schema_version: 1, display_name: lineName, active: true });
        const item = await create<{ id: string }>("items", { schema_version: 1, display_name: itemName, unit: "count", active: true });
        const body = { schema_version: 1, display_name: routingName, active: true, line_id: line.id, item_id: item.id, steps: stationIds.map((workstation_id, i) => ({ step_number: i + 1, workstation_id })) };
        const routing = await create<{ id: string }>("routings", body);
        return { siteId, lineId: line.id, itemId: item.id, routingId: routing.id, lineName, itemName, routingName, stationNames: names, stationIds, body };
    }, { suffix, names });
}

async function openRouting(page: Page, name: string) {
    await section(page, "Routings");
    await page.getByRole("link", { name, exact: true }).click();
    await expect(page.getByRole("heading", { name, exact: true })).toBeVisible();
}

test("WIS-520 routing editor saves, reloads the exact body, and the unchanged revision is refused", async ({ page }, info) => {
    await signIn(page, "shift-lead");
    const suffix = `${info.testId}-${Date.now()}`;
    const item = await page.evaluate(async (suffix) => {
        const me = await (await fetch("/v1/me")).json() as { csrf_token: string; sites: { id: string }[] };
        const site = me.sites[0];
        const headers = { "Content-Type": "application/json", "X-CSRF-Token": me.csrf_token, "Idempotency-Key": crypto.randomUUID(), "If-Match": '"0"' };
        const response = await fetch(`/v1/sites/${site.id}/items`, { method: "POST", headers, body: JSON.stringify({ schema_version: 1, display_name: `Routing item ${suffix}`, unit: "count", active: true }) });
        if (!response.ok) throw new Error(`Could not seed item (${response.status}).`);
        const extraLineName = `Routing fixture line ${suffix}`;
        const lineResponse = await fetch(`/v1/sites/${site.id}/lines`, { method: "POST", headers: { ...headers, "Idempotency-Key": crypto.randomUUID() }, body: JSON.stringify({ schema_version: 1, display_name: extraLineName, active: true }) });
        if (!lineResponse.ok) throw new Error(`Could not seed workstation line (${lineResponse.status}).`);
        const lineId = (await lineResponse.json() as { id: string }).id;
        const workstationId = crypto.randomUUID();
        const stationName = `routing-qa-${suffix}`;
        const profile = { schema_version: 1, workstation_id: workstationId, line_id: lineId, display_name: stationName, template: "production", fields: [{ name: "station_note", label: "Station note", kind: "text", required: false }] };
        const profileResponse = await fetch(`/v1/sites/${site.id}/workstations/${workstationId}/profile-revisions`, { method: "POST", headers: { ...headers, "Idempotency-Key": crypto.randomUUID() }, body: JSON.stringify(profile) });
        if (!profileResponse.ok) throw new Error(`Could not seed third workstation (${profileResponse.status}).`);
        return { siteId: site.id, itemId: (await response.json() as { id: string }).id, itemName: `Routing item ${suffix}`, stationName };
    }, suffix);
    // The site shell already cached the station list before the seed; load the app again.
    await page.goto("/");
    await openSite(page);
    await section(page, "Routings");
    const lineName = `Routing line ${suffix}`;
    await page.getByRole("textbox", { name: "New line name" }).fill(lineName);
    await page.getByRole("textbox", { name: "New line name" }).press("Enter");
    await expect(page.locator(".routing-new-line [role=status]").filter({ hasText: `${lineName} added` })).toBeVisible();
    const posted: Record<string, unknown>[] = [];
    page.on("request", (request) => {
        if (request.method() === "POST" && new URL(request.url()).pathname === `/v1/sites/${item.siteId}/routings`) posted.push(request.postDataJSON() as Record<string, unknown>);
    });
    await page.getByRole("link", { name: "New routing", exact: true }).click();
    await page.getByLabel("Name", { exact: true }).fill(`Routing ${suffix}`);
    await page.getByRole("combobox", { name: "Line", exact: true }).selectOption({ label: lineName });
    await page.getByRole("combobox", { name: "Item", exact: true }).selectOption({ label: item.itemName });
    const addStation = page.getByRole("combobox", { name: "Add station", exact: true });
    for (const name of ["press-01", "weld-01", item.stationName]) {
        await addStation.fill(name);
        await addStation.press("Enter");
    }
    const steps = page.getByRole("list", { name: "Routing steps" });
    await steps.getByRole("listitem").nth(2).getByRole("button", { name: `Move ${item.stationName} up` }).click();
    await steps.getByRole("listitem").nth(1).getByRole("button", { name: `Move ${item.stationName} up` }).click();
    await page.getByRole("button", { name: "Save", exact: true }).click();
    await expect(page.getByRole("heading", { name: `Routing ${suffix}`, exact: true })).toBeVisible();
    await expect.poll(() => posted.length).toBe(1);
    const routingId = new URL(page.url()).pathname.split("/").at(-1)!;
    await expect(page.getByRole("button", { name: "Save", exact: true })).toBeDisabled();
    await section(page, "Routings");
    await page.getByRole("link", { name: `Routing ${suffix}`, exact: true }).click();
    await expect(page.locator(".routing-step-name")).toHaveText([new RegExp(`^${item.stationName}`), /^press-01/, /^weld-01/]);
    const result = await page.evaluate(async ({ siteId, routingId, body }) => {
        const response = await fetch(`/v1/sites/${siteId}/routings/${routingId}`);
        const value = await response.json() as { current_revision: number; revisions: { revision: number; body: unknown }[] };
        const current = value.revisions.find((revision) => revision.revision === value.current_revision)!;
        const duplicate = await fetch(`/v1/sites/${siteId}/routings/${routingId}/revisions`, { method: "POST", headers: { "Content-Type": "application/json", "X-CSRF-Token": (await (await fetch("/v1/me")).json() as { csrf_token: string }).csrf_token, "Idempotency-Key": crypto.randomUUID(), "If-Match": '"1"' }, body: JSON.stringify(body) });
        const problem = await duplicate.json() as { code?: string };
        return { saved: current.body, status: duplicate.status, code: problem.code };
    }, { siteId: item.siteId, routingId, body: posted[0] });
    expect(result.saved).toEqual(posted[0]);
    expect(result.status).toBe(409);
    expect(result.code).toBe("MASTER_DATA_UNCHANGED");
});

test("WIS-520 crafted routing fields are rejected without changing the catalog", async ({ page }, info) => {
    await signIn(page, "shift-lead");
    await openSite(page);
    const seedData = await seed(page, `${info.testId}-${Date.now()}`);
    const result = await page.evaluate(async ({ siteId, csrf, body }) => {
        const routingIds = async () => {
            const ids: string[] = [];
            let cursor = "";
            let complete = false;
            for (let pageNumber = 0; pageNumber < 50; pageNumber++) {
                const url = new URL(`/v1/sites/${siteId}/routings`, location.origin);
                url.searchParams.set("limit", "100");
                if (cursor) url.searchParams.set("cursor", cursor);
                const response = await fetch(url);
                if (!response.ok) throw new Error(`Could not read routings (${response.status}).`);
                const page = await response.json() as { routings: { id: string }[]; next_cursor?: string | null };
                ids.push(...page.routings.map((routing) => routing.id));
                if (!page.next_cursor) { complete = true; break; }
                cursor = page.next_cursor;
            }
            if (!complete) throw new Error("Routing pagination exceeded 50 pages.");
            return ids.sort();
        };
        const before = await routingIds();
        const send = async (value: unknown) => {
            const response = await fetch(`/v1/sites/${siteId}/routings`, { method: "POST", headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf, "Idempotency-Key": crypto.randomUUID(), "If-Match": '"0"' }, body: JSON.stringify(value) });
            return { status: response.status, problem: await response.json() as { code?: string } };
        };
        const withNode = structuredClone(body) as { steps: Record<string, unknown>[] } & Record<string, unknown>;
        withNode.steps[0].x = 120;
        const node = await send(withNode);
        const withCondition = { ...body, condition: "true" };
        const condition = await send(withCondition);
        return { before, after: await routingIds(), node: { status: node.status, code: node.problem.code }, condition: { status: condition.status, code: condition.problem.code } };
    }, { siteId: seedData.siteId, csrf: await page.evaluate(async () => (await (await fetch("/v1/me")).json() as { csrf_token: string }).csrf_token), body: seedData.body });
    expect(result.node).toEqual({ status: 422, code: "INVALID_MASTER_DATA" });
    expect(result.condition).toEqual({ status: 422, code: "INVALID_MASTER_DATA" });
    expect(result.after).toEqual(result.before);
});

test("WIS-520 invalid step response focuses and marks its ordered row", async ({ page }, info) => {
    await signIn(page, "shift-lead");
    await openSite(page);
    const data = await seed(page, `${info.testId}-${Date.now()}`);
    await openRouting(page, data.routingName);
    await page.getByLabel("Name", { exact: true }).fill(`${data.routingName} edited`);
    await page.route(`**/v1/sites/${data.siteId}/routings/${data.routingId}/revisions`, async (route) => {
        const body = route.request().postDataJSON() as { steps: { workstation_id: string }[] };
        body.steps[1].workstation_id = "00000000-0000-4000-8000-000000000000";
        await route.continue({ postData: JSON.stringify(body) });
    });
    await page.getByRole("button", { name: "Save", exact: true }).click();
    const row = page.getByRole("list", { name: "Routing steps" }).getByRole("listitem").nth(1);
    await expect(row).toHaveAttribute("aria-invalid", "true");
    await expect(row).toHaveAttribute("aria-describedby", "routing-field-error");
    await expect(row).toHaveAttribute("aria-keyshortcuts", "Alt+ArrowUp Alt+ArrowDown Delete");
    await expect(row).toBeFocused();
    await expect(page.locator(".routing-node-invalid")).toHaveCount(1);
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("steps[1].workstation_id");
});

test("WIS-520 stale saves show both revisions and require an explicit choice", async ({ page, browser }, info) => {
    await signIn(page, "shift-lead");
    await openSite(page);
    const data = await seed(page, `${info.testId}-${Date.now()}`);
    await openRouting(page, data.routingName);
    const otherContext = await browser.newContext();
    try {
        const other = await otherContext.newPage();
        await signIn(other, "shift-approver");
        await openSite(other);
        await openRouting(other, data.routingName);
        await other.getByRole("list", { name: "Routing steps" }).getByRole("listitem").nth(1).getByRole("button", { name: "Move weld-01 up" }).click();
        await other.getByRole("button", { name: "Save", exact: true }).click();
        await expect(other.getByRole("button", { name: "Save", exact: true })).toBeDisabled();

        let firstPosts = 0;
        page.on("request", (request) => { if (request.method() === "POST" && request.url().endsWith(`/routings/${data.routingId}/revisions`)) firstPosts++; });
        await page.getByLabel("Active", { exact: true }).uncheck();
        await page.getByRole("button", { name: "Save", exact: true }).click();
        await expect(page.getByRole("heading", { name: "Changed on the server (rev 2)" })).toBeVisible();
        await expect(page.getByRole("heading", { name: "Server version" })).toBeVisible();
        await page.waitForTimeout(300);
        expect(firstPosts).toBe(1);
        await expect(page.getByRole("heading", { name: "Your draft" })).toBeVisible();
        await expect(page.getByRole("heading", { name: "Server version" })).toBeVisible();
        await page.getByRole("button", { name: "Keep mine on rev 2" }).click();
        await page.getByRole("button", { name: "Save", exact: true }).click();
        await expect(page.getByRole("button", { name: "Save", exact: true })).toBeDisabled();
        const revision3 = await page.evaluate(async ({ siteId, routingId }) => {
            const value = await (await fetch(`/v1/sites/${siteId}/routings/${routingId}`)).json() as { current_revision: number; revisions: { revision: number; body: unknown }[] };
            return { revision: value.current_revision, body: value.revisions.find((revision) => revision.revision === value.current_revision)?.body };
        }, data);
        expect(revision3).toEqual({ revision: 3, body: { ...data.body, active: false } });

        let otherKeepMinePosts = 0;
        other.on("request", (request) => { if (request.method() === "POST" && request.url().endsWith(`/routings/${data.routingId}/revisions`)) otherKeepMinePosts++; });
        await other.getByLabel("Name", { exact: true }).fill(`${data.routingName} B`);
        await other.getByRole("button", { name: "Save", exact: true }).click();
        await expect(other.getByRole("heading", { name: "Changed on the server (rev 3)" })).toBeVisible();
        await other.waitForTimeout(300);
        expect(otherKeepMinePosts).toBe(1);
        await other.getByRole("button", { name: "Keep mine on rev 3" }).click();
        await other.getByRole("button", { name: "Save", exact: true }).click();
        await expect(other.getByRole("button", { name: "Save", exact: true })).toBeDisabled();
        const revision4 = await page.evaluate(async ({ siteId, routingId }) => {
            const value = await (await fetch(`/v1/sites/${siteId}/routings/${routingId}`)).json() as { current_revision: number; revisions: { revision: number; body: { steps: { workstation_id: string }[] } }[] };
            return { revision: value.current_revision, steps: value.revisions.find((revision) => revision.revision === value.current_revision)?.body.steps.map((step) => step.workstation_id) };
        }, data);
        expect(revision4).toEqual({ revision: 4, steps: [data.stationIds[1], data.stationIds[0]] });

        let otherPosts = 0;
        page.on("request", (request) => { if (request.method() === "POST" && request.url().endsWith(`/routings/${data.routingId}/revisions`)) otherPosts++; });
        await page.getByLabel("Name", { exact: true }).fill(`${data.routingName} stale edit`);
        await page.getByRole("button", { name: "Save", exact: true }).click();
        await expect(page.getByRole("heading", { name: "Changed on the server (rev 4)" })).toBeVisible();
        await expect(page.getByRole("heading", { name: "Server version" })).toBeVisible();
        await page.waitForTimeout(300);
        expect(otherPosts).toBe(1);
        await page.getByRole("button", { name: "Use server version" }).click();
        await expect(page.getByLabel("Name", { exact: true })).toHaveValue(`${data.routingName} B`);
        await expect(page.locator(".routing-step-name")).toHaveText([/^weld-01/, /^press-01/]);
        await expect(page.getByRole("button", { name: "Save", exact: true })).toBeDisabled();
        await page.waitForTimeout(300);
        expect(otherPosts).toBe(1);
    } finally { await otherContext.close(); }
});

test("WIS-520 a save disables routing edits until the server confirms the revision", async ({ page }, info) => {
    await signIn(page, "shift-lead");
    await openSite(page);
    const data = await seed(page, `${info.testId}-${Date.now()}`);
    await openRouting(page, data.routingName);
    const newName = `${data.routingName} saved`;
    await page.getByLabel("Name", { exact: true }).fill(newName);

    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let posts = 0;
    await page.route(`**/v1/sites/${data.siteId}/routings/${data.routingId}/revisions`, async (route) => {
        if (route.request().method() !== "POST") return route.continue();
        posts++;
        await held;
        await route.continue();
    });
    try {
        await page.getByRole("button", { name: "Save", exact: true }).click();
        await expect.poll(() => posts).toBe(1);
        await expect(page.getByLabel("Name", { exact: true })).toBeDisabled();
        await expect(page.getByRole("combobox", { name: "Line", exact: true })).toBeDisabled();
        await expect(page.getByRole("combobox", { name: "Item", exact: true })).toBeDisabled();
        await expect(page.getByLabel("Active", { exact: true })).toBeDisabled();
        await expect(page.getByLabel("Add station", { exact: true })).toBeDisabled();
        await expect(page.getByRole("button", { name: "Move press-01 down" })).toBeDisabled();
    } finally { release(); }
    await expect(page.getByRole("button", { name: "Save", exact: true })).toBeDisabled();
    await expect(page.getByLabel("Name", { exact: true })).toHaveValue(newName);
});

test("WIS-520 Ctrl+S on a clean routing does not post an unchanged revision", async ({ page }, info) => {
    await signIn(page, "shift-lead");
    await openSite(page);
    const data = await seed(page, `${info.testId}-${Date.now()}`);
    await openRouting(page, data.routingName);
    let posts = 0;
    page.on("request", (request) => { if (request.method() === "POST" && request.url().endsWith(`/routings/${data.routingId}/revisions`)) posts++; });
    await page.getByLabel("Name", { exact: true }).focus();
    await page.keyboard.press("Control+s");
    await page.waitForTimeout(300);
    expect(posts).toBe(0);
    await expect(appRegions(page, "alert")).toHaveCount(0);
});

test("WIS-520 discarding a stale routing draft closes the conflict and adopts the server revision", async ({ page, browser }, info) => {
    await signIn(page, "shift-lead");
    await openSite(page);
    const data = await seed(page, `${info.testId}-${Date.now()}`);
    await openRouting(page, data.routingName);
    const otherContext = await browser.newContext();
    try {
        const other = await otherContext.newPage();
        await signIn(other, "shift-approver");
        await openSite(other);
        await openRouting(other, data.routingName);
        await other.getByRole("list", { name: "Routing steps" }).getByRole("listitem").nth(1).getByRole("button", { name: "Move weld-01 up" }).click();
        await other.getByRole("button", { name: "Save", exact: true }).click();
        await expect(other.getByRole("button", { name: "Save", exact: true })).toBeDisabled();

        await page.getByLabel("Name", { exact: true }).fill(`${data.routingName} local edit`);
        await page.getByRole("button", { name: "Save", exact: true }).click();
        await expect(page.getByRole("heading", { name: "Changed on the server (rev 2)" })).toBeVisible();
        await page.getByRole("button", { name: "Discard", exact: true }).click();
        await expect(page.getByRole("heading", { name: /Changed on the server/ })).toHaveCount(0);
        await expect(page.getByLabel("Name", { exact: true })).toHaveValue(data.routingName);
        await expect(page.locator(".routing-step-name")).toHaveText([/^weld-01/, /^press-01/]);
        await expect(page.getByRole("button", { name: "Save", exact: true })).toBeDisabled();
    } finally { await otherContext.close(); }
});

test("WIS-520 moving a routing step preserves Move-button and row focus", async ({ page }, info) => {
    await signIn(page, "shift-lead");
    await openSite(page);
    const data = await seed(page, `${info.testId}-${Date.now()}`, ["press-01", "weld-01", "press-01"]);
    await openRouting(page, data.routingName);
    const moveUp = page.getByRole("list", { name: "Routing steps" }).getByRole("listitem").nth(2).getByRole("button", { name: "Move press-01 up" });
    const button = await moveUp.elementHandle();
    if (!button) throw new Error("Could not resolve the step move button.");
    await button.focus();
    await button.click();
    await expect.poll(() => button.evaluate((element) => {
        const row = element.closest("li");
        return { focused: document.activeElement === element, index: row?.parentElement ? Array.from(row.parentElement.children).indexOf(row) : -1 };
    })).toEqual({ focused: true, index: 1 });
    await expect(page.locator(".routing-step-name")).toHaveText([/^press-01/, /^press-01/, /^weld-01/]);

    const rows = page.getByRole("list", { name: "Routing steps" }).getByRole("listitem");
    await page.evaluate(() => {
        const list = document.querySelector<HTMLOListElement>(".routing-steps");
        if (!list) throw new Error("Could not resolve the routing steps list.");
        // Keep focus-loss coverage deterministic on browsers that retain focus during keyed moves.
        new MutationObserver(() => {
            const active = document.activeElement;
            if (active instanceof HTMLElement && list.contains(active)) active.blur();
        }).observe(list, { childList: true });
    });
    const moveDown = rows.nth(0).getByRole("button", { name: "Move press-01 down" });
    const downButton = await moveDown.elementHandle();
    if (!downButton) throw new Error("Could not resolve the step move-down button.");
    await downButton.focus();
    await downButton.click();
    await expect.poll(() => downButton.evaluate((element) => {
        const row = element.closest("li");
        return { focused: document.activeElement === element, index: row?.parentElement ? Array.from(row.parentElement.children).indexOf(row) : -1 };
    })).toEqual({ focused: true, index: 1 });
    await expect(page.locator(".routing-step-name")).toHaveText([/^press-01/, /^press-01/, /^weld-01/]);

    const keyboardRow = await rows.nth(0).elementHandle();
    if (!keyboardRow) throw new Error("Could not resolve the routing step row.");
    await keyboardRow.focus();
    await page.keyboard.press("Alt+ArrowDown");
    await expect.poll(() => keyboardRow.evaluate((element) => ({
        focused: document.activeElement === element,
        index: element.parentElement ? Array.from(element.parentElement.children).indexOf(element) : -1,
    }))).toEqual({ focused: true, index: 1 });
    await expect(page.locator(".routing-step-name")).toHaveText([/^press-01/, /^press-01/, /^weld-01/]);
});

test("WIS-520 retrying a 503 uses the same idempotency key and creates one revision", async ({ page }, info) => {
    await signIn(page, "shift-lead");
    await openSite(page);
    const data = await seed(page, `${info.testId}-${Date.now()}`);
    await openRouting(page, data.routingName);
    await page.getByLabel("Name", { exact: true }).fill(`${data.routingName} retry`);
    const keys: string[] = [];
    await page.route(`**/v1/sites/${data.siteId}/routings/${data.routingId}/revisions`, async (route) => {
        if (route.request().method() !== "POST") return route.continue();
        keys.push((await route.request().allHeaders())["idempotency-key"]);
        if (keys.length === 1) {
            return route.fulfill({ status: 503, contentType: "application/problem+json", body: JSON.stringify({ type: "about:blank", title: "Temporary outage", status: 503, code: "TEMPORARY_FAILURE", correlation_id: "test" }) });
        }
        await route.continue();
    });
    await page.getByRole("button", { name: "Save", exact: true }).click();
    await expect(page.locator('[role="alert"][title="TEMPORARY_FAILURE"]')).toBeVisible();
    await expect(page.getByRole("button", { name: "Save", exact: true })).toBeEnabled();
    await page.getByRole("button", { name: "Save", exact: true }).click();
    await expect(page.getByRole("button", { name: "Save", exact: true })).toBeDisabled();
    expect(keys).toHaveLength(2);
    expect(keys[1]).toBe(keys[0]);
    const latest = await page.evaluate(async ({ siteId, routingId }) => {
        const value = await (await fetch(`/v1/sites/${siteId}/routings/${routingId}`)).json() as { current_revision: number; revisions: { revision: number; body: { display_name: string } }[] };
        return { revision: value.current_revision, revisions: value.revisions.length, body: value.revisions.find((revision) => revision.revision === value.current_revision)?.body };
    }, data);
    expect(latest).toEqual({ revision: 2, revisions: 2, body: { ...data.body, display_name: `${data.routingName} retry` } });
});

test("WIS-520 read-only viewer gets no editor controls and the API denies a write", async ({ page, browser }, info) => {
    await signIn(page, "shift-lead");
    await openSite(page);
    const data = await seed(page, `${info.testId}-${Date.now()}`);
    await signOut(page);
    const viewerContext = await browser.newContext();
    try {
        const viewer = await viewerContext.newPage();
        await signIn(viewer, "read-only-viewer");
        await openSite(viewer);
        await openRouting(viewer, data.routingName);
        const editor = viewer.locator(".routing-editor");
        await expect(editor.getByRole("list", { name: "Routing steps" }).getByRole("listitem")).toHaveCount(data.stationNames.length);
        await expect(editor.getByRole("button")).toHaveCount(0);
        await expect(editor.getByRole("textbox")).toHaveCount(0);
        await expect(editor.getByRole("combobox")).toHaveCount(0);
        let writes = 0;
        viewer.on("request", (request) => { if (request.method() === "POST" && request.url().includes("/routings/")) writes++; });
        await section(viewer, "Routings");
        await expect(viewer.getByRole("heading", { name: "Routings" })).toBeVisible();
        expect(writes).toBe(0);
        const denied = await viewer.evaluate(async ({ siteId, routingId, body }) => {
            const me = await (await fetch("/v1/me")).json() as { csrf_token: string };
            const changed = { ...body, display_name: `${body.display_name} denied` };
            const response = await fetch(`/v1/sites/${siteId}/routings/${routingId}/revisions`, { method: "POST", headers: { "Content-Type": "application/json", "X-CSRF-Token": me.csrf_token, "Idempotency-Key": crypto.randomUUID(), "If-Match": '"1"' }, body: JSON.stringify(changed) });
            const problem = await response.json() as { code?: string };
            return { status: response.status, code: problem.code };
        }, { siteId: data.siteId, routingId: data.routingId, body: data.body });
        expect(denied).toEqual({ status: 403, code: "PERMISSION_DENIED" });
    } finally { await viewerContext.close(); }
});
