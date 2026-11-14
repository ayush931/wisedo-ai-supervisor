import { expect, test, type Page, type Route } from "@playwright/test";
import { section } from "./session";

// UI-only stub spec (WIS-273 review of #187, M4): the real-backend specs run with the agent off and
// can never reach needs_input, so the clarification form is covered here against a stubbed API.
// Mocks qualify the UI component only, never the server rule.

const siteId = "00000000-0000-4000-8000-000000000911";
const caseId = "00000000-0000-4000-8000-000000000912";
const lineId = "00000000-0000-4000-8000-000000000913";
const orderId = "00000000-0000-4000-8000-000000000914";
const evaluator = { issuer: "supervisor:evaluator", subject: "shortfall-evaluator" };
const workflow = { issuer: "supervisor:temporal", subject: "case-workflow" };

function caseAt(revision: number, state: string, triggers: any[] = []) {
    const transitions = [
        { revision: 1, from_state: "none", to_state: "detected", event: "trigger", reason: null, actor: evaluator, created_at: "2040-01-01T07:45:00Z" },
        { revision: 2, from_state: "detected", to_state: "investigating", event: "context_ready", reason: null, actor: workflow, created_at: "2040-01-01T07:46:00Z" },
        { revision: 3, from_state: "investigating", to_state: "needs_input", event: "evidence_missing", reason: "EVIDENCE_REVOKED", actor: workflow, created_at: "2040-01-01T07:50:00Z" },
    ];
    if (revision >= 4) transitions.push({ revision: 4, from_state: "needs_input", to_state: "investigating", event: "clarified", reason: null, actor: { issuer: "https://issuer.test", subject: "operator-subject" }, created_at: "2040-01-01T08:00:00Z" });
    return {
        id: caseId, site_id: siteId, line_id: lineId, order_id: orderId, exception_kind: "production_shortfall", episode: 1, previous_case_id: null,
        revision, state, context_snapshot_id: "00000000-0000-4000-8000-000000000915", context_revision: 1, context_digest: "a".repeat(64),
        owner_user_id: null, detected_at: "2040-01-01T07:45:00Z", latest_window_end: "2040-01-01T07:45:00Z", trigger_evidence_ids: [caseId], triggers,
        workflow_id: `case:t:${caseId}`, resolution_reason: null, replayed: false, transitions,
        clarifications: revision >= 4 ? [{ id: "00000000-0000-4000-8000-000000000916", body: "The coil ran out at 07:40.", body_sha256: "b".repeat(64), case_revision: 4, context_snapshot_id: null, actor: { issuer: "https://issuer.test", subject: "operator-subject" }, created_at: "2040-01-01T08:00:00Z" }] : [],
    };
}

type Posted = { body: any; ifMatch: string | null; key: string | null };

// openNeedsInputCase signs in as role, stubs the case API and opens the case page. onPost answers
// the clarification; the case GET always answers with `current()`.
async function openNeedsInputCase(page: Page, role: string, current: () => any, onPost: (route: Route, posted: Posted) => Promise<void>) {
    await page.route("**/v1/**", async (route) => {
        const url = new URL(route.request().url());
        const method = route.request().method();
        if (url.pathname === "/v1/me")
            return route.fulfill({ json: { csrf_token: "test-csrf", display_name: "Test User", subject: `${role}-subject`, sites: [{ id: siteId, name: "Clarify Site", role, tenant_id: "00000000-0000-4000-8000-000000000910" }] } });
        if (url.pathname.endsWith("/members")) return route.fulfill({ json: { members: [] } });
        if (url.pathname.endsWith(`/entities/${lineId}`)) return route.fulfill({ json: { entity_id: lineId, display_name: "Bracket line 1" } });
        if (url.pathname.endsWith(`/entities/${orderId}`)) return route.fulfill({ json: { entity_id: orderId, display_name: "Order 4711" } });
        if (url.pathname.endsWith("/cases") && method === "GET") {
            const c = current();
            return route.fulfill({ json: { items: [{ id: c.id, line_id: lineId, order_id: orderId, episode: 1, revision: c.revision, state: c.state, detected_at: c.detected_at, latest_window_end: c.latest_window_end }], next_cursor: null } });
        }
        if (url.pathname.endsWith(`/cases/${caseId}`) && method === "GET") return route.fulfill({ json: current() });
        if (url.pathname.endsWith(`/cases/${caseId}/clarifications`) && method === "POST") {
            const headers = route.request().headers();
            return onPost(route, { body: route.request().postDataJSON(), ifMatch: headers["if-match"] ?? null, key: headers["idempotency-key"] ?? null });
        }
        return route.fulfill({ status: 404, contentType: "application/problem+json", json: { type: "about:blank", title: "Not stubbed", status: 404, code: "NOT_FOUND", correlation_id: "test" } });
    });
    await page.goto("/sites");
    await page.getByRole("listitem").getByRole("link").first().click();
    await section(page, "Cases");
    await page.getByRole("table", { name: "Cases" }).getByRole("link", { name: `Case ${caseId.slice(0, 8)}` }).click();
    await expect(page.getByRole("heading", { name: `Case ${caseId.slice(0, 8)}` })).toBeVisible();
}

test("an operator answers a needs_input case against its exact revision and sees the server's result", async ({ page }) => {
    let current = caseAt(3, "needs_input");
    const posted: Posted[] = [];
    await openNeedsInputCase(page, "operator", () => current, async (route, p) => {
        posted.push(p);
        current = caseAt(4, "investigating");
        await route.fulfill({ json: current });
    });
    // The question asked is the reason of the transition into needs_input, in words.
    const form = page.getByRole("form", { name: "Answer the open question" });
    await expect(form).toContainText("Source revoked during run");
    await expect(form.getByRole("button", { name: "Record clarification" })).toBeDisabled();
    await form.getByLabel("Clarification").fill("The coil ran out at 07:40.");
    await form.getByRole("button", { name: "Record clarification" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Your clarification was recorded as case evidence." })).toContainText("revision 4");
    expect(posted).toHaveLength(1);
    expect(posted[0].body).toEqual({ expected_revision: 3, body: "The coil ran out at 07:40.", new_context_id: null });
    expect(posted[0].ifMatch).toBe('"3"');
    expect(posted[0].key).toMatch(/^[0-9a-f-]{36}$/);
    await expect(page.getByRole("form", { name: "Answer the open question" })).toHaveCount(0); // no longer needs_input
    await expect(page.getByRole("list", { name: "Case timeline" })).toContainText("The coil ran out at 07:40.");
    // System moves are labelled as services, not as members.
    await expect(page.getByRole("list", { name: "Case timeline" })).toContainText("Shortfall evaluator (system)");
    await expect(page.getByRole("list", { name: "Case timeline" })).toContainText("Case workflow (system)");
});

test("a stale clarification is refused, the case reloads, and the page says nothing was submitted", async ({ page }) => {
    let current = caseAt(3, "needs_input");
    await openNeedsInputCase(page, "supervisor", () => current, async (route) => {
        current = caseAt(4, "investigating"); // someone else answered first
        await route.fulfill({ status: 412, contentType: "application/problem+json", json: { type: "about:blank", title: "Revision conflict", status: 412, code: "REVISION_CONFLICT", correlation_id: "test" } });
    });
    const form = page.getByRole("form", { name: "Answer the open question" });
    await form.getByLabel("Clarification").fill("Late answer");
    await form.getByRole("button", { name: "Record clarification" }).click();
    await expect(page.getByRole("status").filter({ hasText: "This changed on the server" })).toContainText("Nothing was submitted.");
    await expect(page.getByText("episode 1 · revision 4")).toBeVisible();
    await expect(page.getByRole("list", { name: "Case timeline" })).not.toContainText("Late answer");
});

test("a viewer sees the open question's case but no clarification form", async ({ page }) => {
    await openNeedsInputCase(page, "viewer", () => caseAt(3, "needs_input"), async (route) => {
        await route.fulfill({ status: 500, json: {} });
    });
    await expect(page.getByRole("list", { name: "Case timeline" })).toBeVisible();
    await expect(page.getByRole("form", { name: "Answer the open question" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Record clarification" })).toHaveCount(0);
});

// Simulator finding (2026-11-05): a later qualifying window that joins an escalated case changes no
// state, so the case page must still show it on the timeline, after the escalation it followed.
test("a shortfall that joined an escalated case is shown on its timeline", async ({ page }) => {
    const joinedContext = "a7c0e000-0000-4000-8000-000000000917";
    const joinedRecord = "00000000-0000-4000-8000-000000000918";
    const c = caseAt(3, "escalated", [
        { source_event_id: caseId, context_snapshot_id: "00000000-0000-4000-8000-000000000915", opened: true, actor: evaluator, created_at: "2040-01-01T07:45:00Z" },
        { source_event_id: joinedRecord, context_snapshot_id: joinedContext, opened: false, actor: evaluator, created_at: "2040-01-01T08:15:00.123456Z" },
    ]);
    c.transitions[2] = { revision: 3, from_state: "investigating", to_state: "escalated", event: "wait_deadline", reason: "HUMAN_WAIT_DEADLINE", actor: workflow, created_at: "2040-01-01T08:00:00Z" };
    await openNeedsInputCase(page, "viewer", () => c, async (route) => route.abort());
    const items = page.getByRole("list", { name: "Case timeline" }).getByRole("listitem");
    await expect(items).toHaveCount(4);
    // The opener is the r1 transition, not a second entry; the join follows the escalation.
    await expect(items.nth(3)).toContainText("Another shortfall joined this case");
    await expect(items.nth(3)).toContainText("The case did not change state.");
    await expect(items.nth(3)).toContainText(joinedContext.slice(0, 8));
    await expect(items.nth(2)).toContainText("Escalated");
});

// Review of 586442f: a join is placed between the transitions recorded around it, and after a
// clarification and the clarified transition it caused at the same revision.
test("joined shortfalls sit between the transitions around them and after a same-revision clarification", async ({ page }) => {
    const operator = { issuer: "https://issuer.test", subject: "operator-subject" };
    const join = (n: number, at: string) => ({ source_event_id: `b0000000-0000-4000-8000-00000000092${n}`, context_snapshot_id: `c${n}000000-0000-4000-8000-000000000930`, opened: false, actor: evaluator, created_at: at });
    const c = caseAt(4, "investigating", [
        { source_event_id: caseId, context_snapshot_id: "00000000-0000-4000-8000-000000000915", opened: true, actor: evaluator, created_at: "2040-01-01T07:45:00Z" },
        join(1, "2040-01-01T07:48:00Z"), // after r2 investigating (07:46), before r3 needs_input (07:50)
        join(2, "2040-01-01T08:05:00.5Z"), // after the clarification and r4 (08:05)
    ]);
    c.transitions[3] = { revision: 4, from_state: "needs_input", to_state: "investigating", event: "clarified", reason: null, actor: operator, created_at: "2040-01-01T08:05:00.001Z" };
    c.clarifications = [{ id: "00000000-0000-4000-8000-000000000916", body: "The coil ran out at 07:40.", body_sha256: "b".repeat(64), case_revision: 4, context_snapshot_id: null, actor: operator, created_at: "2040-01-01T08:05:00.002Z" }];
    await openNeedsInputCase(page, "viewer", () => c, async (route) => route.abort());
    const items = page.getByRole("list", { name: "Case timeline" }).getByRole("listitem");
    await expect(items).toHaveCount(7);
    const texts = await items.allInnerTexts();
    expect(texts.map((t) => (t.includes("Another shortfall joined") ? (t.includes("c1000000") ? "join1" : "join2") : t.startsWith("Clarification") ? "clarification" : t.split(":")[0]))).toEqual([
        "r1", "r2", "join1", "r3", "clarification", "r4", "join2",
    ]);
});
