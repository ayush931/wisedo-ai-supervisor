// WIS-703 UI regressions use labelled API stubs; feedback.spec.ts is the real browser gate.
import { expect, test, type Page } from "@playwright/test";
const site = "00000000-0000-4000-8000-000000000703";
const base = `/v1/sites/${site}`;
const id = "10000000-0000-4000-8000-000000000703";
const claimID = "20000000-0000-4000-8000-000000000703";
const sourceID = "30000000-0000-4000-8000-000000000703";
const spanID = "40000000-0000-4000-8000-000000000703";
const runID = "50000000-0000-4000-8000-000000000703";
const entityID = "60000000-0000-4000-8000-000000000703";
const subject = { kind: "answer", conversation_id: id, message_id: runID };
const claim = { claim_id: claimID, predicate: "cycle_time_seconds", literal: { type: "decimal", value: "45", unit: "s" }, source_span_ids: [spanID], subject_id: entityID, subject_revision: 1, evidence_class: "report", effective_from: "2026-11-01T00:00:00Z" };
const candidate = (proposer = "operator") => ({ candidate_id: id, site_id: site, kind: "answer", proposer_issuer: "issuer", proposer_subject: proposer, revision: 1, content_sha256: "a".repeat(64), created_at: "2026-11-11T00:00:00Z", source_feedback_ids: [id], content: { reason: "The standard changed", question: "What is the cycle time?", expected_answer: "The press cycle time is 45 seconds.", expected_base_revision: 1, claim }, original: { kind: "answer", available: true, question: "What is the cycle time?", answer: "The press cycle time is 60 seconds.", provenance: { run_id: runID } }, missing_requirements: [], review_state: "candidate" });
const problem = (status: number, code: string) => ({ status, contentType: "application/problem+json", json: { status, code, title: code, type: "about:blank" } });

async function stub(page: Page, role: string, who: string, handle: (path: string, method: string, body: any, headers: Record<string, string>, query: URLSearchParams) => any) {
    await page.route("**/v1/**", async (route) => {
        const req = route.request(); const path = new URL(req.url()).pathname;
        const out = handle(path, req.method(), req.method() === "POST" ? req.postDataJSON() : undefined, req.headers(), new URL(req.url()).searchParams);
        if (out) return route.fulfill(out);
        if (path === "/v1/me") return route.fulfill({ json: { csrf_token: "stub", subject: who, display_name: "Stub member", sites: [{ id: site, tenant_id: id, name: "Stub site", role, agent_available: true }] } });
        if (path === `${base}/correction-summary`) return route.fulfill({ json: { approved_count: 1, month_start: "2026-11-01T00:00:00Z", month_end: "2026-12-01T00:00:00Z", timezone: "UTC" } });
        if (path === `${base}/source-spans/${spanID}`) return route.fulfill({ json: { id: spanID, text: "The press cycle time is 45 seconds.", source_id: sourceID, revision: 1 } });
        if (path === `${base}/claims`) return route.fulfill({ json: { claims: [] } });
        if (path === `${base}/memory-candidates`) return route.fulfill({ json: { candidates: [] } });
        if (path.endsWith("/shift-plans")) return route.fulfill({ json: { items: [] } });
        if (path.endsWith("/proposals")) return route.fulfill({ json: { items: [] } });
        if (path.endsWith("/cases")) return route.fulfill({ json: { items: [] } });
        if (path.endsWith("/workstations")) return route.fulfill({ json: { items: [] } });
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
}

test("WIS703 operator corrects own answered turn; failed submission preserves details and keys", async ({ page }) => {
    const writes: { body: any; key: string }[] = [];
    await stub(page, "operator", "operator", (path, method, body, headers) => {
        if (path === `${base}/chat-conversations` && method === "POST") return { json: { conversation_id: id, revision: 1 } };
        if (path === `${base}/chat-conversations/${id}/messages` && method === "POST") return { json: { conversation: { conversation_id: id, revision: 2 }, reply: { message_id: runID, state: "answered", body: "The press cycle time is 60 seconds.", citations: [], corrections: [{ claim_id: claimID, revision: 1, reviewer: "Morgan", reviewed_at: "2026-11-11T00:00:00Z" }] } } };
        if (path === `${base}/feedback` && method === "POST") { writes.push({ body, key: headers["idempotency-key"] }); return writes.length === 1 ? problem(503, "STORAGE_UNAVAILABLE") : { status: 201, json: { candidate_id: id } }; }
        if (path === `${base}/memory-candidates/${id}`) return { json: { ...candidate(), content: { ...candidate().content, claim: undefined, reason: "The cycle time changed" }, missing_requirements: ["proposed_claim"] } };
    });
    await page.goto(`/sites/${site}/today`);
    await page.getByLabel("Your question").fill("What is the cycle time?"); await page.getByRole("button", { name: "Ask", exact: true }).click();
    await expect(page.getByText("updated after a correction by Morgan on")).toBeVisible();
    await page.getByRole("button", { name: "This is wrong" }).click();
    await expect(page.getByText("This shares only the selected question and answer", { exact: false })).toBeVisible();
    await page.getByRole("textbox", { name: "Correction reason", exact: true }).fill("The cycle time changed");
    await page.getByRole("button", { name: "Submit correction" }).click();
    await expect(page.getByRole("textbox", { name: "Correction reason", exact: true })).toHaveValue("The cycle time changed");
    await page.getByRole("button", { name: "Submit correction" }).click();
    await expect(page.getByRole("article", { name: "Correction 10000000" })).toContainText("Still needed: proposed claim");
    expect(writes).toHaveLength(2); expect(writes[0].key).toBe(writes[1].key);
    expect(writes[0].body.subject).toEqual(subject); expect(writes[0].body.proposed_correction.claim).toBeUndefined();
    await expect(page.getByRole("button", { name: "Approve correction" })).toHaveCount(0);
});

test("WIS703 review distinguishes original and corrected values and never publishes automatically", async ({ page }) => {
    let saved: any = candidate(); const writes: { path: string; body: any; headers: Record<string, string> }[] = [];
    await stub(page, "supervisor", "reviewer", (path, method, body, headers) => {
        if (path === `${base}/memory-candidates`) return { json: { candidates: [saved] } };
        if (method === "POST") { writes.push({ path, body, headers }); saved = { ...saved, review_state: "approved", review: { verdict: "approve", review_id: id, created_at: "2026-11-11T00:00:00Z", reason: body.reason } }; return { json: saved }; }
        if (path === `${base}/memory-candidates/${id}`) return { json: saved };
    });
    await page.goto(`/sites/${site}/review`);
    const card = page.getByRole("article", { name: "Correction 10000000" });
    await expect(card).toContainText("The press cycle time is 60 seconds."); await expect(card).toContainText("45 s");
    await expect(card.locator("blockquote").last()).toHaveText("The press cycle time is 45 seconds.");
    await card.getByLabel("Correction review reason").fill("Checked the current standard"); await card.getByRole("button", { name: "Approve correction" }).click();
    await expect(card).toContainText("Approved — awaiting snapshot");
    await expect(card.getByRole("link", { name: "Publish approved knowledge" })).toHaveAttribute("href", `/sites/${site}/process`);
    expect(writes).toHaveLength(1); expect(writes[0].path).toBe(`${base}/memory-candidates/${id}/review`); expect(writes[0].headers["if-match"]).toBe('"1"'); expect(writes[0].body.expected_revision).toBe(1);
});

test("WIS703 self-review is visibly disabled; stale review keeps the reason", async ({ page }) => {
    let proposer = "reviewer";
    await stub(page, "supervisor", "reviewer", (path, method) => {
        if (path === `${base}/memory-candidates`) return { json: { candidates: [candidate(proposer)] } };
        if (path === `${base}/memory-candidates/${id}/review` && method === "POST") return problem(412, "REVISION_CONFLICT");
        if (path === `${base}/memory-candidates/${id}`) return { json: { ...candidate(proposer), revision: 2 } };
    });
    await page.goto(`/sites/${site}/review`);
    await page.getByLabel("Correction review reason").fill("My own correction"); await expect(page.getByRole("button", { name: "Approve correction" })).toBeDisabled();
    proposer = "operator"; await page.reload();
    await page.getByLabel("Correction review reason").fill("Verified passage"); await page.getByRole("button", { name: "Approve correction" }).click();
    await expect(page.getByRole("article", { name: "Correction 10000000" })).toContainText("revision 2");
    await expect(page.getByLabel("Correction review reason")).toHaveValue("Verified passage");
});

test("WIS703 typed correction keeps exact decimals, selected evidence and intent after an unknown outcome", async ({ page }) => {
    const feedback: { body: any; key: string }[] = []; let spans = 0;
    await stub(page, "supervisor", "operator", (path, method, body, headers, query) => {
        if (path === `${base}/memory-candidates`) return { json: { candidates: [{ ...candidate(), content: { ...candidate().content, claim: undefined, expected_base_revision: 0 }, missing_requirements: ["proposed_claim"] }] } };
        if (path === `${base}/claims`) return { json: { claims: query.get("state") === "candidate" ? [] : [{ claim_id: claimID, revision: 1, predicate: "cycle_time_seconds" }] } };
        if (path === `${base}/claims/${claimID}`) return { json: { ...claim, revision: 1 } };
        if (path === `${base}/entities`) return { json: { entities: [{ entity_id: entityID, display_name: "Press", revision: 1 }] } };
        if (path === `${base}/sources`) return { json: { sources: [{ source_id: sourceID, revision: 1, title: "Corrected standard" }] } };
        if (path === `${base}/sources/${sourceID}`) return { json: { source_id: sourceID, revision: 1, rune_length: 34, text: "The press cycle time is 45 seconds." } };
        if (path === `${base}/sources/${sourceID}/spans`) { spans++; expect(body).toEqual({ revision: 1, start_char: 0, end_char: 34 }); return { json: { id: spanID } }; }
        if (path === `${base}/memory-candidates/${id}/revisions`) { feedback.push({ body, key: headers["idempotency-key"] }); return feedback.length === 1 ? problem(503, "STORAGE_UNAVAILABLE") : { json: { ...candidate(), revision: 2 } }; }
    });
    await page.goto(`/sites/${site}/review`); await page.getByRole("button", { name: "Amend correction" }).click();
    await page.getByLabel("Add a proposed fact and source").check(); await page.getByRole("combobox", { name: "Fact to correct", exact: true }).selectOption("existing");
    await page.getByRole("combobox", { name: "Approved fact", exact: true }).selectOption(claimID);
    await page.getByRole("textbox", { name: "Correct value (exact decimal)" }).fill("45.000");
    await page.getByRole("combobox", { name: "Evidence source", exact: true }).selectOption(sourceID);
    await page.getByRole("button", { name: "Save correction revision" }).click();
    await expect(page.getByRole("textbox", { name: "Correct value (exact decimal)" })).toHaveValue("45.000");
    await page.getByRole("button", { name: "Save correction revision" }).click();
    await expect(page.getByRole("article", { name: "Correction 10000000" })).toContainText("revision 2");
    expect(spans).toBe(1); expect(feedback).toHaveLength(2); expect(feedback[0].key).toBe(feedback[1].key);
    expect(feedback[0].body.claim.literal).toEqual({ type: "decimal", value: "45.000", unit: "s" }); expect(feedback[0].body.claim.source_span_ids).toEqual([spanID]);
});

test("WIS703 terminal investigation with no proposal can be corrected by its exact historical run", async ({ page }) => {
    let submitted: any;
    await stub(page, "supervisor", "reviewer", (path, method, body) => {
        if (path === `${base}/cases/${id}`) return { json: { id, revision: 9, state: "needs_input", line_id: entityID, order_id: entityID, window_start: "2026-11-11T00:00:00Z", window_end: "2026-11-11T00:15:00Z", transitions: [], clarifications: [], triggers: [], trigger_evidence_ids: [], detected_at: "2026-11-11T00:00:00Z", latest_window_end: "2026-11-11T00:15:00Z", resolution_reason: null, episode: 1 } };
        if (path === `${base}/cases/${id}/suggestion`) return { json: { case_id: id, case_revision: 9, readiness: "NOT_READY", reasons: ["NO_APPROVED_PLAN"], facts: [], passages: [], observations: [], hypotheses: [], unknowns: [], investigation: { run_id: runID, case_revision: 2, state: "completed", status: "needs_input", reasons: ["REQUIRED_INPUT_MISSING"], provider_actual: false } } };
        if (path === `${base}/feedback` && method === "POST") { submitted = body; return { json: { candidate_id: id } }; }
        if (path === `${base}/memory-candidates/${id}`) return { json: { ...candidate(), kind: "investigation", missing_requirements: ["proposed_claim"] } };
    });
    await page.goto(`/sites/${site}/cases/${id}`);
    await page.getByRole("button", { name: "This is wrong" }).click(); await page.getByRole("textbox", { name: "Correction reason", exact: true }).fill("The missing standard is available");
    await page.getByRole("button", { name: "Submit correction" }).click();
    await expect(page.getByRole("article", { name: "Correction 10000000" })).toBeVisible(); expect(submitted.subject).toEqual({ kind: "investigation", run_id: runID });
});
