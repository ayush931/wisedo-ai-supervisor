import { expect, test, type Browser, type Page } from "@playwright/test";
import { section, signIn } from "./session";

// WIS-631 slice 3: the onboarding pilot in a real browser, for two tenants side by side, against the
// real API and worker with no stubbed route and no SQL. supervisor-demo-seed creates the two pilot
// tenants (Chakan and Pune), each with an owner, a lead and an approver who are members of that
// one site only.
//
// Per tenant: the owner uploads the SOP on the Documents page and marks it authoritative and
// current there; a lead starts a value-stream interview and asks the agent; the agent's proposed
// entities and claims appear with the passage they cite on the interview page; the approver, who
// was not interviewed, approves the claims on Setup -> Facts to review (WIS-641; an interview itself
// is visible only to its participant); the owner publishes a snapshot on Setup -> Approved process;
// the lead asks on Today and gets the approved answer with its citation. Then each tenant's owner
// probes the other tenant's resources and gets the same 404 as for an ID that never existed.
//
// Commissioning the area, the process and its checklist (an agent may not propose those) has no
// screen yet, so it goes through the public API from the signed-in owner's browser, as a caller would.
//
// @onboarding-pilot: the API and worker run SUPERVISOR_AGENT_PROVIDER=fixture with
// SUPERVISOR_AGENT_FIXTURE_SCRIPT=deploy/supervisor/agent-fixture-script.pilot.v1.json and
// SUPERVISOR_CHAT_ANSWERING_ENABLED=true (CI's fifth e2e phase; make supervisor-e2e
// CASE=onboarding-pilot). A scripted fixture is a contract check, not actual model behaviour
// (rule 09). Another mode is BLOCKED, never skipped.

const env = (globalThis as typeof globalThis & { process?: { env?: Record<string, string | undefined> } }).process?.env ?? {};

type Reply = { status: number; body: any };
type Tenant = { plant: string; site: string; owner: string; lead: string; approver: string };

const tenants: Tenant[] = [
    { plant: "Chakan Pilot Plant", site: "00000000-0000-4000-8000-000000000632", owner: "chakan-owner", lead: "chakan-lead", approver: "chakan-approver" },
    { plant: "Pune Pilot Plant", site: "00000000-0000-4000-8000-000000000634", owner: "pune-owner", lead: "pune-lead", approver: "pune-approver" },
];

// The SOP is one paragraph, so it is stored as one passage; the pilot fixture script cites that
// passage for every proposed name and number, and the chat answer quotes it.
const sopText = (plant: string) => `${plant} press SOP. Steel coil is supplied by Deccan Steel. The Press cycle time is 45 s.`;

function requirePilot() {
    if (env.SUPERVISOR_E2E_AGENT !== "pilot")
        throw new Error("BLOCKED: this test needs the API and worker started with SUPERVISOR_AGENT_PROVIDER=fixture, SUPERVISOR_AGENT_MODE_ENABLED=true, SUPERVISOR_CHAT_ANSWERING_ENABLED=true, SUPERVISOR_AGENT_FIXTURE_SCRIPT=deploy/supervisor/agent-fixture-script.pilot.v1.json and SUPERVISOR_AGENT_VISION_MODEL=fixture-vision with SUPERVISOR_AGENT_VISION_MODEL_REVISION=sha256:vision, and SUPERVISOR_E2E_AGENT=pilot");
}

// call uses the signed-in page's own session, CSRF token and site path.
async function call(page: Page, site: string, method: string, path: string, body?: unknown, ifMatch?: number): Promise<Reply> {
    return page.evaluate(async ({ site, method, path, body, ifMatch }) => {
        const me = await (await fetch("/v1/me")).json();
        const headers: Record<string, string> = {};
        if (method !== "GET") {
            headers["Content-Type"] = "application/json";
            headers["X-CSRF-Token"] = me.csrf_token;
            headers["Idempotency-Key"] = crypto.randomUUID();
            if (ifMatch !== undefined) headers["If-Match"] = `"${ifMatch}"`;
        }
        const r = await fetch(`/v1/sites/${site}${path}`, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
        return { status: r.status, body: await r.json().catch(() => null) };
    }, { site, method, path, body, ifMatch });
}

async function ok(reply: Promise<Reply>, status: number, what: string) {
    const r = await reply;
    if (r.status !== status) throw new Error(`${what}: expected ${status}, got ${r.status} ${r.body?.code ?? ""}`);
    return r.body;
}

// as signs a pilot user in; each pilot user belongs to one site, so sign-in lands in it.
async function as(browser: Browser, user: string, t: Tenant) {
    const page = await (await browser.newContext()).newPage();
    await signIn(page, user);
    await expect(page.getByRole("navigation", { name: "Site sections" })).toBeVisible();
    const me = await page.evaluate(async () => (await fetch("/v1/me")).json());
    expect(me.sites.map((s: { id: string }) => s.id)).toEqual([t.site]);
    return page;
}

type Onboarded = {
    doc: string;
    scan: string;
    span: string;
    processID: string;
    sessionID: string;
    runID: string;
    claimIDs: string[];
    entityIDs: string[];
    snapshotID: string;
    conversationID: string;
    cited: string;
};

async function upload(owner: Page, t: Tenant, tag: string) {
    await section(owner, "Documents");
    await expect(owner.getByRole("heading", { name: "Documents", exact: true })).toBeVisible();
    await owner.getByLabel("File").setInputFiles({ name: "press-sop.txt", mimeType: "text/plain", buffer: Buffer.from(sopText(t.plant)) });
    await owner.getByLabel("Title").fill(`Press SOP ${tag}`);
    const stored = owner.waitForResponse((r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/source-documents"));
    await owner.getByRole("button", { name: "Upload", exact: true }).click();
    const response = await stored;
    expect(response.status()).toBe(200);
    const document = await response.json();
    await expect(owner.getByRole("heading", { name: `Stored: Press SOP ${tag}` })).toBeVisible();
    const passages = owner.getByRole("list", { name: "Passages" }).getByRole("listitem");
    await expect(passages).toHaveCount(1);
    await expect(passages.first()).toContainText(sopText(t.plant));
    expect(document.spans).toHaveLength(1);
    const doc = document.source.source_id as string;

    // WIS-641: the owner marks the stored SOP authoritative, then confirms it current, on the same
    // panel; each shows only from the server's receipt. Approval later needs this designation.
    const panel = owner.getByRole("group", { name: `Authority of Press SOP ${tag}` });
    await expect(panel).toContainText("Not authoritative");
    await panel.getByLabel("Decision reason").fill("Owner verified the SOP");
    await panel.getByRole("button", { name: "Mark authoritative" }).click();
    await expect(panel.getByRole("status").filter({ hasText: /\S/ })).toHaveText(/^Marked authoritative\. Decision [0-9a-f]{8} recorded\.$/);
    await expect(panel.getByText("Authoritative", { exact: true })).toBeVisible();
    await panel.getByLabel("Decision reason").fill("Current shop-floor revision");
    await panel.getByRole("button", { name: "Confirm current" }).click();
    await expect(panel.getByRole("status").filter({ hasText: /\S/ })).toHaveText(/^Confirmed current\. Decision [0-9a-f]{8} recorded\.$/);
    await expect(panel.getByText("Confirmed current", { exact: true })).toBeVisible();
    const subject = await owner.evaluate(async () => (await (await fetch("/v1/me")).json()).subject as string);
    const authority = await ok(call(owner, t.site, "GET", `/sources/${doc}/authority`), 200, "read SOP authority");
    expect(authority).toMatchObject({ verdict: "designate", review_evidence_eligible: true, actor_subject: subject });
    const currentness = await ok(call(owner, t.site, "GET", `/sources/${doc}/currentness`), 200, "read SOP currentness");
    expect(currentness).toMatchObject({ verdict: "confirm_current", verification_current: true, authority_decision_id: authority.decision_id, actor_subject: subject });
    return { doc, span: document.spans[0].id as string };
}

// A small, real PNG page: the upload classifies it from its bytes and reads its size from the header.
const pagePNG = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAADgAAAAcCAAAAAAR0CPiAAAAGklEQVR4nGP4TyZgGNU4qnFU46jGUY0jVSMAy8caO/z+4X4AAAAASUVORK5CYII=", "base64");
// The text the pilot fixture script's "ocr" purpose transcribes (a scripted fixture, rule 09).
const ocrText = "Incoming inspection: check every steel coil for edge cracks before it is stored.";

// scanPage (WIS-643): the owner uploads a photo of a page on Documents. The worker's fixture
// vision model transcribes it into a model-extracted source; the owner reads that text on the
// authority panel and can mark it authoritative only after ticking that they compared it with the
// original page. The receipt and the stored authority show the confirmed model-extracted text.
async function scanPage(owner: Page, t: Tenant, tag: string) {
    await section(owner, "Documents");
    const title = `Inspection photo ${tag}`;
    await owner.getByLabel("Title").fill(title);
    await owner.getByLabel("File").setInputFiles({ name: "inspection.png", mimeType: "image/png", buffer: pagePNG });
    const queued = owner.waitForResponse((r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/source-scans"));
    await owner.getByRole("button", { name: "Upload", exact: true }).click();
    expect((await queued).status()).toBe(202);
    const progress = owner.getByRole("region", { name: `Photo: ${title}` });
    await expect(progress.getByRole("status").filter({ hasText: /\S/ })).toHaveText("Read by the vision model.", { timeout: 60_000 });
    await expect(progress.getByLabel("Extracted text")).toHaveText(ocrText);

    await owner.reload(); // the site document list is read on load
    const item = owner.getByRole("list", { name: "Site documents" }).getByRole("listitem", { name: title });
    await expect(item).toContainText("model-extracted text (OCR)");
    await item.getByRole("button", { name: "Authority" }).click();
    const panel = item.getByRole("group", { name: `Authority of ${title}` });
    await expect(panel.getByRole("note")).toContainText("Text read by a model");
    await expect(panel.getByLabel(`Model-read text of ${title}`)).toHaveText(ocrText);
    await panel.getByLabel("Decision reason").fill("Compared with the photographed page");
    const mark = panel.getByRole("button", { name: "Mark authoritative" });
    await expect(mark).toBeDisabled(); // never designated without the explicit comparison
    const decided = owner.waitForResponse((r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/authority-decisions"));
    await panel.getByRole("checkbox", { name: /I compared this text with the original page/ }).check();
    await mark.click();
    const decision = await decided;
    expect(decision.status()).toBe(200);
    expect(JSON.parse(decision.request().postData() ?? "{}")).toMatchObject({ verdict: "designate", confirm_model_extracted: true });
    await expect(panel.getByRole("status").filter({ hasText: /\S/ })).toHaveText(/^Marked authoritative\. Decision [0-9a-f]{8} recorded\.$/);
    const scan = (await decision.json()).source_id as string;
    const authority = await ok(call(owner, t.site, "GET", `/sources/${scan}/authority`), 200, "read scan authority");
    expect(authority).toMatchObject({ verdict: "designate", source_model_extracted: true, review_evidence_eligible: true });
    return scan;
}

// commission registers the area and the process (both citing the SOP, so the interview's agent
// turn reads it) and starts the value-stream checklist, all on Setup -> Processes (WIS-644). Each
// shows only from the server's receipt; the IDs come from the receipts and the rows are read back.
async function commission(owner: Page, t: Tenant, doc: string, tag: string) {
    const processName = `Bracket process ${tag}`, areaName = `Press area ${tag}`;
    await section(owner, "Processes");
    await expect(owner.getByRole("heading", { name: "Processes", level: 1 })).toBeVisible();
    const add = owner.getByRole("form", { name: "Add an area or process" });
    const ids: Record<string, string> = {};
    for (const [kind, name] of [["Area", areaName], ["Process", processName]]) {
        await add.getByLabel(kind, { exact: true }).check();
        await add.getByLabel("Name").fill(name);
        await add.getByLabel("Cites").selectOption({ label: `Press SOP ${tag} · document · r1` });
        const created = owner.waitForResponse((r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/entities"));
        await add.getByRole("button", { name: `Add ${kind.toLowerCase()}` }).click();
        const response = await created;
        expect(response.status()).toBe(200);
        ids[kind] = (await response.json()).entity_id;
        await expect(add.getByRole("status").filter({ hasText: /\S/ })).toHaveText(`${kind} "${name}" added (revision 1).`);
    }
    const start = owner.getByRole("form", { name: "Start a value-stream checklist" });
    await start.getByLabel("Process").selectOption({ label: processName });
    await start.getByLabel("Area").selectOption({ label: areaName });
    await start.getByLabel("Reason").fill("Pilot value-stream interview");
    const registered = owner.waitForResponse((r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/discovery-inventories"));
    await start.getByRole("button", { name: "Start checklist" }).click();
    expect((await registered).status()).toBe(200);
    await expect(start.getByRole("status").filter({ hasText: /\S/ })).toHaveText(new RegExp(`^Value-stream checklist started for "${processName}" with \\d+ questions to cover\\.$`));
    for (const [kind, type, name] of [["Area", "area", areaName], ["Process", "process", processName]])
        expect(await ok(call(owner, t.site, "GET", `/entities/${ids[kind]}`), 200, `read ${type}`)).toMatchObject({ type, display_name: name, source_id: doc, source_revision: 1 });
    const inventories = await ok(call(owner, t.site, "GET", `/discovery-inventories?process_id=${ids.Process}`), 200, "read checklist");
    expect(inventories.inventories).toEqual([expect.objectContaining({ process_id: ids.Process, process_display_name: processName, capability: "process_understanding" })]);
    return { processID: ids.Process, processName };
}

// answerLeftovers answers this lead's open questions from earlier runs: a participant may hold at
// most 3 unanswered questions across sessions, and reruns share the pilot site.
async function answerLeftovers(lead: Page, t: Tenant) {
    const sessions = await ok(call(lead, t.site, "GET", "/interviews?limit=100"), 200, "list interviews");
    for (const s of sessions.sessions.filter((x: { status: string }) => x.status === "active")) {
        const list = await ok(call(lead, t.site, "GET", `/interviews/${s.session_id}/questions?limit=100`), 200, "list questions");
        for (const q of list.questions.filter((x: { answer_state: string }) => x.answer_state === "unanswered"))
            await ok(call(lead, t.site, "POST", `/interviews/${s.session_id}/answers`, { question_id: q.question_id, question_revision: q.revision, text: "Earlier pilot run", client_answer_id: crypto.randomUUID() }), 201, "answer leftover");
    }
}

async function interview(lead: Page, t: Tenant, processName: string) {
    await answerLeftovers(lead, t);
    await section(lead, "Interviews");
    const option = `${processName} — Value stream`;
    const picker = lead.getByLabel("Process inventory");
    await expect(picker).toBeVisible();
    while ((await picker.getByRole("option", { name: option, exact: true }).count()) === 0) await lead.getByRole("button", { name: "Load more process inventories" }).click();
    await picker.selectOption({ label: option });
    await lead.getByRole("button", { name: "Start interview" }).click();
    await expect(lead.getByRole("heading", { name: /^Interview [0-9a-f]{8}$/ })).toBeVisible();
    await expect(lead.getByRole("heading", { name: /^Open questions/ })).toBeVisible();
    const sessionID = lead.url().split("/").pop()!;

    // The agent turn: the worker runs it; the page watches the run until it finishes.
    const started = lead.waitForResponse((r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith(`/interviews/${sessionID}/agent-turns`));
    await lead.getByRole("button", { name: "Ask agent for follow-up questions" }).click();
    const turn = await started;
    expect(turn.status()).toBe(202);
    const runID = (await turn.json()).run_id as string;
    await expect(lead.getByRole("status").filter({ hasText: /^Follow-up run [0-9a-f]{8}/ })).toContainText("Finished", { timeout: 60_000 });
    const run = await ok(call(lead, t.site, "GET", `/interviews/${sessionID}/agent-turns/${runID}`), 200, "read agent turn");
    expect(run).toMatchObject({ state: "completed", outcome_status: "proposed" });
    expect(run.entity_ids).toHaveLength(3);
    expect(run.claim_ids).toHaveLength(2);
    return { sessionID, runID, claimIDs: run.claim_ids as string[], entityIDs: run.entity_ids as string[] };
}

const facts = (page: Page) => page.getByRole("region", { name: "Proposed by the agent" });

// proposed: the lead sees the run's proposals, with the passage they cite, on the interview page.
async function proposed(lead: Page, t: Tenant) {
    const region = facts(lead);
    await expect(region.getByRole("heading", { name: "Entities 3" })).toBeVisible({ timeout: 30_000 });
    for (const [type, name] of [["Material", "Steel coil"], ["Supplier", "Deccan Steel"], ["Operation", "Press"]])
        await expect(region.getByRole("listitem").filter({ hasText: type }).filter({ hasText: name })).toBeVisible();
    await expect(region.getByRole("heading", { name: "Claims 2" })).toBeVisible();
    for (const name of ["Claim: Steel coil supplied by Deccan Steel", "Claim: Press cycle time 45 s"]) {
        const card = region.getByRole("article", { name });
        await expect(card).toContainText("Candidate");
        await expect(card.locator("blockquote")).toHaveText(sopText(t.plant));
    }
}

// review: the approver, who was not interviewed and cannot open the lead's interview, finds this
// run's two candidate claims on Setup -> Facts to review and approves each; it shows Approved only
// from the server's receipt.
async function review(approver: Page, t: Tenant, sessionID: string, claimIDs: string[]) {
    expect((await call(approver, t.site, "GET", `/interviews/${sessionID}`)).status).toBe(404);
    await section(approver, "Facts to review");
    await expect(approver.getByRole("heading", { name: "Facts to review", exact: true })).toBeVisible();
    const list = approver.getByRole("list", { name: "Candidate facts" });
    for (const id of claimIDs) {
        const item = list.getByRole("listitem", { name: `Candidate fact ${id.slice(0, 8)}` });
        await expect(list).toBeVisible();
        while ((await item.count()) === 0) {
            const more = approver.getByRole("button", { name: "Load more candidate facts" });
            await expect(more, `candidate claim ${id} is not listed`).toBeEnabled({ timeout: 10_000 });
            await more.click();
            await expect(approver.getByRole("button", { name: "Loading…" })).toHaveCount(0);
        }
        const card = item.getByRole("article");
        await expect(card).toContainText("Candidate");
        await expect(card.locator("blockquote")).toHaveText(sopText(t.plant));
        await card.getByLabel("Review reason").fill("Matches the SOP");
        await card.getByRole("button", { name: "Approve", exact: true }).click();
        await expect(card.getByRole("status").filter({ hasText: /\S/ })).toContainText(/^Approved\. Review [0-9a-f]{8} recorded\.$/);
    }
    const reviewer = await approver.evaluate(async () => (await (await fetch("/v1/me")).json()).subject as string);
    for (const id of claimIDs) {
        const r = await ok(call(approver, t.site, "GET", `/claims/${id}/review`), 200, "read review");
        expect(r).toMatchObject({ claim_id: id, verdict: "approve", actor_subject: reviewer });
    }
}

// publish: the owner seals the approved facts on Setup -> Approved process (WIS-641); the receipt
// names the snapshot, which is current (not stale) at the revision the page read.
async function publish(owner: Page) {
    await section(owner, "Approved process");
    const form = owner.getByRole("form", { name: "Publish approved knowledge" });
    await expect(form).toContainText(/Knowledge revision \d+\./);
    await form.getByLabel("Snapshot reason").fill("Pilot onboarding reviewed");
    const sealed = owner.waitForResponse((r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/process-snapshots"));
    await form.getByRole("button", { name: "Publish snapshot" }).click();
    const response = await sealed;
    expect(response.status()).toBe(200);
    const snapshot = await response.json();
    expect(snapshot.stale).toBe(false);
    await expect(form.getByRole("status").filter({ hasText: /\S/ })).toHaveText(`Snapshot ${snapshot.snapshot_id.slice(0, 8)} published at knowledge revision ${snapshot.inventory_revision}.`);
    return snapshot.snapshot_id as string;
}

// ask: the lead asks on Today; the worker answers from the approved snapshot, citing an SOP span.
// On a fresh database that is this run's span. A rerun on the same pilot site also has earlier
// runs' identical approved SOPs, and chat may cite any of them, so the test checks what does not
// depend on that: one cited span, readable here, holding exactly this plant's SOP text.
async function ask(lead: Page, t: Tenant, snapshotID: string) {
    await lead.getByRole("navigation", { name: "Site sections" }).getByRole("link", { name: "Today", exact: true }).click();
    await expect(lead.getByRole("heading", { name: "Ask about how the plant runs" })).toBeVisible();
    const posted = lead.waitForResponse((r) => r.request().method() === "POST" && /\/chat-conversations\/[^/]+\/messages$/.test(new URL(r.url()).pathname));
    await lead.getByLabel("Your question").fill("What is the press cycle time?");
    await lead.getByRole("button", { name: "Ask", exact: true }).click();
    const reply = await posted;
    expect(reply.status()).toBe(201);
    const conversationID = new URL(reply.url()).pathname.split("/").at(-2)!;
    await expect(lead.getByText(sopText(t.plant), { exact: false })).toBeVisible({ timeout: 60_000 });
    await expect(lead.getByLabel("Citations")).toContainText(/^evidence_span [0-9a-f]{8} · rev 1$/);
    const page = await ok(call(lead, t.site, "GET", `/chat-conversations/${conversationID}/messages?limit=100`), 200, "read chat");
    const answer = page.messages.find((m: { role?: string; state: string }) => m.state === "answered");
    expect(answer).toBeTruthy();
    expect(answer.snapshot_id).toBe(snapshotID);
    expect(answer.citations).toHaveLength(1);
    const cited = answer.citations[0].id as string;
    const span = await ok(call(lead, t.site, "GET", `/source-spans/${cited}`), 200, "read cited span");
    expect(span.text).toBe(sopText(t.plant));
    return { conversationID, cited };
}

// probe: every resource of one tenant, read by the other tenant's owner under its own site and
// under the first tenant's site, is the same 404 as a resource that never existed.
async function probe(outsider: Page, own: Tenant, other: Tenant, o: Onboarded) {
    const paths = [
        `/sources/${o.doc}`, `/sources/${o.scan}`, `/source-spans/${o.span}`, `/source-spans/${o.cited}`, `/entities/${o.processID}`, ...o.entityIDs.map((id) => `/entities/${id}`), ...o.claimIDs.map((id) => `/claims/${id}`),
        `/interviews/${o.sessionID}`, `/interviews/${o.sessionID}/agent-turns/${o.runID}`, `/process-snapshots/${o.snapshotID}`, `/chat-conversations/${o.conversationID}`,
    ];
    const never = await call(outsider, own.site, "GET", `/sources/${crypto.randomUUID()}`);
    expect(never.status).toBe(404);
    let probes = 0;
    for (const p of paths) {
        for (const site of [own.site, other.site]) {
            const r = await call(outsider, site, "GET", p);
            expect({ path: p, site, status: r.status, code: r.body?.code }).toEqual({ path: p, site, status: 404, code: never.body.code });
            probes++;
        }
    }
    return probes;
}

test("@onboarding-pilot two tenants onboard side by side in the browser, and neither can read the other", async ({ browser }) => {
    test.setTimeout(300_000);
    requirePilot();
    const tag = crypto.randomUUID().slice(0, 6);
    const pages = await Promise.all(tenants.map(async (t) => ({ owner: await as(browser, t.owner, t), lead: await as(browser, t.lead, t), approver: await as(browser, t.approver, t) })));
    const authorityWrites: string[] = [];
    for (const p of pages)
        for (const page of [p.owner, p.lead, p.approver])
            page.on("request", (request) => {
                const path = new URL(request.url()).pathname;
                if (request.method() === "POST" && /\/(?:promotions|approvals|dispatch|tasks)(?:\/|$)/.test(path)) authorityWrites.push(path);
            });

    // Both tenants are onboarded step by step in turn, so each step runs while the other tenant's
    // data from the same step already exists.
    const sop = [] as { doc: string; span: string }[];
    for (const [i, t] of tenants.entries()) sop.push(await upload(pages[i].owner, t, tag));
    const scans = [] as string[];
    for (const [i, t] of tenants.entries()) scans.push(await scanPage(pages[i].owner, t, tag));
    const commissioned = [] as { processID: string; processName: string }[];
    for (const [i, t] of tenants.entries()) commissioned.push(await commission(pages[i].owner, t, sop[i].doc, tag));
    const turns = [] as Awaited<ReturnType<typeof interview>>[];
    for (const [i, t] of tenants.entries()) turns.push(await interview(pages[i].lead, t, commissioned[i].processName));
    for (const [i, t] of tenants.entries()) await proposed(pages[i].lead, t);
    for (const [i, t] of tenants.entries()) await review(pages[i].approver, t, turns[i].sessionID, turns[i].claimIDs);
    const snapshots = [] as string[];
    for (const p of pages) snapshots.push(await publish(p.owner));
    const onboarded: Onboarded[] = [];
    for (const [i, t] of tenants.entries()) {
        const { conversationID, cited } = await ask(pages[i].lead, t, snapshots[i]);
        onboarded.push({ ...sop[i], scan: scans[i], ...commissioned[i], ...turns[i], snapshotID: snapshots[i], conversationID, cited });
    }
    // Each answer quotes its own plant's SOP and never the other's.
    await expect(pages[0].lead.getByText(tenants[1].plant)).toHaveCount(0);
    await expect(pages[1].lead.getByText(tenants[0].plant)).toHaveCount(0);

    const probes = [await probe(pages[1].owner, tenants[1], tenants[0], onboarded[0]), await probe(pages[0].owner, tenants[0], tenants[1], onboarded[1])];
    expect(probes).toEqual([28, 28]);
    // Onboarding proposes and reviews facts; nothing in it approves an action or dispatches a task.
    expect(authorityWrites).toEqual([]);
});
