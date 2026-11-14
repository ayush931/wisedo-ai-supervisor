import { expect, type Page } from "@playwright/test";

const env = (globalThis as typeof globalThis & { process?: { env?: Record<string, string | undefined> } }).process?.env ?? {};

// signIn drives the real Keycloak login. A missing fixture credential is BLOCKED, never a skip.
export async function signIn(page: Page, username: string) {
    const password = env.SUPERVISOR_LOCAL_USER_PASSWORD;
    if (!password) throw new Error("BLOCKED: SUPERVISOR_LOCAL_USER_PASSWORD required (run supervisor-seed and supervisor-demo-seed)");
    await page.goto("/");
    await page.getByRole("link", { name: "Sign in", exact: true }).click();
    await page.getByLabel("Username or email").fill(username);
    await page.getByLabel("Password", { exact: true }).fill(password);
    await page.getByRole("button", { name: "Sign In", exact: true }).click();
    // One site lands on it directly; several show the site list.
    await expect(page.getByRole("heading", { name: "Your sites" }).or(page.getByRole("navigation", { name: "Site sections" }))).toBeVisible();
    // A build preview shares the fixture IdP callback; return to the build the test targets.
    const origin=env.SUPERVISOR_PUBLIC_ORIGIN;
    if(origin&&new URL(page.url()).origin!==new URL(origin).origin){
        await page.goto(origin);
        await expect(page.getByRole("heading", { name: "Your sites" }).or(page.getByRole("navigation", { name: "Site sections" }))).toBeVisible();
    }
}

// openSite enters the first authorized site (a one-site member is already in it); the backend has no SPA fallback, so tests never
// deep-link with page.goto and navigate the way a user does.
export async function openSite(page: Page) {
    const list = page.getByRole("heading", { name: "Your sites" });
    const nav = page.getByRole("navigation", { name: "Site sections" });
    await expect(list.or(nav)).toBeVisible();
    if (await list.isVisible()) await page.getByRole("listitem").getByRole("link").first().click();
    await expect(nav).toBeVisible();
}

// section opens a page that lives under a top-level tab: Shifts and Job orders under Plan,
// everything else (Workstations, Master data, Cases, …) under Setup.
const planPages = ["Shifts", "Job orders", "Tasks"];
export async function section(page: Page, name: string) {
    const tab = planPages.includes(name) ? "Plan" : "Setup";
    await page.getByRole("navigation", { name: "Site sections" }).getByRole("link", { name: tab, exact: true }).click();
    await page.getByRole("navigation", { name: `${tab} pages` }).getByRole("link", { name, exact: true }).click();
}

// signOut also ends the Keycloak session: without an id_token_hint (WIS-713, no provider token is
// stored) Keycloak asks to confirm, then returns to the app's signed-out page.
export async function signOut(page: Page) {
    await page.getByRole("button", { name: "Sign out", exact: true }).click();
    const signInLink = page.getByRole("link", { name: "Sign in", exact: true });
    const confirm = page.getByRole("button", { name: "Logout", exact: true });
    await expect(signInLink.or(confirm)).toBeVisible();
    if (await confirm.isVisible()) await confirm.click();
    await expect(signInLink).toBeVisible();
}

// clientNavigate moves the in-app router without a server request.
export async function clientNavigate(page: Page, path: string) {
    await page.evaluate((to) => {
        history.pushState(null, "", to);
        dispatchEvent(new PopStateEvent("popstate"));
    }, path);
}

// openSeedShift opens the demo seed's plan for the current shift: the approved plan on press-01's
// line whose window contains now. The shift list is ordered by random plan ID and can hold earlier
// seeded windows and other specs' plans (shift-run.spec.ts), so neither position nor line alone
// identifies it. The list is still visited the way a user does; the plan opens by its ID.
export async function openSeedShift(page: Page) {
    const found = await page.evaluate(async () => {
        const me = await (await fetch("/v1/me")).json();
        const site = me.sites[0].id as string;
        const stations = await (await fetch(`/v1/sites/${site}/workstations`)).json();
        const line = stations.workstations.find((w: { display_name: string }) => w.display_name === "press-01")?.line_id as string | undefined;
        if (!line) return { site, plan: undefined };
        const now = Date.now();
        let cursor = "";
        for (let i = 0; i < 50; i++) {
            const page = await (await fetch(`/v1/sites/${site}/shift-plans?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`)).json();
            const hit = page.plans.find((p: { line_id: string; starts_at: string; ends_at: string; approved_revision: number | null }) =>
                p.line_id === line && p.approved_revision !== null && Date.parse(p.starts_at) <= now && now < Date.parse(p.ends_at));
            if (hit) return { site, plan: hit.id as string };
            if (!page.next_cursor) break;
            cursor = page.next_cursor;
        }
        return { site, plan: undefined };
    });
    if (!found.plan) throw new Error("BLOCKED: no approved seed plan for the current shift on press-01's line (run supervisor-demo-seed)");
    await section(page, "Shifts");
    await expect(page.getByRole("heading", { name: "Shifts", exact: true })).toBeVisible();
    await clientNavigate(page, `/sites/${found.site}/shifts/${found.plan}`);
    await expect(page.getByRole("heading", { name: "Stations" })).toBeVisible();
}
