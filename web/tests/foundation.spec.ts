import { test, expect } from "@playwright/test";
import { openSite, signOut } from "./session";

const localUserPassword = (
    globalThis as typeof globalThis & {
        process?: { env?: Record<string, string | undefined> };
    }
).process?.env?.SUPERVISOR_LOCAL_USER_PASSWORD;

test("real OIDC session and durable probe through API, SQL and Temporal", async ({
    page,
}) => {
    if (!localUserPassword)
        throw new Error("BLOCKED: local fixture credential required");
    await page.goto("/");
    await page.getByRole("link", { name: "Sign in", exact: true }).click();
    await page.getByLabel("Username or email").fill("foundation-reviewer");
    await page
        .getByLabel("Password", { exact: true })
        .fill(localUserPassword);
    await page.getByRole("button", { name: "Sign In", exact: true }).click();
    // The shell lands on the site (or the site list); the check is linked from Today.
    await openSite(page);
    await page.getByRole("link", { name: "Connection check", exact: true }).click();
    await expect(
        page.getByRole("heading", { name: "Connection check", exact: true }),
    ).toBeVisible();
    await page
        .getByLabel("Check description")
        .fill("Synthetic browser verification");
    await page
        .getByRole("button", { name: "Run connection check", exact: true })
        .click();
    await expect(
        page.getByRole("heading", {
            name: "Processing confirmed",
            exact: true,
        }),
    ).toBeVisible({ timeout: 30000 });
    const receipt = await page.locator(".identifier").innerText();
    await page
        .getByRole("button", { name: "Run connection check", exact: true })
        .click();
    await expect(page.locator(".identifier")).toHaveText(receipt);
    // Same-origin session, CSRF and non-disclosing scope denial are real HTTP checks.
    const result = await page.evaluate(async () => {
        const me = await (await fetch("/v1/me")).json();
        const csrf = await fetch(
            `/v1/sites/${me.sites[0].id}/foundation/probes`,
            {
                method: "POST",
                headers: {
                    "Content-Type": "application/json",
                    "Idempotency-Key": "no-csrf",
                },
                body: JSON.stringify({ message: "denied" }),
            },
        );
        const foreign = await fetch(
            "/v1/sites/00000000-0000-4000-8000-999999999999/foundation/probes/00000000-0000-4000-8000-999999999998",
        );
        return {
            csrf: csrf.status,
            foreign: foreign.status,
            storage: localStorage.length,
        };
    });
    expect(result).toEqual({ csrf: 403, foreign: 404, storage: 0 });
    // Privileged actions use a fresh provider proof while preserving the session's
    // original absolute expiry; the same real Keycloak account must return.
    await page.getByRole("button", { name: "Confirm identity", exact: true }).click();
    await page.getByLabel("Password", { exact: true }).fill(localUserPassword);
    await page.getByRole("button", { name: "Sign In", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Connection check", exact: true })).toBeVisible();
    // Sign-out now also ends the Keycloak session, which asks to confirm first (WIS-713).
    await signOut(page);
    expect(
        await page.evaluate(async () => (await fetch("/v1/me")).status),
    ).toBe(401);
});
