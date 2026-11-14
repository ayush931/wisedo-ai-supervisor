import { expect, test, type Page } from "@playwright/test";

// WIS-618: the onboarding checklist reads each value-stream inventory's coverage and shows its nine
// stages in the order material flows, with their state and who is asked. Every test stubs the API.

const site = "00000000-0000-4000-8000-000000000931";
const press = "11111111-0000-4000-8000-000000000932";
const paint = "22222222-0000-4000-8000-000000000933";
const shortfall = "33333333-0000-4000-8000-000000000934";
const me = {
    csrf_token: "test-csrf",
    display_name: "Stubbed supervisor",
    subject: "supervisor-subject",
    sites: [{ id: site, name: "Stubbed Site", role: "supervisor", tenant_id: "00000000-0000-4000-8000-000000000930", agent_available: false }],
};
const inventory = (id: string, name: string, capability: string) => ({
    inventory_id: id, process_id: `p-${id}`, process_display_name: name, capability, current_revision: 1, created_at: "2026-11-02T00:00:00Z", updated_at: "2026-11-02T00:00:00Z",
});
const keys: [string, string][] = [
    ["stage_responsibility", "owner"], ["dispatch_delivery", "owner"], ["material_suppliers", "owner"], ["receiving_inspection", "supervisor"],
    ["storage_locations", "operator"], ["item_routing", "supervisor"], ["cycle_setup_times", "operator"], ["in_process_quality", "supervisor"], ["packing_finished_goods", "operator"],
];
const coverage = (states: Record<string, string>) => ({
    schema_version: 1, evaluator_version: 2, inventory_id: press, inventory_revision: 1, template_version: 1, knowledge_inventory_revision: 3,
    evaluated_at: "2026-11-02T08:00:00Z", scope_current: true, bound_count: 2, approved_count: Object.values(states).filter((s) => s === "approved").length,
    applicable_count: 9, evaluation_state: "partial_support", knowledge_ready: false, authorization_state: "disabled",
    // Listed by key, as the server does: the screen orders the stages itself.
    requirements: keys.map(([key, role], i) => ({
        requirement_id: `r-${i}`, key, interview_role: role, binding_supported: true, state: states[key] ?? "missing", reason_code: states[key] === "approved" ? "qualified_document_evidence" : "decision_missing",
    })),
});
const problem = (status: number, code: string) => ({ status, contentType: "application/problem+json", body: JSON.stringify({ code, status, title: code, type: "about:blank" }) });

async function open(page: Page, inventories: unknown[], coverages: Record<string, () => object>) {
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: me });
        if (path === `/v1/sites/${site}/discovery-inventories`) return route.fulfill({ json: { inventories } });
        for (const [id, reply] of Object.entries(coverages)) {
            if (path === `/v1/sites/${site}/discovery-inventories/${id}/coverage`) return route.fulfill(reply());
        }
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/onboarding`);
    await expect(page.getByRole("heading", { name: "Onboarding checklist", exact: true })).toBeVisible();
}

test("stubbed value-stream checklist lists the nine stages in flow order with their state and counts", async ({ page }) => {
    await open(page, [inventory(press, "Bracket pressing", "process_understanding"), inventory(shortfall, "Bracket pressing", "shortfall_investigation")], {
        [press]: () => ({ json: coverage({ material_suppliers: "approved", item_routing: "approved", cycle_setup_times: "candidate" }) }),
    });
    const card = page.getByRole("article", { name: "Checklist: Bracket pressing" });
    // Only the value-stream inventory has a checklist here.
    await expect(page.getByRole("article")).toHaveCount(1);
    await expect(card.getByText("2 of 9 stages approved")).toBeVisible();
    const stages = card.getByRole("rowheader");
    await expect(stages).toHaveText(["Suppliers", "Receiving", "Storage", "Routing", "Times", "Quality", "Packing", "Dispatch", "Responsibility"]);
    const row = (name: string) => card.getByRole("row").filter({ has: page.getByRole("rowheader", { name, exact: true }) });
    await expect(row("Suppliers")).toContainText("Approved");
    await expect(row("Times")).toContainText("Waiting for review");
    await expect(row("Dispatch")).toContainText("Missing");
    await expect(row("Dispatch")).toContainText("No reviewed facts yet");
    await expect(row("Storage")).toContainText("Operator");
    await expect(card.getByRole("link", { name: "Start an interview" })).toHaveAttribute("href", `/sites/${site}/interviews`);
});

test("stubbed checklist that cannot be read is shown as unavailable, not as empty, while the others load", async ({ page }) => {
    let fail = true;
    await open(page, [inventory(press, "Bracket pressing", "process_understanding"), inventory(paint, "Painting", "process_understanding")], {
        [press]: () => (fail ? problem(503, "KNOWLEDGE_UNAVAILABLE") : { json: coverage({}) }),
        [paint]: () => ({ json: coverage({ dispatch_delivery: "approved" }) }),
    });
    const broken = page.getByRole("article", { name: "Checklist: Bracket pressing" });
    await expect(broken.getByRole("button", { name: "Retry checklist" })).toBeVisible();
    await expect(broken.getByText(/stages approved/)).toHaveCount(0);
    await expect(page.getByRole("article", { name: "Checklist: Painting" }).getByText("1 of 9 stages approved")).toBeVisible();
    fail = false;
    await broken.getByRole("button", { name: "Retry checklist" }).click();
    await expect(broken.getByText("0 of 9 stages approved")).toBeVisible();
});

test("stubbed site without a value-stream checklist explains how one is registered", async ({ page }) => {
    await open(page, [inventory(shortfall, "Bracket pressing", "shortfall_investigation")], {});
    // The EmptyState title, not Astryx's page-wide announcer region, which briefly repeats it.
    await expect(page.getByRole("heading", { name: /No value-stream checklist is registered at this site yet/ })).toBeVisible();
    await expect(page.getByRole("article")).toHaveCount(0);
});
