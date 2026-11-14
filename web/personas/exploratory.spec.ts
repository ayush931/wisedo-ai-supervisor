import { test, type Locator, type Page } from "@playwright/test";
import { spawn, execFile } from "node:child_process";
import { promisify } from "node:util";
import { createHash } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import { login, origin, root, save, site, users } from "./helpers";

type Choice = { id: string; kind: "observe" | "finish" | "navigate" | "click"; label: string };
type Decision = { action_id: string; observation: string; expected_behavior: string; finding: boolean; confidence: "low" | "medium" | "high" };
type ModelReply = { decision?: Decision; refused?: string; model: string; endpoint_model: Record<string, unknown>; model_revision: string; model_revision_source: string; prompt_sha256: string; request_sha256: string; input_tokens: number; output_tokens: number; wall_millis: number; provider_actual: string; qualification: false };
const hash = (b: string | Buffer) => createHash("sha256").update(b).digest("hex");
const binary = process.env.FACILITY_SIM_BINARY || "";

function modelStep(input: unknown): Promise<ModelReply> {
    return new Promise((resolve, reject) => {
        // Fixed executable/argv; model text is never shell code or an executable path.
        const child = spawn(binary, ["persona-step"], { stdio: ["pipe", "pipe", "pipe"] });
        let stdout = "", stderr = "";
        const timer = setTimeout(() => child.kill("SIGKILL"), 50_000);
        child.stdout.on("data", chunk => { stdout += chunk; if (stdout.length > 20_000) child.kill("SIGKILL"); });
        child.stderr.on("data", chunk => { stderr = (stderr + chunk).slice(0, 2000); });
        child.on("error", e => { clearTimeout(timer); reject(e); });
        child.on("close", code => {
            clearTimeout(timer);
            if (code !== 0) return reject(new Error(`BLOCKED: local persona request failed (${code}): ${stderr}`));
            try { resolve(JSON.parse(stdout)); } catch { reject(new Error("Invalid persona helper response")); }
        });
        child.stdin.end(JSON.stringify(input));
    });
}

async function offered(page: Page) {
    const choices: Choice[] = [{ id: "observe", kind: "observe", label: "Observe this screen" }, { id: "finish", kind: "finish", label: "Finish and report limits" }];
    const controls = new Map<string, Locator>();
    const links = page.getByRole("link");
    for (let i = 0; i < await links.count() && choices.length < 50; i++) {
        const link = links.nth(i), href = await link.getAttribute("href"), label = (await link.innerText()).trim();
        if (!href || !label || label.length > 150 || !await link.isVisible()) continue;
        const url = new URL(href, origin);
        if (url.origin !== origin || !url.pathname.startsWith(`/sites/${site}/`) || url.search || url.hash) continue;
        const id = `link-${i}`; choices.push({ id, kind: "navigate", label }); controls.set(id, link);
    }
    const buttons = page.getByRole("button");
    for (let i = 0; i < await buttons.count() && choices.length < 62; i++) {
        const button = buttons.nth(i), label = (await button.innerText()).trim();
        // Exact read controls only. Approve, publish, record, end, submit, and chat are never offered.
        if (!/^(Refresh now|Details|History(?: \(\d+\))?(?: H)?|Load more records|Load more shifts|Next|Back)$/.test(label) || !await button.isVisible() || !await button.isEnabled()) continue;
        const id = `button-${i}`; choices.push({ id, kind: "click", label }); controls.set(id, button);
    }
    return { choices, controls };
}

test("WIS-709 local LLM personas produce exploratory leads only", async ({ browser }) => {
    test.setTimeout(10 * 60_000);
    if (!binary.startsWith("/var/tmp/") || binary.split("/").includes("..")) throw new Error("BLOCKED: FACILITY_SIM_BINARY must be set to a binary under /var/tmp");
    const runPath = process.env.FACILITY_RUN_REPORT;
    if (!runPath?.startsWith("/var/tmp/") || runPath.split("/").includes("..")) throw new Error("BLOCKED: FACILITY_RUN_REPORT from the live run is required");
    const runBytes = await readFile(runPath), run = JSON.parse(runBytes.toString());
    const build = (await promisify(execFile)("git", ["rev-parse", "HEAD"])).stdout.trim();
    const dirty = (await promisify(execFile)("git", ["status", "--porcelain"])).stdout.length > 0;
    const report: { schema_version: number; classification: string; gate_evidence: boolean; seed: number; schedule: unknown; run_report_sha256: string; build_commit: string; working_tree_dirty: boolean; binary_sha256: string; endpoint: string; personas: unknown[]; failure?: string } = {
        schema_version: 1, classification: "exploratory leads, not confirmed defects or independent review", gate_evidence: false, seed: 278,
        schedule: run.schedule, run_report_sha256: hash(runBytes), build_commit: build, working_tree_dirty: dirty,
        binary_sha256: hash(await readFile(binary)), endpoint: "http://127.0.0.1:64601", personas: [],
    };
    await save("findings.json", report);
    try {
        // Sequential model phases plus the Go file lock: one facility request in flight.
        for (const persona of [
            { name: "handover-reader", role: "supervisor", user: users.planner, goal: "Find a completed shift handover. Describe whether its source summary and the shift time are visible. If unavailable, report where you got stuck." },
            { name: "line-investigator", role: "supervisor", user: users.planner, goal: "Find evidence about why the Shaft heat treatment line is slow. Navigate the visible plant pages, and separate reported reasons from measured data. Never guess a cause." },
        ]) {
            const page = await login(browser, persona.user);
            const steps: unknown[] = [], memory: string[] = [], blocked: string[] = [];
            const result = { name: persona.name, role: persona.role, goal: persona.goal, steps, blocked, completion: "step limit", reproduction_confidence: "exploratory: not independently reproduced" };
            report.personas.push(result);
            // Models have no product mutation authority or external network navigation.
            await page.route("**/*", async route => {
                const r = route.request(), url = new URL(r.url());
                if (url.origin !== origin || !["GET", "HEAD"].includes(r.method())) {
                    blocked.push(`${r.method()} ${url.pathname}`); return route.abort("blockedbyclient");
                }
                return route.continue();
            });
            try {
                for (let step = 0; step < 6; step++) {
                    if (!page.url().startsWith(`${origin}/sites/${site}`)) throw new Error("Persona left its authenticated site");
                    const screen = (await page.locator("body").ariaSnapshot()).slice(0, 11_000);
                    const { choices, controls } = await offered(page);
                    const screenshot = `${root}/${persona.name}-${step}.png`;
                    await page.screenshot({ path: screenshot, fullPage: true });
                    const reply = await modelStep({ role: persona.role, goal: persona.goal, screen, memory: memory.slice(-6), choices });
                    if (reply.refused || !reply.decision) {
                        steps.push({ step, path: new URL(page.url()).pathname, screenshot, screenshot_phase: "before_action", ...reply });
                        result.completion = "model output refused; no action executed";
                        await save("findings.json", report); break;
                    }
                    const choice = choices.find(c => c.id === reply.decision!.action_id);
                    if (!choice) throw new Error("Model selected an unoffered control");
                    const entry = { step, path: new URL(page.url()).pathname, screen_sha256: hash(screen), screenshot, screenshot_phase: "before_action", action: choice, ...reply };
                    steps.push(entry);
                    memory.push(`${choice.kind}: ${choice.label}. ${reply.decision.observation}`.slice(0, 580));
                    await save(`${persona.name}-memory.json`, { persona: persona.name, goal: persona.goal, observations: memory.slice(-6), unresolved_leads: steps.slice(-2) });
                    await save("findings.json", report);
                    console.log(`EXPLORATORY ${persona.name} step=${step} action=${choice.kind} tokens=${reply.input_tokens + reply.output_tokens}`);
                    if (choice.kind === "finish") { result.completion = "model finished (unverified)"; break; }
                    if (choice.kind !== "observe") {
                        const control = controls.get(choice.id); if (!control || !await control.isVisible()) throw new Error("Offered control changed before execution");
                        await control.click({ timeout: 5000 });
                    }
                    await page.waitForLoadState("networkidle", { timeout: 5000 }).catch(() => {});
                }
            } finally { await page.context().close(); await save("findings.json", report); }
        }
    } catch (e) { report.failure = String(e); throw e; }
    finally {
        await save("findings.json", report);
        const lines = ["# WIS-709 exploratory persona leads", "", "These model observations are unverified leads, never gate evidence or independent human review.", "", `Build: ${build}; binary SHA-256: ${report.binary_sha256}.`, ""];
        for (const value of report.personas) {
            const p = value as { name: string; goal: string; completion: string; steps: { step: number; action: Choice; screenshot: string; decision?: Decision; refused?: string; model: string; model_revision: string; prompt_sha256: string }[] };
            lines.push(`## ${p.name}`, "", p.goal, "", `Completion: ${p.completion}.`, "");
            for (const s of p.steps) { if (!s.decision) {lines.push(`- Step ${s.step}: model output refused (${s.refused}); no action executed. Screenshot: ${s.screenshot}.`);continue;} lines.push(`- Step ${s.step}: ${s.action.kind} ${JSON.stringify(s.action.label)}. Observation: ${JSON.stringify(s.decision.observation)}. Expected: ${JSON.stringify(s.decision.expected_behavior)}. Model confidence: ${s.decision.confidence}; reproduction confidence: unknown. Screenshot: ${s.screenshot}. Model (endpoint-reported): ${s.model}; revision ${s.model_revision} (configured, not endpoint-reported); prompt: ${s.prompt_sha256}.`); }
            lines.push("");
        }
        if (report.failure) lines.push(`Run failure: ${report.failure}`);
        await writeFile(`${root}/findings.md`, lines.join("\n") + "\n", { mode: 0o600 });
    }
});
