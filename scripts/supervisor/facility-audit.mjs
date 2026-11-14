// Records which provider actually served the AC2/AC3 phase, from the server's own run and call
// rows (not a client label), and fails unless every run and call in that window is the pinned
// local model with at most one call in flight. Read-only: SELECTs through psql. It waits for
// outstanding local work to finish first, so the model lock is never released under a live call.
import fs from 'node:fs';
import cp from 'node:child_process';

const root = process.env.FACILITY_ARTIFACTS, container = process.env.FACILITY_PG_CONTAINER;
if (!root?.startsWith('/var/tmp/supervisor-facility/runs/') || root.split('/').includes('..') || !/^[0-9a-f]{12,64}$/.test(container ?? '')) {
  throw new Error('BLOCKED: facility artifacts and PostgreSQL container required');
}
const MODEL = 'unsloth/Qwen3.5-9B-GGUF:Q4_K_M';
const REVISION = 'sha256:03b74727a860a56338e042c4420bb3f04b2fec5734175f4cb9fa853daf52b7e8';
const TENANT = '00000000-0000-4000-8000-000000000290', SITE = '00000000-0000-4000-8000-000000000291';
const provenance = JSON.parse(fs.readFileSync(`${root}/local-model-provenance.json`, 'utf8'));
const start = new Date(provenance.started_at).toISOString();
const psql = sql => cp.execFileSync('docker', ['exec', container, 'psql', '-X', '-U', 'postgres', '-d', 'supervisor', '-At', '-v', 'ON_ERROR_STOP=1', '-c', sql], { encoding: 'utf8' }).trim();
const failures = [];
// Outstanding local work: a running local run or an unfinished actual call at this site.
const outstanding = () => Number(psql(`SELECT (SELECT count(*) FROM supervisor.agent_runs WHERE tenant_id='${TENANT}' AND site_id='${SITE}' AND state='running' AND provider_name='local-openai-compatible')+(SELECT count(*) FROM supervisor.agent_model_calls WHERE tenant_id='${TENANT}' AND site_id='${SITE}' AND provider_actual AND ended_at IS NULL)`));
const settleBy = Date.now() + 300_000;
let open = outstanding();
while (open > 0 && Date.now() < settleBy) { cp.execFileSync('sleep', ['2']); open = outstanding(); }
if (open > 0) failures.push(`${open} local runs or calls still outstanding after 300 s`);
const end = new Date().toISOString();
// Every run and call whose interval overlaps the window, including work already running when it opened.
const overlaps = `started_at<'${end}' AND (ended_at IS NULL OR ended_at>'${start}')`;
const sql = `SELECT jsonb_build_object(
 'runs',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY x.started_at),'[]') FROM (SELECT r.id,r.request_kind,r.case_id,r.conversation_id,r.provider_name,r.provider_actual,r.model,r.model_revision,r.limit_profile_id,r.state,r.outcome_status,r.outcome_reasons,r.started_at,r.ended_at,t.reply_message_id FROM supervisor.agent_runs r LEFT JOIN supervisor.chat_answer_turns t ON (t.tenant_id,t.site_id,t.run_id)=(r.tenant_id,r.site_id,r.id) WHERE r.tenant_id='${TENANT}' AND r.site_id='${SITE}' AND r.${overlaps}) x),
 'calls',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY x.started_at),'[]') FROM (SELECT run_id,seq,attempt,provider_actual,model,model_revision,input_tokens,output_tokens,wall_ms,status,reason_code,started_at,ended_at FROM supervisor.agent_model_calls WHERE tenant_id='${TENANT}' AND site_id='${SITE}' AND ${overlaps}) x))`;
const rows = JSON.parse(psql(sql));
const read = name => { try { return JSON.parse(fs.readFileSync(`${root}/${name}`, 'utf8')); } catch { return null; } };
const manuals = read('manuals/manual-upload-report.json'), scenarios = read('scenarios/scenario-report.json');
// Bind the measured phases to this window: they ran on the very containers local-model started,
// the scenario run began inside the window, and the chat runs they caused are audited local runs.
const containers = name => { const e = read(name); return e ? `${e.api?.container}/${e.worker?.container}` : null; };
const local = containers('local-model-runtime-env.json');
for (const phase of ['manuals', 'scenarios']) {
  if (!local || containers(`${phase}-runtime-env.json`) !== local) failures.push(`${phase} did not run on the containers local-model verified`);
}
if (!scenarios || !(Date.parse(scenarios.started_at) >= Date.parse(start))) failures.push('scenario run did not start inside the local window');
const scenarioReplies = (scenarios?.results ?? []).map(r => r.observed?.evidence?.chat_observation?.reply?.message_id).filter(Boolean);
for (const id of scenarioReplies) {
  if (!rows.runs.some(r => r.reply_message_id === id)) failures.push(`scenario chat reply ${id} has no audited local run`);
}
for (const r of rows.runs) {
  if (r.provider_name !== 'local-openai-compatible' || r.provider_actual !== true || r.model !== MODEL || r.model_revision !== REVISION) failures.push(`run ${r.id} (${r.request_kind}) used ${r.provider_name} ${r.model} ${r.model_revision}`);
}
let active = 0, max = 0;
const points = [];
for (const c of rows.calls) {
  if (c.provider_actual !== true || c.model !== MODEL || c.model_revision !== REVISION) failures.push(`call ${c.run_id}/${c.seq} used ${c.model} ${c.model_revision}`);
  if (!c.ended_at) failures.push(`call ${c.run_id}/${c.seq} has no end`);
  points.push([Date.parse(c.started_at), 1], [Date.parse(c.ended_at ?? end), -1]);
}
points.sort((a, b) => a[0] - b[0] || a[1] - b[1]);
for (const [, d] of points) { active += d; max = Math.max(max, active); }
if (max > 1) failures.push(`${max} model calls overlapped; the pin allows one`);
const manualRun = rows.runs.find(r => manuals?.reply?.message_id && r.reply_message_id === manuals.reply.message_id) ?? null;
if (!manualRun) failures.push('no server run answered the AC3 question in the local window');
if (!rows.runs.some(r => r.request_kind === 'conversation')) failures.push('no conversation run in the local window');
const audit = {
  schema_version: 1,
  passed: failures.length === 0,
  failures,
  classification: 'measured pinned-local-model synthetic plant; not qualification',
  window: { start, end },
  model: MODEL,
  model_revision: REVISION,
  model_revision_source: 'configured pin, recorded on each server run; the endpoint reports no weight digest',
  max_overlapping_calls: max,
  manual_reply_run: manualRun,
  scenario_reply_runs: rows.runs.filter(r => scenarioReplies.includes(r.reply_message_id)).map(r => r.id),
  // Case runs come from production-shortfall case detection, not from AC2 or AC3.
  case_runs_note: 'request_kind=case runs are investigations started by case detection on loaded plant data; they are recorded here because they ran on the local model, not because AC2 or AC3 asked for them',
  runs: rows.runs,
  calls: rows.calls,
};
fs.writeFileSync(`${root}/local-model-audit.json`, JSON.stringify(audit, null, 2) + '\n', { mode: 0o600 });
console.log(JSON.stringify({ passed: audit.passed, failures, runs: rows.runs.length, calls: rows.calls.length, max_overlapping_calls: max, manual_reply_run: manualRun?.id ?? null }, null, 2));
if (!audit.passed) process.exitCode = 1;
