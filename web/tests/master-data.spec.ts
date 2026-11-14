import { expect, test } from "@playwright/test";
import { openSite, section, signIn, signOut } from "./session";

test("master data reads persisted records and a viewer has no mutation controls", async ({ page, browser }) => {
    await signIn(page, "foundation-reviewer");
    await openSite(page);
    const data = await page.evaluate(async (suffix) => {
        const meResponse = await fetch("/v1/me");
        if (!meResponse.ok) throw new Error(`Could not read the session (${meResponse.status}).`);
        const me = await meResponse.json() as { csrf_token: string; sites: { id: string }[] };
        const site = me.sites[0];
        if (!site) throw new Error("The owner fixture has no site.");
        const stationsResponse = await fetch(`/v1/sites/${site.id}/workstations?limit=100`);
        if (!stationsResponse.ok) throw new Error(`Could not read workstations (${stationsResponse.status}).`);
        const stations = (await stationsResponse.json() as { workstations: { workstation_id: string; display_name?: string | null }[] }).workstations;
        const press = stations.find((station) => station.display_name === "press-01");
        const weld = stations.find((station) => station.display_name === "weld-01");
        if (!press || !weld) throw new Error("The demo fixture requires press-01 and weld-01.");
        async function create<T extends { id: string }>(resource: string, body: unknown): Promise<T> {
            const response = await fetch(`/v1/sites/${site.id}/${resource}`, {
                method: "POST",
                headers: {
                    "Content-Type": "application/json",
                    "X-CSRF-Token": me.csrf_token,
                    "Idempotency-Key": crypto.randomUUID(),
                    "If-Match": '"0"',
                },
                body: JSON.stringify(body),
            });
            if (!response.ok) {
                const problem = await response.json().catch(() => ({})) as { code?: string };
                throw new Error(`Could not seed ${resource} (${response.status} ${problem.code ?? "request failed"}).`);
            }
            return await response.json() as T;
        }
        const lineName = `E2E line ${suffix}`;
        const itemName = `E2E item ${suffix}`;
        const routingName = `E2E routing ${suffix}`;
        const calendarName = `E2E calendar ${suffix}`;
        const line = await create<{ id: string }>("lines", { schema_version: 1, display_name: lineName, active: true });
        const item = await create<{ id: string }>("items", { schema_version: 1, display_name: itemName, unit: "count", active: true });
        await create("shift-calendars", {
            schema_version: 1,
            display_name: calendarName,
            time_zone: "America/Detroit",
            shifts: [{ day_of_week: 1, start_local: "07:00", end_local: "15:00" }],
        });
        await create("routings", {
            schema_version: 1,
            display_name: routingName,
            active: true,
            line_id: line.id,
            item_id: item.id,
            steps: [
                { step_number: 1, workstation_id: press.workstation_id },
                { step_number: 2, workstation_id: weld.workstation_id },
            ],
        });
        return { lineName, itemName, routingName, calendarName, pressName: press.display_name, weldName: weld.display_name };
    }, String(Date.now()));

    await section(page, "Master data");
    await expect(page.getByRole("heading", { name: "Master data" })).toBeVisible();
    await expect(page.getByRole("table", { name: "Lines master data" }).getByText(data.lineName)).toBeVisible();
    await page.getByRole("button", { name: /^Items/ }).click();
    await expect(page.getByRole("table", { name: "Items master data" }).getByText(data.itemName)).toBeVisible();
    await page.getByRole("button", { name: /^Routings/ }).click();
    const routing = page.getByRole("row").filter({ hasText: data.routingName });
    await expect(routing).toBeVisible();
    await expect(routing.locator(".master-steps li")).toHaveText([`1 ${data.pressName}`, `2 ${data.weldName}`]);
    await page.getByRole("button", { name: /^Shift calendars/ }).click();
    await expect(page.getByRole("table", { name: "Shift calendars master data" }).getByText(data.calendarName)).toBeVisible();
    const calendar = page.getByRole("row").filter({ hasText: data.calendarName });
    await expect(calendar.getByText("Mon 07:00–15:00")).toBeVisible();

    await signOut(page);
    const viewerContext = await browser.newContext();
    try {
        const viewer = await viewerContext.newPage();
        await signIn(viewer, "read-only-viewer");
        await openSite(viewer);
        await section(viewer, "Master data");
        await expect(viewer.getByRole("table", { name: "Lines master data" }).getByText(data.lineName)).toBeVisible();
        await expect(viewer.locator(".page button")).toHaveCount(4);
        await expect(viewer.getByRole("button", { name: /create|new|edit|delete|save|retire|add/i })).toHaveCount(0);
    } finally {
        await viewerContext.close();
    }
});
