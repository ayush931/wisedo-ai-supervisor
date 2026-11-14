// Labelled stubbed API responses (WIS-630 Tasks screen): these exercise the real page against fixed
// server replies and check every write the page sends (Idempotency-Key, If-Match, body). They are
// not evidence of backend behaviour: the WIS-277 rules are covered by
// internal/httpapi/tasks_integration_test.go (CASE=WIS-277). A real-backend task spec needs a
// seeded task, which only the executor may create; that is a recorded follow-up.
import { test, expect, type Page, type Route } from "@playwright/test";
import type { TaskPageV1, TaskV1 } from "../../contracts/typescript/oee";

const site = "00000000-0000-4000-8000-000000000611";
const tenant = "00000000-0000-4000-8000-000000000612";
const line = "00000000-0000-4000-8000-000000000621";
const order = "00000000-0000-4000-8000-000000000622";
const asset = "00000000-0000-4000-8000-000000000623";
const caseId = "00000000-0000-4000-8000-000000000631";
const mineA = "a0000000-0000-4000-8000-000000000641"; // assigned to the operator, page 1
const other = "b0000000-0000-4000-8000-000000000642"; // assigned to someone else, page 1
const mineB = "c0000000-0000-4000-8000-000000000643"; // assigned to the operator, page 2
const NOW = new Date("2026-11-03T10:00:00Z");
const at = (minutes: number) => new Date(NOW.getTime() + minutes * 60000).toISOString();
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const executor = { issuer: "supervisor:executor", subject: "approved-action-executor" };
const operator = { issuer: "https://issuer.test", subject: "operator-1" };
const problem = (route: Route, status: number, code: string) =>
    route.fulfill({ status, contentType: "application/problem+json", json: { type: "about:blank", title: code, status, code, correlation_id: "stub" } });

function task(id: string, state: string, revision: number, assignee = operator.subject, extra: Partial<TaskV1> = {}): TaskV1 {
    const events: TaskV1["events"] = [{ revision: 1, from_state: null, to_state: "assigned", event: "create", reason: null, actor: executor, created_at: at(-60) }];
    const walk: [string, string, string][] = [["assigned", "acknowledged", "acknowledge"], ["acknowledged", "in_progress", "start"], ["in_progress", "completed", "complete"]];
    for (let r = 2; r <= revision; r++) {
        const [from, to, event] = walk[r - 2] ?? walk[2];
        events.push({ revision: r, from_state: from, to_state: to, event, reason: null, actor: { ...operator, subject: assignee }, created_at: at(-60 + r) });
    }
    return {
        id, action_id: "00000000-0000-4000-8000-000000000651", case_id: caseId, asset_id: asset, line_id: line, order_id: order,
        assignee_id: "00000000-0000-4000-8000-000000000661", assignee_subject: assignee, template: "inspect_report_only",
        description: "Inspect the infeed belt on Press 01 and report its tension.", due_at: at(240), state, revision,
        created_at: at(-60), acknowledged_at: revision >= 2 ? at(-58) : null, started_at: revision >= 3 ? at(-57) : null, completed_at: null,
        completion_evidence_ids: [], completions: [], outcome: null, events, replayed: false, ...extra,
    };
}
const summary = (t: TaskV1): TaskPageV1["items"][number] => ({ id: t.id, case_id: t.case_id, line_id: t.line_id, assignee_id: t.assignee_id, assignee_subject: t.assignee_subject, state: t.state, revision: t.revision, due_at: t.due_at, created_at: t.created_at });

type Write = { path: string; body: any; headers: Record<string, string> };
type Options = { role?: string; subject?: string; tasks?: TaskV1[]; replies?: Record<string, { status: number; code: string }[]> };

async function stub(page: Page, o: Options = {}) {
    const writes: Write[] = [];
    const reads: Record<string, number> = {};
    const tasks = new Map((o.tasks ?? []).map((t) => [t.id, t]));
    const replies = Object.fromEntries(Object.entries(o.replies ?? {}).map(([k, v]) => [k, [...v]]));
    await page.clock.setFixedTime(NOW);
    await page.route("**/v1/**", async (route) => {
        const req = route.request();
        const url = new URL(req.url());
        const path = url.pathname.replace(`/v1/sites/${site}`, "");
        if (url.pathname === "/v1/me") return route.fulfill({ json: { csrf_token: "stub", subject: o.subject ?? operator.subject, display_name: "Sanjay Patil", sites: [{ id: site, name: "Chakan Plant", role: o.role ?? "operator", tenant_id: tenant, agent_available: false }] } });
        if (req.method() === "GET") reads[path] = (reads[path] ?? 0) + 1;
        if (path === "/members") return route.fulfill({ json: { members: [{ subject: operator.subject, display_name: "Sanjay Patil" }, { subject: "operator-2", display_name: "Meera Joshi" }] } });
        if (path === "/workstations") return route.fulfill({ json: { workstations: [] } });
        const names: Record<string, string> = { [line]: "Press line 1", [order]: "Order PR-77", [asset]: "Press 01" };
        for (const [id, display_name] of Object.entries(names)) if (path === `/entities/${id}`) return route.fulfill({ json: { id, display_name } });
        // Two pages in ID order, so the list must follow next_cursor to find every task.
        if (path === "/tasks") {
            const all = [...tasks.values()].sort((a, b) => a.id.localeCompare(b.id));
            const cursor = url.searchParams.get("cursor");
            const page1 = all.slice(0, 2);
            const body: TaskPageV1 = cursor ? { items: all.slice(2).map(summary), next_cursor: null } : { items: page1.map(summary), next_cursor: all.length > 2 ? page1[1].id : null };
            return route.fulfill({ json: body });
        }
        const m = /^\/tasks\/([^/]+)(?:\/([a-z]+))?$/.exec(path);
        if (m && tasks.has(m[1])) {
            const t = tasks.get(m[1])!;
            if (req.method() === "GET") return route.fulfill({ json: t, headers: { ETag: `"${t.revision}"` } });
            const body = req.postDataJSON();
            writes.push({ path, body, headers: req.headers() });
            const next = replies[m[2]]?.shift();
            if (next) return problem(route, next.status, next.code);
            const to: Record<string, string> = { acknowledge: "acknowledged", start: "in_progress", block: "blocked", cancel: "canceled", reopen: "in_progress", complete: "completed" };
            const done = task(t.id, to[m[2]], t.revision + 1, t.assignee_subject, {
                state: to[m[2]],
                completions: m[2] === "complete" ? [{ epoch: 1, task_revision: t.revision + 1, completion_note: body.completion_note, evidence: body.evidence_ids.map((s: string) => ({ source_id: s, revision: 1 })), actor: operator, created_at: at(0) }] : [],
                outcome: m[2] === "complete" ? { epoch: 1, case_id: caseId, baseline_context_id: null, status: "pending", rule_version: "wis277-outcome-1", completed_at: at(0), windows: [], interpretation: "", observed_at: null } : null,
            });
            tasks.set(t.id, done);
            return route.fulfill({ json: done, headers: { ETag: `"${done.revision}"` } });
        }
        if (path === "/sources" && req.method() === "POST") {
            const body = req.postDataJSON();
            writes.push({ path, body, headers: req.headers() });
            return route.fulfill({ json: { source_id: body.source_id, revision: 1, kind: body.kind, title: body.title, text: body.text, current_revision: 1, state_revision: 1 } });
        }
        return problem(route, 404, "NOT_FOUND");
    });
    return { writes, reads, tasks };
}
const open = (page: Page, id?: string) => page.goto(`/sites/${site}/tasks${id ? `/${id}` : ""}`);

// expectWrite checks what the page sent: a fresh UUID key, If-Match on the task's revision, and
// the same revision in the body.
function expectWrite(w: Write, path: string, revision: number) {
    expect(w.path).toBe(path);
    expect(w.headers["idempotency-key"]).toMatch(uuid);
    expect(w.headers["if-match"]).toBe(`"${revision}"`);
    expect(w.body.expected_revision).toBe(revision);
}

test("stubbed task list follows the cursor; an operator sees the tasks assigned to them, reached from Plan", async ({ page }) => {
    await stub(page, { tasks: [task(mineA, "assigned", 1), task(other, "assigned", 1, "operator-2"), task(mineB, "in_progress", 3)] });
    await page.goto(`/sites/${site}/station`);
    await page.getByRole("navigation", { name: "Site sections" }).getByRole("link", { name: "Plan", exact: true }).click();
    await page.getByRole("navigation", { name: "Plan pages" }).getByRole("link", { name: "Tasks", exact: true }).click();
    const table = page.getByRole("table", { name: "Tasks" });
    await expect(page.getByText("Showing tasks assigned to you.")).toBeVisible();
    // The task on page 2 is shown, and the other person's task on page 1 is not.
    await expect(table.getByRole("row")).toHaveCount(3);
    await expect(table.getByRole("link", { name: `Task ${mineB.slice(0, 8)}` })).toBeVisible();
    await expect(table.getByRole("link", { name: `Task ${other.slice(0, 8)}` })).toHaveCount(0);
    await expect(table.getByRole("row").nth(2)).toContainText("In progress");
    await expect(table).toContainText("Press line 1");
    await expect(page.getByRole("checkbox", { name: "Only tasks assigned to me" })).toHaveCount(0);
});

test("stubbed supervisor list shows every task and can narrow to their own", async ({ page }) => {
    await stub(page, { role: "supervisor", subject: "lead", tasks: [task(mineA, "assigned", 1), task(other, "assigned", 1, "lead"), task(mineB, "completed", 4)] });
    await open(page);
    const table = page.getByRole("table", { name: "Tasks" });
    await expect(table.getByRole("row")).toHaveCount(4);
    await expect(table).toContainText("Sanjay Patil");
    await page.getByRole("checkbox", { name: "Only tasks assigned to me" }).check();
    await expect(table.getByRole("row")).toHaveCount(2);
    await expect(table).toContainText("you");
});

test("stubbed task detail shows the approved action, assignee, history, and keeps outcome apart from completion", async ({ page }) => {
    await stub(page, { tasks: [task(mineA, "acknowledged", 2)] });
    await open(page, mineA);
    await expect(page.getByRole("heading", { name: `Task ${mineA.slice(0, 8)}` })).toBeVisible();
    await expect(page.getByText("Inspect the infeed belt on Press 01 and report its tension.")).toBeVisible();
    const action = page.getByRole("definition").filter({ hasText: "Inspect and report only" });
    await expect(action).toBeVisible();
    const facts = page.locator("dl[aria-label='Approved action']");
    await expect(facts).toContainText("you");
    await expect(facts).toContainText("Press 01");
    await expect(facts).toContainText("Order PR-77");
    const history = page.getByRole("list", { name: "Task history" }).getByRole("listitem");
    await expect(history).toHaveCount(2);
    await expect(history.nth(0)).toContainText("Created as");
    await expect(history.nth(0)).toContainText("Approved-action executor (system)");
    await expect(history.nth(1)).toContainText("acknowledged");
    await expect(page.getByText("Not completed yet.")).toBeVisible();
    const outcome = page.getByRole("region", { name: "Outcome observation" });
    await expect(outcome).toContainText("Observed only after the task is completed.");
    await expect(page.getByRole("button", { name: "Start" })).toBeVisible();
});

test("stubbed acknowledge sends Idempotency-Key and If-Match and shows only the server's new state", async ({ page }) => {
    const { writes } = await stub(page, { tasks: [task(mineA, "assigned", 1)] });
    await open(page, mineA);
    await page.getByRole("button", { name: "Acknowledge" }).click();
    await expect(page.getByRole("status").filter({ hasText: "You acknowledged the task. The task is now at revision 2." })).toBeVisible();
    await expect(page.getByRole("heading", { name: `Task ${mineA.slice(0, 8)}` }).locator("..")).toContainText("Acknowledged");
    expect(writes).toHaveLength(1);
    expectWrite(writes[0], `/tasks/${mineA}/acknowledge`, 1);
    expect(Object.keys(writes[0].body)).toEqual(["expected_revision"]);
});

test("stubbed complete writes the report as manual evidence first, then cites exactly that source", async ({ page }) => {
    const { writes } = await stub(page, { tasks: [task(mineA, "in_progress", 3)] });
    await open(page, mineA);
    await page.getByLabel("Report (what you inspected and found)").fill("Belt tension low at 42 N; reported to maintenance, nothing adjusted.");
    await page.getByLabel("Completion note").fill("Inspection done.");
    await page.getByRole("button", { name: "Complete task" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Your report was recorded and the task is completed." })).toBeVisible();
    expect(writes.map((w) => w.path)).toEqual(["/sources", `/tasks/${mineA}/complete`]);
    const [source, complete] = writes;
    expect(source.body.kind).toBe("manual");
    expect(source.body.source_id).toMatch(uuid);
    expect(source.body.text).toBe("Belt tension low at 42 N; reported to maintenance, nothing adjusted.");
    expect(source.headers["if-match"]).toBe('"0"');
    expect(source.headers["idempotency-key"]).toMatch(uuid);
    expectWrite(complete, `/tasks/${mineA}/complete`, 3);
    expect(complete.body.evidence_ids).toEqual([source.body.source_id]);
    expect(complete.body.completion_note).toBe("Inspection done.");
    expect(complete.headers["idempotency-key"]).not.toBe(source.headers["idempotency-key"]);
    // Completion is a report; the outcome is shown separately and is not recovery.
    await expect(page.getByRole("list", { name: "Completion reports" })).toContainText(`source ${source.body.source_id.slice(0, 8)} r1`);
    const outcome = page.getByRole("region", { name: "Outcome observation" });
    await expect(outcome).toContainText("Waiting for windows after completion");
    await expect(page.getByRole("main")).not.toContainText(/resolved|recovered/i);
});

test("stubbed stale revision: 412 reloads the task and resubmits nothing", async ({ page }) => {
    const { writes, reads, tasks } = await stub(page, { tasks: [task(mineA, "assigned", 1)], replies: { acknowledge: [{ status: 412, code: "REVISION_CONFLICT" }] } });
    await open(page, mineA);
    await expect(page.getByRole("button", { name: "Acknowledge" })).toBeVisible();
    const before = reads[`/tasks/${mineA}`];
    // Someone else acknowledged it meanwhile; the reload shows that revision.
    tasks.set(mineA, task(mineA, "acknowledged", 2));
    await page.getByRole("button", { name: "Acknowledge" }).click();
    await expect(page.getByRole("status").filter({ hasText: "This changed on the server." })).toContainText("Nothing was submitted.");
    await expect.poll(() => reads[`/tasks/${mineA}`]).toBeGreaterThan(before);
    await expect(page.getByRole("button", { name: "Start" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Acknowledge" })).toHaveCount(0);
    expect(writes).toHaveLength(1);
});

test("stubbed 412 on complete keeps the saved report: the retry cites it without writing it again", async ({ page }) => {
    const { writes, tasks } = await stub(page, { tasks: [task(mineA, "in_progress", 3)], replies: { complete: [{ status: 412, code: "REVISION_CONFLICT" }] } });
    await open(page, mineA);
    await page.getByLabel("Report (what you inspected and found)").fill("Belt tension low.");
    await page.getByLabel("Completion note").fill("Done.");
    // Meanwhile the task moved to revision 4 (still in progress); the reload after the 412 shows it.
    tasks.set(mineA, task(mineA, "in_progress", 4));
    await page.getByRole("button", { name: "Complete task" }).click();
    await expect(page.getByRole("status").filter({ hasText: "but the task was not completed" }).first()).toBeVisible();
    expect(writes.map((w) => w.path)).toEqual(["/sources", `/tasks/${mineA}/complete`]);
    await expect(page.getByText("revision 4", { exact: false }).first()).toBeVisible();
    await expect(page.getByLabel("Report (what you inspected and found)")).toHaveValue("Belt tension low.");
    await page.getByRole("button", { name: "Complete task" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Your report was recorded and the task is completed." })).toBeVisible();
    expect(writes.map((w) => w.path)).toEqual(["/sources", `/tasks/${mineA}/complete`, `/tasks/${mineA}/complete`]);
    expectWrite(writes[2], `/tasks/${mineA}/complete`, 4);
    expect(writes[2].body.evidence_ids).toEqual([writes[0].body.source_id]);
});

// Review of PR #227: a completion whose answer is lost (connection reset, or a proxy 502 after the
// server committed) has an unknown outcome. The page must not say the task was not completed, and the
// retry must send the same key and body so the server replays instead of completing twice.
for (const lost of ["connection reset", "502"] as const) {
    test(`stubbed complete whose answer is lost (${lost}) says the outcome is unknown and retries with the same key`, async ({ page }) => {
        const { writes } = await stub(page, { tasks: [task(mineA, "in_progress", 3)], replies: lost === "502" ? { complete: [{ status: 502, code: "BAD_GATEWAY" }] } : {} });
        if (lost === "connection reset") {
            let first = true;
            await page.route(`**/v1/sites/${site}/tasks/${mineA}/complete`, async (route) => {
                if (!first) return route.fallback();
                first = false;
                writes.push({ path: `/tasks/${mineA}/complete`, body: route.request().postDataJSON(), headers: route.request().headers() });
                return route.abort("connectionreset");
            });
        }
        await open(page, mineA);
        await page.getByLabel("Report (what you inspected and found)").fill("Belt tension low.");
        await page.getByLabel("Completion note").fill("Done.");
        await page.getByRole("button", { name: "Complete task" }).click();
        await expect(page.getByRole("status").filter({ hasText: "Whether the task was completed is not known" })).toBeVisible();
        await expect(page.getByText("the task was not completed")).toHaveCount(0);
        await page.getByRole("button", { name: "Complete task" }).click();
        await expect(page.getByRole("status").filter({ hasText: "Your report was recorded and the task is completed." })).toBeVisible();
        expect(writes.map((w) => w.path)).toEqual(["/sources", `/tasks/${mineA}/complete`, `/tasks/${mineA}/complete`]);
        expect(writes[2].headers["idempotency-key"]).toBe(writes[1].headers["idempotency-key"]);
        expect(writes[2].body).toEqual(writes[1].body);
    });
}

test("stubbed forbidden role: a viewer gets no task controls", async ({ page }) => {
    await stub(page, { role: "viewer", subject: "read-only-viewer", tasks: [task(mineA, "assigned", 1)] });
    await open(page, mineA);
    await expect(page.getByText("Only the assignee or a site supervisor can change this task.")).toBeVisible();
    for (const name of ["Acknowledge", "Start", "Complete task", "Block task", "Cancel task", "Reopen task"]) await expect(page.getByRole("button", { name, exact: true })).toHaveCount(0);
    await expect(page.getByText("Inspect the infeed belt on Press 01 and report its tension.")).toBeVisible();
});

test("stubbed server 403 TASK_ROLE_REQUIRED after the click changes nothing on screen", async ({ page }) => {
    // The page offers the action from the role it knows; the server, which decides, refuses it.
    const { writes } = await stub(page, { tasks: [task(mineA, "assigned", 1)], replies: { acknowledge: [{ status: 403, code: "TASK_ROLE_REQUIRED" }] } });
    await open(page, mineA);
    await page.getByRole("button", { name: "Acknowledge" }).click();
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("Only the person assigned to this task");
    await expect(page.getByRole("heading", { name: `Task ${mineA.slice(0, 8)}` }).locator("..")).toContainText("Assigned");
    expect(writes).toHaveLength(1);
});

test("stubbed outcome: observed improvement is labelled an observation; an unknown window value is not zero", async ({ page }) => {
    const done = task(mineA, "completed", 4, operator.subject, {
        completions: [{ epoch: 1, task_revision: 4, completion_note: "Done.", evidence: [{ source_id: "d0000000-0000-4000-8000-000000000671", revision: 1 }], actor: operator, created_at: at(-30) }],
        outcome: {
            epoch: 1, case_id: caseId, baseline_context_id: null, status: "observed_improvement", rule_version: "wis277-outcome-1", completed_at: at(-30), observed_at: at(5),
            interpretation: "Threshold recovery: in both windows after completion the deficit was at most 5% of expected. This is an observation, not proof that the task caused it.",
            windows: [
                { start: at(-15), end: at(0), readiness: "READY", reason_codes: [], expected_good_units_exact: "75", actual_good_units_exact: "74", deficit_good_units_exact: "1", unit: "count", plan_revision_id: null, context_digest: "e".repeat(64), record_ids: [] },
                { start: at(0), end: at(15), readiness: "NOT_READY", reason_codes: ["NO_HEARTBEAT"], expected_good_units_exact: "75", actual_good_units_exact: null, deficit_good_units_exact: null, unit: "count", plan_revision_id: null, context_digest: null, record_ids: [] },
            ],
        },
    });
    await stub(page, { role: "supervisor", subject: "lead", tasks: [done] });
    await open(page, mineA);
    const outcome = page.getByRole("region", { name: "Outcome observation" });
    await expect(outcome).toContainText("Recovery observed after completion (not proof the task caused it)");
    await expect(outcome).toContainText("This is an observation, not proof that the task caused it.");
    const rows = outcome.getByRole("table", { name: "Outcome windows" }).getByRole("row");
    await expect(rows.nth(1)).toContainText("74 count");
    await expect(rows.nth(2).getByLabel("Not reported yet")).toHaveCount(2);
    await expect(rows.nth(2)).not.toContainText("0 count");
    // A supervisor may reopen a completed task for correction, with a reason.
    await expect(page.getByRole("button", { name: "Reopen task" })).toBeDisabled();
});

test("stubbed operator opening another person's task gets no assignee actions", async ({ page }) => {
    // The API lets any site member read a task; only the assignee may acknowledge, start or complete.
    await stub(page, { tasks: [task(other, "assigned", 1, "operator-2")] });
    await open(page, other);
    await expect(page.locator("dl[aria-label='Approved action']")).toContainText("Meera Joshi");
    await expect(page.getByText("Only the assignee or a site supervisor can change this task.")).toBeVisible();
    await expect(page.getByRole("button", { name: "Acknowledge", exact: true })).toHaveCount(0);
});
