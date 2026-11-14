package chat

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
)

func TestCaptureSnapshotErrorClassification(t *testing.T) {
	storageErr := errors.New("database unavailable")
	for _, tc := range []struct {
		name   string
		err    error
		reason string
		fatal  error
	}{
		{"unavailable pinned evidence", errors.Join(knowledge.ErrSnapshotEvidenceUnavailable, knowledge.ErrNotFound), ReasonSnapshotEvidenceUnavailable, nil},
		{"missing snapshot", knowledge.ErrNotFound, ReasonNoReadableEvidence, nil},
		{"storage failure", storageErr, "", storageErr},
		{"no error", nil, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, fatal := captureSnapshotError(tc.err)
			if reason != tc.reason || (fatal == nil) != (tc.fatal == nil) || tc.fatal != nil && !errors.Is(fatal, tc.fatal) {
				t.Fatalf("got reason=%q fatal=%v", reason, fatal)
			}
		})
	}
}

func TestCaptureSourceErrorFailsClosed(t *testing.T) {
	other := errors.New("storage read failed")
	for _, tc := range []struct {
		name   string
		err    error
		skip   bool
		reason string
		fatal  error
	}{
		{"missing source", evidence.ErrNotFound, true, "", nil},
		{"denied source", evidence.ErrDenied, false, ReasonNoReadableEvidence, nil},
		{"other failure", other, false, "", other},
		{"readable source", nil, false, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := tc.err
			if input != nil {
				input = fmt.Errorf("participant read: %w", input)
			}
			skip, reason, fatal := captureSourceError(input)
			if skip != tc.skip || reason != tc.reason || !errors.Is(fatal, tc.fatal) || (fatal == nil) != (tc.fatal == nil) {
				t.Fatalf("got skip=%v reason=%q error=%v", skip, reason, fatal)
			}
		})
	}
}

// WIS-700: the rarity score is exact. Claim "a" holds three words each also held by two other
// claims (1/3+1/3+1/3) and ties with claim "b", whose one word no other claim holds (df=1, score
// exactly 1); the tie breaks by claim ID. The claims are given in reverse ID order, so an order
// that only kept input order, or compared rounded floats, would fail.
func TestWIS700RankClaimsExactRarity(t *testing.T) {
	claim := func(id string) knowledge.SnapshotClaim {
		return knowledge.SnapshotClaim{ClaimInput: knowledge.ClaimInput{ClaimID: id, SourceSpanIDs: []string{"span-" + id}}}
	}
	claims := []knowledge.SnapshotClaim{claim("e"), claim("d"), claim("c"), claim("b"), claim("a")}
	words := [][]string{
		{"oven", "curing"},                        // e: 1/3 + 1/3
		{"oven", "temperature"},                   // d: 1/3 + 1/3
		{"curing", "temperature"},                 // c: 1/3 + 1/3
		{"fasteners"},                             // b: 1 (df 1)
		{"oven", "curing", "temperature", "oven"}, // a: 1/3 * 3 = 1; a repeated word counts once
	}
	want := []struct {
		id    string
		score *big.Rat
	}{{"a", big.NewRat(1, 1)}, {"b", big.NewRat(1, 1)}, {"c", big.NewRat(2, 3)}, {"d", big.NewRat(2, 3)}, {"e", big.NewRat(2, 3)}}
	first := rankClaims(claims, words)
	for run := 0; run < 3; run++ {
		got := rankClaims(slices.Clone(claims), words)
		if len(got) != len(want) {
			t.Fatalf("run %d: %d claims, want %d", run, len(got), len(want))
		}
		for i, w := range want {
			if got[i].id != w.id || got[i].score.Cmp(w.score) != 0 || got[i].id != first[i].id {
				t.Fatalf("run %d position %d: %s %s, want %s %s", run, i, got[i].id, got[i].score.RatString(), w.id, w.score.RatString())
			}
		}
	}
	// A single matched word that no other claim holds scores exactly 1; no word scores 0.
	single := rankClaims([]knowledge.SnapshotClaim{claim("x"), claim("y")}, [][]string{{"supplier"}, nil})
	if single[0].id != "x" || single[0].score.Cmp(big.NewRat(1, 1)) != 0 || single[1].score.Sign() != 0 {
		t.Fatalf("single word: %s %s, %s %s", single[0].id, single[0].score.RatString(), single[1].id, single[1].score.RatString())
	}
}
