import { expect, test, type Page, type Request } from "@playwright/test";
import { appRegions } from "./regions";

// WIS-616: the facts an onboarding agent turn proposed (WIS-612) are listed under the finished
// follow-up run on the interview page, with the passages they cite, and a reviewer approves or
// rejects each candidate claim there. Every test stubs the API (UI behaviour, not agent evidence).

const site = "00000000-0000-4000-8000-000000000911";
const sessionID = "11111111-0000-4000-8000-000000000912";
const runID = "33333333-0000-4000-8000-000000000913";
const coil = "44444444-0000-4000-8000-000000000914";
const mill = "55555555-0000-4000-8000-000000000915";
const supplied = "66666666-0000-4000-8000-000000000916";
const lead = "77777777-0000-4000-8000-000000000917";
const span = "88888888-0000-4000-8000-000000000918";
const reviewID = "99999999-0000-4000-8000-000000000919";
const passage = "Steel coil comes from Bharat Steel, about 14 days lead time.";

const me = {
    csrf_token: "test-csrf",
    display_name: "Stubbed Owner",
    subject: "owner-subject",
    sites: [{ id: site, name: "Stubbed Site", role: "owner", tenant_id: "00000000-0000-4000-8000-000000000910", agent_available: true }],
};
const session = { created_at: "2026-11-01T00:00:00Z", participant_actor_id: "owner-subject", revision: 1, role: "owner", session_id: sessionID, status: "active", updated_at: "2026-11-01T00:00:00Z" };
const run = { run_id: runID, state: "completed", outcome_status: "proposed", outcome_reasons: [], question_ids: [], claim_ids: [supplied, lead], entity_ids: [coil, mill] };
const entity = (id: string, type: string, name: string) => ({
    actor_issuer: "supervisor:agent", actor_subject: runID, created_at: "2026-11-01T00:00:00Z", current_revision: 1, display_name: name,
    entity_id: id, revision: 1, source_id: "source-1", source_revision: 1, sources_current: true, state: "candidate", type,
});
const claim = (id: string, revision: number, state: string, rest: Record<string, unknown>) => ({
    actor_issuer: "supervisor:agent", actor_subject: runID, claim_id: id, corroboration: { authoritative: false, independent_groups: 1 },
    created_at: "2026-11-01T00:00:00Z", current_revision: revision, effective_from: "2026-11-01T00:00:00Z", evidence_class: "report",
    revision, source_span_ids: [span], sources_current: true, state, subject_id: coil, subject_revision: 1, ...rest,
});
const suppliedClaim = (revision = 1, state = "candidate") => claim(supplied, revision, state, { predicate: "supplied_by", object_entity_id: mill, object_revision: 1 });
const leadClaim = (revision = 1, state = "candidate") => claim(lead, revision, state, { predicate: "lead_time_days", literal: { type: "decimal", value: "14", unit: "day" } });
const review = (verdict: string, revision = 1) => ({
    actor_issuer: "https://idp.test", actor_subject: "owner-subject", approval_eligible: verdict === "approve", authenticated_at: "2026-11-01T00:00:00Z",
    claim_id: supplied, claim_revision: revision, created_at: "2026-11-01T00:00:00Z", reason: "Matches the SOP", resolution_refs: [], review_id: reviewID, schema_version: 1, verdict,
});
// The server answers errors as RFC 9457 problem documents; the client parses only that media type.
const problem = (status: number, code: string) => ({ status, contentType: "application/problem+json", body: JSON.stringify({ code, status, title: code, type: "about:blank" }) });

type Reply = { status?: number; json?: unknown; contentType?: string; body?: string };
type Overrides = {
    entities?: Record<string, () => Reply>;
    claims?: Record<string, () => Reply>;
    spans?: () => Reply;
    review?: (request: Request) => Promise<Reply> | Reply;
};

// open stubs the whole API for an owner whose remembered follow-up run finished with two
// proposed entities and two claims, then opens the interview.
async function open(page: Page, overrides: Overrides = {}) {
    const base = `/v1/sites/${site}`;
    await page.route("**/v1/**", async (route) => {
        const url = new URL(route.request().url());
        const path = url.pathname;
        if (path === "/v1/me") return route.fulfill({ json: me });
        if (path === `${base}/interviews/${sessionID}`) return route.fulfill({ json: session });
        if (path === `${base}/interviews/${sessionID}/questions`) return route.fulfill({ json: { questions: [] } });
        if (path === `${base}/sources`) return route.fulfill({ json: { sources: [] } });
        if (path === `${base}/interviews/${sessionID}/agent-turns/${runID}`) return route.fulfill({ json: run });
        if (path === `${base}/entities/${coil}`) return route.fulfill((overrides.entities?.[coil] ?? (() => ({ json: entity(coil, "material", "Steel coil") })))());
        if (path === `${base}/entities/${mill}`) return route.fulfill((overrides.entities?.[mill] ?? (() => ({ json: entity(mill, "supplier", "Bharat Steel") })))());
        for (const [id, fallback] of [[supplied, () => ({ json: suppliedClaim() })], [lead, () => ({ json: leadClaim() })]] as const) {
            if (path === `${base}/claims/${id}`) return route.fulfill((overrides.claims?.[id] ?? fallback)());
        }
        if (path === `${base}/source-spans/${span}`) {
            return route.fulfill((overrides.spans ?? (() => ({ json: { access_epoch: 1, end_char: passage.length, id: span, revision: 1, source_id: "source-1", source_sha256: "x", start_char: 0, text: passage } })))());
        }
        if (path === `${base}/claims/${supplied}/reviews` && route.request().method() === "POST" && overrides.review) {
            return route.fulfill(await overrides.review(route.request()));
        }
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto("/");
    await page.evaluate(([key, value]) => sessionStorage.setItem(key, value), [`supervisor.interview.turn.${sessionID}`, runID]);
    await page.goto(`/sites/${site}/interviews/${sessionID}`);
}

const facts = (page: Page) => page.getByRole("region", { name: "Proposed by the agent" });
const suppliedCard = (page: Page) => facts(page).getByRole("article", { name: "Claim: Steel coil supplied by Bharat Steel" });

test("stubbed finished turn lists its proposed entities and claims with the passages they cite (UI behaviour)", async ({ page }) => {
    await open(page);
    const region = facts(page);
    await expect(region.getByRole("heading", { name: "Entities 2" })).toBeVisible();
    await expect(region.getByRole("listitem").filter({ hasText: "Material" }).filter({ hasText: "Steel coil" })).toBeVisible();
    await expect(region.getByRole("listitem").filter({ hasText: "Supplier" }).filter({ hasText: "Bharat Steel" })).toBeVisible();
    await expect(region.getByRole("heading", { name: "Claims 2" })).toBeVisible();
    const card = suppliedCard(page);
    await expect(card).toContainText("Candidate");
    await expect(card.locator("blockquote")).toHaveText(passage);
    const leadCard = region.getByRole("article", { name: "Claim: Steel coil lead time 14 day" });
    await expect(leadCard).toContainText("Candidate");
    await expect(leadCard.locator("blockquote")).toHaveText(passage);
});

// WIS-625 review (#252): a claim citing OCR text says so where it is reviewed, so the reviewer checks
// the passage against the original page before approving.
test("stubbed claim citing model-extracted text is marked where it is reviewed (UI behaviour)", async ({ page }) => {
    await open(page, { claims: { [supplied]: () => ({ json: { ...suppliedClaim(), corroboration: { authoritative: false, independent_groups: 0, model_extracted_sources: 1 } } }) } });
    await expect(suppliedCard(page)).toContainText("A cited source is model-extracted text (OCR of a scanned page)");
    await expect(facts(page).getByRole("article", { name: "Claim: Steel coil lead time 14 day" })).not.toContainText("model-extracted");
});

test("stubbed approve: a reason is required, the request is exact, and approval shows only from the server receipt (UI behaviour)", async ({ page }) => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => (release = resolve));
    let sent: Request | null = null;
    let approved = false;
    await open(page, {
        claims: { [supplied]: () => ({ json: approved ? suppliedClaim(1, "approved") : suppliedClaim() }) },
        review: async (request) => {
            sent = request;
            await gate;
            approved = true;
            return { json: review("approve") };
        },
    });
    const card = suppliedCard(page);
    await expect(card.getByRole("button", { name: "Approve" })).toBeDisabled();
    await expect(card.getByRole("button", { name: "Reject" })).toBeDisabled();
    await card.getByLabel("Review reason").fill("Matches the SOP");
    await card.getByRole("button", { name: "Approve" }).click();
    await expect.poll(() => sent).not.toBeNull();
    await expect(card.getByText(/^Approved/)).toHaveCount(0);
    await expect(card.getByRole("button", { name: "Approve" })).toBeDisabled();
    const request = sent as unknown as Request;
    expect(request.headers()["if-match"]).toBe('"1"');
    expect(request.headers()["x-csrf-token"]).toBe("test-csrf");
    expect(request.headers()["idempotency-key"]).toBeTruthy();
    expect(request.postDataJSON()).toEqual({ claim_revision: 1, verdict: "approve", reason: "Matches the SOP" });
    release();
    await expect(card.getByRole("status").filter({ hasText: /\S/ })).toHaveText(`Approved. Review ${reviewID.slice(0, 8)} recorded.`);
    await expect(card).toContainText("Approved");
    await expect(card.getByRole("button", { name: "Approve" })).toHaveCount(0);
});

test("stubbed REVIEW_BLOCKED says what approval needs, and rejecting still works (UI behaviour)", async ({ page }) => {
    let rejected = false;
    await open(page, {
        claims: { [supplied]: () => ({ json: rejected ? suppliedClaim(1, "rejected") : suppliedClaim() }) },
        review: (request) => {
            if (request.postDataJSON().verdict === "approve") return problem(409, "REVIEW_BLOCKED");
            rejected = true;
            return { json: review("reject") };
        },
    });
    const card = suppliedCard(page);
    await card.getByLabel("Review reason").fill("Matches the SOP");
    await card.getByRole("button", { name: "Approve" }).click();
    await expect(card.getByRole("alert").filter({ hasText: /\S/ })).toContainText("Approval needs a cited source the owner has marked authoritative, and no open conflict on this claim. You can still reject it.");
    await card.getByRole("button", { name: "Reject" }).click();
    await expect(card.getByRole("status").filter({ hasText: /\S/ })).toHaveText(`Rejected. Review ${reviewID.slice(0, 8)} recorded.`);
    await expect(card).toContainText("Rejected");
});

test("stubbed 412 on a review reloads the claim, and the next attempt binds its current revision (UI behaviour)", async ({ page }) => {
    let reads = 0;
    const bodies: unknown[] = [];
    const matches: (string | undefined)[] = [];
    await open(page, {
        claims: { [supplied]: () => ({ json: ++reads === 1 ? suppliedClaim(1) : suppliedClaim(2) }) },
        review: (request) => {
            bodies.push(request.postDataJSON());
            matches.push(request.headers()["if-match"]);
            return bodies.length === 1 ? problem(412, "REVISION_CONFLICT") : { json: review("approve", 2) };
        },
    });
    const card = suppliedCard(page);
    await card.getByLabel("Review reason").fill("Matches the SOP");
    await card.getByRole("button", { name: "Approve" }).click();
    await expect(card.getByRole("alert").filter({ hasText: /\S/ })).toContainText("This changed on the server.");
    await expect.poll(() => reads).toBeGreaterThanOrEqual(2);
    await card.getByRole("button", { name: "Approve" }).click();
    await expect(card.getByRole("status").filter({ hasText: /\S/ })).toHaveText(`Approved. Review ${reviewID.slice(0, 8)} recorded.`);
    expect(matches).toEqual(['"1"', '"2"']);
    expect(bodies[1]).toEqual({ claim_revision: 2, verdict: "approve", reason: "Matches the SOP" });
});

test("stubbed 428 on a review offers a fresh sign-in and records nothing (UI behaviour)", async ({ page }) => {
    await open(page, { review: () => problem(428, "REAUTHENTICATION_REQUIRED") });
    const card = suppliedCard(page);
    await card.getByLabel("Review reason").fill("Matches the SOP");
    await card.getByRole("button", { name: "Reject" }).click();
    await expect(card.getByRole("alert").filter({ hasText: /\S/ })).toContainText("Confirm your identity to continue. Nothing was submitted.");
    await expect(card.getByRole("button", { name: "Confirm identity" })).toBeVisible();
    await expect(card).toContainText("Candidate");
    await expect(appRegions(card, "status")).toHaveCount(0);
});

test("stubbed unreadable claim and passage are shown as unavailable, never as an empty list (UI behaviour)", async ({ page }) => {
    await open(page, { claims: { [lead]: () => problem(503, "STORAGE_UNAVAILABLE") }, spans: () => problem(404, "NOT_FOUND") });
    const region = facts(page);
    await expect(region.getByRole("heading", { name: "Claims 2" })).toBeVisible();
    await expect(suppliedCard(page)).toContainText("Cited passage unavailable");
    const missing = region.getByRole("article", { name: `Claim ${lead.slice(0, 8)}` });
    await expect(missing).toContainText("could not be loaded");
    await expect(missing.getByRole("alert").filter({ hasText: /\S/ })).toContainText("The service is busy or unavailable.");
});

// Audit gaps (WIS-616): a load failure for one item is shown for that item with its reason; a
// review receipt survives a failed reload; a claim already decided shows its state and no controls.
test("stubbed claim whose subject entity fails to load says so on that claim (UI behaviour)", async ({ page }) => {
    await open(page, { entities: { [coil]: () => problem(503, "STORAGE_UNAVAILABLE") } });
    const card = facts(page).getByRole("article", { name: `Claim: entity ${coil.slice(0, 8)} supplied by Bharat Steel` });
    await expect(card).toContainText("The subject of this claim could not be loaded.");
    await expect(card.getByRole("alert").filter({ hasText: /\S/ })).toContainText("The service is busy or unavailable.");
    await expect(card.locator("blockquote")).toHaveText(passage);
});

test("stubbed cited passage that fails to load shows why, on that passage (UI behaviour)", async ({ page }) => {
    await open(page, { spans: () => problem(503, "STORAGE_UNAVAILABLE") });
    const card = suppliedCard(page);
    await expect(card).toContainText(`Cited passage unavailable (${span.slice(0, 8)}).`);
    await expect(card.getByRole("group", { name: `Cited passage unavailable (${span.slice(0, 8)}).` }).getByRole("alert")).toContainText("The service is busy or unavailable.");
});

test("stubbed review receipt stays on screen when the reload after it fails (UI behaviour)", async ({ page }) => {
    let reviewed = false;
    await open(page, {
        claims: { [supplied]: () => (reviewed ? problem(503, "STORAGE_UNAVAILABLE") : { json: suppliedClaim() }) },
        review: () => {
            reviewed = true;
            return { json: review("approve") };
        },
    });
    const card = suppliedCard(page);
    await card.getByLabel("Review reason").fill("Matches the SOP");
    await card.getByRole("button", { name: "Approve" }).click();
    await expect(card.getByRole("status").filter({ hasText: /\S/ })).toHaveText(`Approved. Review ${reviewID.slice(0, 8)} recorded.`);
    await expect(card).toContainText("The claim could not be refreshed; what is shown may be out of date.");
    await expect(card.getByRole("alert").filter({ hasText: /\S/ })).toContainText("The service is busy or unavailable.");
    await expect(card.getByRole("status").filter({ hasText: /\S/ })).toHaveText(`Approved. Review ${reviewID.slice(0, 8)} recorded.`);
});

test("stubbed claim already approved shows its state and no review controls (UI behaviour)", async ({ page }) => {
    await open(page, { claims: { [supplied]: () => ({ json: suppliedClaim(1, "approved") }) } });
    const card = suppliedCard(page);
    await expect(card).toContainText("Approved");
    await expect(card.getByRole("button", { name: "Approve" })).toHaveCount(0);
    await expect(card.getByLabel("Review reason")).toHaveCount(0);
});
