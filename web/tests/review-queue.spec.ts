import { expect, test, type Page, type Request } from "@playwright/test";

// WIS-641: Setup -> Facts to review lists the site's candidate claims for any reviewer, with the
// same claim card and review controls as the interview page. Every test stubs the API (UI
// behaviour); the list's tenant scope, revocation filter and paging are the server's
// GET /claims?state=candidate, and the real reviewer flow is web/tests/onboarding-pilot.spec.ts.

const site = "00000000-0000-4000-8000-000000000641";
const coil = "44444444-0000-4000-8000-000000000641";
const mill = "55555555-0000-4000-8000-000000000641";
const supplied = "66666666-0000-4000-8000-000000000641";
const cycle = "77777777-0000-4000-8000-000000000641";
const span = "88888888-0000-4000-8000-000000000641";
const passage = "Steel coil is supplied by Deccan Steel. The Press cycle time is 45 s.";
const meFor = (role: string) => ({
    csrf_token: "test-csrf",
    display_name: "Stubbed reviewer",
    subject: "reviewer-subject",
    sites: [{ id: site, name: "Stubbed Site", role, tenant_id: "00000000-0000-4000-8000-000000000640", agent_available: true }],
});
const entity = (id: string, type: string, name: string) => ({
    actor_issuer: "supervisor:agent", actor_subject: "run", created_at: "2026-11-05T00:00:00Z", current_revision: 1, display_name: name,
    entity_id: id, revision: 1, source_id: "source-1", source_revision: 1, sources_current: true, state: "candidate", type,
});
const claim = (id: string, state: string, rest: Record<string, unknown>) => ({
    actor_issuer: "supervisor:agent", actor_subject: "run", claim_id: id, corroboration: { authoritative: true, independent_groups: 1 },
    created_at: "2026-11-05T00:00:00Z", current_revision: 1, effective_from: "2026-11-05T00:00:00Z", evidence_class: "report",
    revision: 1, source_span_ids: [span], sources_current: true, state, subject_id: coil, subject_revision: 1, ...rest,
});
const suppliedClaim = (state = "candidate") => claim(supplied, state, { predicate: "supplied_by", object_entity_id: mill, object_revision: 1 });
const cycleClaim = () => claim(cycle, "candidate", { subject_id: coil, predicate: "cycle_time_seconds", literal: { type: "decimal", value: "45", unit: "s" } });
const summary = (id: string) => ({ claim_id: id, corroboration: { authoritative: true, independent_groups: 1 }, current_revision: 1, predicate: "supplied_by", review_state: "candidate", revision: 1, sources_current: true, state: "candidate", subject_id: coil });
const problem = (status: number, code: string) => ({ status, contentType: "application/problem+json", body: JSON.stringify({ code, status, title: code, type: "about:blank" }) });
type Reply = { status?: number; json?: unknown; contentType?: string; body?: string };

async function open(page: Page, role: string, list: (r: Request) => Reply, review?: (r: Request) => Reply) {
    const base = `/v1/sites/${site}`;
    let reviewed = false;
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor(role) });
        if (path === `${base}/claims` && route.request().method() === "GET") return route.fulfill(list(route.request()));
        if (path === `${base}/entities/${coil}`) return route.fulfill({ json: entity(coil, "material", "Steel coil") });
        if (path === `${base}/entities/${mill}`) return route.fulfill({ json: entity(mill, "supplier", "Deccan Steel") });
        if (path === `${base}/claims/${supplied}`) return route.fulfill({ json: suppliedClaim(reviewed ? "approved" : "candidate") });
        if (path === `${base}/claims/${cycle}`) return route.fulfill({ json: cycleClaim() });
        if (path === `${base}/source-spans/${span}`) return route.fulfill({ json: { access_epoch: 1, end_char: passage.length, id: span, revision: 1, source_id: "source-1", source_sha256: "x", start_char: 0, text: passage } });
        if (path === `${base}/claims/${supplied}/reviews` && route.request().method() === "POST" && review) {
            const reply = review(route.request());
            reviewed = (reply.status ?? 200) === 200;
            return route.fulfill(reply);
        }
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/review`);
    await expect(page.getByRole("heading", { name: "Facts to review", exact: true })).toBeVisible();
}

const card = (page: Page, name: string) => page.getByRole("list", { name: "Candidate facts" }).getByRole("article", { name });

test("stubbed reviewer sees the site's candidate claims with their passages and approves one from the server receipt", async ({ page }) => {
    const lists: Request[] = [];
    const reviews: Request[] = [];
    await open(page, "supervisor", (r) => {
        lists.push(r);
        return { json: { claims: [summary(supplied), summary(cycle)] } };
    }, (r) => {
        reviews.push(r);
        return { json: { actor_issuer: "i", actor_subject: "reviewer-subject", approval_eligible: true, authenticated_at: "2026-11-05T00:00:00Z", claim_id: supplied, claim_revision: 1, created_at: "2026-11-05T00:00:00Z", reason: "Matches the SOP", resolution_refs: [], review_id: "99999999-0000-4000-8000-000000000641", schema_version: 1, verdict: "approve" } };
    });
    const supply = card(page, "Claim: Steel coil supplied by Deccan Steel");
    await expect(supply).toContainText("Candidate");
    expect(lists.map((r) => new URL(r.url()).searchParams.toString())).toEqual(["state=candidate&limit=50"]);
    await expect(supply.locator("blockquote")).toHaveText(passage);
    await expect(card(page, "Claim: Steel coil cycle time 45 s")).toContainText("Candidate");
    await supply.getByLabel("Review reason").fill("Matches the SOP");
    await supply.getByRole("button", { name: "Approve", exact: true }).click();
    await expect(supply.getByRole("status").filter({ hasText: /\S/ })).toHaveText("Approved. Review 99999999 recorded.");
    await expect(supply).toContainText("Approved");
    expect(reviews).toHaveLength(1);
    expect(JSON.parse(reviews[0].postData() ?? "{}")).toEqual({ claim_revision: 1, verdict: "approve", reason: "Matches the SOP" });
    expect(reviews[0].headers()["if-match"]).toBe('"1"');
});

test("stubbed Load more follows the cursor and appends the next page once", async ({ page }) => {
    const cursors: (string | null)[] = [];
    await open(page, "owner", (r) => {
        const cursor = new URL(r.url()).searchParams.get("cursor");
        cursors.push(cursor);
        return cursor ? { json: { claims: [summary(cycle)] } } : { json: { claims: [summary(supplied)], next_cursor: supplied } };
    });
    await expect(card(page, "Claim: Steel coil supplied by Deccan Steel")).toBeVisible();
    await page.getByRole("button", { name: "Load more candidate facts" }).click();
    await expect(card(page, "Claim: Steel coil cycle time 45 s")).toBeVisible();
    await expect(page.getByRole("list", { name: "Candidate facts" }).getByRole("listitem")).toHaveCount(2);
    await expect(page.getByRole("button", { name: "Load more candidate facts" })).toHaveCount(0);
    expect(cursors).toEqual([null, supplied]);
});

test("stubbed empty queue says what will appear; a failed load is never shown as empty", async ({ page }) => {
    await open(page, "supervisor", () => ({ json: { claims: [] } }));
    await expect(page.getByText("No candidate facts to review.", { exact: false })).toBeVisible();
    const other = await page.context().newPage();
    await open(other, "supervisor", () => problem(503, "SERVICE_UNAVAILABLE"));
    await expect(other.getByText("The candidate facts could not be loaded.", { exact: false })).toBeVisible();
    await expect(other.getByText("No candidate facts to review.", { exact: false })).toHaveCount(0);
});

for (const role of ["viewer", "operator"]) {
    test(`stubbed ${role} reads the candidates but gets no review controls`, async ({ page }) => {
        await open(page, role, () => ({ json: { claims: [summary(supplied)] } }));
        await expect(page.getByText("Reviewing needs the owner or supervisor role at this site", { exact: false })).toBeVisible();
        const supply = card(page, "Claim: Steel coil supplied by Deccan Steel");
        await expect(supply.locator("blockquote")).toHaveText(passage);
        await expect(supply.getByRole("button", { name: "Approve", exact: true })).toHaveCount(0);
        await expect(supply.getByLabel("Review reason")).toHaveCount(0);
    });
}
