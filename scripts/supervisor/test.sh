#!/usr/bin/env bash
set -euo pipefail
mode=${1:-unit}
case "$mode" in
 unit) ./scripts/supervisor/untested-packages.sh; args=(-race ./...);;
 integration)
  : "${SUPERVISOR_TEST_ADMIN_URL:?required disposable PostgreSQL admin URL}"
  : "${SUPERVISOR_TEST_DATABASE_URL:?required restricted PostgreSQL URL}"
  case "${CASE:-all}" in
   all) : "${SUPERVISOR_TEST_TEMPORAL_ADDRESS:?required actual Temporal service}"; packages=(./cmd/supervisor ./internal/store ./internal/identity ./internal/dispatch ./internal/migrate ./internal/bootstrap ./internal/httpapi ./internal/membership ./internal/evidence ./internal/knowledge ./internal/operational ./internal/ingestion ./internal/plans ./internal/plantcontext ./internal/canonical ./internal/oeeservice ./internal/approvals ./internal/roster ./internal/cases ./internal/onboarding ./internal/contracts ./internal/agent ./internal/chat ./internal/feedback ./internal/workstation ./cmd/supervisor-load ./cmd/supervisor-eval ./cmd/supervisor-demo-seed ./internal/pilot);;
   agent|WIS-275) packages=(./internal/agent ./internal/migrate ./internal/evidence ./internal/knowledge);;
   eval|WIS-280) packages=(./cmd/supervisor-eval);;
   knowledge|WIS-270) packages=(./internal/knowledge ./internal/httpapi ./internal/evidence);;
   ingestion|WIS-272) packages=(./internal/production ./internal/canonical ./internal/plans ./internal/plantcontext ./internal/operational ./internal/ingestion ./internal/migrate ./internal/httpapi);;
   evidence) packages=(./internal/evidence ./internal/httpapi);;
   membership) packages=(./internal/membership ./internal/httpapi);;
   bootstrap) packages=(./internal/bootstrap ./internal/httpapi);;
   isolation) packages=(./internal/store);;
   identity) packages=(./internal/identity);;
   worker-crash) : "${SUPERVISOR_TEST_TEMPORAL_ADDRESS:?required actual Temporal service}"; packages=(./internal/dispatch);;
   dispatch|WIS-290) : "${SUPERVISOR_TEST_TEMPORAL_ADDRESS:?required actual Temporal service}"; packages=(./internal/dispatch ./internal/migrate ./internal/store ./cmd/supervisor ./internal/httpapi);;
   migration) packages=(./internal/migrate);;
   cases|WIS-273) : "${SUPERVISOR_TEST_TEMPORAL_ADDRESS:?required actual Temporal service}"; packages=(./internal/cases ./internal/agent ./internal/migrate ./internal/httpapi ./internal/dispatch ./cmd/supervisor-demo-seed);;
   oee|WIS-229) packages=(./internal/oee ./internal/oeeservice ./internal/migrate ./internal/httpapi);;
   proposals|WIS-274) packages=(./internal/approvals ./internal/migrate ./internal/httpapi);;
   tasks|execution|WIS-277) packages=(./internal/tasks ./internal/outcome ./internal/cases ./internal/approvals ./internal/agent ./internal/migrate ./internal/httpapi ./internal/contracts ./internal/dispatch);;
   value-stream|WIS-611) packages=(./internal/knowledge ./internal/migrate ./internal/contracts);;
   # WIS-703 corrections are gated here too: memory review, chat, agent and the regression-case runner.
   feedback|WIS-279|WIS-703) packages=(./internal/feedback ./internal/migrate ./internal/httpapi ./internal/contracts ./internal/knowledge ./internal/chat ./internal/agent ./cmd/supervisor-eval);;
   recovery|roster|WIS-276) packages=(./internal/roster ./internal/agent ./internal/migrate ./internal/httpapi ./internal/contracts ./internal/recovery);;
   interviews|WIS-271) packages=(./internal/agent ./internal/dispatch ./internal/onboarding ./internal/migrate ./internal/httpapi ./internal/contracts ./internal/knowledge ./internal/evidence);;
   workstation|WIS-520) packages=(./internal/workstation ./internal/migrate ./internal/httpapi);;
   master_data) packages=(./internal/workstation ./internal/migrate ./internal/httpapi ./internal/contracts);;
   drafts|configuration_drafts) packages=(./internal/workstation ./internal/migrate ./internal/httpapi ./internal/contracts ./internal/evidence);;
   agentdrafts) packages=(./internal/agent ./internal/dispatch ./internal/onboarding ./internal/migrate ./internal/httpapi ./internal/contracts ./internal/knowledge ./internal/evidence ./internal/workstation);;
   chat|WIS-517) packages=(./internal/chat ./internal/agent ./internal/dispatch ./internal/config ./internal/migrate ./internal/httpapi ./internal/contracts);;
   provision) packages=(./cmd/supervisor ./internal/migrate ./internal/bootstrap ./internal/httpapi);;
   factory-sim) packages=(./cmd/supervisor-factory-sim);;
   load) packages=(./cmd/supervisor-load);;
   onboarding-pilot|WIS-631) packages=(./internal/pilot);;
   onboarding-pilot-outcome) packages=(./internal/pilot);;
   *) echo 'BLOCKED: unknown foundation CASE' >&2;exit 2;;
  esac
  integration_tags=integration
  if [[ "${SUPERVISOR_TEST_CPSAT:-0}" == 1 ]]; then integration_tags=integration,cpsat; fi
  # The measured pilot outcome waits for real post-completion windows (32-50 minutes).
  if [[ ${CASE:-all} == onboarding-pilot-outcome ]]; then integration_tags=integration,pilotoutcome; fi
  args=(-race -tags="$integration_tags" "${packages[@]}")
  if [[ ${CASE:-all} == factory-sim && ${SUPERVISOR_FACTORY_SIM_14_DAYS:-0} == 1 ]]; then
   args=(-timeout=30m "${args[@]}")
  fi
  if [[ ${CASE:-all} == onboarding-pilot-outcome ]]; then
   args=(-timeout=70m -run 'TestWIS631PilotOutcomeMeasured' "${args[@]}")
  fi;;
 *) echo 'Unknown gate' >&2;exit 2;;
esac
report=$(mktemp)
trap 'rm -f "$report"' EXIT
go test -count=1 -json "${args[@]}" | tee "$report"
# Required gates cannot pass through test skips or an empty selector.
awk '/"Action":"skip".*"Test":/{bad=1} /"Action":"pass".*"Test":/{passed++} END{if(bad||!passed)exit 1}' "$report"
# The race build has a looser PDF sandbox (pdf_confine_race.go); the production image is static
# (CGO_ENABLED=0), so its stricter sandbox is tested in that build too.
if [[ "$mode" == unit ]]; then
  CGO_ENABLED=0 go test -count=1 -run 'TestPDF|TestScan' ./internal/onboarding/
fi
