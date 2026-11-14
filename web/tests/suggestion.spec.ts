// Labelled stubbed API responses (WIS-629 advisor suggestion on the case page): these exercise the
// real page against fixed server replies. They are not evidence of backend behaviour: assembly,
// citation checks, revocation and readiness are covered against real PostgreSQL by
// internal/agent/advisor_integration_test.go (CASE=WIS-273) and internal/advisor unit tests.
import { test, expect, type Page, type Route } from "@playwright/test";
import { appRegions } from "./regions";
import type { CaseSuggestionV1 } from "../../contracts/typescript/oee";

const site = "00000000-0000-4000-8000-000000000711";
const tenant = "00000000-0000-4000-8000-000000000712";
const caseId = "00000000-0000-4000-8000-000000000721";
const line = "00000000-0000-4000-8000-000000000731";
const order = "00000000-0000-4000-8000-000000000732";
const source = "00000000-0000-4000-8000-000000000741";
const NOW = new Date("2026-11-03T10:00:00Z");
const at = (minutes: number) => new Date(NOW.getTime() + minutes * 60000).toISOString();
const problem = (route: Route, status: number, code: string) =>
    route.fulfill({ status, contentType: "application/problem+json", json: { type: "about:blank", title: code, status, code, correlation_id: "stub" } });

const kase = {
    id: caseId, state: "needs_input", episode: 1, revision: 4, line_id: line, order_id: order, exception_kind: "production_shortfall",
    detected_at: at(-30), latest_window_end: at(-15), trigger_evidence_ids: [], triggers: [], resolution_reason: null, previous_case_id: null,
    context_snapshot_id: "00000000-0000-4000-8000-000000000751", context_revision: 1, context_digest: "a".repeat(64), workflow_id: "wf",
    transitions: [{ revision: 1, from_state: "none", to_state: "detected", event: "trigger", reason: null, actor: { issuer: "supervisor:evaluator", subject: "shortfall-evaluator" }, created_at: at(-30) }],
    clarifications: [],
};
const obs = (id: string, metric: string, value: string, start: number) => ({ measurement_id: id, context_id: "00000000-0000-4000-8000-000000000751", metric_name: metric, value_decimal: value, unit: "count", window_start: at(start), window_end: at(start + 15) });

function suggestion(edit: Partial<CaseSuggestionV1> = {}): CaseSuggestionV1 {
    return {
        schema_version: 2, rule_version: "wis629-advisor-1", case_id: caseId, case_revision: 4, readiness: "READY", reasons: [],
        snapshot: { snapshot_id: "00000000-0000-4000-8000-000000000761", inventory_revision: 3, created_at: "2026-11-02T08:00:00Z", stale: false },
        plan: { plan_revision_id: "00000000-0000-4000-8000-000000000771", plan_revision: 1, approval_id: "00000000-0000-4000-8000-000000000772" },
        investigation: { run_id: "00000000-0000-4000-8000-000000000781", case_revision: 4, state: "completed", status: "needs_input", reasons: [], model: "fixture-model", provider_actual: false },
        facts: [{ claim_id: "00000000-0000-4000-8000-000000000791", subject_id: line, subject_name: "Press line 1", predicate: "cycle_time_seconds", object_entity_id: null, object_name: null, value: "60", unit: "s",
            effective_from: "2026-01-01T00:00:00Z", effective_to: "2027-01-01T00:00:00Z",
            span_ids: ["00000000-0000-4000-8000-000000000792"] }],
        passages: [{ span_id: "00000000-0000-4000-8000-000000000792", source_id: source, revision: 1, text: "Press line 1 runs at a 60 s cycle." }],
        observations: [obs("00000000-0000-4000-8000-000000000801", "expected_good_units", "75", -30), obs("00000000-0000-4000-8000-000000000802", "deficit_good_units", "15", -30)],
        hypotheses: [{ text: "A stoppage on the press may explain the deficit.", evidence_ids: ["00000000-0000-4000-8000-000000000802"], contradicting_evidence_ids: [] }],
        unknowns: [{ field: "stop_reason", question: "What stopped the press during the window?", required: true }],
        ...edit,
    };
}

async function stub(page: Page, reply: (route: Route) => Promise<void>) {
    await page.clock.setFixedTime(NOW);
    await page.route("**/v1/**", async (route) => {
        const url = new URL(route.request().url());
        const path = url.pathname.replace(`/v1/sites/${site}`, "");
        if (url.pathname === "/v1/me") return route.fulfill({ json: { csrf_token: "stub", subject: "lead", display_name: "Rahul Deshmukh", sites: [{ id: site, name: "Chakan Plant", role: "supervisor", tenant_id: tenant, agent_available: true }] } });
        if (path === "/members") return route.fulfill({ json: { members: [] } });
        if (path === `/entities/${line}`) return route.fulfill({ json: { id: line, display_name: "Press line 1" } });
        if (path === `/entities/${order}`) return route.fulfill({ json: { id: order, display_name: "Order 7" } });
        if (path === `/cases/${caseId}`) return route.fulfill({ json: kase });
        if (path === `/cases/${caseId}/suggestion`) return reply(route);
        return problem(route, 404, "NOT_FOUND");
    });
    await page.goto(`/sites/${site}/cases/${caseId}`);
}
const panel = (page: Page) => page.getByRole("region", { name: "Advisor suggestion" });

test("stubbed suggestion labels each statement and cites its passage, measurement or run; it offers no action", async ({ page }) => {
    await stub(page, (route) => route.fulfill({ json: suggestion() }));
    const p = panel(page);
    await expect(p).toContainText("Ready");
    await expect(p).toContainText("Read-only. It approves and dispatches nothing");
    await expect(p).toContainText("Approved process snapshot of");
    const facts = p.getByRole("list", { name: "Approved facts" }).getByRole("listitem");
    await expect(facts).toHaveCount(1);
    await expect(facts.first()).toContainText("Approved fact");
    await expect(facts.first()).toContainText("Press line 1 cycle time 60 s");
    await expect(facts.first()).toContainText("in effect from 2026-01-01T00:00:00Z until 2027-01-01T00:00:00Z");
    await expect(facts.first().locator("blockquote")).toContainText("Press line 1 runs at a 60 s cycle.");
    await expect(facts.first().locator("blockquote")).toContainText(`source ${source.slice(0, 8)} r1`);
    const rows = p.getByRole("table", { name: "Observations" }).getByRole("row");
    await expect(rows).toHaveCount(3);
    await expect(rows.nth(1)).toContainText("Observation");
    await expect(rows.nth(2)).toContainText("Deficit");
    await expect(rows.nth(2)).toContainText("15 count");
    const hyp = p.getByRole("list", { name: "Hypotheses" }).getByRole("listitem");
    await expect(hyp.first()).toContainText("Hypothesis (model, unverified)");
    await expect(hyp.first()).toContainText("cites 1 record");
    await expect(p.getByRole("list", { name: "Open questions" })).toContainText("What stopped the press during the window? (required)");
    await expect(p).toContainText("fixture or scripted model, not an actual model");
    // Rule 05: the suggestion carries no control that approves or dispatches anything. Its only
    // button is WIS-703's "This is wrong" correction control for the investigation result
    // (product-owner decision 2026-11-11: corrections cover investigation results too).
    await expect(p.getByRole("button")).toHaveCount(1);
    await expect(p.getByRole("button", { name: "This is wrong", exact: true })).toHaveCount(1);
});

// WIS-629: the suggestion lists each cited passage once; every fact citing it still shows it.
test("stubbed facts citing one shared passage each show it", async ({ page }) => {
    const s = suggestion();
    await stub(page, (route) => route.fulfill({ json: { ...s, facts: [s.facts[0], { ...s.facts[0], claim_id: "00000000-0000-4000-8000-000000000793", predicate: "setup_time_seconds", value: "300" }] } }));
    const facts = panel(page).getByRole("list", { name: "Approved facts" }).getByRole("listitem");
    await expect(facts).toHaveCount(2);
    for (const f of await facts.all()) await expect(f.locator("blockquote")).toContainText("Press line 1 runs at a 60 s cycle.");
});

test("stubbed limited suggestion shows why: a stale snapshot and model output left out", async ({ page }) => {
    await stub(page, (route) => route.fulfill({ json: suggestion({ readiness: "LIMITED", reasons: ["HYPOTHESIS_CITATION_INVALID", "SNAPSHOT_STALE"], hypotheses: [], unknowns: [], snapshot: { snapshot_id: "00000000-0000-4000-8000-000000000761", inventory_revision: 3, created_at: "2026-11-02T08:00:00Z", stale: true } }) }));
    const p = panel(page);
    await expect(p).toContainText("Limited");
    await expect(p).toContainText("Model output cited outside its evidence; left out");
    await expect(p).toContainText("approved knowledge has changed since this snapshot");
    await expect(p).toContainText("No hypothesis available.");
    await expect(p.getByRole("list", { name: "Approved facts" })).toBeVisible();
});

test("stubbed NOT_READY suggestion shows no statements", async ({ page }) => {
    await stub(page, (route) => route.fulfill({ json: suggestion({ readiness: "NOT_READY", reasons: ["CONTEXT_NOT_ELIGIBLE", "NO_APPROVED_PLAN"], facts: [], observations: [], hypotheses: [], unknowns: [] }) }));
    const p = panel(page);
    await expect(p).toContainText("Not ready");
    await expect(p).toContainText("No plan approved for this window now");
    await expect(p).toContainText("No suggestion: the Supervisor cannot state this shortfall against a plan approved now.");
    await expect(p.getByRole("table", { name: "Observations" })).toHaveCount(0);
    await expect(p.getByRole("list", { name: "Approved facts" })).toHaveCount(0);
});

test("stubbed NOT_READY for a changed earlier window says so, not that no plan is approved", async ({ page }) => {
    await stub(page, (route) => route.fulfill({ json: suggestion({ readiness: "NOT_READY", reasons: ["CONTEXT_NOT_ELIGIBLE"], facts: [], observations: [], hypotheses: [], unknowns: [] }) }));
    const p = panel(page);
    await expect(p).toContainText("No suggestion: a window of this case changed since it was opened, so the shortfall cannot be stated.");
    await expect(p).not.toContainText("against a plan approved now");
});

test("stubbed switched-off advisor says so and the case page still works", async ({ page }) => {
    await stub(page, (route) => problem(route, 404, "ADVISOR_DISABLED"));
    await expect(panel(page)).toContainText("Advisor suggestions are switched off for this deployment.");
    await expect(appRegions(panel(page), "alert")).toHaveCount(0);
    await expect(page.getByRole("list", { name: "Case timeline" })).toBeVisible();
});

test("stubbed source text renders as plain text, never markup", async ({ page }) => {
    const hostile = '<img src=x onerror="document.title=1">';
    const s = suggestion();
    await stub(page, (route) => route.fulfill({ json: { ...s, facts: [{ ...s.facts[0], subject_name: hostile }], passages: [{ ...s.passages[0], text: hostile }], hypotheses: [{ ...s.hypotheses[0], text: hostile }] } }));
    const p = panel(page);
    await expect(p.getByRole("list", { name: "Approved facts" })).toContainText(hostile);
    await expect(p.getByRole("list", { name: "Hypotheses" })).toContainText(hostile);
    await expect(p.locator("img")).toHaveCount(0);
});
