import { expect, test, type Browser, type Page } from "@playwright/test";
import { appRegions } from "./regions";
import { openSite, section, signIn } from "./session";

// WIS-271 slice 5: the interview workspace against the real backend. Demo-seed users:
// foundation-reviewer is the owner, operator-1 an operator, read-only-viewer a viewer.
//
// SUPERVISOR_E2E_AGENT states how the API under test was started (deploy/supervisor/README.md):
//   @agent-off     - SUPERVISOR_AGENT_PROVIDER unset: GET /v1/me reports AGENT_NOT_CONFIGURED, so both
//                    agent buttons are disabled with that reason before any click. The server's own
//                    503 AGENT_UNAVAILABLE refusal is proven by the "stubbed /v1/me" test.
//                    This is what CI runs.
//   @agent-fixture - SUPERVISOR_AGENT_PROVIDER=fixture: the server's fixture adapter has no script,
//                    so every run is refused SPECIALIST_FAILED and the UI must show that reason.
//   @agent-scripted - fixture provider with agent-fixture-script.v1.json: a real worker run creates
//                     a cited, human-reviewed line draft.
// A tagged test against the other mode is BLOCKED, never skipped (rule 09); select one tag with
// --grep-invert. The stubbed draft hand-off below remains UI-only coverage.

const env = (globalThis as typeof globalThis & { process?: { env?: Record<string, string | undefined> } }).process?.env ?? {};
const stubbedSiteID = "00000000-0000-4000-8000-000000000711";
const stubbedInterviewID = "11111111-0000-4000-8000-000000000712";
const secondStubbedInterviewID = "22222222-0000-4000-8000-000000000713";

const stubbedMe = {
    csrf_token: "test-csrf",
    display_name: "Stubbed Owner",
    subject: "owner-subject",
    sites: [{ id: stubbedSiteID, name: "Stubbed Site", role: "owner", tenant_id: "00000000-0000-4000-8000-000000000714", agent_available: false }],
};

const stubbedSession = (sessionID: string) => ({
    created_at: "2026-10-28T00:00:00Z",
    participant_actor_id: "owner-subject",
    revision: 1,
    role: "owner",
    session_id: sessionID,
    status: "active",
    updated_at: "2026-10-28T00:00:00Z",
});

const stubbedQuestion = (questionID: string, prompt: string) => ({
    answer_state: "unanswered",
    prompt,
    prompt_origin: "template",
    question_id: questionID,
    reason: "missing",
    requirement_id: questionID,
    revision: 1,
    role: "owner",
    source_span_ids: [],
});

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

async function ok(reply: Promise<Reply>, status: number, what: string) {
    const r = await reply;
    if (r.status !== status) throw new Error(`${what}: expected ${status}, got ${r.status} ${r.body?.code ?? ""}`);
    return r.body;
}

// processInventory registers a fresh area/process discovery inventory (WIS-265) through the public
// API. Each run makes its own, so the shared site may hold several inventories.
async function processInventory(page: Page) {
    const sourceID = crypto.randomUUID(), areaID = crypto.randomUUID(), processID = crypto.randomUUID();
    const inventoryID = crypto.randomUUID();
    const processDisplayName = `E2E process ${processID.slice(0, 6)}`;
    const text = "Synthetic area and process inventory";
    const source = await ok(call(page, "POST", "/sources", { source_id: sourceID, kind: "manual", title: "E2E interview scope", text }, 0), 200, "create source");
    await ok(call(page, "POST", `/sources/${sourceID}/spans`, { revision: source.revision, start_char: 0, end_char: text.length }, source.state_revision), 200, "create inventory evidence span");
    for (const [id, type, name] of [[areaID, "area", "E2E area"], [processID, "process", "E2E process"]])
        await ok(call(page, "POST", "/entities", { entity_id: id, type, display_name: type === "process" ? processDisplayName : `${name} ${processID.slice(0, 6)}`, source_id: sourceID, source_revision: 1 }, 0), 200, `create ${type}`);
    await ok(call(page, "POST", "/discovery-inventories", {
        inventory_id: inventoryID, area_id: areaID, area_revision: 1, process_id: processID, process_revision: 1,
        capability: "shortfall_investigation", reason: "E2E interview inventory",
    }, 0), 200, "register inventory");
    return { inventoryID, areaID, processID, processDisplayName };
}

// answerLeftovers answers this user's open questions from earlier runs: the server caps a
// participant at 3 unanswered questions across sessions, and the specs share one site.
async function answerLeftovers(page: Page) {
    const sessions = await ok(call(page, "GET", "/interviews?limit=100"), 200, "list interviews");
    for (const s of sessions.sessions.filter((x: { status: string }) => x.status === "active")) {
        const page1 = await ok(call(page, "GET", `/interviews/${s.session_id}/questions?limit=100`), 200, "list questions");
        for (const q of page1.questions.filter((x: { answer_state: string }) => x.answer_state === "unanswered"))
            await ok(call(page, "POST", `/interviews/${s.session_id}/answers`, { question_id: q.question_id, question_revision: q.revision, text: "Earlier e2e run", client_answer_id: crypto.randomUUID() }), 201, "answer leftover");
    }
}

// stubAgentAvailable stubs only GET /v1/me so every site reports agent_available=true; everything else
// stays real. Install it before sign-in: the app reads /v1/me once per load. Against an agent-off
// server it lets UI tests reach the agent buttons, and the server still refuses any real request.
async function stubAgentAvailable(page: Page) {
    await page.route("**/v1/me", async (route) => {
        const response = await route.fetch();
        // Before sign-in /v1/me answers 401 with a problem body; pass anything but a session through untouched.
        if (response.status() !== 200) return route.fulfill({ response });
        const me = await response.json();
        for (const site of me.sites) {
            site.agent_available = true;
            delete site.agent_unavailable_reason_code;
        }
        await route.fulfill({ response, json: me });
    });
}

async function as(browser: Browser, user: string, agentAvailable = false) {
    const page = await (await browser.newContext()).newPage();
    if (agentAvailable) await stubAgentAvailable(page);
    await signIn(page, user);
    await openSite(page);
    return page;
}

// An inventory option reads "<process> — <checklist>" (WIS-617); processInventory registers a
// shortfall checklist.
async function startInterview(page: Page, processDisplayName: string, checklist = "Shortfall investigation") {
    const option = `${processDisplayName} — ${checklist}`;
    await section(page, "Interviews");
    await expect(page.getByRole("heading", { name: "Interviews", exact: true })).toBeVisible();
    const picker = page.getByLabel("Process inventory");
    await expect(picker).toBeVisible();
    while (await picker.getByRole("option", { name: option, exact: true }).count() === 0) {
        const loadMore = page.getByRole("button", { name: "Load more process inventories" });
        await expect(loadMore).toBeVisible();
        await loadMore.click();
    }
    await picker.selectOption({ label: option });
    await page.getByRole("button", { name: "Start interview" }).click();
    await expect(page.getByRole("heading", { name: /^Interview [0-9a-f]{8}$/ })).toBeVisible();
    // The loading state already shows that heading; wait until the session and questions loaded.
    await expect(page.getByRole("heading", { name: /^Open questions/ })).toBeVisible();
}

async function answerFirstQuestion(page: Page) {
    const sessionID = page.url().split("/").pop()!;
    const saved = page.waitForResponse((response) => response.request().method() === "POST" && new URL(response.url()).pathname.endsWith(`/interviews/${sessionID}/answers`));
    const first = page.locator(".interview-question").first();
    await first.getByLabel("Your answer").focus();
    await page.keyboard.type("We run two shifts; the press line is the bottleneck.");
    await first.getByRole("button", { name: "Save answer" }).focus();
    await page.keyboard.press("Enter");
    await expect(page.getByText(/Answer saved as evidence revision \d+/)).toBeVisible();
    await expect(page.getByText(/^Answered or dismissed \(1\)$/)).toBeVisible();
    const answer = await (await saved).json();
    expect(answer.source_id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i);
    return answer.source_id as string;
}

// requireAgent fails BLOCKED (never skips) when the API under test was started differently.
function requireAgent(mode: "off" | "fixture" | "scripted") {
    if (env.SUPERVISOR_E2E_AGENT !== mode)
        throw new Error(`BLOCKED: this test needs the API and worker started ${mode === "off" ? "without SUPERVISOR_AGENT_PROVIDER" : mode === "fixture" ? "with SUPERVISOR_AGENT_PROVIDER=fixture and no fixture script" : "with SUPERVISOR_AGENT_PROVIDER=fixture and SUPERVISOR_AGENT_FIXTURE_SCRIPT=deploy/supervisor/agent-fixture-script.v1.json"} and SUPERVISOR_E2E_AGENT=${mode}`);
}

// answeredInterview starts an owner interview and answers one template question by keyboard.
async function answeredInterview(browser: Browser) {
    const page = await as(browser, "foundation-reviewer");
    const inventory = await processInventory(page);
    await answerLeftovers(page);
    await startInterview(page, inventory.processDisplayName);

    // Template questions for the owner role; prompts are plain text with an origin label.
    const first = page.locator(".interview-question").first();
    await expect(first).toBeVisible();
    await expect(first.getByTitle("Deterministic discovery template")).toContainText("Template");
    await expect(page.getByText("AI-worded")).toHaveCount(0);

    await answerFirstQuestion(page);
    return page;
}

test("@agent-off owner answers by keyboard; with the agent turned off both agent buttons show the disabled state", async ({ browser }) => {
    requireAgent("off");
    const page = await answeredInterview(browser);
    await expect(page.getByText(/Agent support is not configured/)).toBeVisible();
    await expect(page.getByRole("button", { name: "Ask agent for follow-up questions" })).toBeDisabled();
    await expect(page.getByRole("button", { name: "Draft configuration" })).toBeDisabled();
    // A disabled agent is a state, not a failure toast.
    await expect(appRegions(page, "alert")).toHaveCount(0);
});

// A /v1/me read before a restart can still say the agent is available; the server's refusal must
// then reach the UI. Only GET /v1/me is stubbed; the 503 comes from the real agent-off backend.
test("@agent-off stubbed /v1/me: a stale available flag still shows the server's AGENT_UNAVAILABLE refusal after a click (UI behaviour)", async ({ browser }) => {
    requireAgent("off");
    const page = await as(browser, "foundation-reviewer", true);
    const inventory = await processInventory(page);
    await answerLeftovers(page);
    await startInterview(page, inventory.processDisplayName);

    const followUp = page.getByRole("button", { name: "Ask agent for follow-up questions" });
    await expect(followUp).toBeEnabled();
    const refused = page.waitForResponse((r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/agent-turns"));
    await followUp.click();
    const reply = await refused;
    expect(reply.status()).toBe(503);
    expect((await reply.json()).code).toBe("AGENT_UNAVAILABLE");
    await expect(page.getByText(/Agent support is currently unavailable/)).toBeVisible();
    await expect(followUp).toBeDisabled();
    await expect(page.getByRole("button", { name: "Draft configuration" })).toBeDisabled();
    await expect(appRegions(page, "alert")).toHaveCount(0);
});

test("stubbed inventory list: one process inventory is auto-selected and starts by display name", async ({ browser }) => {
    const page = await as(browser, "foundation-reviewer");
    const inventory = await processInventory(page);
    await answerLeftovers(page);
    await page.route("**/discovery-inventories**", async (route) => {
        const request = route.request();
        const url = new URL(request.url());
        if (request.method() !== "GET" || url.searchParams.has("cursor")) return route.continue();
        return route.fulfill({
            status: 200,
            contentType: "application/json",
            body: JSON.stringify({ inventories: [{
                inventory_id: inventory.inventoryID,
                process_id: inventory.processID,
                process_display_name: inventory.processDisplayName,
                capability: "shortfall_investigation",
                current_revision: 1,
                created_at: "2026-10-26T00:00:00Z",
                updated_at: "2026-10-26T00:00:00Z",
            }] }),
        });
    });

    await section(page, "Interviews");
    const picker = page.getByLabel("Process inventory");
    await expect(picker).toHaveValue(inventory.inventoryID);
    await expect(picker.getByRole("option", { name: `${inventory.processDisplayName} — Shortfall investigation`, exact: true })).toHaveCount(1);
    await page.getByRole("button", { name: "Start interview" }).click();
    await expect(page.getByRole("heading", { name: /^Interview [0-9a-f]{8}$/ })).toBeVisible();
    await expect(page.getByRole("heading", { name: /^Open questions/ })).toBeVisible();
});

test("stubbed inventory list: empty inventory list explains how to get process-specific interview questions", async ({ browser }) => {
    const page = await as(browser, "foundation-reviewer");
    await page.route("**/discovery-inventories**", async (route) => {
        const request = route.request();
        if (request.method() !== "GET") return route.continue();
        return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ inventories: [] }) });
    });
    await section(page, "Interviews");
    await expect(page.getByText(/No process inventories are registered at this site/)).toBeVisible();
    await expect(page.getByText(/contact a site owner to register an area and process as a discovery inventory/)).toBeVisible();
    await expect(page.getByLabel("Process inventory")).toHaveCount(0);
});

test("stubbed inventory list: picker follows its cursor and disambiguates equal process names by process ID", async ({ browser }) => {
    const page = await as(browser, "foundation-reviewer");
    const firstID = crypto.randomUUID(), secondID = crypto.randomUUID();
    const firstProcess = crypto.randomUUID(), secondProcess = crypto.randomUUID();
    const cursors: string[] = [];
    const item = (inventoryID: string, processID: string) => ({
        inventory_id: inventoryID,
        process_id: processID,
        process_display_name: "Assembly process",
        capability: "shortfall_investigation",
        current_revision: 1,
        created_at: "2026-10-26T00:00:00Z",
        updated_at: "2026-10-26T00:00:00Z",
    });
    await page.route("**/discovery-inventories**", async (route) => {
        const request = route.request();
        const url = new URL(request.url());
        if (request.method() !== "GET") return route.continue();
        const cursor = url.searchParams.get("cursor") ?? "";
        cursors.push(cursor);
        return route.fulfill({
            status: 200,
            contentType: "application/json",
            body: JSON.stringify(cursor ? { inventories: [item(secondID, secondProcess)] } : { inventories: [item(firstID, firstProcess)], next_cursor: firstID }),
        });
    });

    await section(page, "Interviews");
    const picker = page.getByLabel("Process inventory");
    await expect(picker.getByRole("option", { name: "Assembly process — Shortfall investigation", exact: true })).toHaveCount(1);
    await picker.selectOption({ label: "Assembly process — Shortfall investigation" });
    await page.getByRole("button", { name: "Load more process inventories" }).click();
    await expect(picker.getByRole("option", { name: `Assembly process · ${firstProcess.slice(0, 8)} — Shortfall investigation`, exact: true })).toHaveCount(1);
    await expect(picker.getByRole("option", { name: `Assembly process · ${secondProcess.slice(0, 8)} — Shortfall investigation`, exact: true })).toHaveCount(1);
    // The selection made on the first page survives loading the next one.
    await expect(picker).toHaveValue(firstID);
    expect(cursors).toEqual(["", firstID]);
});

// Real backend (WIS-617): a process with its shortfall and its value-stream checklists offers both,
// each named by its checklist, and the interview starts on the one chosen.
test("a process with two checklists offers both and starts the chosen one", async ({ browser }) => {
    const page = await as(browser, "foundation-reviewer");
    const inventory = await processInventory(page);
    const streamID = crypto.randomUUID();
    await ok(call(page, "POST", "/discovery-inventories", {
        inventory_id: streamID, area_id: inventory.areaID, area_revision: 1, process_id: inventory.processID, process_revision: 1,
        capability: "process_understanding", reason: "E2E value-stream checklist for the same process",
    }, 0), 200, "register value-stream inventory");
    await answerLeftovers(page);

    await section(page, "Interviews");
    const picker = page.getByLabel("Process inventory");
    await expect(picker).toBeVisible();
    const loadMore = page.getByRole("button", { name: "Load more process inventories" });
    while (await loadMore.isVisible()) {
        await loadMore.click();
        await expect(page.getByText("Loading more process inventories…")).toHaveCount(0);
    }
    await expect(picker.getByRole("option", { name: `${inventory.processDisplayName} — Shortfall investigation`, exact: true })).toHaveCount(1);
    await expect(picker.getByRole("option", { name: `${inventory.processDisplayName} — Value stream`, exact: true })).toHaveCount(1);
    await picker.selectOption({ label: `${inventory.processDisplayName} — Value stream` });
    const created = page.waitForResponse((r) => r.request().method() === "POST" && /\/interviews$/.test(new URL(r.url()).pathname));
    await page.getByRole("button", { name: "Start interview" }).click();
    const response = await created;
    expect(response.status()).toBe(201);
    expect(JSON.parse(response.request().postData() ?? "{}")).toMatchObject({ process_id: inventory.processID, inventory_id: streamID });
    await expect(page.getByRole("heading", { name: /^Interview [0-9a-f]{8}$/ })).toBeVisible();
});

test("@agent-fixture follow-up and draft runs show the server's refusal reason (unscripted fixture provider)", async ({ browser }) => {
    requireAgent("fixture");
    const page = await answeredInterview(browser);
    await expect(page.getByRole("button", { name: "Ask agent for follow-up questions" })).toBeEnabled();
    await expect(page.getByRole("button", { name: "Draft configuration" })).toBeEnabled();
    await page.getByRole("button", { name: "Ask agent for follow-up questions" }).click();
    const turn = page.getByRole("status").filter({ hasText: /^Follow-up run [0-9a-f]{8}/ });
    await expect(turn).toContainText("Refused", { timeout: 45000 });
    await expect(turn).toContainText("Model call failed");
    await expect(turn).toContainText("The model call failed, so nothing was proposed or saved.");
    await expect(page.getByText("No new questions were added.")).toBeVisible();
    await expect(page.getByText("AI-worded")).toHaveCount(0);

    await page.getByRole("checkbox", { name: "Items" }).check();
    await page.getByRole("button", { name: "Draft configuration" }).click();
    const run = page.getByRole("status").filter({ hasText: /^Draft run [0-9a-f]{8}/ });
    await expect(run).toContainText("Refused", { timeout: 45000 });
    await expect(run).toContainText("The model call failed, so nothing was proposed or saved.");
    await expect(page.getByText("No drafts were created.")).toBeVisible();
    await expect(page.getByRole("list", { name: "Drafts created by this run" })).toHaveCount(0);

    // The run record survives a reload: the outcome is read back from the server, not memory.
    await page.reload();
    await expect(page.getByRole("status").filter({ hasText: /^Draft run [0-9a-f]{8}/ })).toContainText("Refused", { timeout: 15000 });
});

test("@agent-scripted worker creates its cited line draft for human review", async ({ browser }) => {
    test.setTimeout(120_000);
    requireAgent("scripted");
    const page = await answeredInterview(browser);
    await expect(page.getByRole("button", { name: "Ask agent for follow-up questions" })).toBeEnabled();
    await expect(page.getByRole("button", { name: "Draft configuration" })).toBeEnabled();
    const authorityWrites: string[] = [];
    page.on("request", (request) => {
        const path = new URL(request.url()).pathname;
        if (request.method() === "POST" && /\/(?:promotions|approvals|dispatch|tasks)(?:\/|$)/.test(path)) authorityWrites.push(path);
    });

    const startRequest = page.waitForResponse((response) => response.request().method() === "POST" && new URL(response.url()).pathname.endsWith("/configuration-draft-requests"));
    await page.getByRole("button", { name: "Draft configuration" }).click();
    const startResponse = await startRequest;
    expect(startResponse.status()).toBe(202);
    const started = await startResponse.json();
    const runID = started.run_id as string;
    expect(runID).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i);
    const run = page.getByRole("status").filter({ hasText: /^Draft run [0-9a-f]{8}/ });
    await expect(run).toContainText("Finished", { timeout: 45000 });

    const result = await ok(call(page, "GET", `/configuration-draft-requests/${runID}`), 200, "read scripted draft run");
    expect(result).toMatchObject({ state: "completed", outcome_status: "proposed", draft_ids: [expect.any(String)] });
    expect(result.draft_ids).toHaveLength(1);
    const draftID = result.draft_ids[0] as string;
    expect(draftID).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i);

    const draft = await ok(call(page, "GET", `/configuration-drafts/${draftID}`), 200, "read scripted line draft");
    expect(draft).toMatchObject({ draft_id: draftID, kind: "line", author_kind: "agent", status: "draft", run_id: runID });
    const revision = draft.revisions[draft.current_revision - 1];
    expect(revision.body).toEqual({ schema_version: 1, display_name: "Fixture line", active: true });
    expect(revision.citations.map((citation: { path: string }) => citation.path).sort()).toEqual(["/active", "/display_name"]);

    await page.getByRole("list", { name: "Drafts created by this run" }).getByRole("button", { name: `Open draft ${draftID.slice(0, 8)} in Drafts` }).click();
    await expect(page.getByRole("heading", { name: `Line draft ${draftID.slice(0, 8)}` })).toBeVisible();
    await expect(page.getByText("Agent draft — not live configuration")).toBeVisible();
    await expect(page.getByRole("textbox", { name: "Display name" })).toHaveValue("Fixture line");
    await expect(page.getByRole("checkbox", { name: "Active" })).toBeChecked();
    const promote = page.getByRole("button", { name: /Promote r1/ });
    await expect(promote).toBeVisible();
    expect(authorityWrites).toEqual([]);
});

// STUBBED RUN RECORD: this covers only the UI hand-off from a finished draft run to Master data ->
// Drafts. The draft is a real human draft; only the draft-run GET is replaced. It is not evidence
// of agent behaviour (rule 09).
test("stubbed draft-run record: Open draft opens that real draft in Drafts once (UI hand-off only, not agent evidence)", async ({ browser }) => {
    const page = await as(browser, "foundation-reviewer");
    const inventory = await processInventory(page);
    await answerLeftovers(page);
    await startInterview(page, inventory.processDisplayName);
    const sessionID = page.url().split("/").pop()!;

    const draft = await ok(call(page, "POST", "/configuration-drafts", {
        kind: "line", body: { schema_version: 1, display_name: `E2E hand-off ${sessionID.slice(0, 6)}`, active: true }, field_spans: [],
    }), 201, "create human draft");
    const draftID: string = draft.draft_id;
    const runID = crypto.randomUUID();
    await page.route(`**/configuration-draft-requests/${runID}`, (route) => route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ run_id: runID, session_id: sessionID, state: "completed", outcome_status: "proposed", outcome_reasons: [], draft_ids: [draftID] }),
    }));
    // The page resumes the last run per session from sessionStorage; re-enter the interview in-app.
    await page.evaluate(([key, value]) => sessionStorage.setItem(key, value), [`supervisor.interview.drafts.${sessionID}`, runID]);
    await section(page, "Interviews");
    await page.getByRole("link", { name: `Interview ${sessionID.slice(0, 8)}` }).click();

    await expect(page.getByRole("status").filter({ hasText: /^Draft run [0-9a-f]{8}/ })).toContainText("Finished");
    await page.getByRole("list", { name: "Drafts created by this run" }).getByRole("button", { name: `Open draft ${draftID.slice(0, 8)} in Drafts` }).click();
    await expect(page.getByRole("heading", { name: new RegExp(`draft ${draftID.slice(0, 8)}`) })).toBeVisible();

    // The hand-off is one-shot: coming back to Master data shows the catalog, not that draft.
    await page.getByRole("navigation", { name: "Site sections" }).getByRole("link", { name: "Today" }).click();
    await section(page, "Master data");
    await expect(page.getByRole("heading", { name: "Master data", exact: true })).toBeVisible();
    await expect(page.getByRole("heading", { name: new RegExp(`draft ${draftID.slice(0, 8)}`) })).toHaveCount(0);
});

test("operators answer but cannot ask the agent; viewers cannot start an interview", async ({ browser }) => {
    const owner = await as(browser, "foundation-reviewer");
    const inventory = await processInventory(owner);

    const operator = await as(browser, "operator-1");
    await answerLeftovers(operator);
    await startInterview(operator, inventory.processDisplayName);
    await expect(operator.getByText("Asking the agent for questions or drafts needs the owner or supervisor role at this site.")).toBeVisible();
    await expect(operator.getByRole("button", { name: "Ask agent for follow-up questions" })).toHaveCount(0);
    await expect(operator.getByRole("button", { name: "Draft configuration" })).toHaveCount(0);

    const viewer = await as(browser, "read-only-viewer");
    const viewerInventoryReads: string[] = [];
    viewer.on("request", (request) => {
        if (new URL(request.url()).pathname.endsWith("/discovery-inventories")) viewerInventoryReads.push(request.url());
    });
    await section(viewer, "Interviews");
    await expect(viewer.getByText(/Viewers cannot be interviewed/)).toBeVisible();
    await expect(viewer.getByLabel("Process inventory")).toHaveCount(0);
    await expect(viewer.getByText(/Loading process inventories/)).toHaveCount(0);
    expect(viewerInventoryReads).toEqual([]);
    await expect(viewer.getByRole("button", { name: "Start interview" })).toHaveCount(0);
});

// UI-behaviour regression tests (review of 4ef2510). Every server reply named below is STUBBED with
// page.route; the rest of the page talks to the real backend. They test the client's handling of
// those replies, not server or agent behaviour.

async function ownInterview(browser: Browser, agentAvailable = false) {
    const page = await as(browser, "foundation-reviewer", agentAvailable);
    const inventory = await processInventory(page);
    await answerLeftovers(page);
    await startInterview(page, inventory.processDisplayName);
    return { page, sessionID: page.url().split("/").pop()! };
}

test("stubbed list page: interview questions and extra sources follow their cursors", async ({ page }) => {
    const sessionID = stubbedInterviewID;
    const questionCursors: (string | null)[] = [];
    const sourceCursors: (string | null)[] = [];
    const source = (sourceID: string, title: string) => ({
        access_epoch: 1,
        authority: "human",
        authority_class: "primary",
        byte_length: title.length,
        independence_group_id: "manual",
        kind: "manual",
        revision: 1,
        rune_length: title.length,
        sha256: "sha256:test",
        source_id: sourceID,
        state_revision: 1,
        title,
    });

    // stubbed list page: page two adds both a question and a browsable extra source.
    await page.route("**/v1/**", async (route) => {
        const url = new URL(route.request().url());
        if (url.pathname === "/v1/me") return route.fulfill({ json: stubbedMe });
        if (url.pathname === `/v1/sites/${stubbedSiteID}/interviews/${sessionID}`) return route.fulfill({ json: stubbedSession(sessionID) });
        if (url.pathname === `/v1/sites/${stubbedSiteID}/interviews/${sessionID}/questions`) {
            const cursor = url.searchParams.get("cursor");
            questionCursors.push(cursor);
            expect(url.searchParams.get("limit")).toBe("100");
            return route.fulfill({ json: cursor
                ? { questions: [stubbedQuestion("second-question", "Second page question")] }
                : { questions: [stubbedQuestion("first-question", "First page question")], next_cursor: "questions-page-2" } });
        }
        if (url.pathname === `/v1/sites/${stubbedSiteID}/sources`) {
            const cursor = url.searchParams.get("cursor");
            sourceCursors.push(cursor);
            return route.fulfill({ json: cursor
                // first-source shifted onto page two as well: it must still render one checkbox.
                ? { sources: [source("first-source", "First page source"), source("second-source", "Second page source")] }
                : { sources: [source("first-source", "First page source")], next_cursor: "sources-page-2" } });
        }
        return route.fulfill({ status: 404, json: { code: "NOT_FOUND", status: 404, title: "Not found" } });
    });

    await page.goto(`/sites/${stubbedSiteID}/interviews/${sessionID}`);
    await expect(page.getByRole("heading", { name: "Open questions 2" })).toBeVisible();
    await expect(page.getByText("Second page question", { exact: true })).toBeVisible();
    const firstSource = page.getByRole("checkbox", { name: /First page source/ });
    await expect(firstSource).toBeVisible();
    await page.getByRole("button", { name: "Load more sources" }).click();
    await expect(page.getByRole("checkbox", { name: /Second page source/ })).toBeVisible();
    await expect(page.getByRole("checkbox", { name: /First page source/ })).toHaveCount(1);
    expect(questionCursors).toEqual([null, "questions-page-2"]);
    expect(sourceCursors).toEqual([null, "sources-page-2"]);
});

// InterviewPage is keyed by session (main.tsx), so a switch remounts it; this covers that remount,
// not the refresh ordering guard (see the overlapping-refresh test below).
test("remount on a session switch discards a late questions page from the previous interview", async ({ page }) => {
    const sessionID = stubbedInterviewID;
    const secondSessionID = secondStubbedInterviewID;
    let releaseOld!: () => void;
    const oldGate = new Promise<void>((resolve) => { releaseOld = resolve; });
    let oldStarted!: () => void;
    const oldRequestStarted = new Promise<void>((resolve) => { oldStarted = resolve; });
    let oldFinished!: () => void;
    const oldRequestFinished = new Promise<void>((resolve) => { oldFinished = resolve; });
    // stubbed list page: hold the old session response until the newer session is visible.
    await page.route("**/v1/**", async (route) => {
        const url = new URL(route.request().url());
        const base = `/v1/sites/${stubbedSiteID}/interviews`;
        if (url.pathname === "/v1/me") return route.fulfill({ json: stubbedMe });
        if (url.pathname === base) return route.fulfill({ json: { sessions: [stubbedSession(sessionID), stubbedSession(secondSessionID)] } });
        if (url.pathname === `/v1/sites/${stubbedSiteID}/discovery-inventories`) return route.fulfill({ json: { inventories: [] } });
        if (url.pathname === `${base}/${sessionID}`) return route.fulfill({ json: stubbedSession(sessionID) });
        if (url.pathname === `${base}/${secondSessionID}`) return route.fulfill({ json: stubbedSession(secondSessionID) });
        if (url.pathname === `${base}/${sessionID}/questions`) {
            oldStarted();
            await oldGate;
            await route.fulfill({ json: { questions: [stubbedQuestion("old-question", "Old session question")] } });
            oldFinished();
            return;
        }
        if (url.pathname === `${base}/${secondSessionID}/questions`) return route.fulfill({ json: { questions: [stubbedQuestion("new-question", "Current session question")] } });
        if (url.pathname === `/v1/sites/${stubbedSiteID}/sources`) return route.fulfill({ json: { sources: [] } });
        return route.fulfill({ status: 404, json: { code: "NOT_FOUND", status: 404, title: "Not found" } });
    });

    await page.goto(`/sites/${stubbedSiteID}/interviews/${sessionID}`);
    await oldRequestStarted;
    await section(page, "Interviews");
    await page.getByRole("table", { name: "Interview sessions" }).getByRole("link", { name: `Interview ${sessionID.slice(0, 8)}` }).click();
    await oldRequestStarted;
    await section(page, "Interviews");
    await page.getByRole("table", { name: "Interview sessions" }).getByRole("link", { name: `Interview ${secondSessionID.slice(0, 8)}` }).click();
    await expect(page.getByText("Current session question", { exact: true })).toBeVisible();
    releaseOld();
    await oldRequestFinished;
    await expect(page.getByText("Current session question", { exact: true })).toBeVisible();
    await expect(page.getByText("Old session question", { exact: true })).toHaveCount(0);
});

test("overlapping refreshes in one interview: only the latest may write questions or clear refreshing", async ({ page }) => {
    const base = `/v1/sites/${stubbedSiteID}/interviews/${stubbedInterviewID}`;
    // Refresh k serves page 1 (two open questions) then page 2 ("Refresh k question"); page 2 of a
    // held refresh waits for its gate.
    let refreshes = 0;
    const gates = new Map<number, Promise<void>>();
    const releases = new Map<number, () => void>();
    const started = new Map<number, () => void>();
    const startedP = new Map<number, Promise<void>>();
    const hold = (k: number) => {
        gates.set(k, new Promise<void>((r) => releases.set(k, r)));
        startedP.set(k, new Promise<void>((r) => started.set(k, r)));
    };
    // Count page-2 replies the app has parsed, so the test waits on the app, not the clock.
    await page.addInitScript(() => {
        const w = window as unknown as { __pageTwos: number };
        w.__pageTwos = 0;
        const json = Response.prototype.json;
        Response.prototype.json = async function (this: Response) {
            const value = await json.call(this);
            if (this.url.includes("cursor=p2-")) w.__pageTwos++;
            return value;
        };
    });
    const parsed = (n: number) => page.waitForFunction((n) => (window as unknown as { __pageTwos: number }).__pageTwos >= n, n);
    // Two frames let React commit whatever the parsed reply set.
    const settle = () => page.evaluate(() => new Promise<void>((r) => requestAnimationFrame(() => requestAnimationFrame(() => r()))));
    await page.route("**/v1/**", async (route) => {
        const url = new URL(route.request().url());
        if (url.pathname === "/v1/me") return route.fulfill({ json: stubbedMe });
        if (url.pathname === base) return route.fulfill({ json: stubbedSession(stubbedInterviewID) });
        if (url.pathname === `${base}/answers`) return route.fulfill({ json: { source_revision: 1 } });
        if (url.pathname === `${base}/questions`) {
            const cursor = url.searchParams.get("cursor");
            if (!cursor) {
                const k = ++refreshes;
                return route.fulfill({ json: { questions: [stubbedQuestion("question-a", "Question A"), stubbedQuestion("question-b", "Question B")], next_cursor: `p2-${k}` } });
            }
            const k = Number(cursor.slice(3));
            started.get(k)?.();
            await gates.get(k);
            return route.fulfill({ json: { questions: [stubbedQuestion(`from-${k}`, `Refresh ${k} question`)] } });
        }
        if (url.pathname === `/v1/sites/${stubbedSiteID}/sources`) return route.fulfill({ json: { sources: [] } });
        return route.fulfill({ status: 404, json: { code: "NOT_FOUND", status: 404, title: "Not found" } });
    });
    const answer = async (prompt: string) => {
        const row = page.locator(".interview-question").filter({ hasText: prompt });
        await row.getByLabel("Your answer").fill("An answer");
        await row.getByRole("button", { name: "Save answer" }).click();
    };
    const refreshing = page.getByText("refreshing…");

    await page.goto(`/sites/${stubbedSiteID}/interviews/${stubbedInterviewID}`);
    await expect(page.getByText("Refresh 1 question", { exact: true })).toBeVisible();

    // The older refresh lands while the newer one is still in flight: it neither writes nor clears refreshing.
    hold(2);
    hold(3);
    await answer("Question A");
    await startedP.get(2);
    await answer("Question B");
    await startedP.get(3);
    releases.get(2)!();
    await parsed(2);
    await settle();
    await expect(page.getByText("Refresh 2 question", { exact: true })).toHaveCount(0);
    await expect(refreshing).toBeVisible();
    releases.get(3)!();
    await expect(page.getByText("Refresh 3 question", { exact: true })).toBeVisible();
    await expect(refreshing).toHaveCount(0);

    // The newer refresh completes first: the older one's late page cannot replace its questions.
    hold(4);
    await answer("Question A");
    await startedP.get(4);
    await answer("Question B");
    await expect(page.getByText("Refresh 5 question", { exact: true })).toBeVisible();
    releases.get(4)!();
    await parsed(5);
    await settle();
    await expect(page.getByText("Refresh 5 question", { exact: true })).toBeVisible();
    await expect(page.getByText("Refresh 4 question", { exact: true })).toHaveCount(0);
});

// reenter opens the interview again in-app, so its run panels mount and read sessionStorage.
async function reenter(page: Page, sessionID: string) {
    await section(page, "Interviews");
    await expect(page.getByRole("heading", { name: "Interviews", exact: true })).toBeVisible();
    await page.getByRole("table", { name: "Interview sessions" }).getByRole("link", { name: `Interview ${sessionID.slice(0, 8)}` }).click();
    await expect(page.getByRole("heading", { name: `Interview ${sessionID.slice(0, 8)}` })).toBeVisible();
    await expect(page.getByRole("heading", { name: /^Open questions/ })).toBeVisible();
}

const problemJSON = (status: number, code: string) => ({
    status,
    contentType: "application/problem+json",
    body: JSON.stringify({ type: "about:blank", title: code, status, code, correlation_id: crypto.randomUUID() }),
});

// Also stubbed /v1/me: CI runs agent-off, and the enabled button needs agent_available=true.
test("stubbed 404 for a remembered run (stubbed /v1/me): the run is forgotten, the button is enabled and polling stops (UI behaviour)", async ({ browser }) => {
    const { page, sessionID } = await ownInterview(browser, true);
    const runID = crypto.randomUUID();
    let reads = 0;
    await page.route(`**/agent-turns/${runID}`, (route) => {
        reads++;
        return route.fulfill(problemJSON(404, "NOT_FOUND"));
    });
    await page.evaluate(([key, value]) => sessionStorage.setItem(key, value), [`supervisor.interview.turn.${sessionID}`, runID]);
    await reenter(page, sessionID);

    await expect(page.getByText(`Follow-up run ${runID.slice(0, 8)} is no longer available to you`)).toBeVisible({ timeout: 15000 });
    await expect(page.getByRole("button", { name: "Ask agent for follow-up questions" })).toBeEnabled();
    expect(await page.evaluate((key) => sessionStorage.getItem(key), `supervisor.interview.turn.${sessionID}`)).toBe("");
    // Absence needs a window: three poll intervals (2 s each) with no further read.
    await page.waitForTimeout(6000);
    expect(reads).toBe(1);
});

test("stubbed 412 on an answer: the questions reload at a newer revision and the typed answer is kept (UI behaviour)", async ({ browser }) => {
    const { page, sessionID } = await ownInterview(browser);
    const first = page.locator(".interview-question").first();
    const prompt = await first.locator(".interview-prompt").innerText();
    let stubbed = false;
    await page.route(`**/interviews/${sessionID}/answers`, (route) => {
        if (route.request().method() !== "POST" || stubbed) return route.continue();
        stubbed = true;
        return route.fulfill(problemJSON(412, "REVISION_CONFLICT"));
    });
    // After the 412 the reloaded page serves that question one revision newer, as a real
    // concurrent edit would; the server's other fields are passed through unchanged.
    let bumpedReads = 0;
    await page.route(`**/interviews/${sessionID}/questions*`, async (route) => {
        if (!stubbed) return route.continue();
        const response = await route.fetch();
        const body = await response.json();
        for (const q of body.questions) if (q.prompt === prompt) q.revision += 1;
        bumpedReads++;
        return route.fulfill({ response, json: body });
    });
    await first.getByLabel("Your answer").fill("Kept across the reload");
    await first.getByRole("button", { name: "Save answer" }).click();
    await expect.poll(() => bumpedReads).toBeGreaterThan(0);
    const row = page.locator(".interview-question").filter({ hasText: prompt });
    await expect(row).toContainText(/· r2(?!\d)/);
    await expect(row.getByLabel("Your answer")).toHaveValue("Kept across the reload");
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toBeVisible();
});

test("stubbed out-of-order run replies: a late 'running' cannot overwrite 'completed' (UI behaviour)", async ({ browser }) => {
    const { page, sessionID } = await ownInterview(browser);
    // Count run replies the app has finished parsing, so the test waits on the app, not the clock.
    await page.evaluate(() => {
        const w = window as unknown as { __runReplies: number };
        w.__runReplies = 0;
        const json = Response.prototype.json;
        Response.prototype.json = async function (this: Response) {
            const value = await json.call(this);
            if (this.url.includes("/configuration-draft-requests/")) w.__runReplies++;
            return value;
        };
    });
    const runID = crypto.randomUUID();
    const reply = (state: string) => ({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ run_id: runID, session_id: sessionID, state, ...(state === "completed" ? { outcome_status: "proposed" } : {}), outcome_reasons: [], draft_ids: [] }),
    });
    let release!: () => void;
    const gate = new Promise<void>((resolve) => (release = resolve));
    let reads = 0;
    let lateDelivered!: Promise<void>;
    await page.route(`**/configuration-draft-requests/${runID}`, (route) => {
        reads++;
        if (reads === 1) {
            lateDelivered = gate.then(() => route.fulfill(reply("running")));
            return lateDelivered;
        }
        if (reads === 2) return route.fulfill(reply("completed"));
        return new Promise<void>(() => {}); // any further poll never answers
    });
    await page.evaluate(([key, value]) => sessionStorage.setItem(key, value), [`supervisor.interview.drafts.${sessionID}`, runID]);
    await reenter(page, sessionID);

    const status = page.getByRole("status").filter({ hasText: /^Draft run [0-9a-f]{8}/ });
    await expect(status).toContainText("Finished", { timeout: 15000 });
    release();
    await lateDelivered;
    await page.waitForFunction(() => (window as unknown as { __runReplies: number }).__runReplies >= 2);
    await expect(status).toContainText("Finished");
    await expect(status).not.toContainText("Running");
});
