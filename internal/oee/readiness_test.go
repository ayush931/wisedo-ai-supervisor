package oee

import (
	"slices"
	"testing"
)

func TestReadinessPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reasons []ReasonCode
		status  Status
		ordered []ReasonCode
	}{
		{"no reasons", nil, StatusReady, []ReasonCode{}},
		{"missing policy", []ReasonCode{ReasonMissingPolicy}, StatusNotReady, []ReasonCode{ReasonMissingPolicy}},
		{"unsupported rule beats stale source", []ReasonCode{ReasonUnsupportedRule, ReasonStaleSource}, StatusInvalid, []ReasonCode{ReasonStaleSource, ReasonUnsupportedRule}},
		{"invalid quality counts", []ReasonCode{ReasonInvalidQualityCounts}, StatusInvalid, []ReasonCode{ReasonInvalidQualityCounts}},
		{"availability overshoot", []ReasonCode{ReasonAvailabilityOvershoot}, StatusInvalid, []ReasonCode{ReasonAvailabilityOvershoot}},
		{"performance overshoot alone", []ReasonCode{ReasonPerformanceOvershoot}, StatusLimited, []ReasonCode{ReasonPerformanceOvershoot}},
		{"exceeds theoretical alone", []ReasonCode{ReasonExceedsTheoretical}, StatusLimited, []ReasonCode{ReasonExceedsTheoretical}},
		{"limited plus missing data", []ReasonCode{ReasonPerformanceOvershoot, ReasonMissingData}, StatusNotReady, []ReasonCode{ReasonMissingData, ReasonPerformanceOvershoot}},
	} {
		set := map[ReasonCode]struct{}{}
		for _, c := range tc.reasons {
			set[c] = struct{}{}
		}
		got := EvaluateReadiness(set)
		if got.Status != tc.status || !slices.Equal(got.ReasonCodes, tc.ordered) || got.ReasonCodes == nil {
			t.Errorf("%s: got %s %v, want %s %v", tc.name, got.Status, got.ReasonCodes, tc.status, tc.ordered)
		}
	}
}
