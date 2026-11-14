import fs from 'node:fs';
import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';

// summarize derives every acceptance check from the phase reports. It is pure so that
// facility-report.test.mjs can pin each check's inputs (WIS-709 review, PR #304).
// exists reports whether an artifact file is present; any error other than "not found" counts as
// present, so an unreadable leftover cannot pass the session check.
export function exists(root, name) {
  try { fs.lstatSync(`${root}/${name}`); return true; }
  catch (e) { return e?.code !== 'ENOENT'; }
}

export function summarize(read, present = () => false) {
  const live = read('live/run-report.json');
  const manuals = read('manuals/manual-upload-report.json');
  const edges = read('scenarios/scenario-report.json');
  // revoked-document runs on the fixture provider (deterministic first citation); merge it in.
  const fixtureEdges = read('scenarios-fixture/scenario-report.json');
  const audit = read('local-model-audit.json');
  const scripted = read('scripted/scripted-report.json');
  const exploratory = read('exploratory/findings.json');
  const product = [...(edges?.results ?? []), ...(fixtureEdges?.results ?? [])];
  const productIDs = new Set(product.map(r => r.expected?.id));
  const facility = edges?.facility_model_checks ?? [];
  const checks = {
    live: live?.passed === true && live.shift_runs?.length === 18,
    manuals: manuals?.passed === true,
    // AC2 is the 20 product edge cases alone; scenario-report "passed" also covers facility checks.
    edges: product.length === 20 && productIDs.size === 20 && product.every(r => r.passed === true)
      && (edges?.excluded ?? []).every(id => fixtureEdges?.results?.some(r => r.expected.id === id)),
    // Facility self-checks test simulator code; they are outside AC2 but still fail the run.
    facility_model_checks: facility.length === 1 && facility.every(r => r.passed === true),
    // AC3 and the local AC2 cases ran on the pinned local model, verified from the server's own run records.
    local_provider: audit?.passed === true && audit.manual_reply_run?.provider_actual === true,
    scripted: scripted?.passed === true && scripted.unexpected_errors?.length === 0,
    exploratory_report: !!exploratory && !exploratory.failure && exploratory.gate_evidence === false && exploratory.personas?.length === 2,
    // Raw session cookies never outlive the phase that needed them.
    sessions_removed: !present('auth/sessions.json') && !present('auth/.sessions.json'),
  };
  return {
    schema_version: 1,
    passed: Object.values(checks).every(Boolean),
    checks,
    provider: audit ? { local_phase: audit.window, model: audit.model, model_revision: audit.model_revision, actual_runs: audit.runs?.length, actual_calls: audit.calls?.length, max_overlapping_calls: audit.max_overlapping_calls, failures: audit.failures } : null,
    manual_failure: checks.manuals ? null : {
      failure: manuals?.failure || 'Missing or incomplete manual report',
      question: manuals?.question,
      expected_value: manuals?.expected_value,
      expected_span: manuals?.expected_span,
      reply: manuals?.reply,
    },
    edge_failures: product.filter(r => !r.passed).map(r => ({ id: r.expected.id, expected: r.expected, observed: r.observed, failure: r.failure })),
    facility_failures: facility.filter(r => !r.passed).map(r => ({ id: r.expected.id, failure: r.failure })),
    product_gaps: edges?.product_gaps || [],
    limits: [
      'Scripted identities are not independent human review.',
      'Accelerated model day is not 24 elapsed real hours.',
      'AC3 and 19 AC2 cases are measurements of the pinned local model on a synthetic plant, not qualification; revoked-document runs on the fixture provider, whose first citation is deterministic.',
      'Exploratory model findings never count as product gate evidence.',
      'Repository make gates are recorded separately.',
    ],
  };
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  const root = process.env.FACILITY_ARTIFACTS;
  assert(root?.startsWith('/var/tmp/supervisor-facility/runs/') && !root.split('/').includes('..'));
  const read = name => {
    try { return JSON.parse(fs.readFileSync(`${root}/${name}`, 'utf8')); }
    catch { return null; }
  };
  const report = { ...summarize(read, name => exists(root, name)), artifacts: root };
  fs.writeFileSync(`${root}/acceptance-report.json`, JSON.stringify(report, null, 2) + '\n', {mode: 0o600});
  fs.writeFileSync(`${root}/acceptance-report.md`, [
    '# Facility acceptance', `Passed: ${report.passed}`,
    ...Object.entries(report.checks).map(([k, v]) => `- ${k}: ${v ? 'PASS' : 'FAIL'}`),
    '', ...(report.provider ? [`AC2/AC3 provider: ${report.provider.model} (${report.provider.model_revision}); ${report.provider.actual_runs} actual runs, ${report.provider.actual_calls} calls, at most ${report.provider.max_overlapping_calls} overlapping.`] : ['AC2/AC3 provider: no audit']),
    '', ...(report.manual_failure ? [`Manual failure: ${report.manual_failure.failure}`] : []),
    ...report.edge_failures.map(r => `Edge failure ${r.id}: ${r.failure}`),
    ...report.facility_failures.map(r => `Facility check failure ${r.id}: ${r.failure}`),
    '', '## Product gaps (reported, not gated)', ...report.product_gaps.map(g => `- ${g}`),
    '', ...report.limits,
  ].join('\n') + '\n', {mode: 0o600});
  console.log(JSON.stringify({passed: report.passed, checks: report.checks, edge_failures: report.edge_failures.map(r => r.id), artifacts: root}, null, 2));
  if (!report.passed) process.exitCode = 1;
}
