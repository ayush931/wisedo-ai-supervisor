import { expect, test, type Page, type Route } from "@playwright/test";
import { signIn, signOut } from "./session";

// WIS-713: sign out must end the sign-in. The stubbed cases pin the UI contract: a failed
// POST /auth/logout is shown and the person stays visibly signed in (the server session is still
// valid), and a confirmed logout follows the provider's end-session URL. The server side (cookie
// revoked, prompt=login, end_session_url) is TestWIS713SignOutEndsProviderSession.

const site = "00000000-0000-4000-8000-000000000713";
const me = {
    csrf_token: "test-csrf",
    display_name: "Previous Person",
    subject: "previous-subject",
    sites: [{ id: site, name: "Stubbed Site", role: "supervisor", tenant_id: "00000000-0000-4000-8000-000000000714", agent_available: false }],
};
const notFound = { status: 404, contentType: "application/problem+json", body: JSON.stringify({ code: "NOT_FOUND", status: 404, title: "NOT_FOUND", type: "about:blank" }) };

async function signedIn(page: Page, logout: (route: Route) => Promise<void>) {
    let signedOut = false;
    await page.route("**/v1/**", async (route) => {
        if (new URL(route.request().url()).pathname === "/v1/me") return signedOut ? route.fulfill({ status: 401, contentType: "application/problem+json", body: JSON.stringify({ code: "AUTHENTICATION_REQUIRED", status: 401, title: "AUTHENTICATION_REQUIRED", type: "about:blank" }) }) : route.fulfill({ json: me });
        return route.fulfill(notFound);
    });
    await page.route("**/auth/logout", async (route) => {
        expect(route.request().method()).toBe("POST");
        expect(route.request().headers()["x-csrf-token"]).toBe("test-csrf");
        await logout(route);
    });
    await page.goto("/");
    await expect(page.getByRole("button", { name: "Sign out", exact: true })).toBeVisible();
    return () => (signedOut = true);
}

const failures: [string, (route: Route) => Promise<void>][] = [
    ["CSRF rejected (403)", (r) => r.fulfill({ status: 403, contentType: "application/json", body: '{"error":"request could not be completed"}' })],
    ["server unavailable (503)", (r) => r.fulfill({ status: 503, contentType: "application/json", body: '{"error":"request could not be completed"}' })],
    ["connection lost", (r) => r.abort("connectionreset")],
];
for (const [name, fail] of failures) {
    test(`a failed sign out is shown and the person stays signed in: ${name}`, async ({ page }) => {
        await signedIn(page, fail);
        await page.getByRole("button", { name: "Sign out", exact: true }).click();
        await expect(page.getByRole("alert").getByText("Sign out failed. You are still signed in.")).toBeVisible();
        await expect(page.getByRole("button", { name: "Sign out", exact: true })).toBeVisible();
        await expect(page.getByText("Previous Person")).toBeVisible();
        await expect(page.getByRole("link", { name: "Sign in", exact: true })).toHaveCount(0);
    });
}

test("a confirmed sign out follows the provider's end-session URL", async ({ page }) => {
    const end = "/stub-idp/logout?client_id=supervisor";
    let markSignedOut = () => {};
    markSignedOut = await signedIn(page, async (r) => {
        markSignedOut();
        await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ end_session_url: new URL(end, page.url()).toString() }) });
    });
    await page.route("**/stub-idp/logout**", (r) => r.fulfill({ contentType: "text/html", body: "<h1>Provider signed out</h1>" }));
    await page.getByRole("button", { name: "Sign out", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Provider signed out" })).toBeVisible();
    expect(new URL(page.url()).pathname).toBe("/stub-idp/logout");
});

test("a sign out without a provider end-session shows the signed-out page", async ({ page }) => {
    let markSignedOut = () => {};
    markSignedOut = await signedIn(page, async (r) => {
        markSignedOut();
        await r.fulfill({ status: 204 });
    });
    await page.getByRole("button", { name: "Sign out", exact: true }).click();
    await expect(page.getByRole("link", { name: "Sign in", exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Sign out", exact: true })).toHaveCount(0);
});

// Real Keycloak (supervisor-e2e stack): sign out goes through Keycloak's logout confirmation (no
// id_token_hint is stored), ends the Keycloak session, and Sign in then shows the provider's empty
// form; it never reopens the previous account.
test("after sign out, sign in shows the identity provider's form, not the previous account", async ({ page }) => {
    await signIn(page, "foundation-reviewer");
    const sso = async () => (await page.context().cookies()).filter((c) => c.name === "KEYCLOAK_IDENTITY" && c.value !== "");
    expect(await sso()).toHaveLength(1);
    await page.getByRole("button", { name: "Sign out", exact: true }).click();
    await expect(page.getByText("Do you want to log out?")).toBeVisible();
    await page.getByRole("button", { name: "Logout", exact: true }).click();
    await expect(page.getByRole("link", { name: "Sign in", exact: true })).toBeVisible();
    expect(await sso()).toHaveLength(0);
    await page.getByRole("link", { name: "Sign in", exact: true }).click();
    await expect(page.getByLabel("Username or email")).toBeVisible();
    await expect(page.getByLabel("Username or email")).toHaveValue("");
    await expect(page.getByRole("button", { name: "Sign out", exact: true })).toHaveCount(0);
});
