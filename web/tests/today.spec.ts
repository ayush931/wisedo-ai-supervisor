// Labelled stubbed API responses (WIS-278 Today, Approvals, landing by role): these exercise the
// real pages against fixed server replies; they are not evidence of backend or agent behaviour.
import { test, expect, type Page, type Route } from "@playwright/test";

const site = "00000000-0000-4000-8000-000000000311";
const tenant = "00000000-0000-4000-8000-000000000312";
const lineA = "00000000-0000-4000-8000-000000000321";
const lineB = "00000000-0000-4000-8000-000000000322";
const lineC = "00000000-0000-4000-8000-000000000323";
const livePlanA = "00000000-0000-4000-8000-000000000331";
const livePlanB = "00000000-0000-4000-8000-000000000332";
const livePlanC = "00000000-0000-4000-8000-000000000333";
const draftPlan = "00000000-0000-4000-8000-000000000334";
const proposal = "00000000-0000-4000-8000-000000000341";
const decidedProposal = "00000000-0000-4000-8000-000000000342";
const lapsedProposal = "43000000-0000-4000-8000-000000000343";
const openCase = "51000000-0000-4000-8000-000000000351";
const closedCase = "52000000-0000-4000-8000-000000000352";
const caseLine = "00000000-0000-4000-8000-000000000361";
const caseOrder = "00000000-0000-4000-8000-000000000362";
const conversation = "00000000-0000-4000-8000-000000000371";
const ws1 = "00000000-0000-4000-8000-000000000381";
const ws2 = "00000000-0000-4000-8000-000000000382";
const NOW = new Date("2026-10-30T10:00:00Z");
const at = (minutes: number) => new Date(NOW.getTime() + minutes * 60000).toISOString();
const actor = { issuer: "https://issuer.test", subject: "lead" };
const problem = (route: Route, status: number, code: string) =>
    route.fulfill({ status, contentType: "application/problem+json", json: { type: "about:blank", title: code, status, code, correlation_id: "stub" } });

const plan = (id: string, line: string, starts: string, ends: string, approved: number | null) => ({ id, line_id: line, starts_at: starts, ends_at: ends, approved_revision: approved, current_revision: 1, state_revision: 1 });
const live = [plan(livePlanA, lineA, at(-120), at(360), 1), plan(livePlanB, lineB, at(-120), at(360), 1), plan(livePlanC, lineC, at(-120), at(360), 1)];
const draft = plan(draftPlan, lineA, at(600), at(1080), null);
const detail = (p: ReturnType<typeof plan>) => ({ ...p, replayed: false, revisions: [{ revision: 1, state: "approved", created_at: at(-600), canonical_payload_digest: "a".repeat(64), digest_schema_version: 1, submitted_by: actor, approval: null, plan: { schema_version: 1, line_id: p.line_id, starts_at: p.starts_at, ends_at: p.ends_at, timezone: "Asia/Kolkata", assignments: [] } }] });
const station = (good: number | null, target: number) => ({
    workstation_id: ws1, job_order_id: ws1, bound_job_order_revisions: [1], bound_plan_revisions: [1], record_ids: [], readiness: "LIMITED", reasons: ["SHIFT_IN_PROGRESS"],
    planned: { item_code: "BD-102", job_order_revision: 1, target_cycle_time_seconds: null, target_quantity: target },
    recorded: { good_quantity: good, reject_quantity: good, remaining_quantity: null, closed_downtime_seconds: "0", completion_record_id: null, completion_state: "in_progress", completion_values_as_recorded: [], corrections: [], cycle_time: null, downtime: [], last_event_at: null, late_record_ids: [], material_exceptions: [], over_production: false, quality: { accepts_as_recorded: 0, dispositions_as_recorded: [], holds_as_recorded: 0, measurement_count: 0, measurement_record_ids: [], rejects_as_recorded: 0 }, quantity_record_ids: [] },
});
const snapshot = (planId: string, stations: ReturnType<typeof station>[], revision = 1) => ({
    plan_id: planId, revision, replayed: false, canonical_payload_digest: "b".repeat(64), digest_schema_version: 1, generated_at: at(-5), generated_by: actor,
    summary: { as_of: at(-5), starts_at: at(-120), ends_at: at(360), line_id: lineA, plan_id: planId, plan_revision: 1, plan_revision_digest: "c".repeat(64), formula_version: 1, schema_version: 1, readiness: "LIMITED", reasons: ["SHIFT_IN_PROGRESS"], shift_ended: false, source_record_count: 2, notes: [], policy: { revision: 1, stale_after_seconds: 900 }, jobs: { planned: stations.length, completed: 0, in_progress: stations.length, not_started: 0, unknown: 0, remaining: stations.length }, stations },
});
// The real canonical payload is the server's envelope; the action lives in body.
const action = { action_kind: "maintenance_investigation.create", template: "inspect_report_only", line_id: caseLine, order_id: caseOrder, instructions: "Inspect the grinder wheel on Grinder 05 and report its wear.", case_id: openCase, expires_at: at(20) };
const proposalBody = (state = "pending", schema = "ApprovedActionV1", id = proposal, expires = at(20)) => ({
    id, state, case_id: openCase, case_revision: 3, revision: 2, proposal_revision: 1, issued_at: at(-10), expires_at: expires, policy_revision: 1, replayed: false, canonical_payload_sha256: "d".repeat(64), decisions: [], submitted_by: null,
    canonical_payload: { schema, schema_version: 1, tenant_id: tenant, site_id: site, body: { ...action, expires_at: expires } },
});

type Options = {
    role?: string;
    plans?: ReturnType<typeof plan>[];
    proposals?: "list" | "empty" | "fail";
    cases?: "list" | "empty";
    decisions?: { status: number; code?: string }[];
    chatReply?: Record<string, unknown>;
    schema?: string;
    lapsed?: boolean;
    entry?: string;
};
async function stub(page: Page, o: Options = {}) {
    const writes: { path: string; body: any; headers: Record<string, string> }[] = [];
    const decisions = [...(o.decisions ?? [])];
    await page.clock.setFixedTime(NOW);
    await page.route("**/v1/**", async (route) => {
        const req = route.request();
        const url = new URL(req.url());
        const path = url.pathname.replace(`/v1/sites/${site}`, "");
        if (url.pathname === "/v1/me") return route.fulfill({ json: { csrf_token: "stub", subject: "lead", display_name: "Rahul Deshmukh", sites: [{ id: site, name: "Chakan Plant", role: o.role ?? "supervisor", tenant_id: tenant, agent_available: false }] } });
        if (req.method() === "POST") writes.push({ path, body: req.postDataJSON(), headers: req.headers() });
        if (path === "/workstations") return route.fulfill({ json: { workstations: [ws1, ws2].map((id, i) => ({ workstation_id: id, line_id: lineA, display_name: `Grinder 0${i + 5}`, current_revision: 1 })) } });
        if (path === `/workstations/${ws1}/authorizations`) return route.fulfill({ json: { authorizations: [{ workstation_id: ws1, active: true, current_revision: 1, replayed: false, events: [], grantee: { issuer: actor.issuer, subject: "lead" } }] } });
        if (path === `/workstations/${ws2}/authorizations`) return route.fulfill({ json: { authorizations: [{ workstation_id: ws2, active: true, current_revision: 1, replayed: false, events: [], grantee: { issuer: actor.issuer, subject: "someone-else" } }] } });
        if (path === "/members") return route.fulfill({ json: { members: [] } });
        if (path === "/shift-plans") return route.fulfill({ json: { plans: o.plans ?? [...live, draft] } });
        for (const p of [...(o.plans ?? []), ...live, draft]) if (path === `/shift-plans/${p.id}`) return route.fulfill({ json: detail(p) });
        if (path === `/shift-plans/${livePlanA}/summaries` && req.method() === "GET") return route.fulfill({ json: { summaries: [{ revision: 1, plan_revision: 1, readiness: "LIMITED", generated_at: at(-5), generated_by: actor, canonical_payload_digest: "b".repeat(64) }] } });
        if (path === `/shift-plans/${livePlanA}/summaries/1`) return route.fulfill({ json: snapshot(livePlanA, [station(0, 100)]) });
        if (path === `/shift-plans/${livePlanB}/summaries` && req.method() === "GET") return route.fulfill({ json: { summaries: [{ revision: 1, plan_revision: 1, readiness: "LIMITED", generated_at: at(-5), generated_by: actor, canonical_payload_digest: "b".repeat(64) }] } });
        if (path === `/shift-plans/${livePlanB}/summaries/1`) return route.fulfill({ json: snapshot(livePlanB, [station(40, 50), station(null, 50)]) });
        if (path === `/shift-plans/${livePlanC}/summaries` && req.method() === "GET") return route.fulfill({ json: { summaries: [] } });
        if (path === `/shift-plans/${livePlanC}/summaries`) return route.fulfill({ status: 201, json: snapshot(livePlanC, [station(7, 20)]) });
        const names: Record<string, string> = { [lineA]: "Brake disc line", [lineB]: "Knuckle line", [lineC]: "Shaft line" };
        for (const [id, name] of Object.entries(names)) if (path === `/lines/${id}`) return route.fulfill({ json: { id, current_revision: 1, replayed: false, revisions: [{ revision: 1, body: { display_name: name, active: true, schema_version: 1 } }] } });
        if (path === `/entities/${caseLine}`) return route.fulfill({ json: { id: caseLine, display_name: "Grinding cell" } });
        if (path === `/entities/${caseOrder}`) return route.fulfill({ json: { id: caseOrder, display_name: "Order A-100-0A6D" } });
        if (path === "/proposals") {
            if (o.proposals === "fail") return problem(route, 503, "STORAGE_UNAVAILABLE");
            if (o.proposals === "empty") return route.fulfill({ json: { items: [], next_cursor: null } });
            const items = [{ id: decidedProposal, state: "approved", case_id: closedCase, expires_at: at(5), proposal_revision: 1, revision: 3 }, { id: proposal, state: "pending", case_id: openCase, expires_at: at(20), proposal_revision: 1, revision: 2 }];
            if (o.lapsed) items.push({ id: lapsedProposal, state: "pending", case_id: openCase, expires_at: at(-1), proposal_revision: 1, revision: 2 });
            return route.fulfill({ json: { items, next_cursor: null } });
        }
        if (path === `/proposals/${proposal}`) return route.fulfill({ json: proposalBody("pending", o.schema) });
        if (path === `/proposals/${lapsedProposal}`) return route.fulfill({ json: proposalBody("pending", undefined, lapsedProposal, at(-1)) });
        if (path === `/proposals/${proposal}/decisions`) {
            const next = decisions.shift() ?? { status: 200 };
            if (next.status !== 200) return problem(route, next.status, next.code!);
            return route.fulfill({ json: proposalBody(req.postDataJSON().decision === "approve" ? "approved" : "rejected") });
        }
        if (path === "/cases") {
            const items = [
                { id: closedCase, line_id: caseLine, order_id: caseOrder, episode: 1, revision: 9, state: "resolved", detected_at: at(-300), latest_window_end: at(-270) },
                { id: openCase, line_id: caseLine, order_id: caseOrder, episode: 1, revision: 3, state: "awaiting_approval", detected_at: at(-30), latest_window_end: at(-15) },
            ];
            return route.fulfill({ json: { items: o.cases === "empty" ? [] : items, next_cursor: null } });
        }
        const conv = { conversation_id: conversation, title: "Today", revision: 1, created_at: at(0), updated_at: at(0) };
        if (path === "/chat-conversations") return route.fulfill({ status: 201, json: conv });
        if (path === `/chat-conversations/${conversation}/messages`) {
            const body = req.postDataJSON();
            const question = { message_id: "q1", seq: 1, author_kind: "human", state: "received", body: body.text, citations: [], created_at: at(0), request_id: body.request_id };
            return route.fulfill({ status: 201, json: { conversation: { ...conv, revision: 2 }, replayed: false, question, reply: { message_id: "r1", seq: 2, author_kind: "assistant", citations: [], created_at: at(0), reply_to: "q1", ...o.chatReply } } });
        }
        return problem(route, 404, "NOT_FOUND");
    });
    if (o.entry) await page.goto(o.entry);
    else {
        await page.goto("/sites");
        await page.getByRole("link", { name: "Chakan Plant" }).click();
    }
    return writes;
}
const needsYou = (page: Page) => page.getByRole("region", { name: "Needs you" });

test("stubbed Today orders proposals, then open cases, then plans awaiting approval; settled items are left out", async ({ page }) => {
    await stub(page);
    await expect(page).toHaveURL(new RegExp(`/sites/${site}/today$`));
    await expect(page.getByRole("heading", { level: 1 })).toContainText("3 things need you.");
    // The header shows the live shift window in the plan's timezone, not the browser's.
    await expect(page.getByText(/Chakan Plant · .*13:30–21:30 IST/)).toBeVisible();
    const cards = needsYou(page).getByRole("article");
    await expect(cards).toHaveCount(3);
    await expect(cards.nth(0)).toContainText("Inspect the grinder wheel on Grinder 05 and report its wear.");
    await expect(cards.nth(0)).toContainText("Expires in 20 min");
    await expect(cards.nth(0)).toContainText("Grinding cell");
    await expect(cards.nth(1)).toContainText("Production shortfall detected");
    await expect(cards.nth(1)).toContainText("Awaiting approval");
    // A display name that already says "Order" is not prefixed again.
    await expect(cards.nth(1)).toContainText("Order A-100-0A6D ·");
    await expect(cards.nth(1)).not.toContainText("Order Order");
    await expect(cards.nth(1).getByRole("link", { name: "Open case" })).toHaveAttribute("href", `/sites/${site}/cases/${openCase}`);
    await expect(cards.nth(2)).toContainText("Plan awaiting approval");
    // In the plan's timezone, not the browser's.
    await expect(cards.nth(2)).toContainText("Sat 31 Oct · 01:30–09:30 IST");
    await expect(cards.nth(2).getByRole("link", { name: "Review" })).toHaveAttribute("href", `/sites/${site}/shifts/${draftPlan}`);
    // A resolved case and an already approved proposal are not shown as needing anyone.
    await expect(needsYou(page).getByRole("article", { name: `Case ${closedCase.slice(0, 8)}` })).toHaveCount(0);
});

test("stubbed Today empty states say what will appear; a failed section never reads as nothing", async ({ page }) => {
    await stub(page, { plans: [], proposals: "empty", cases: "empty" });
    await expect(page.getByRole("heading", { level: 1 })).toContainText("Nothing needs you right now.");
    await expect(page.getByText(/Nothing waiting\. Proposals that need your decision, open shortfall cases and shift plans awaiting approval will appear here\./)).toBeVisible();
    await expect(page.getByText(/No shift plan covers the current time\./)).toBeVisible();

    await page.unrouteAll({ behavior: "ignoreErrors" });
    await stub(page, { plans: [], proposals: "fail", cases: "empty" });
    await expect(needsYou(page).getByRole("alert").filter({ hasText: /\S/ })).toContainText("The service is busy or unavailable.");
    await expect(page.getByRole("heading", { level: 1 })).not.toContainText("Nothing needs you");
    await expect(page.getByText(/Nothing waiting\./)).toHaveCount(0);
});

test("stubbed proposal decision: a reason is required, 428 asks for a fresh sign-in, success only from the server reply", async ({ page }) => {
    const writes = await stub(page, { decisions: [{ status: 428, code: "REAUTHENTICATION_REQUIRED" }] });
    const card = needsYou(page).getByRole("article").first();
    await card.getByRole("button", { name: "Approve" }).click();
    await expect(card.getByRole("alert").filter({ hasText: /\S/ })).toHaveText("Enter a reason before you approve or reject.");
    expect(writes.filter((w) => w.path.endsWith("/decisions"))).toEqual([]);

    await card.getByLabel("Reason for your decision").fill("Wear matches the evidence.");
    await card.getByRole("button", { name: "Approve" }).click();
    await expect(card.getByRole("alert").filter({ hasText: /\S/ })).toContainText("Confirm your identity to continue. Nothing was submitted.");
    await expect(card.getByRole("button", { name: "Confirm identity" })).toBeVisible();
    await expect(page.getByRole("status").filter({ hasText: /^✓ Approved/ })).toHaveCount(0);

    await card.getByRole("button", { name: "Approve" }).click();
    const done = page.getByRole("status").filter({ hasText: "Approved: Inspect the grinder wheel" });
    await expect(done).toContainText("server state approved");
    await expect(done).toBeFocused();
    const sent = writes.filter((w) => w.path === `/proposals/${proposal}/decisions`);
    expect(sent).toHaveLength(2);
    expect(sent[1].body).toEqual({ decision: "approve", expected_revision: 2, reason: "Wear matches the evidence." });
    expect(sent[1].headers["if-match"]).toBe('"2"');
    expect(sent[1].headers["idempotency-key"]).toMatch(/^[0-9a-f-]{36}$/);
});

test("stubbed proposal reject sends a reject decision; an ineligible reviewer gets a plain refusal", async ({ page }) => {
    const writes = await stub(page, { decisions: [{ status: 403, code: "REVIEWER_NOT_ELIGIBLE" }] });
    await page.getByRole("navigation", { name: "Site sections" }).getByRole("link", { name: "Approvals" }).click();
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("2 waiting on you");
    const card = page.getByRole("article").filter({ hasText: "Inspect the grinder wheel" });
    await card.getByLabel("Reason for your decision").fill("Not needed.");
    await card.getByRole("button", { name: "Approve" }).click();
    await expect(card.getByRole("alert").filter({ hasText: /\S/ })).toContainText("You cannot decide this proposal");
    await card.getByRole("button", { name: "Reject" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Rejected: Inspect the grinder wheel" })).toBeVisible();
    expect(writes.filter((w) => w.path.endsWith("/decisions")).map((w) => w.body.decision)).toEqual(["approve", "reject"]);
});

test("stubbed Ask shows the server's reason when the assistant cannot answer", async ({ page }) => {
    const writes = await stub(page, { chatReply: { state: "unavailable", reason_code: "ASSISTANT_NOT_QUALIFIED" } });
    await expect(page.getByRole("heading", { name: "Ask about how the plant runs" })).toBeVisible();
    await page.getByLabel("Your question").fill("How is a changeover done on the press?");
    await page.getByRole("button", { name: "Ask", exact: true }).click();
    await expect(page.getByText("No answer: The assistant has not passed qualification yet, so it does not answer.")).toBeVisible();
    const post = writes.find((w) => w.path.endsWith("/messages"))!;
    expect(post.headers["if-match"]).toBe('"1"');
    expect(post.body.text).toBe("How is a changeover done on the press?");
    expect(post.body.request_id).toMatch(/^[0-9a-f-]{36}$/);
});

test("stubbed Ask shows an answer with its citations", async ({ page }) => {
    await stub(page, { chatReply: { state: "answered", body: "Hold the first five parts for inspection.", citations: [{ kind: "source", id: caseOrder, revision: 4 }] } });
    await page.getByLabel("Your question").fill("What happens after a changeover?");
    await page.getByRole("button", { name: "Ask", exact: true }).click();
    await expect(page.getByText("Hold the first five parts for inspection.")).toBeVisible();
    await expect(page.getByLabel("Citations")).toHaveText(`source ${caseOrder.slice(0, 8)} · rev 4`);
});

// WIS-627: an answer whose passages were chosen by word match only, although semantic retrieval
// is on, says so; a hybrid or word answer shows no such note. WIS-647: nor does a hybrid answer
// show the unreranked note, which only a hybrid_unreranked answer shows.
test("stubbed Ask labels a word-fallback answer and no other", async ({ page }) => {
    await stub(page, { chatReply: { state: "answered", body: "Bake at 180 degrees.", citations: [], retrieval: "word_fallback" } });
    await page.getByLabel("Your question").fill("How hot is the oven?");
    await page.getByRole("button", { name: "Ask", exact: true }).click();
    await expect(page.getByText("Bake at 180 degrees.")).toBeVisible();
    await expect(page.getByText(/found by word match only/)).toBeVisible();

    await page.unrouteAll({ behavior: "ignoreErrors" });
    await stub(page, { chatReply: { state: "answered", body: "Bake at 180 degrees.", citations: [], retrieval: "hybrid" } });
    await page.getByLabel("Your question").fill("How hot is the oven?");
    await page.getByRole("button", { name: "Ask", exact: true }).click();
    await expect(page.getByText("Bake at 180 degrees.")).toBeVisible();
    await expect(page.getByText(/found by word match only/)).toHaveCount(0);
    await expect(page.getByText(/reranking step did not answer/)).toHaveCount(0);

    // WIS-647: a hybrid answer whose configured rerank did not answer says so.
    await page.unrouteAll({ behavior: "ignoreErrors" });
    await stub(page, { chatReply: { state: "answered", body: "Bake at 180 degrees.", citations: [], retrieval: "hybrid_unreranked" } });
    await page.getByLabel("Your question").fill("How hot is the oven?");
    await page.getByRole("button", { name: "Ask", exact: true }).click();
    await expect(page.getByText("Bake at 180 degrees.")).toBeVisible();
    await expect(page.getByText(/reranking step did not answer/)).toBeVisible();
    await expect(page.getByText(/found by word match only/)).toHaveCount(0);
});

// WIS-640: an answer's approved values are shown with the approved claim's unit; an answer without
// them shows none.
test("stubbed Ask shows the approved values of an answer", async ({ page }) => {
    const quantities = [{ statement: 0, claim_id: "11111111-1111-4111-8111-111111111111", predicate: "cycle_time_seconds", value: "45", unit: "s", span_id: "22222222-2222-4222-8222-222222222222" }];
    await stub(page, { chatReply: { state: "answered", body: "The press takes 45 seconds a part.", citations: [], quantities } });
    await page.getByLabel("Your question").fill("How long does the press take?");
    await page.getByRole("button", { name: "Ask", exact: true }).click();
    await expect(page.getByLabel("Approved values")).toHaveText("45 s · cycle time seconds");

    await page.unrouteAll({ behavior: "ignoreErrors" });
    await stub(page, { chatReply: { state: "answered", body: "The press takes 45 seconds a part.", citations: [] } });
    await page.getByLabel("Your question").fill("How long does the press take?");
    await page.getByRole("button", { name: "Ask", exact: true }).click();
    await expect(page.getByText("The press takes 45 seconds a part.")).toBeVisible();
    await expect(page.getByLabel("Approved values")).toHaveCount(0);
});

test("stubbed lines on shift keep unknown apart from zero and generate a missing summary", async ({ page }) => {
    const writes = await stub(page, { plans: [live[0], live[1], { ...live[2], approved_revision: null }, draft] });
    const lines = page.getByRole("region", { name: "Lines on shift" });
    const a = lines.getByRole("article").filter({ hasText: "Brake disc line" });
    const b = lines.getByRole("article").filter({ hasText: "Knuckle line" });
    const c = lines.getByRole("article").filter({ hasText: "Shaft line" });
    // A recorded zero is zero.
    await expect(a.locator(".big-line")).toHaveText("0 of 100 planned");
    await expect(a).toContainText("Sum of good parts recorded at each station, not line output.");
    await expect(a.getByText("Not approved")).toHaveCount(0);
    await expect(a.getByRole("progressbar")).toHaveAttribute("aria-valuenow", "0");
    // One station without a reported count makes the line total unknown, never 40.
    await expect(b.locator(".big-line strong")).toHaveText("—");
    await expect(b.locator(".big-line")).toContainText("good not reported for every station");
    await expect(b.getByRole("progressbar")).toHaveCount(0);
    // A live plan that is not approved is marked, never shown as the approved shift.
    await expect(c.getByText("Not approved")).toBeVisible();
    // No summary: say so, offer to generate one, and show it only from the server's reply.
    await expect(c).toContainText("No summary yet.");
    await c.getByRole("button", { name: "Update summary" }).click();
    await expect(c.getByRole("status").filter({ hasText: /\S/ })).toHaveText("✓ Generated summary r1");
    await expect(c.locator(".big-line")).toHaveText("7 of 20 planned");
    expect(writes.filter((w) => w.path.endsWith("/summaries")).map((w) => w.body)).toEqual([{ schema_version: 1 }]);
});

test("stubbed viewer sees Today read-only: no Ask, no decisions, no summary generation", async ({ page }) => {
    await stub(page, { role: "viewer" });
    await expect(page).toHaveURL(new RegExp(`/sites/${site}/today$`));
    await expect(needsYou(page).getByRole("article")).toHaveCount(3);
    await expect(page.getByRole("heading", { name: "Ask about how the plant runs" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Update summary" })).toHaveCount(0);
});

test("stubbed operator lands on My station, sees only granted stations, and the choice is remembered", async ({ page }) => {
    await stub(page, { role: "operator" });
    await expect(page).toHaveURL(new RegExp(`/sites/${site}/station$`));
    const tabs = page.getByRole("navigation", { name: "Site sections" });
    await expect(tabs.getByRole("link")).toHaveText(["My station", "Today", "Plan", "Setup"]);
    const mine = page.getByRole("list", { name: "Your stations" });
    await expect(mine.getByRole("link")).toHaveText(["Grinder 05"]);
    await mine.getByRole("link", { name: "Grinder 05" }).click();
    await expect(page).toHaveURL(new RegExp(`/workstations/${ws1}$`));
    await tabs.getByRole("link", { name: "My station" }).click();
    await expect(page.getByRole("link", { name: "Continue at Grinder 05" })).toBeVisible();
});

test("stubbed proposal with a non-envelope payload is shown but cannot be decided (B1)", async ({ page }) => {
    await stub(page, { schema: "SomethingElseV1" });
    const card = needsYou(page).getByRole("article").first();
    await expect(card.getByRole("alert").filter({ hasText: /\S/ })).toContainText("not a recognised approved action");
    await expect(card.getByRole("button", { name: "Approve" })).toHaveCount(0);
    await expect(card).not.toContainText("Inspect the grinder wheel");
});

test("stubbed owner cannot decide proposals: read-only card, not counted as needing them", async ({ page }) => {
    await stub(page, { role: "owner" });
    await expect(page.getByRole("heading", { level: 1 })).toContainText("2 things need you.");
    const card = needsYou(page).getByRole("article").first();
    await expect(card).toContainText("Inspect the grinder wheel on Grinder 05 and report its wear.");
    await expect(card).toContainText("Only a site supervisor can approve or reject this proposal.");
    await expect(card.getByRole("button", { name: "Approve" })).toHaveCount(0);
    await expect(card.getByRole("button", { name: "Reject" })).toHaveCount(0);
    await expect(page.getByRole("heading", { name: "Ask about how the plant runs" })).toBeVisible();
});

test("stubbed expired proposals are listed apart and not counted; expiry and stale refusals read plainly", async ({ page }) => {
    await stub(page, { lapsed: true, decisions: [{ status: 409, code: "APPROVAL_EXPIRED" }, { status: 409, code: "PROPOSAL_STALE" }] });
    await expect(page.getByRole("heading", { level: 1 })).toContainText("3 things need you.");
    const lapsed = page.getByRole("region", { name: "Expired without a decision" });
    await expect(lapsed.getByRole("article")).toHaveCount(1);
    await expect(lapsed.getByRole("article")).toContainText("Expired");
    await expect(lapsed.getByRole("button", { name: "Approve" })).toHaveCount(0);
    const card = needsYou(page).getByRole("article").first();
    await card.getByLabel("Reason for your decision").fill("Evidence supports it.");
    await card.getByRole("button", { name: "Approve" }).click();
    await expect(card.getByRole("alert").filter({ hasText: /\S/ })).toContainText("This proposal expired before a decision. Nothing was approved.");
    await card.getByRole("button", { name: "Approve" }).click();
    await expect(card.getByRole("alert").filter({ hasText: /\S/ })).toContainText("The evidence behind this proposal changed. It can no longer be approved.");
});

for (const [role, landing] of [["supervisor", "today"], ["operator", "station"]]) {
    test(`stubbed one-site ${role} goes from / straight to ${landing}`, async ({ page }) => {
        await stub(page, { role, entry: "/" });
        await expect(page).toHaveURL(new RegExp(`/sites/${site}/${landing}$`));
        await expect(page.getByRole("heading", { name: "Your sites" })).toHaveCount(0);
    });
}
