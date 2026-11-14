import { expect, test, type Browser, type Page } from "@playwright/test";
import { openSite, section, signIn } from "./session";

// WIS-635: the task workspace against the real backend, from a detected shortfall to an
// operator's completed report, with no SQL and no stubbed route. Each run builds its own line
// through the public API (the same steps as supervisor-demo-seed's seedCase), because the shared
// demo case is canceled and reopened by cases.spec.ts and its heartbeats are long stale by the
// time this phase runs. The worker then does the rest on its own ticks:
//   fold -> case evaluator opens the case -> StartDetected (WIS-636) -> scripted investigation
//   -> validated maintenance proposal -> a site supervisor approves -> the executor creates the task.
//
// @agent-scripted: the API and worker run SUPERVISOR_AGENT_PROVIDER=fixture with
// SUPERVISOR_AGENT_FIXTURE_SCRIPT=deploy/supervisor/agent-fixture-script.v1.json and
// SUPERVISOR_TASK_EXECUTION_ENABLED=true (CI's fourth e2e phase). A scripted fixture is a contract
// check, not actual model behaviour (rule 09). Another mode is BLOCKED, never skipped.

const env = (globalThis as typeof globalThis & { process?: { env?: Record<string, string | undefined> } }).process?.env ?? {};

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

async function ok(reply: Promise<Reply>, status: number | number[], what: string) {
    const r = await reply;
    const want = Array.isArray(status) ? status : [status];
    if (!want.includes(r.status)) throw new Error(`${what}: expected ${want.join("/")}, got ${r.status} ${r.body?.code ?? ""}`);
    return r.body;
}

async function as(browser: Browser, user: string) {
    const page = await (await browser.newContext()).newPage();
    await signIn(page, user);
    await openSite(page);
    return page;
}

function requireScripted() {
    if (env.SUPERVISOR_E2E_AGENT !== "scripted")
        throw new Error("BLOCKED: this test needs the API and worker started with SUPERVISOR_AGENT_PROVIDER=fixture, SUPERVISOR_AGENT_FIXTURE_SCRIPT=deploy/supervisor/agent-fixture-script.v1.json and SUPERVISOR_TASK_EXECUTION_ENABLED=true, and SUPERVISOR_E2E_AGENT=scripted");
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));
const stamp = (d: Date) => d.toISOString().replace(/\.\d{3}Z$/, "Z");

// A telemetry source this run owns: it numbers its own events and waits for the worker's fold.
class Feed {
    sequence = 0;
    constructor(private owner: Page, readonly id: string) {}

    async send(events: Record<string, unknown>[]) {
        for (const e of events) {
            this.sequence++;
            e.source_sequence = this.sequence;
            e.revision = 1;
        }
        await ok(call(this.owner, "POST", `/operational-sources/${this.id}/batches`, { schema_version: 1, source_id: this.id, source_revision: 2, mapping_revision: 2, events }), 202, "send batch");
        await this.folded();
    }

    // folded waits until the worker has processed every event sent so far.
    async folded() {
        for (let n = 0; n < 60; n++) {
            const live = await ok(call(this.owner, "GET", `/operational-sources/${this.id}/liveness`), 200, "read liveness");
            if ((live.ingest?.processed_sequence ?? 0) >= this.sequence) return;
            await sleep(1000);
        }
        throw new Error(`source ${this.id.slice(0, 8)}: the worker did not fold sequence ${this.sequence} within 60 s`);
    }

    heartbeat() {
        const now = new Date().toISOString();
        return this.send([{ external_key: `heartbeat-${this.sequence + 1}`, kind: "heartbeat", event_at: now, payload: { observed_at: now } }]);
    }
}

// registerFeed creates an event_api source mapped to the line (and order), reviewed by a second
// supervisor, as seedCase does.
async function registerFeed(owner: Page, reviewer: Page, scope: string, mappings: Record<string, unknown>[]) {
    const id = crypto.randomUUID();
    await ok(call(owner, "POST", "/operational-sources", { source_id: id, type: "event_api", authority_scope: scope, enabled: true, access_policy_revision: 1, freshness_seconds: 900, clock_skew_seconds: 30 }, 0), 200, "register source");
    await ok(call(owner, "POST", `/operational-sources/${id}/mappings`, { mappings }, 1), 200, "map source");
    await ok(call(reviewer, "POST", `/operational-sources/${id}/reviews`, { verdict: "approved", reason: "E2E telemetry scope verified" }, 2), 200, "review source");
    return new Feed(owner, id);
}

// shortfallLine builds a fresh line with an approved plan, two READY 15-minute windows that each
// deliver 80 of 100 planned units, a confirmed maintenance assignee and a machine stoppage.
async function shortfallLine(owner: Page, lead: Page, approver: Page, assigneeSubject: string) {
    const run = crypto.randomUUID().slice(0, 8);
    const register = crypto.randomUUID();
    await ok(call(owner, "POST", "/sources", { source_id: register, kind: "manual", title: `E2E asset register ${run}`, text: `E2E line ${run} and its order` }, 0), [200, 201], "register source");
    const line = await ok(call(owner, "POST", "/entities", { entity_id: crypto.randomUUID(), type: "asset", asset_kind: "line", display_name: `E2E line ${run}`, source_id: register, source_revision: 1 }, 0), [200, 201], "create line");
    const order = await ok(call(owner, "POST", "/entities", { entity_id: crypto.randomUUID(), type: "order", display_name: `E2E order ${run}`, source_id: register, source_revision: 1 }, 0), [200, 201], "create order");
    const inventory = await ok(call(owner, "GET", "/knowledge-inventory"), 200, "read inventory");
    const snapshot = await ok(call(owner, "POST", "/process-snapshots", { snapshot_id: crypto.randomUUID(), reason: "E2E approved process cut" }, inventory.revision), [200, 201], "snapshot");

    const end = new Date(Math.floor(Date.now() / 900_000) * 900_000 - 900_000);
    const windows = [new Date(end.getTime() - 1_800_000), new Date(end.getTime() - 900_000), end];
    const scheduleID = crypto.randomUUID(), scheduleText = `E2E order ${run} on E2E line ${run} at one unit per second`;
    const schedule = await ok(call(owner, "POST", "/sources", { source_id: scheduleID, kind: "manual", title: `E2E shift plan ${run}`, text: scheduleText }, 0), [200, 201], "schedule source");
    const span = await ok(call(owner, "POST", `/sources/${scheduleID}/spans`, { revision: schedule.revision, start_char: 0, end_char: scheduleText.length }, schedule.state_revision), [200, 201], "schedule span");
    const plan = {
        order_id: order.entity_id, asset_id: line.entity_id, window_start: stamp(windows[0]), window_end: stamp(windows[2]), plan_revision: 1, unit: "count",
        rate_segments: [{ start: stamp(windows[0]), end: stamp(windows[2]), target_good_units_per_second: "1" }],
        planned_breaks: [0, 1].map((i) => ({ start: stamp(new Date(windows[i].getTime() + 100_000)), end: stamp(windows[i + 1]) })),
    };
    const captured = await ok(call(lead, "POST", "/plan-revisions", { plan, source_span_ids: [span.id] }), 201, "capture plan");
    await ok(call(approver, "POST", `/plan-revisions/${captured.id}/reviews`, { decision: "approve", reason: "E2E: matches the shift plan" }, 0), 200, "approve plan");

    const production = await registerFeed(owner, approver, "production_delta,heartbeat,watermark", [
        { external_type: "asset", external_key: `line-${run}`, entity_id: line.entity_id, entity_revision: line.revision },
        { external_type: "order", external_key: `order-${run}`, entity_id: order.entity_id, entity_revision: order.revision },
    ]);
    const machine = await registerFeed(owner, approver, "machine_state,heartbeat", [
        { external_type: "asset", external_key: `line-${run}`, entity_id: line.entity_id, entity_revision: line.revision },
    ]);
    // The roster and the stoppage come before the contexts: the evaluator can open the case on its
    // next tick, and the investigation that follows needs both.
    await ok(call(approver, "POST", "/maintenance-roster/decisions", { line_id: line.entity_id, assignee_subject: assigneeSubject, decision: "confirm", reason: "E2E line maintenance on call" }, 0), [200, 201], "confirm assignee");
    const now = new Date().toISOString();
    await machine.send([
        { external_key: "stop-1", kind: "machine_state", event_at: stamp(new Date(windows[1].getTime() + 300_000)), asset_external_key: `line-${run}`, payload: { state: "stopped" } },
        { external_key: "heartbeat-0", kind: "heartbeat", event_at: now, payload: { observed_at: now } },
    ]);
    for (const i of [0, 1]) {
        const at = new Date().toISOString();
        await production.send([
            { external_key: `delta-${i}`, kind: "production_delta", event_at: stamp(windows[i + 1]), asset_external_key: `line-${run}`, order_external_key: `order-${run}`,
                payload: { order_id: order.entity_id, asset_id: line.entity_id, interval_start: stamp(windows[i]), interval_end: stamp(windows[i + 1]), good_units: "80", unit: "count" } },
            { external_key: `heartbeat-${i}`, kind: "heartbeat", event_at: at, payload: { observed_at: at } },
        ]);
        await production.send([{ external_key: `watermark-${i}`, kind: "watermark", event_at: new Date().toISOString(), payload: { complete_through: stamp(windows[i + 1]), through_sequence: production.sequence } }]);
        const context = await ok(call(owner, "POST", "/contexts", { order_id: order.entity_id, asset_id: line.entity_id, window_start: stamp(windows[i]), window_end: stamp(windows[i + 1]), process_snapshot_id: snapshot.snapshot_id }), 201, `context window ${i}`);
        expect(context.readiness, `context window ${i} reasons ${JSON.stringify(context.reason_codes ?? context.reasons ?? [])}`).toBe("READY");
    }
    return { lineID: line.entity_id as string, feeds: [production, machine] };
}

// Heartbeats are trusted for at most 120 s (limits.MaxHeartbeatAge), and the investigation
// and the executor both re-check them. keepFresh beats every feed once 30 s have passed.
function freshness(feeds: Feed[]) {
    let last = Date.now();
    return {
        async now() {
            for (const f of feeds) await f.heartbeat();
            last = Date.now();
        },
        async keep() {
            if (Date.now() - last > 30_000) await this.now();
        },
    };
}

async function caseOnLine(page: Page, lineID: string) {
    let cursor = "";
    for (let n = 0; n < 1000; n++) {
        const list = await ok(call(page, "GET", `/cases?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`), 200, "list cases");
        const found = list.items.find((c: { line_id: string }) => c.line_id === lineID);
        if (found) return found;
        if (!list.next_cursor) return undefined;
        cursor = list.next_cursor;
    }
}

async function findAll(page: Page, path: string, match: (item: any) => boolean) {
    const out: any[] = [];
    let cursor = "";
    for (let n = 0; n < 1000; n++) {
        const list = await ok(call(page, "GET", `${path}?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`), 200, `list ${path}`);
        out.push(...list.items.filter(match));
        if (!list.next_cursor) return out;
        cursor = list.next_cursor;
    }
    return out;
}

async function openTask(page: Page, short: string) {
    await section(page, "Tasks");
    const table = page.getByRole("table", { name: "Tasks" });
    await expect(table).toBeVisible();
    const link = table.getByRole("link", { name: `Task ${short}` });
    await expect(link).toBeVisible();
    await link.click();
    await expect(page.getByRole("heading", { name: `Task ${short}` })).toBeVisible();
}

test("@agent-scripted a detected shortfall becomes an approved task the operator acknowledges, starts and completes; a stale session is refused and a supervisor reopens it", async ({ browser }) => {
    requireScripted();
    test.setTimeout(8 * 60_000);
    const owner = await as(browser, "foundation-reviewer");
    const lead = await as(browser, "shift-lead");
    const approver = await as(browser, "shift-approver");
    const operator = await as(browser, "operator-1");
    const assignee = (await operator.evaluate(async () => (await fetch("/v1/me")).json())).subject as string;

    const world = await shortfallLine(owner, lead, approver, assignee);
    const beats = freshness(world.feeds);

    // The worker opens the case, starts it (WIS-636), investigates and requests approval.
    let c: any;
    for (let n = 0; n < 60; n++) {
        await beats.keep();
        c = await caseOnLine(lead, world.lineID);
        if (c?.state === "awaiting_approval") break;
        if (c && ["needs_input", "escalated", "refused", "resolved", "canceled"].includes(c.state))
            throw new Error(`case ${c.id.slice(0, 8)} reached ${c.state} instead of awaiting_approval`);
        await sleep(3000);
    }
    expect(c?.state, "the worker did not bring the case to awaiting_approval within 3 minutes").toBe("awaiting_approval");
    const timeline = await ok(call(lead, "GET", `/cases/${c.id}`), 200, "read case");
    expect(timeline.state).toBe("awaiting_approval");

    const [proposal] = await findAll(lead, "/proposals", (p) => p.case_id === c.id && p.state === "pending");
    expect(proposal, "no pending proposal for the case").toBeTruthy();
    const view = await ok(call(lead, "GET", `/proposals/${proposal.id}`), 200, "read proposal");
    const payload = typeof view.canonical_payload === "string" ? JSON.parse(view.canonical_payload) : view.canonical_payload;
    expect(view.submitted_by?.subject ?? "", "an agent run submits, never a person").not.toBe(assignee);

    // The supervisor approves on the Approvals page, with every source freshly heartbeated: the
    // executor re-checks them at dispatch. The card is found by its link to this case, since a
    // reused site can hold other runs' proposals with the same scripted instructions.
    await beats.now();
    await lead.getByRole("navigation", { name: "Site sections" }).getByRole("link", { name: "Approvals", exact: true }).click();
    const card = lead.locator("article").filter({ has: lead.locator(`a[href$="/cases/${c.id}"]`) });
    await expect(card).toBeVisible();
    await expect(card).toContainText("Proposed inspection task (report only)");
    const decided = lead.waitForResponse((r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith(`/proposals/${proposal.id}/decisions`));
    await card.getByLabel("Reason for your decision").fill("E2E: inspect and report on the line");
    await card.getByRole("button", { name: "Approve", exact: true }).click();
    expect((await decided).status()).toBe(200);
    await expect(lead.getByRole("status").filter({ hasText: "server state approved" })).toBeVisible();
    expect((await ok(call(lead, "GET", `/proposals/${proposal.id}`), 200, "read approved proposal")).state).not.toBe("pending");

    let task: any;
    for (let n = 0; n < 30 && !task; n++) {
        [task] = await findAll(lead, "/tasks", (t) => t.case_id === c.id);
        if (!task) await sleep(2000);
    }
    if (!task) {
        const after = await ok(call(lead, "GET", `/proposals/${proposal.id}`), 200, "reread proposal");
        throw new Error(after.state === "approved"
            ? "BLOCKED: the approval was not consumed within 60 s; start the worker with SUPERVISOR_TASK_EXECUTION_ENABLED=true"
            : `the executor did not create a task; the proposal is ${after.state}`);
    }
    expect(task.state).toBe("assigned");
    expect(task.assignee_subject).toBe(assignee);
    const short = task.id.slice(0, 8);

    // Two sessions of the assignee have the task open; one acknowledges first.
    const stale = await as(browser, "operator-1");
    await openTask(operator, short);
    // The stale session's 10 s task poller would fetch revision 2 on a slow runner and hide
    // Acknowledge before the click: fake its clock before the task page mounts and freeze it once
    // revision 1 is on screen, so only the conflict's own reload can bring revision 2 in.
    await stale.clock.install();
    await openTask(stale, short);
    await stale.clock.pauseAt((await stale.evaluate(() => Date.now())) + 1000);
    // The task shows the instructions the supervisor approved, verbatim.
    const instructions = payload?.body?.instructions;
    expect(typeof instructions === "string" && instructions.length > 0, "the approved payload carries instructions").toBe(true);
    await expect(operator.getByText(instructions, { exact: true })).toBeVisible();
    await expect(operator.getByRole("definition").filter({ hasText: "Inspect and report only" })).toBeVisible();

    await operator.getByRole("button", { name: "Acknowledge" }).click();
    await expect(operator.getByRole("status").filter({ hasText: "You acknowledged the task. The task is now at revision 2." })).toBeVisible();

    // The second session's acknowledge carries revision 1: the server's 412 reloads it, and
    // nothing is resubmitted.
    const refused = stale.waitForResponse((r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith(`/tasks/${task.id}/acknowledge`));
    await stale.getByRole("button", { name: "Acknowledge" }).click();
    expect((await refused).request().headers()["if-match"]).toBe('"1"');
    expect((await refused).status()).toBe(412);
    await expect(stale.getByRole("status").filter({ hasText: "This changed on the server." })).toContainText("Nothing was submitted.");
    await expect(stale.getByRole("button", { name: "Start" })).toBeVisible();
    await expect(stale.getByRole("button", { name: "Acknowledge" })).toHaveCount(0);
    await stale.clock.resume();

    await operator.getByRole("button", { name: "Start" }).click();
    await expect(operator.getByRole("status").filter({ hasText: "Work on the task started." })).toBeVisible();
    await operator.getByLabel("Report (what you inspected and found)").fill("E2E: guard on the infeed found open; reported, nothing adjusted.");
    await operator.getByLabel("Completion note").fill("Inspection done.");
    await operator.getByRole("button", { name: "Complete task" }).click();
    await expect(operator.getByRole("status").filter({ hasText: "Your report was recorded and the task is completed." })).toBeVisible();
    await expect(operator.getByRole("list", { name: "Completion reports" })).toContainText("r1");

    const completed = await ok(call(lead, "GET", `/tasks/${task.id}`), 200, "read completed task");
    expect(completed.state).toBe("completed");

    // A supervisor reopens it for correction; the report stays in the history.
    await openTask(lead, short);
    await lead.getByLabel("Reason").fill("E2E: report the guard's part number too");
    await lead.getByRole("button", { name: "Reopen task" }).click();
    await expect(lead.getByRole("status").filter({ hasText: "The task was reopened." })).toBeVisible();
    await expect(lead.getByRole("list", { name: "Task history" })).toContainText("E2E: report the guard's part number too");
    await expect(lead.getByRole("list", { name: "Completion reports" })).toContainText("r1");

    const reopened = await ok(call(operator, "GET", `/tasks/${task.id}`), 200, "read reopened task");
    expect(reopened.state).not.toBe("completed");
    expect(reopened.revision).toBeGreaterThan(completed.revision);
});
