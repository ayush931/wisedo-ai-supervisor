package knowledge

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// WIS-621: the stored snapshot bound is MaxSnapshotBytes, not the 64 KiB graph-read budget. A body
// just over 64 KiB encodes byte for byte; one over the bound is refused whole.
func TestEncodeSnapshotByteBound(t *testing.T) {
	sized := func(target int) SnapshotContents {
		c := SnapshotContents{SchemaVersion: 1, TenantID: "t", SiteID: "s", Entities: []SnapshotEntity{}, Claims: []SnapshotClaim{}, Sources: []SnapshotSource{}, Spans: []SnapshotSpan{}, Resolutions: []SnapshotResolution{}}
		value, _ := json.Marshal(strings.Repeat("x", 0))
		probe := SnapshotClaim{ClaimInput: ClaimInput{Literal: &Literal{Type: "string", Value: value}}}
		c.Claims = []SnapshotClaim{probe}
		withEmpty, _ := json.Marshal(c)
		value, _ = json.Marshal(strings.Repeat("x", target-len(withEmpty)))
		c.Claims[0].Literal.Value = value
		return c
	}
	for _, tc := range []struct {
		name string
		size int
		err  error
	}{
		{"over the old 64 KiB bound", 64<<10 + 1, nil},
		{"exactly the bound", MaxSnapshotBytes, nil},
		{"one byte over the bound", MaxSnapshotBytes + 1, ErrTraversalLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contents := sized(tc.size)
			want, _ := json.Marshal(contents)
			if len(want) != tc.size {
				t.Fatalf("fixture is %d bytes, want %d", len(want), tc.size)
			}
			got, err := encodeSnapshot(contents)
			if !errors.Is(err, tc.err) || (tc.err == nil) != (err == nil) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if tc.err == nil && string(got) != string(want) {
				t.Fatal("canonical body changed")
			}
			if tc.err != nil && got != nil {
				t.Fatal("refused snapshot returned a body")
			}
		})
	}
}
