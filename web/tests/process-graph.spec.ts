import { expect, test, type Page } from "@playwright/test";

// WIS-618: the approved process view reads the latest snapshot's neighbourhood of a process, then its
// materials' suppliers and locations one hop further at the same instant, and shows the approved
// facts by stage with their cited passages. Every test stubs the API.

const site = "00000000-0000-4000-8000-000000000941";
const process = "11111111-0000-4000-8000-000000000942";
const press = "22222222-0000-4000-8000-000000000943";
const coil = "33333333-0000-4000-8000-000000000944";
const mill = "44444444-0000-4000-8000-000000000945";
const rack = "55555555-0000-4000-8000-000000000946";
const snapshot = "66666666-0000-4000-8000-000000000947";
const span = "77777777-0000-4000-8000-000000000948";
const asOf = "2026-11-02T09:00:00Z";
const me = {
    csrf_token: "test-csrf", display_name: "Stubbed viewer", subject: "viewer-subject",
    sites: [{ id: site, name: "Stubbed Site", role: "viewer", tenant_id: "00000000-0000-4000-8000-000000000940", agent_available: false }],
};
const entity = (id: string, type: string, name: string) => ({ entity_id: id, type, display_name: name, revision: 1, source_id: "s", source_revision: 1 });
let n = 0;
const claim = (subject: string, predicate: string, rest: Record<string, unknown>) => ({
    claim_id: `c0000000-0000-4000-8000-${String(++n).padStart(12, "0")}`, subject_id: subject, subject_revision: 1, predicate, revision: 1, review_id: "r",
    effective_from: "2026-10-01T00:00:00Z", evidence_class: "report", source_span_ids: [span], resolution_refs: [], authority: { decision_id: "d", revision: 1, source_id: "s", source_revision: 1 }, ...rest,
});
const head = {
    schema_version: 1, entity_id: process, as_of: asOf, depth: 2, readiness: "READY", snapshot_id: snapshot, sha256: "a".repeat(64), captured_at: "2026-11-02T08:30:00Z", inventory_revision: 4,
    graph: {
        truncated: false,
        entities: [entity(process, "process", "Bracket production"), entity(press, "operation", "Press bracket"), entity(coil, "material", "Steel coil")],
        claims: [
            claim(press, "part_of", { object_entity_id: process, object_revision: 1 }),
            claim(press, "consumes", { object_entity_id: coil, object_revision: 1 }),
            claim(press, "cycle_time_seconds", { literal: { type: "decimal", value: "42", unit: "s" } }),
        ],
    },
};
const more = {
    schema_version: 1, snapshot_id: snapshot, sha256: "a".repeat(64), inventory_revision: 4, truncated: false,
    entities: [entity(coil, "material", "Steel coil"), entity(mill, "supplier", "Bharat Steel"), entity(rack, "location", "Rack A")],
    claims: [claim(coil, "supplied_by", { object_entity_id: mill, object_revision: 1 }), claim(coil, "stored_at", { object_entity_id: rack, object_revision: 1 })],
};
const passage = "Steel coil comes from Bharat Steel and is stored in Rack A.";
const problem = (status: number, code: string) => ({ status, contentType: "application/problem+json", body: JSON.stringify({ code, status, title: code, type: "about:blank" }) });

// WIS-642: the route summary the server computes for the same snapshot and instant.
const blank = "88888888-0000-4000-8000-000000000949";
const value = (v: string | null, status = v === null ? "missing" : "known") => ({ status, value: v, unit: "s", claim_ids: v === null ? [] : ["c1"], source_span_ids: v === null ? [] : [span] });
const total = (v: string | null, missing: number) => ({ value: v, unit: "s", steps_missing: missing, claim_ids: v === null ? [] : ["c1", "c2"] });
const routeOf = (over: Record<string, unknown>) => ({
    schema_version: 1, snapshot_id: snapshot, sha256: "a".repeat(64), inventory_revision: 4, process_id: process, process_name: "Bracket production", as_of: asOf,
    readiness: "READY", reason_codes: [], step_count: 2, order_claim_ids: ["c3"],
    steps: [
        { entity_id: blank, display_name: "Blank", position: 1, cycle_time_seconds: value("60"), setup_time_seconds: value(null) },
        { entity_id: press, display_name: "Press bracket", position: 2, cycle_time_seconds: value("42.5"), setup_time_seconds: value(null) },
    ],
    cycle_time_seconds: total("102.5", 0), setup_time_seconds: total(null, 2),
    longest_lead_time_days: { value: "14", unit: "day", entity_id: mill, entity_name: "Bharat Steel", materials: 2, materials_without_lead_time: 1, claim_ids: ["c4"], source_span_ids: [span] },
    ...over,
});

async function open(page: Page, read: object, onGraph: (url: URL) => void = () => {}, routeBody: object = routeOf({}), onRoute: (url: URL) => void = () => {}) {
    await page.route("**/v1/**", async (route) => {
        const url = new URL(route.request().url());
        const path = url.pathname;
        if (path === "/v1/me") return route.fulfill({ json: me });
        if (path === `/v1/sites/${site}/discovery-inventories`) {
            return route.fulfill({ json: { inventories: [{ inventory_id: "i1", process_id: process, process_display_name: "Bracket production", capability: "process_understanding", current_revision: 1, created_at: asOf, updated_at: asOf }] } });
        }
        if (path === `/v1/sites/${site}/knowledge`) return route.fulfill({ json: read });
        if (path === `/v1/sites/${site}/process-snapshots/${snapshot}/graph`) {
            onGraph(url);
            return route.fulfill({ json: more });
        }
        if (path === `/v1/sites/${site}/process-snapshots/${snapshot}/route-summary`) {
            onRoute(url);
            return route.fulfill({ json: routeBody });
        }
        if (path === `/v1/sites/${site}/source-spans/${span}`) return route.fulfill({ json: { id: span, source_id: "s", revision: 1, start_char: 0, end_char: passage.length, text: passage, source_sha256: "b", access_epoch: 1 } });
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/process`);
    await expect(page.getByRole("heading", { name: "Approved process", exact: true })).toBeVisible();
}

test("stubbed approved process shows facts by stage, reading suppliers and storage from the materials at the same instant", async ({ page }) => {
    const graphs: URL[] = [];
    await open(page, head, (url) => graphs.push(url));
    // One registered process is selected on its own.
    await expect(page.getByRole("combobox", { name: "Process" })).toHaveValue(process);
    await expect(page.getByRole("region", { name: "Suppliers" })).toContainText("Material Steel coil supplied by Supplier Bharat Steel");
    await expect(page.getByRole("region", { name: "Receiving and storage" })).toContainText("Material Steel coil stored at Location Rack A");
    await expect(page.getByRole("region", { name: "Routing" })).toContainText("Operation Press bracket consumes Material Steel coil");
    await expect(page.getByRole("region", { name: "Times" })).toContainText("Operation Press bracket cycle time 42 s");
    await expect(page.getByRole("region", { name: "Dispatch" })).toContainText("No approved facts for this stage yet.");
    expect(graphs).toHaveLength(1);
    expect(graphs[0].searchParams.getAll("root")).toEqual([coil]);
    expect(graphs[0].searchParams.get("as_of")).toBe(asOf);
    expect(graphs[0].searchParams.getAll("predicate")).toContain("supplied_by");
});

test("stubbed fact opens the passage it cites", async ({ page }) => {
    await open(page, head);
    const suppliers = page.getByRole("region", { name: "Suppliers" });
    await suppliers.getByRole("button", { name: "Show source" }).click();
    await expect(suppliers.getByRole("list", { name: /^Sources of:/ })).toContainText(passage);
    await suppliers.getByRole("button", { name: "Hide source" }).click();
    await expect(suppliers.getByRole("list", { name: /^Sources of:/ })).toHaveCount(0);
});

test("stubbed process without an approved snapshot says why, and shows no facts", async ({ page }) => {
    await open(page, { schema_version: 1, entity_id: process, as_of: asOf, depth: 2, readiness: "NOT_READY", reason_code: "NO_SNAPSHOT" });
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("No approved snapshot of the process knowledge has been sealed yet");
    await expect(page.getByRole("region", { name: "Suppliers" })).toHaveCount(0);
});

test("stubbed route lists the operations in approved order with the server's exact totals", async ({ page }) => {
    const asked: URL[] = [];
    await open(page, head, () => {}, routeOf({}), (url) => asked.push(url));
    const card = page.getByRole("region", { name: "Route" });
    await expect(card.getByRole("list", { name: "Operations in order" }).getByRole("listitem")).toHaveText([/^Blank · cycle 60 s · setup missing/, /^Press bracket · cycle 42\.5 s · setup missing/]);
    const totals = card.getByRole("definition");
    await expect(totals).toHaveText([
        "2",
        "102.5 s",
        "not known: 2 of 2 steps have no approved setup time",
        /^14 days \(Bharat Steel\) · 1 of 2 materials have no approved lead time/,
    ]);
    expect(asked).toHaveLength(1);
    expect(asked[0].searchParams.get("process_id")).toBe(process);
    expect(asked[0].searchParams.get("as_of")).toBe(asOf); // the same instant as the snapshot read
});

test("stubbed route with a gap says why, lists the operations without an order, and shows no totals", async ({ page }) => {
    const unordered = routeOf({}).steps.map((s) => ({ ...s, position: null }));
    await open(page, head, () => {}, routeOf({ readiness: "NOT_READY", reason_codes: ["ROUTE_INCOMPLETE"], steps: unordered, cycle_time_seconds: total(null, 0), setup_time_seconds: total(null, 2), longest_lead_time_days: { ...routeOf({}).longest_lead_time_days, value: null, entity_id: null, entity_name: null, claim_ids: [], source_span_ids: [] } }));
    const card = page.getByRole("region", { name: "Route" });
    await expect(card.getByRole("alert").filter({ hasText: /\S/ })).toHaveText("The approved order has a gap: some operations are not linked to the next one.");
    await expect(card.getByRole("list", { name: "Operations in order" })).toHaveCount(0);
    await expect(card.getByRole("list", { name: "Operations, order unknown" }).getByRole("listitem")).toHaveText(["Blank", "Press bracket"]);
    await expect(card.getByRole("definition").nth(1)).toHaveText("not computed");
    await expect(card.getByRole("definition").nth(3)).toHaveText(/^not computed/);
});

test("stubbed route with a missing cycle time shows it missing, never as zero, and no total", async ({ page }) => {
    const steps = routeOf({}).steps;
    await open(page, head, () => {}, routeOf({ steps: [steps[0], { ...steps[1], cycle_time_seconds: value(null) }], cycle_time_seconds: total(null, 1) }));
    const card = page.getByRole("region", { name: "Route" });
    await expect(card.getByRole("list", { name: "Operations in order" }).getByRole("listitem").nth(1)).toContainText("cycle missing");
    await expect(card.getByRole("definition").nth(1)).toHaveText("not known: 1 of 2 steps have no approved cycle time");
    await expect(card).not.toContainText(/(cycle|time) 0(\.0+)? s/);
});
