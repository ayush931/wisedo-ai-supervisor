// Labelled stubbed API responses: these exercise the real form, not solver quality.
import { test, expect, type Page } from "@playwright/test";
const site = "00000000-0000-4000-8000-000000000111";
const line = "00000000-0000-4000-8000-000000000112";
const station = "00000000-0000-4000-8000-000000000113";
const job = "00000000-0000-4000-8000-000000000114";
const id = "00000000-0000-4000-8000-000000000115";
const order = { schema_version: 1, order_id: id, operation_id: id, item_code: "Test part", target_quantity: 120, target_cycle_time_seconds: "3", instruction_document_id: id, instruction_revision: 1, instruction_sha256: "a".repeat(64), template: "production", fields: [{ name: "good_units", label: "Good units", kind: "decimal", unit: "count", required: true }] };
const assignment = { ...order, workstation_id: station, job_order_id: job, job_order_revision: 2 };
delete (assignment as Partial<typeof assignment>).schema_version;
async function setup(page: Page, method: string, infeasible = false, gate?: Promise<void>, extra: Record<string, unknown> = {}) {
    const writes: { path: string; body: any }[] = [];
    let saved: any;
    await page.route("**/v1/**", async route => {
        const path = new URL(route.request().url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: { csrf_token: "stub", subject: "lead", display_name: "Stub lead", sites: [{ id: site, name: "Stubbed suggestions site", role: "supervisor", tenant_id: id }] } });
        if (path.endsWith("/workstations")) return route.fulfill({ json: { workstations: [{ workstation_id: station, line_id: line, display_name: "Press", current_revision: 1 }] } });
        if (path.endsWith("/job-orders")) return route.fulfill({ json: { job_orders: [{ id: job, current_revision: 1, current_order: { ...order, target_quantity: 100 } }] } });
        if (path.endsWith("/members")) return route.fulfill({ json: { members: [] } });
        if (path.endsWith("/suggestions")) {
            if (gate) await gate;
            const body = route.request().postDataJSON();
            expect(Object.keys(body).sort()).toEqual(["ends_at", "job_order_ids", "line_id", "schema_version", "starts_at", "timezone"]);
            expect(body.job_order_ids).toEqual([job]);
            expect(route.request().headers()["idempotency-key"]).toBeUndefined();
            if (infeasible) return route.fulfill({ status: 422, contentType: "application/problem+json", json: { type: "about:blank", title: "No feasible schedule", status: 422, code: "NO_FEASIBLE_SCHEDULE", correlation_id: "stub" } });
            return route.fulfill({ json: { schema_version: 1, method, is_optimized: method === "ortools_cp_sat", assignments: [assignment], operations: [], job_orders: [{ id: job, current_revision: 2, current_order: order }], unplaced_orders: [{ job_order_id: id, reason_code: "ROUTING_UNLINKED" }], warnings: [], ...extra } });
        }
        if (route.request().method() === "POST") {
            const body = route.request().postDataJSON(); writes.push({ path, body });
            if (path.endsWith("/shift-plans")) {
                saved = { id, line_id: line, starts_at: body.starts_at, ends_at: body.ends_at, current_revision: 1, state_revision: 1, approved_revision: null, revisions: [{ revision: 1, state: "draft", plan: body, submitted_by: { issuer: "stub", subject: "lead" }, canonical_payload_digest: "a".repeat(64), approval: null }] };
                return route.fulfill({ status: 201, json: saved });
            }
            if (path.endsWith("/approvals")) return route.fulfill({ status: 403, contentType: "application/problem+json", json: { status: 403, code: "SELF_REVIEW_FORBIDDEN", correlation_id: "stub" } });
        }
        if (path.endsWith("/shift-plans/" + id)) return route.fulfill({ json: saved });
        if (path.endsWith("/summaries")) return route.fulfill({ json: { summaries: [] } });
        if (path.endsWith("/handovers")) return route.fulfill({ json: { handovers: [] } });
        return route.fulfill({ json: {} });
    });
    await page.goto(`/sites/${site}/shifts/new`);
    await expect(page.getByRole("heading", { name: "New shift plan" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Suggest assignments", exact: true })).toBeVisible();
    await page.locator("#pf-line").selectOption(line);
    await page.getByLabel("Timezone", { exact: true }).fill("UTC");
    await page.getByLabel("Starts", { exact: true }).fill("2026-10-20T08:00");
    await page.getByLabel("Ends", { exact: true }).fill("2026-10-20T16:00");
    await page.getByLabel("Job orders to suggest").selectOption(job);
    return writes;
}
for (const method of ["greedy", "ortools_cp_sat"]) {
    test(`stubbed ${method}: suggestion fills editable draft; creation still needs separate approval`, async ({ page }) => {
        const writes = await setup(page, method);
        await page.getByRole("button", { name: "Suggest assignments", exact: true }).click();
        await expect(page.getByLabel("Row 1 workstation")).toHaveValue(station);
        await expect(page.getByLabel("Row 1 target quantity")).toHaveValue("120");
        await expect(page.getByRole("status").filter({ hasText: method === "greedy" ? "Heuristic" : "Optimised (CP-SAT)" })).toBeVisible();
        await expect(page.getByText(/Link this job order to a routing step/)).toBeVisible();
        expect(writes).toEqual([]);
        await page.getByLabel("Row 1 target quantity").fill("110");
        await page.getByRole("button", { name: "Create draft plan" }).click();
        await expect(page.getByText("Draft · rev 1 awaiting approval")).toBeVisible();
        expect(writes).toHaveLength(1);
        expect(writes[0].body.assignments[0]).toMatchObject({ target_quantity: 110, job_order_revision: 2 });
        await page.getByRole("button", { name: "Approve r1", exact: true }).click();
        await expect(page.getByRole("dialog").getByText("The approver must not be a submitter; the server decides.")).toBeVisible();
        await page.getByRole("dialog").getByRole("button", { name: "Approve r1" }).click();
        await expect(page.getByRole("alert").getByText("You submitted this revision. Another supervisor must approve it.")).toBeVisible();
    });
}
test("stubbed suggestion requires confirmation before replacing edited rows", async ({ page }) => {
    await setup(page, "greedy");
    await page.getByLabel("Row 1 target quantity").fill("77");
    page.once("dialog", dialog => dialog.dismiss());
    await page.getByRole("button", { name: "Suggest assignments", exact: true }).click();
    await expect(page.getByLabel("Row 1 target quantity")).toHaveValue("77");
    page.once("dialog", dialog => dialog.accept());
    await page.getByRole("button", { name: "Suggest assignments", exact: true }).click();
    await expect(page.getByLabel("Row 1 target quantity")).toHaveValue("120");
});
test("stubbed infeasible selection has a clear announced message and saves nothing", async ({ page }) => {
    const writes = await setup(page, "greedy", true);
    await page.getByRole("button", { name: "Suggest assignments", exact: true }).click();
    await expect(page.getByRole("alert").getByText(/No feasible plan/)).toBeVisible();
    expect(writes).toEqual([]);
});

test("stubbed pending suggestion announces progress and protects rows during keyboard activation", async ({ page }) => {
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    await setup(page, "greedy", false, gate);
    const button = page.getByRole("button", { name: "Suggest assignments", exact: true });
    await button.focus();
    await page.keyboard.press("Enter");
    try {
        await expect(page.getByRole("status").filter({ hasText: "Suggesting assignments…" })).toBeVisible();
        await expect(button).toBeDisabled();
        await expect(page.getByLabel("Row 1 target quantity")).toBeDisabled();
        await expect(page.getByRole("button", { name: "Create draft plan" })).toBeDisabled();
    } finally { release(); }
    await expect(page.getByLabel("Row 1 target quantity")).toHaveValue("120");
    await expect(button).toBeEnabled();
});

for (const changed of ["line", "start", "end", "timezone", "selection"]) {
 test(`stubbed suggestion status is cleared when ${changed} changes`, async ({page}) => {
  await setup(page,"greedy");
  await page.getByRole("button",{name:"Suggest assignments",exact:true}).click();
  await expect(page.getByRole("status").filter({hasText:"Heuristic"})).toBeVisible();
  if(changed==="line") await page.locator("#pf-line").selectOption("");
  if(changed==="start") await page.getByLabel("Starts",{exact:true}).fill("2026-10-20T09:00");
  if(changed==="end") await page.getByLabel("Ends",{exact:true}).fill("2026-10-20T17:00");
  if(changed==="timezone") await page.getByLabel("Timezone",{exact:true}).fill("America/Detroit");
  if(changed==="selection") await page.getByLabel("Job orders to suggest").selectOption([]);
  await expect(page.getByText(/Heuristic/)).toHaveCount(0);
  await expect(page.getByText(/Link this job order to a routing step/)).toHaveCount(0);
 });
}
test("stubbed refusal status clears when the selection changes", async ({page})=>{
 await setup(page,"greedy",true);
 await page.getByRole("button",{name:"Suggest assignments",exact:true}).click();
 await expect(page.getByRole("alert").getByText(/No feasible plan/)).toBeVisible();
 await page.getByLabel("Job orders to suggest").selectOption([]);
 await expect(page.getByText(/No feasible plan/)).toHaveCount(0);
});
// WIS-628: approved onboarding cycle times are shown as advice; the draft keeps the job order's value.
test("stubbed suggestion shows approved cycle times that differ, without changing the draft", async ({ page }) => {
    const writes = await setup(page, "greedy", false, undefined, {
        warnings: ["APPROVED_CYCLE_TIME_DIFFERS"],
        approved_cycle_times: [
            { job_order_id: job, operation_id: id, status: "differs", approved_seconds: "2.5", job_order_seconds: "3", claim_ids: [id], source_span_ids: [id] },
            { job_order_id: id, operation_id: id, status: "matches", approved_seconds: "4", job_order_seconds: "4", claim_ids: [id], source_span_ids: [id] },
        ],
    });
    await page.getByRole("button", { name: "Suggest assignments", exact: true }).click();
    const panel = page.getByRole("status", { name: "Approved cycle times" });
    await expect(panel).toContainText("Test part · differs from the approved value · approved 2.5 s · job order 3 s · cited in 1 approved passage");
    await expect(panel.getByRole("listitem")).toHaveCount(1);
    await expect(page.getByText(/The plan uses the job orders' own values/)).toBeVisible();
    await expect(page.getByLabel("Row 1 workstation")).toHaveValue(station);
    expect(writes).toEqual([]);
});
test("stubbed suggestion with only matching cycle times shows no cycle-time panel", async ({ page }) => {
    await setup(page, "greedy", false, undefined, {
        approved_cycle_times: [{ job_order_id: job, operation_id: id, status: "matches", approved_seconds: "3", job_order_seconds: "3", claim_ids: [id], source_span_ids: [id] }],
    });
    await page.getByRole("button", { name: "Suggest assignments", exact: true }).click();
    await expect(page.getByLabel("Row 1 workstation")).toHaveValue(station);
    await expect(page.getByRole("status", { name: "Approved cycle times" })).toHaveCount(0);
});
