package agent

import (
	"slices"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
)

// TestMachineScopeFollowsApprovedLineage: machine evidence covers the case line and only the
// machines the pinned approved snapshot places on that line (part_of_asset) during the bound
// windows. Another line's machines, other predicates, claims outside the windows and unparseable
// bounds are excluded, so a stoppage elsewhere can never support this case.
func TestMachineScopeFollowsApprovedLineage(t *testing.T) {
	const line, otherLine = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	contexts := []plantcontext.Snapshot{
		{AssetID: line, Window: plantcontext.Window{Start: "2026-09-27T00:00:00Z", End: "2026-09-27T00:15:00Z"}},
		{AssetID: line, Window: plantcontext.Window{Start: "2026-09-27T00:15:00Z", End: "2026-09-27T00:30:00Z"}},
	}
	ended, future := "2026-09-26T23:00:00Z", "2026-09-27T01:00:00Z"
	claim := func(subject, predicate, object, from string, to *string) knowledge.SnapshotClaim {
		var c knowledge.SnapshotClaim
		c.SubjectID, c.Predicate, c.ObjectEntityID, c.EffectiveFrom, c.EffectiveTo = subject, predicate, object, from, to
		return c
	}
	snap := &knowledge.Snapshot{Contents: knowledge.SnapshotContents{Claims: []knowledge.SnapshotClaim{
		claim("press-on-line", "part_of_asset", line, "2026-01-01T00:00:00Z", nil),
		claim("press-until-later", "part_of_asset", line, "2026-01-01T00:00:00Z", &future),
		claim("press-moved-away", "part_of_asset", line, "2026-01-01T00:00:00Z", &ended),
		claim("press-added-later", "part_of_asset", line, "2026-09-27T00:30:00Z", nil),
		claim("press-other-line", "part_of_asset", otherLine, "2026-01-01T00:00:00Z", nil),
		claim("press-located", "located_in", line, "2026-01-01T00:00:00Z", nil),
		claim("press-bad-date", "part_of_asset", line, "yesterday", nil),
	}}}
	s, ok := newMachineScope(contexts, snap)
	if !ok {
		t.Fatal("scope must be built for two consecutive windows of one line")
	}
	want := []string{line, "press-on-line", "press-until-later"}
	if !slices.Equal(s.assets, want) {
		t.Fatalf("assets %v, want %v", s.assets, want)
	}
	if !s.start.Equal(time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)) || !s.end.Equal(time.Date(2026, 9, 27, 0, 30, 0, 0, time.UTC)) {
		t.Fatalf("window %s..%s", s.start, s.end)
	}
	// The tool loads captured contexts in ref (ID) order, not window order: same scope.
	reversed := []plantcontext.Snapshot{contexts[1], contexts[0]}
	if r, ok := newMachineScope(reversed, snap); !ok || !r.start.Equal(s.start) || !r.end.Equal(s.end) || !slices.Equal(r.assets, s.assets) {
		t.Fatalf("contexts out of window order give %+v %v", r, ok)
	}
	if s, ok := newMachineScope(contexts, nil); !ok || !slices.Equal(s.assets, []string{line}) {
		t.Fatalf("without an approved snapshot only the line itself counts: %v %v", s.assets, ok)
	}
	mixed := []plantcontext.Snapshot{contexts[0], {AssetID: otherLine, Window: contexts[1].Window}}
	if _, ok := newMachineScope(mixed, snap); ok {
		t.Fatal("contexts of different assets must not produce a scope")
	}
	broken := []plantcontext.Snapshot{{AssetID: line, Window: plantcontext.Window{Start: "not a time", End: "2026-09-27T00:30:00Z"}}}
	if _, ok := newMachineScope(broken, snap); ok {
		t.Fatal("an unparseable window must fail closed")
	}
}
