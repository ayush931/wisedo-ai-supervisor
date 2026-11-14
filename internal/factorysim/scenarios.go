package factorysim

// ScenarioOutcome is fixed before execution; HTTP and kernel results are separate evidence.
type ScenarioOutcome struct {
	ID        string `json:"id"`
	Layer     string `json:"layer"`
	Status    int    `json:"http_status,omitempty"`
	Code      string `json:"code,omitempty"`
	Readiness string `json:"readiness,omitempty"`
	Assertion string `json:"assertion"`
}

// Scenarios are the 20 product edge cases of acceptance criterion 2. Each gates on an outcome
// the Supervisor decides deterministically; model output is recorded, never gated.
func Scenarios() []ScenarioOutcome {
	return []ScenarioOutcome{
		{"duplicate-batch", "http", 202, "", "", "same receipt, replayed true, no event growth"},
		{"out-of-order-batch", "http", 409, "SOURCE_SEQUENCE_CONFLICT", "", "transport sequence gap refused"},
		{"late-batch", "http", 202, "", "", "old source time retained separately from receipt time"},
		{"late-count-correction", "http", 201, "", "", "ended-run correction names predecessor; original retained, total revised"},
		{"clock-skew", "http", 200, "CLOCK_SUSPECT", "NOT_READY", "future heartbeat fails clock quality"},
		{"counter-reset", "http", 202, "", "", "reset yields 3 and folds without quarantine; unevidenced drop refused by normalization"},
		{"liveness-gap", "http", 200, "HEARTBEAT_STALE", "NOT_READY", "silence beyond source freshness"},
		{"zero-planned-time", "plantcontext kernel", 0, "NO_PLANNED_PRODUCTION", "LIMITED", "expected zero, actual null"},
		{"zero-denominator", "oee kernel", 0, "ZERO_DENOMINATOR", "NOT_READY", "availability has no numeric value"},
		{"missing-actuals", "plantcontext kernel", 0, "MISSING_ACTUALS", "NOT_READY", "actual null"},
		{"overnight-shift", "http", 201, "", "", "plan creation only: dated 22:00 to next-day 06:00 Asia/Kolkata accepted, exactly eight hours; the future run is not started"},
		{"contradictory-interview", "http", 200, "value_conflict", "", "open value_conflict between exactly the two claims retained; the before snapshot holds the manual claim, the after-conflict snapshot holds neither; chat reply is a labelled model observation, not gated"},
		{"encrypted-pdf", "http", 422, "PDF_ENCRYPTED", "", "encrypted bytes refused"},
		{"active-pdf", "http", 415, "PDF_ACTIVE_CONTENT", "", "JavaScript action refused"},
		{"revoked-document", "http", 200, "EVIDENCE_REVOKED", "needs_input", "first answer cites the target span; prior answer then redacted; new answer unavailable/SNAPSHOT_EVIDENCE_UNAVAILABLE with no citations"},
		{"stale-snapshot", "http", 409, "SNAPSHOT_STALE", "", "old graph refuses after knowledge change"},
		{"cross-tenant", "http", 404, "NOT_FOUND", "", "existing and absent first-tenant resources indistinguishable"},
		{"self-approval", "http", 403, "SELF_REVIEW_FORBIDDEN", "", "submitter cannot approve own plan"},
		{"membership-revoked", "http", 404, "NOT_FOUND", "", "operator loses protected access during running shift"},
		{"expired-session", "http", 401, "AUTHENTICATION_REQUIRED", "", "expired session cannot write and leaves no row; the same subject with an unexpired session writes (200)"},
	}
}

// FacilityModelChecks test the simulator's own models, not the Supervisor. They are reported
// separately and are not counted in acceptance criterion 2.
func FacilityModelChecks() []ScenarioOutcome {
	return []ScenarioOutcome{
		{"overdue-gauge", "facility model", 0, "CALIBRATION_OVERDUE", "NOT_READY", "expired gauge reading explicitly held"},
	}
}
