//go:build integration

package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestChatPassageProbeCountsRankerRows (WIS-626 review): the probe's foreign-span and equivalence
// checks read the ranking queries' rows, not the selected passages. Every claim here cites 20
// spans, so nothing is ever selected; when the before query loses its tenant/site filter (run as
// a role that bypasses RLS) the 20 foreign spans it returns must still be counted, and its rows
// must no longer match the indexed query's.
func TestChatPassageProbeCountsRankerRows(t *testing.T) {
	admin := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if admin == "" {
		t.Fatal("BLOCKED: SUPERVISOR_TEST_ADMIN_URL is required (real PostgreSQL)")
	}
	m := PassageManifest{SchemaVersion: 1, Kind: "chat-passages", Issue: "WIS-626", Seed: 626, Claims: 2, SmallClaimSpans: 20, LargeClaimSpans: 20,
		ForeignTenantSpans: 20, Queries: 2, Questions: []string{"What temperature does the curing oven run at?"}}
	run := func() PassageReport {
		t.Helper()
		var r PassageReport
		if err := measurePassages(context.Background(), m, admin, admin, &r); err != nil {
			t.Fatal(err)
		}
		if r.Errors != 0 || r.PassagesSelected != 0 {
			t.Fatalf("errors %d, passages selected %d", r.Errors, r.PassagesSelected)
		}
		return r
	}
	if r := run(); r.CrossTenantRanked != 0 || r.RankRowMismatches != 0 || r.SelectionMismatches != 0 {
		t.Fatalf("intact rankings: foreign %d, row mismatches %d, selection mismatches %d", r.CrossTenantRanked, r.RankRowMismatches, r.SelectionMismatches)
	}
	intact := legacyRankSQL
	t.Cleanup(func() { legacyRankSQL = intact })
	broken := strings.Replace(intact, `WHERE s.tenant_id=$1 AND s.site_id=$2 AND`, `WHERE ($1::uuid,$2::uuid) IS NOT NULL AND`, 1)
	if broken == intact {
		t.Fatal("scope filter not found in legacyRankSQL")
	}
	legacyRankSQL = broken
	r := run()
	if r.CrossTenantRanked != 20 {
		t.Errorf("scope-broken ranking: %d foreign spans counted, want 20", r.CrossTenantRanked)
	}
	if r.RankRowMismatches != 1 {
		t.Errorf("scope-broken ranking: %d rank row mismatches, want 1", r.RankRowMismatches)
	}
}

// TestWIS700ChatPassageProbeTimesNameMatching: the probe's claims each name a subject entity, so
// the WIS-646 name-matching query (chat.ClaimTermsSQL) runs at the fixture's claim count, is timed
// once per query and explained, and name matches are counted from names and aliases alone. It used
// to pass no entities. The question's words ("station", "cell", "20") occur in no passage and no
// predicate, only in entity names, so what the timed selection reads depends on the entities it
// is given: with them, claim 20 ("... cell 20") is selected first; without, claims are taken by ID
// alone (PR #303 review: the test must fail if the timed call gets nil entities).
func TestWIS700ChatPassageProbeTimesNameMatching(t *testing.T) {
	admin := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if admin == "" {
		t.Fatal("BLOCKED: SUPERVISOR_TEST_ADMIN_URL is required (real PostgreSQL)")
	}
	m := PassageManifest{SchemaVersion: 1, Kind: "chat-passages", Issue: "WIS-700", Seed: 700, Claims: 30, SmallClaimSpans: 2, LargeClaimSpans: 4,
		ForeignTenantSpans: 4, Queries: 2, Questions: []string{"Which station is cell 20?", "What is the objective?"}}
	var r PassageReport
	if err := measurePassages(context.Background(), m, admin, admin, &r); err != nil {
		t.Fatal(err)
	}
	if r.Errors != 0 || r.SelectionMismatches != 0 || r.ClaimTerms.Count != m.Queries || len(r.ClaimTermsPlan) == 0 {
		t.Fatalf("errors %d, selection mismatches %d, claim terms timed %d, plan %q", r.Errors, r.SelectionMismatches, r.ClaimTerms.Count, r.ClaimTermsPlan)
	}
	// "station" and "cell" are in all 30 subjects' aliases and names ("<word> station",
	// "<word> <word> cell <n>"): 30 name matches. "objective" is every claim's predicate and in no
	// name, so it adds none; counting predicate words as names would give 60.
	if r.NameMatches != 30 || r.NameChangedSelections < 1 {
		t.Fatalf("name matches %d (want 30), selections changed by names %d (want at least 1)", r.NameMatches, r.NameChangedSelections)
	}
}
