package dispatch

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestTenantsIntersectsAllowlist(t *testing.T) {
	a, b := uuid.NewString(), uuid.NewString()
	for _, tc := range []struct {
		name, want string
		allow      []string
	}{
		{name: "no allowlist selects all", want: a + "," + b},
		{name: "disjoint selects none", allow: []string{uuid.NewString()}},
		{name: "subset", allow: []string{strings.ToUpper(a)}, want: a},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := filterTenants([]string{a, b}, tc.allow)
			want := []string(nil)
			if tc.want != "" {
				want = strings.Split(tc.want, ",")
			}
			if !slices.Equal(got, want) {
				t.Fatalf("filterTenants=%v want=%v", got, want)
			}
		})
	}
}
