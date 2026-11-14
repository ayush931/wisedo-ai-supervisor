// WIS-709 PR #304 review: AC2 comes from the 20 product results alone; the gauge self-check is
// separate and still fails the run. Run: node --test scripts/supervisor/facility-report.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { summarize, exists } from './facility-report.mjs';

const result = (id, passed) => ({ expected: { id }, observed: {}, passed, ...(passed ? {} : { failure: 'differs' }) });
function artifacts({ product = true, gauge = true, audit = true, sessions = false, fixtureCase = true } = {}) {
  const files = {
    'live/run-report.json': { passed: true, shift_runs: Array(18).fill({}) },
    'manuals/manual-upload-report.json': { passed: true },
    // scenario-report "passed" aggregates both lists, as the Go runner writes it.
    'scenarios/scenario-report.json': {
      passed: product && gauge,
      results: Array.from({ length: 19 }, (_, i) => result(`case-${i}`, product || i !== 7)),
      facility_model_checks: [result('overdue-gauge', gauge)],
      product_gaps: [],
      excluded: ['revoked-document'],
    },
    ...(fixtureCase ? { 'scenarios-fixture/scenario-report.json': { passed: true, results: [result('revoked-document', true)], facility_model_checks: [], excluded: [] } } : {}),
    'local-model-audit.json': { passed: audit, manual_reply_run: { provider_actual: audit } },
    'scripted/scripted-report.json': { passed: true, unexpected_errors: [] },
    'exploratory/findings.json': { gate_evidence: false, personas: [{}, {}] },
  };
  if (sessions) files['auth/sessions.json'] = { owner: 'cookie' };
  return name => files[name] ?? null;
}
const none = () => false;

test('all phases pass', () => {
  const r = summarize(artifacts(), none);
  assert.equal(r.passed, true);
  assert.ok(Object.values(r.checks).every(Boolean));
});

test('product cases pass and the gauge check fails: AC2 passes, the run fails', () => {
  const r = summarize(artifacts({ gauge: false }), none);
  assert.equal(r.checks.edges, true);
  assert.equal(r.checks.facility_model_checks, false);
  assert.equal(r.passed, false);
  assert.deepEqual(r.facility_failures.map(f => f.id), ['overdue-gauge']);
  assert.deepEqual(r.edge_failures, []);
});

test('a failed product case fails AC2 even when the gauge passes', () => {
  const r = summarize(artifacts({ product: false }), none);
  assert.equal(r.checks.edges, false);
  assert.equal(r.checks.facility_model_checks, true);
  assert.deepEqual(r.edge_failures.map(f => f.id), ['case-7']);
});

test('AC2/AC3 not verified on the local provider fails the run', () => {
  const r = summarize(artifacts({ audit: false }), none);
  assert.equal(r.checks.local_provider, false);
  assert.equal(r.passed, false);
});

test('a session file left behind fails the run', () => {
  const r = summarize(artifacts(), name => name === 'auth/sessions.json');
  assert.equal(r.checks.sessions_removed, false);
  assert.equal(r.passed, false);
});

test('AC2 fails when the fixture-provider case report is missing', () => {
  const r = summarize(artifacts({ fixtureCase: false }), none);
  assert.equal(r.checks.edges, false);
  assert.equal(r.passed, false);
});

// PR #304 second review: the session check looks at the file system, not at parsed JSON.
test('malformed, temporary or unreadable session leftovers fail; absence passes', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'facility-report-'));
  try {
    fs.mkdirSync(path.join(root, 'auth'));
    const check = () => summarize(artifacts(), name => exists(root, name)).checks.sessions_removed;
    assert.equal(check(), true);
    fs.writeFileSync(path.join(root, 'auth/sessions.json'), '{not json');
    assert.equal(check(), false, 'malformed leftover');
    fs.rmSync(path.join(root, 'auth/sessions.json'));
    fs.writeFileSync(path.join(root, 'auth/.sessions.json'), '{}');
    assert.equal(check(), false, 'temporary session file');
    fs.rmSync(path.join(root, 'auth/.sessions.json'));
    // A name that cannot be stat'ed for a reason other than ENOENT counts as present.
    fs.writeFileSync(path.join(root, 'auth-file'), 'x');
    assert.equal(exists(root, 'auth-file/sessions.json'), true, 'ENOTDIR is not absence');
  } finally { fs.rmSync(root, { recursive: true, force: true }); }
});
