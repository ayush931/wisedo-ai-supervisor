// @feedback WIS-703: real API, PostgreSQL and worker; no routes or model responses are stubbed.
// The explicitly labelled pilot fixture quotes retrieved evidence and copies its exact claim pin.
import { writeFile } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { expect, test, type Page } from "@playwright/test";
import { openSite, section, signIn } from "./session";
const env = process.env;
const site = "00000000-0000-4000-8000-000000000291";
async function call(page: Page, method: string, path: string, body?: unknown, revision?: number) {
    return page.evaluate(async ({ method, path, body, revision, site }) => {
        const me = await (await fetch("/v1/me")).json();
        const headers: Record<string, string> = {};
        if (method !== "GET") { headers["Content-Type"] = "application/json"; headers["X-CSRF-Token"] = me.csrf_token; headers["Idempotency-Key"] = crypto.randomUUID(); if (revision !== undefined) headers["If-Match"] = `"${revision}"`; }
        const r = await fetch(`/v1/sites/${site}${path}`, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
        return { status: r.status, body: await r.json() };
    }, { method, path, body, revision, site });
}
async function ok(page: Page, method: string, path: string, body?: unknown, revision?: number) {
    const r = await call(page, method, path, body, revision);
    expect(r.status, `${method} ${path}: ${r.body?.code ?? ""}`).toBeGreaterThanOrEqual(200); expect(r.status).toBeLessThan(300); return r.body;
}
async function source(owner: Page, text: string, title: string) {
    const s = await ok(owner, "POST", "/sources", { source_id: randomUUID(), kind: "manual", title, text }, 0);
    const span = await ok(owner, "POST", `/sources/${s.source_id}/spans`, { revision: 1, start_char: 0, end_char: Array.from(text).length }, 1);
    await ok(owner, "POST", `/sources/${s.source_id}/authority-decisions`, { source_revision: 1, expected_source_state_revision: 1, verdict: "designate", reason: "Verified synthetic browser regression source" }, 0);
    return { source: s.source_id, span: span.id };
}
async function today(page: Page) { await page.getByRole("navigation", { name: "Site sections" }).getByRole("link", { name: "Today", exact: true }).click(); }
async function ask(page: Page, value: string) {
    await today(page);
    await page.getByLabel("Your question").fill("What is the WIS703 press cycle time?");
    const posted = page.waitForResponse((r) => r.request().method() === "POST" && /chat-conversations\/[^/]+\/messages$/.test(new URL(r.url()).pathname));
    await page.getByRole("button", { name: "Ask", exact: true }).click();
    const result = await (await posted).json();
    await expect(page.getByRole("region", { name: "Ask about how the plant runs" })).toContainText(`The WIS703 press cycle time is ${value} seconds.`, { timeout: 90_000 });
    const messages = await ok(page, "GET", `/chat-conversations/${result.conversation.conversation_id}/messages`);
    const reply = messages.messages.find((m: any) => m.message_id === result.reply.message_id);
    expect(reply.state).toBe("answered"); return { reply, conversation: result.conversation.conversation_id };
}

test("@feedback WIS703 operator A corrects, B reviews, human publishes, and operators A and C receive the exact corrected fact", async ({ browser }) => {
    test.setTimeout(240_000);
    if (env.SUPERVISOR_E2E_AGENT !== "pilot") throw new Error("BLOCKED: CASE=feedback requires the pilot fixture API/worker with chat answering and SUPERVISOR_E2E_AGENT=pilot; use an isolated freshly seeded site.");
    const pages: Page[] = [];
    async function login(user: string) { const page = await (await browser.newContext()).newPage(); pages.push(page); await signIn(page, user); await openSite(page); return page; }
    try {
        const owner = await login("foundation-reviewer");
        const a = await login("operator-1"); const b = await login("shift-approver"); const c = await login("operator-2");
        const original = await source(owner, "The WIS703 press cycle time is 60 seconds.", "WIS703 original standard");
        const corrected = await source(owner, "The WIS703 press cycle time is 45 seconds.", "WIS703 corrected standard");
        const entity = randomUUID(), claim = randomUUID();
        await ok(owner, "POST", "/entities", { entity_id: entity, type: "operation", display_name: "WIS703 press", source_id: original.source, source_revision: 1 }, 0);
        await ok(owner, "POST", "/claims", { claim_id: claim, subject_id: entity, subject_revision: 1, predicate: "cycle_time_seconds", literal: { type: "decimal", value: "60", unit: "s" }, source_span_ids: [original.span], evidence_class: "report", effective_from: "2026-01-01T00:00:00Z" }, 0);
        await ok(b, "POST", `/claims/${claim}/reviews`, { claim_revision: 1, verdict: "approve", reason: "Original synthetic standard" }, 1);
        const inv = await ok(owner, "GET", "/knowledge-inventory");
        await ok(owner, "POST", "/process-snapshots", { snapshot_id: randomUUID(), reason: "Publish original synthetic standard" }, inv.revision);
        const old = await ask(a, "60"); expect(old.reply.corrections ?? []).toHaveLength(0);
        await a.getByRole("button", { name: "This is wrong" }).click();
        await expect(a.getByText("This shares only the selected question and answer", { exact: false })).toBeVisible();
        await a.getByRole("textbox", { name: "Correction reason", exact: true }).fill("The verified standard is 45 seconds.");
        await a.getByLabel("Expected factual answer (optional)").fill("The WIS703 press cycle time is 45 seconds.");
        await a.getByLabel("Add a proposed fact and source").check(); await a.getByRole("combobox", { name: "Fact to correct", exact: true }).selectOption("existing");
        await a.getByRole("combobox", { name: "Approved fact", exact: true }).selectOption(claim);
        await a.getByRole("textbox", { name: "Correct value (exact decimal)" }).fill("45");
        await a.getByRole("combobox", { name: "Evidence source", exact: true }).selectOption(corrected.source);
        const submission = a.waitForResponse((r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/feedback"));
        await a.getByRole("button", { name: "Submit correction" }).click();
        const submitted = await submission; expect(submitted.status()).toBe(201); const feedback = await submitted.json(); const candidate = feedback.candidate_id;
        await expect(a.getByRole("article", { name: `Correction ${candidate.slice(0, 8)}` })).toBeVisible();
        expect((await call(a, "POST", `/memory-candidates/${candidate}/review`, { verdict: "approve", reason: "Operator cannot approve", expected_revision: 1 }, 1)).status).toBe(403);
        expect((await call(b, "GET", `/chat-conversations/${old.conversation}/messages`)).status).toBe(404);
        await section(b, "Facts to review"); const card = b.getByRole("article", { name: `Correction ${candidate.slice(0, 8)}` });
        await expect(card).toContainText("60 seconds"); await expect(card).toContainText("45 s");
        await card.getByLabel("Correction review reason").fill("Checked against the current standard"); await card.getByRole("button", { name: "Approve correction" }).click();
        await expect(card).toContainText("Approved — awaiting snapshot");
        const stillOld = await ask(c, "60"); expect(stillOld.reply.corrections ?? []).toHaveLength(0);
        await section(owner, "Approved process"); await owner.getByLabel("Snapshot reason").fill("Publish reviewed WIS703 correction");
        await owner.getByRole("button", { name: "Publish snapshot", exact: true }).click(); await expect(owner.getByRole("status").filter({ hasText: /\S/ })).toContainText("published at knowledge revision");
        for (const reader of [a, c]) {
            const answer = await ask(reader, "45");
            expect(answer.reply.claim_citations).toContainEqual(expect.objectContaining({ claim_id: claim, revision: 2 }));
            expect(answer.reply.corrections).toContainEqual(expect.objectContaining({ claim_id: claim, revision: 2 }));
            await expect(reader.getByText("updated after a correction by", { exact: false })).toBeVisible();
            await expect(reader.getByText("1 corrections approved this month", { exact: false })).toBeVisible({ timeout: 45000 });
        }
        const saved = await ok(owner, "GET", `/memory-candidates/${candidate}`); expect(saved.published_snapshot_id).toBeTruthy();
        const cases = await ok(owner, "GET", "/correction-evaluation-cases"); expect(cases.cases.filter((x: any) => x.candidate_id === candidate)).toHaveLength(1);
        const selection = { schema_version: 1, tenant_id: "00000000-0000-4000-8000-000000000290", site_id: site, snapshot_id: saved.published_snapshot_id, case_ids: cases.cases.filter((x: any) => x.candidate_id === candidate).map((x: any) => x.case_id) };
        await test.info().attach("correction-evaluation-selection", { body: JSON.stringify(selection), contentType: "application/json" });
        if (env.SUPERVISOR_EVAL_CORRECTION_SET_FILE && env.SUPERVISOR_EVAL_CORRECTION_SESSION_FILE) {
            const cookie = (await owner.context().cookies()).find((c) => c.name === "supervisor");
            if (!cookie) throw new Error("Owner session cookie missing for correction evaluation");
            await writeFile(env.SUPERVISOR_EVAL_CORRECTION_SESSION_FILE, cookie.value, { mode: 0o600 });
            await writeFile(env.SUPERVISOR_EVAL_CORRECTION_SET_FILE, JSON.stringify(selection), { mode: 0o600 });
        }
    } finally { for (const page of pages) await page.context().close(); }
});
