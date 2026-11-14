// Shift run against the real backend and the WIS-520 demo seed: a supervisor starts a fresh
// approved plan, an operator records on it, and the supervisor ends it, which shows the final
// summary and the handover draft. Setup uses the public API; every assertion reads the UI.
// The spec builds its own line, workstation, job order and plan, so it never ends the shared seed
// shift (shift.spec.ts and walkthrough.spec.ts record and approve on it).
import { test, expect, type Browser, type Page } from "@playwright/test";
import type { JobOrderBodyV1, JobOrderPageV1, ShiftPlanV1, StationProfileBodyV1, StationProfileV1 } from "../../contracts/typescript/oee";
import { clientNavigate, openSite, signIn } from "./session";
import { problemText, shiftRunText } from "../src/labels";

type Reply = { status: number; body: any };

async function call(page: Page, method: string, path: string, body?: unknown, ifMatch?: number): Promise<Reply> {
    return page.evaluate(async ({ method, path, body, ifMatch }) => {
        const me = await (await fetch("/v1/me")).json();
        const headers: Record<string, string> = {};
        if (method !== "GET") {
            headers["Content-Type"] = "application/json";
            headers["X-CSRF-Token"] = me.csrf_token;
            headers["Idempotency-Key"] = crypto.randomUUID();
            if (ifMatch !== undefined) headers["If-Match"] = `"${ifMatch}"`;
        }
        const r = await fetch(`/v1/sites/${me.sites[0].id}${path}`, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
        return { status: r.status, body: await r.json().catch(() => null) };
    }, { method, path, body, ifMatch });
}

async function ok(reply: Promise<Reply>, statuses: number[], what: string) {
    const r = await reply;
    if (!statuses.includes(r.status)) throw new Error(`${what}: expected ${statuses.join("/")}, got ${r.status} ${r.body?.code ?? ""}`);
    return r.body;
}

async function as(browser: Browser, user: string) {
    const page = await (await browser.newContext()).newPage();
    await signIn(page, user);
    await openSite(page);
    return page;
}

// stage intercepts one mutation route: the first POST gets a stubbed refusal (labelled; the server
// never sees it), the second is held until release() and then goes to the real backend. GETs pass.
async function stage(page: Page, url: string, status: number, code: string) {
    let calls = 0;
    let release!: () => void;
    const gate = new Promise<void>((resolve) => (release = resolve));
    await page.route(url, async (route) => {
        if (route.request().method() !== "POST") return route.fallback();
        if (calls++ === 0)
            return route.fulfill({ status, contentType: "application/problem+json", json: { type: "about:blank", title: code, status, code, correlation_id: "stub" } });
        await gate;
        return route.continue();
    });
    return { release: () => release(), done: () => page.unroute(url) };
}

// RFC 3339 in Asia/Kolkata (+05:30, no DST): the plan's offsets must agree with its timezone.
const ist = (ms: number) => new Date(Math.floor(ms / 1000) * 1000 + 19800e3).toISOString().slice(0, 19) + "+05:30";

// freshPlan copies the seed press-01 profile and job order onto a new line and workstation, plans
// [now-1m, now+60m) there as shift-lead, and has shift-approver approve it.
async function freshPlan(lead: Page, approver: Page, tag: string) {
    const stations = await ok(call(lead, "GET", "/workstations"), [200], "list workstations");
    const press = stations.workstations.find((w: { display_name: string }) => w.display_name === "press-01");
    if (!press) throw new Error("BLOCKED: seed workstation press-01 not found (run supervisor-demo-seed)");
    const profile: StationProfileV1 = await ok(call(lead, "GET", `/workstations/${press.workstation_id}/profile`), [200], "read press-01 profile");
    let cursor = "";
    let seed: JobOrderBodyV1 | undefined;
    for (let i = 0; i < 50 && !seed; i++) {
        const page: JobOrderPageV1 = await ok(call(lead, "GET", `/job-orders?limit=100&include=current${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`), [200], "list job orders");
        seed = page.job_orders.find((j) => j.current_order?.item_code === "Bracket A-100, op 10 Press")?.current_order;
        if (!page.next_cursor) break;
        cursor = page.next_cursor;
    }
    if (!seed) throw new Error("BLOCKED: seed press job order not found (run supervisor-demo-seed)");

    const line = crypto.randomUUID();
    const ws = crypto.randomUUID();
    const name = `run-check-${tag}`;
    const body: StationProfileBodyV1 = { ...profile.revisions[profile.current_revision - 1].profile, workstation_id: ws, line_id: line, display_name: name };
    await ok(call(lead, "POST", `/workstations/${ws}/profile-revisions`, body, 0), [200, 201], "create station profile");
    const { routing_id: _r, step_number: _s, item_id: _i, due_at: _d, ...copy } = seed;
    const order: JobOrderBodyV1 = { ...copy, item_code: `Run check ${tag}`, order_id: crypto.randomUUID(), operation_id: crypto.randomUUID() };
    const job = await ok(call(lead, "POST", "/job-orders", order), [200, 201], "create job order");

    const now = Date.now();
    const created: ShiftPlanV1 = await ok(
        call(lead, "POST", "/shift-plans", {
            schema_version: 1,
            line_id: line,
            starts_at: ist(now - 60e3),
            ends_at: ist(now + 60 * 60e3),
            timezone: "Asia/Kolkata",
            assignments: [
                {
                    workstation_id: ws,
                    job_order_id: job.id,
                    job_order_revision: job.current_revision,
                    order_id: order.order_id,
                    operation_id: order.operation_id,
                    item_code: order.item_code,
                    target_quantity: order.target_quantity,
                    ...(order.target_cycle_time_seconds ? { target_cycle_time_seconds: order.target_cycle_time_seconds } : {}),
                    instruction_document_id: order.instruction_document_id,
                    instruction_revision: order.instruction_revision,
                    instruction_sha256: order.instruction_sha256,
                    template: order.template,
                    fields: order.fields,
                },
            ],
        }),
        [200, 201],
        "create shift plan",
    );
    const rev = created.revisions[created.current_revision - 1];
    await ok(call(approver, "POST", `/shift-plans/${created.id}/approvals`, { canonical_payload_digest: rev.canonical_payload_digest }, created.state_revision), [200], "approve shift plan");
    return { plan: created.id, ws, name, issuer: rev.submitted_by.issuer };
}

test("a supervisor starts and ends a fresh shift; the operator records only while it runs", async ({ browser }) => {
    test.setTimeout(4 * 60_000);
    const tag = crypto.randomUUID().slice(0, 8);
    const lead = await as(browser, "shift-lead");
    const approver = await as(browser, "shift-approver");
    const operator = await as(browser, "operator-1");
    const { plan, ws, name, issuer } = await freshPlan(lead, approver, tag);
    const subject = await operator.evaluate(async () => (await (await fetch("/v1/me")).json()).subject as string);
    await ok(call(lead, "POST", `/workstations/${ws}/authorizations`, { grantee_issuer: issuer, grantee_subject: subject, action: "grant" }, 0), [200, 201], "authorize operator-1");
    const site = await lead.evaluate(async () => (await (await fetch("/v1/me")).json()).sites[0].id as string);
    const planPath = `/sites/${site}/shifts/${plan}`;
    const stationPath = `/sites/${site}/workstations/${ws}`;
    const toolbar = operator.getByRole("toolbar", { name: "Record" });
    const alert = operator.getByRole("alert").filter({ hasText: /\S/ });
    const receipt = (kind: string) => operator.getByRole("status").filter({ hasText: `Recorded: ${kind}` });

    // 1. Planned: the lead sees Start; the operator's entry is disabled with the reason.
    await clientNavigate(lead, planPath);
    await expect(lead.getByText("Planned · not started")).toBeVisible();
    await expect(lead.getByRole("button", { name: "End shift" })).toHaveCount(0);
    await clientNavigate(operator, stationPath);
    await expect(operator.getByRole("heading", { name, level: 1 })).toBeVisible();
    await expect(operator.getByText("Shift not started")).toBeVisible();
    await expect(operator.getByRole("note").filter({ hasText: shiftRunText.planned })).toBeVisible();
    await expect(toolbar.getByRole("button", { name: "Acknowledge" })).toBeDisabled();

    // 2. Negative: the server refuses an entry before start. Hide the run read so the UI lets
    //    the tap through, and show the server's own refusal.
    await operator.route(`**/v1/sites/*/shift-plans/${plan}/run`, (route) => route.fulfill({ status: 503, contentType: "application/problem+json", json: { type: "about:blank", title: "stub", status: 503, code: "STORAGE_UNAVAILABLE", correlation_id: "stub" } }));
    await operator.getByRole("button", { name: "Refresh now" }).click();
    await expect(operator.getByText("Shift not started")).toHaveCount(0);
    await toolbar.getByRole("button", { name: "Acknowledge" }).click();
    await expect(alert.filter({ hasText: problemText.SHIFT_NOT_STARTED })).toBeVisible();
    await expect(receipt("Acknowledged")).toHaveCount(0);
    await operator.unroute(`**/v1/sites/*/shift-plans/${plan}/run`);

    // 3. The lead starts the shift; the state shows only from the server's receipt.
    //    A refusal (stubbed) shows its sentence and changes nothing; a pending start changes nothing.
    const planned = lead.getByText("Planned · not started");
    const start = await stage(lead, `**/v1/sites/*/shift-plans/${plan}/run`, 409, "SHIFT_RUN_ALREADY_STARTED");
    await lead.getByRole("button", { name: "Start shift" }).click();
    await expect(lead.getByRole("alert").filter({ hasText: problemText.SHIFT_RUN_ALREADY_STARTED })).toBeVisible();
    await expect(planned).toBeVisible();
    await expect(lead.getByText(/Running since/)).toHaveCount(0);
    await lead.getByRole("button", { name: "Start shift" }).click();
    await expect(lead.getByRole("button", { name: "Start shift" })).toBeDisabled();
    await expect(planned).toBeVisible();
    await expect(lead.getByText(/Running since/)).toHaveCount(0);
    await expect(lead.getByRole("button", { name: "End shift" })).toHaveCount(0);
    start.release();
    await expect(lead.getByRole("status").filter({ hasText: /Started shift on plan r1/ })).toBeVisible();
    await expect(lead.getByText(/Running since .*\d{2}:\d{2} \S+ by you/)).toBeVisible();
    await expect(lead.getByRole("button", { name: "Start shift" })).toHaveCount(0);
    await start.done();

    // 4. Negative: an operator sees the run state but neither Start nor End.
    await clientNavigate(operator, planPath);
    await expect(operator.getByText(/Running since .*\d{2}:\d{2} \S+ by Rahul Deshmukh/)).toBeVisible();
    await expect(operator.getByRole("button", { name: "Start shift" })).toHaveCount(0);
    await expect(operator.getByRole("button", { name: "End shift" })).toHaveCount(0);

    // 5. The operator records while the shift runs.
    await clientNavigate(operator, stationPath);
    await expect(operator.getByText(/Shift started .*\d{2}:\d{2}/)).toBeVisible();
    await toolbar.getByRole("button", { name: "Acknowledge" }).click();
    await expect(receipt("Acknowledged")).toBeVisible();
    await toolbar.getByRole("button", { name: "Start", exact: true }).click();
    await expect(receipt("Started")).toBeVisible();
    await operator.getByRole("button", { name: /\+ Good/ }).click();
    await operator.getByRole("button", { name: "Add 1 good" }).click();
    await operator.getByRole("button", { name: /^Record/ }).click();
    await expect(receipt("Quantity")).toBeVisible();

    // 6. The lead ends the shift with the next-shift window; the final summary and handover show.
    await lead.getByRole("button", { name: "End shift" }).click();
    const form = lead.getByRole("form", { name: "End this shift" });
    await expect(form.getByText("Operators can no longer record new entries for this shift.")).toBeVisible();
    await expect(form.getByLabel(/Next shift starts/)).not.toHaveValue("");
    const running = lead.getByText(/Running since .*\d{2}:\d{2} \S+ by you/);
    const end = await stage(lead, `**/v1/sites/*/shift-plans/${plan}/run/end`, 409, "SHIFT_RUN_ENDED");
    await form.getByRole("button", { name: "End shift" }).click();
    await expect(lead.getByRole("alert").filter({ hasText: problemText.SHIFT_RUN_ENDED })).toBeVisible();
    await expect(running).toBeVisible();
    await expect(lead.getByText(/Ended at/)).toHaveCount(0);
    await form.getByRole("button", { name: "End shift" }).click();
    await expect(form.getByRole("button", { name: "End shift" })).toBeDisabled();
    await expect(running).toBeVisible();
    await expect(lead.getByText(/Ended at/)).toHaveCount(0);
    await expect(lead.getByRole("heading", { name: /^Handover r\d+ → / })).toHaveCount(0);
    end.release();
    await expect(lead.getByRole("status").filter({ hasText: /Ended shift · summary r1 · handover draft r1/ })).toBeVisible();
    await expect(lead.getByText(/Ended at .*\d{2}:\d{2} \S+ by you/)).toBeVisible();
    const summary = lead.locator(".summaryline");
    await expect(summary).toContainText("Summary r1 of plan r1");
    await expect(summary).toContainText("shift ended");
    await expect(summary).not.toContainText("Shift in progress");
    await expect(lead.getByRole("heading", { name: /^Handover r1 → / })).toBeVisible();
    await expect(lead.getByText("from summary r1")).toBeVisible();
    await expect(lead.getByRole("button", { name: "End shift" })).toHaveCount(0);
    await end.done();

    // 7. After the end the operator's entry is closed again, with the matching reason.
    await clientNavigate(operator, stationPath);
    // The station page is already mounted; it learns of the end on its next 10 s poll.
    await expect(operator.getByText(/Shift ended .*\d{2}:\d{2}/)).toBeVisible({ timeout: 15_000 });
    await expect(operator.getByRole("note").filter({ hasText: shiftRunText.ended })).toBeVisible();
    await expect(toolbar.getByRole("button", { name: /Note/ })).toBeDisabled();
    // Corrections stay open after the end; the server decides (late data appends a summary revision).
    await operator.getByRole("button", { name: /History/ }).click();
    await expect(operator.getByRole("button", { name: "Correct" }).first()).toBeEnabled();
});

// Labelled stubbed API responses (not backend evidence). A Start retried after its first response was
// lost replays the ORIGINAL stored receipt (rule 07: state "started", replayed:true) even though
// another supervisor has ended the run since. The page must read the authoritative run again and show
// the ended state, never "Running" from the old receipt.
test("stubbed Start replay after the run ended shows the ended state from a fresh read", async ({ page }) => {
    const site = "00000000-0000-4000-8000-000000000a11", id = "00000000-0000-4000-8000-000000000a12", line = "00000000-0000-4000-8000-000000000a13";
    const other = { issuer: "https://issuer.test", subject: "approver" };
    let ended = false;
    const run = (state: "planned" | "started" | "ended", replayed = false) => ({
        schema_version: 1, plan_id: id, state, plan_state_revision: 2, replayed, summary_revised: false,
        started_plan_revision: state === "planned" ? null : 1, started_by: state === "planned" ? null : { issuer: other.issuer, subject: "lead" }, started_at: state === "planned" ? null : "2026-11-10T04:05:00Z",
        ended_plan_revision: state === "ended" ? 1 : null, ended_by: state === "ended" ? other : null, ended_at: state === "ended" ? "2026-11-10T06:00:00Z" : null,
        summary: state === "ended" ? { revision: 1, canonical_payload_digest: "b".repeat(64) } : null, handover_revision: state === "ended" ? 1 : null, latest_summary_revision: state === "ended" ? 1 : null,
    });
    const plan = { id, line_id: line, starts_at: "2026-11-10T04:00:00Z", ends_at: "2026-11-10T12:00:00Z", current_revision: 1, state_revision: 2, approved_revision: 1,
        revisions: [{ revision: 1, state: "approved", plan: { schema_version: 1, line_id: line, timezone: "Asia/Kolkata", starts_at: "2026-11-10T09:30:00+05:30", ends_at: "2026-11-10T17:30:00+05:30", assignments: [] },
            submitted_by: other, canonical_payload_digest: "a".repeat(64), created_at: "2026-11-10T03:00:00Z", approval: null }] };
    await page.route("**/v1/**", async (route) => {
        const req = route.request();
        const path = new URL(req.url()).pathname.replace(`/v1/sites/${site}`, "");
        if (path === "/v1/me") return route.fulfill({ json: { csrf_token: "stub", subject: "lead", display_name: "Lead", sites: [{ id: site, name: "Stub site", role: "supervisor", tenant_id: site, agent_available: false }] } });
        if (path === "/workstations") return route.fulfill({ json: { workstations: [] } });
        if (path === "/members") return route.fulfill({ json: { members: [{ subject: "approver", display_name: "Ananya Kulkarni", role: "supervisor" }] } });
        if (path === `/shift-plans/${id}/run` && req.method() === "POST") {
            ended = true; // another supervisor ended it meanwhile; the replay still carries the original body
            return route.fulfill({ status: 201, json: run("started", true) });
        }
        if (path === `/shift-plans/${id}/run`) return route.fulfill({ json: run(ended ? "ended" : "planned") });
        if (path === `/shift-plans/${id}`) return route.fulfill({ json: plan });
        if (path.endsWith("/summaries")) return route.fulfill({ json: { summaries: [] } });
        if (path.endsWith("/handovers")) return route.fulfill({ json: { handovers: [] } });
        return route.fulfill({ status: 404, contentType: "application/problem+json", json: { type: "about:blank", title: "NOT_FOUND", status: 404, code: "NOT_FOUND", correlation_id: "stub" } });
    });
    await page.goto(`/sites/${site}/shifts/${id}`);
    await expect(page.getByText("Planned · not started")).toBeVisible();
    await page.getByRole("button", { name: "Start shift" }).click();
    await expect(page.getByText(/Ended at .*\d{2}:\d{2} \S+ by Ananya Kulkarni/)).toBeVisible();
    await expect(page.getByText(/Running since/)).toHaveCount(0);
    await expect(page.getByRole("button", { name: "End shift" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Start shift" })).toHaveCount(0);
});

// Labelled stubbed API responses (not backend evidence) for two edge cases.
const edge = { site: "00000000-0000-4000-8000-000000000b11", plan: "00000000-0000-4000-8000-000000000b12", line: "00000000-0000-4000-8000-000000000b13", ws: "00000000-0000-4000-8000-000000000b14", job: "00000000-0000-4000-8000-000000000b15" };
const edgeActor = { issuer: "https://issuer.test", subject: "lead" };
const edgeRun = (state: "started") => ({
    schema_version: 1, plan_id: edge.plan, state, plan_state_revision: 2, replayed: false, summary_revised: false,
    started_plan_revision: 1, started_by: edgeActor, started_at: "2026-11-10T04:05:00Z", ended_plan_revision: null, ended_by: null, ended_at: null,
    summary: null, handover_revision: null, latest_summary_revision: null,
});
const edgeProblem = (status: number, code: string) => ({ status, contentType: "application/problem+json", json: { type: "about:blank", title: code, status, code, correlation_id: "stub" } });
const edgeMe = (role: string) => ({ json: { csrf_token: "stub", subject: "lead", display_name: "Lead", sites: [{ id: edge.site, name: "Stub site", role, tenant_id: edge.site, agent_available: false }] } });

// A /records refusal clears the station's binding in the same refresh. A run read that answers after
// it must not bring the shift badge back for a binding the page no longer has.
test("stubbed run read answering after a /records 404 does not restore the shift badge", async ({ page }) => {
    let runAnswered = false;
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname.replace(`/v1/sites/${edge.site}`, "");
        if (path === "/v1/me") return route.fulfill(edgeMe("operator"));
        if (path === "/workstations") return route.fulfill({ json: { workstations: [{ workstation_id: edge.ws, line_id: edge.line, display_name: "Edge press", current_revision: 1 }] } });
        if (path === "/members") return route.fulfill({ json: { members: [] } });
        if (path === `/workstations/${edge.ws}/profile`)
            return route.fulfill({ json: { current_revision: 1, replayed: false, revisions: [{ revision: 1, profile: { display_name: "Edge press", fields: [], line_id: edge.line, schema_version: 1, template: "production", workstation_id: edge.ws } }] } });
        if (path === `/workstations/${edge.ws}/current-job-order`)
            return route.fulfill({ json: { workstation_id: edge.ws, job_order_id: edge.job, line_id: edge.line, plan_id: edge.plan, plan_revision: 1, pinned_revision: 1, current_revision: 1, starts_at: "2026-11-10T04:00:00Z", ends_at: "2026-11-10T12:00:00Z", timezone: "Asia/Kolkata", state: "needs_replan", target_quantity: 10, order: null } });
        if (path === `/shift-plans/${edge.plan}/run`) {
            await new Promise((resolve) => setTimeout(resolve, 1000)); // answers after /records
            runAnswered = true;
            return route.fulfill({ json: edgeRun("started") });
        }
        if (path === `/workstations/${edge.ws}/records`) return route.fulfill(edgeProblem(404, "NOT_FOUND"));
        return route.fulfill(edgeProblem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${edge.site}/workstations/${edge.ws}`);
    await expect(page.getByRole("heading", { name: "Edge press", level: 1 })).toBeVisible();
    await expect.poll(() => runAnswered).toBe(true);
    await page.waitForTimeout(300); // let the late response settle
    await expect(page.getByText("Shift started")).toHaveCount(0);
});

// Date.parse keeps milliseconds only: a plan ending 12:00:00.000001 IST must default the next shift to
// 12:01, never 12:00 (which would start before this shift ends and be refused).
test("stubbed End shift defaults the next start past a sub-millisecond plan end", async ({ page }) => {
    const plan = { id: edge.plan, line_id: edge.line, starts_at: "2026-11-10T00:30:00Z", ends_at: "2026-11-10T06:30:00.000001Z", current_revision: 1, state_revision: 2, approved_revision: 1,
        revisions: [{ revision: 1, state: "approved", plan: { schema_version: 1, line_id: edge.line, timezone: "Asia/Kolkata", starts_at: "2026-11-10T06:00:00+05:30", ends_at: "2026-11-10T12:00:00.000001+05:30", assignments: [] },
            submitted_by: edgeActor, canonical_payload_digest: "a".repeat(64), created_at: "2026-11-10T00:00:00Z", approval: null }] };
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname.replace(`/v1/sites/${edge.site}`, "");
        if (path === "/v1/me") return route.fulfill(edgeMe("supervisor"));
        if (path === "/workstations") return route.fulfill({ json: { workstations: [] } });
        if (path === "/members") return route.fulfill({ json: { members: [] } });
        if (path === `/shift-plans/${edge.plan}/run`) return route.fulfill({ json: edgeRun("started") });
        if (path === `/shift-plans/${edge.plan}`) return route.fulfill({ json: plan });
        if (path.endsWith("/summaries")) return route.fulfill({ json: { summaries: [] } });
        if (path.endsWith("/handovers")) return route.fulfill({ json: { handovers: [] } });
        return route.fulfill(edgeProblem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${edge.site}/shifts/${edge.plan}`);
    await page.getByRole("button", { name: "End shift" }).click();
    const form = page.getByRole("form", { name: "End this shift" });
    await expect(form.getByLabel(/Next shift starts/)).toHaveValue("2026-11-10T12:01");
});
