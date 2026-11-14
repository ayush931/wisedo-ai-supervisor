package operational

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestRegistryContractsAndMappingDigest(t *testing.T) {
	id, target := uuid.NewString(), uuid.NewString()
	c := Config{SourceID: id, Type: "event_api", AuthorityScope: "machine_state:line-1", Enabled: true, AccessPolicyRevision: 1, FreshnessSeconds: 120, ClockSkewSeconds: 30}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c0 := c
	for _, ref := range []string{"bearer-token-value", "https://example.invalid/credentials", "vault://user@vault/secret", "vault:///secret"} {
		bad := c
		bad.CredentialRef = ref
		if !errors.Is(bad.Validate(), ErrInvalid) {
			t.Fatalf("accepted non-opaque credential material %q", ref)
		}
	}
	for _, bad := range []Config{{SourceID: id, Type: "python", AuthorityScope: "x", AccessPolicyRevision: 1, FreshnessSeconds: 1}, {SourceID: id, Type: "csv", AuthorityScope: "x", AccessPolicyRevision: 0, FreshnessSeconds: 1}, {SourceID: id, Type: "manual", AuthorityScope: " x ", AccessPolicyRevision: 1, FreshnessSeconds: 1}} {
		if !errors.Is(bad.Validate(), ErrInvalid) {
			t.Fatalf("accepted invalid config: %+v", bad)
		}
	}
	// WIS-720: the revision is bounded so a revocation (+1) always fits.
	for _, c := range []struct {
		apr int64
		ok  bool
	}{{MaxAccessPolicyRevision - 1, true}, {MaxAccessPolicyRevision, false}, {1<<63 - 1, false}} {
		cfg := c0
		cfg.AccessPolicyRevision = c.apr
		if err := cfg.Validate(); (err == nil) != c.ok {
			t.Fatalf("access_policy_revision %d: Validate()=%v, want ok=%v", c.apr, err, c.ok)
		}
	}
	a := Mapping{ExternalType: "asset", ExternalKey: "press-01", EntityID: target, EntityRevision: 3}
	b := Mapping{ExternalType: "order", ExternalKey: "WO-9", EntityID: uuid.NewString(), EntityRevision: 1}
	input := []Mapping{b, a}
	sorted, err := canonicalMappings(input)
	if err != nil {
		t.Fatal(err)
	}
	if got := mappingDigest(sorted); got != mappingDigest([]Mapping{a, b}) {
		t.Fatalf("mapping digest depends on input order: %s", got)
	}
	if input[0] != b || input[1] != a {
		t.Fatal("canonicalization reordered the caller's mappings")
	}
	// Rule 04: a changed entity revision must change the digest.
	revised := a
	revised.EntityRevision = 4
	if mappingDigest([]Mapping{revised, b}) == mappingDigest(sorted) {
		t.Fatal("mapping digest ignores the entity revision")
	}
	sameTarget := a
	sameTarget.ExternalKey = "press-01-alias"
	for name, in := range map[string][]Mapping{"duplicate external key": {a, a}, "duplicate target revision": {a, sameTarget}} {
		if _, err = canonicalMappings(in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}

// G3: a row-level mapping failure names the caller's input index and member, and still
// matches ErrInvalid so the existing 422 INVALID_SOURCE code is unchanged.
func TestCanonicalMappingsReportsRowPath(t *testing.T) {
	ok := func(key string) Mapping {
		return Mapping{ExternalType: "asset", ExternalKey: key, EntityID: uuid.NewString(), EntityRevision: 1}
	}
	first, second := ok("press-01"), ok("press-02")
	twin := second
	twin.ExternalKey = "press-02-alias"
	for name, c := range map[string]struct {
		in   []Mapping
		path string
	}{
		"bad type":           {[]Mapping{first, {ExternalType: "line", ExternalKey: "x", EntityID: uuid.NewString(), EntityRevision: 1}}, "mappings[1].external_type"},
		"padded key":         {[]Mapping{first, second, {ExternalType: "order", ExternalKey: " WO-1", EntityID: uuid.NewString(), EntityRevision: 1}}, "mappings[2].external_key"},
		"empty key":          {[]Mapping{{ExternalType: "order", EntityID: uuid.NewString(), EntityRevision: 1}}, "mappings[0].external_key"},
		"non-canonical uuid": {[]Mapping{first, {ExternalType: "asset", ExternalKey: "k", EntityID: "NOT-A-UUID", EntityRevision: 1}}, "mappings[1].entity_id"},
		"zero revision":      {[]Mapping{{ExternalType: "asset", ExternalKey: "k", EntityID: uuid.NewString()}}, "mappings[0].entity_revision"},
		// Sorting must not move the index: the duplicate is input row 2 though it sorts first.
		"duplicate key":    {[]Mapping{second, ok("press-03"), {ExternalType: "asset", ExternalKey: "press-02", EntityID: uuid.NewString(), EntityRevision: 1}}, "mappings[2].external_key"},
		"duplicate target": {[]Mapping{second, twin}, "mappings[1].entity_id"},
		"too many":         {make([]Mapping, 10001), "mappings"},
	} {
		_, err := canonicalMappings(c.in)
		var row *RowError
		if !errors.As(err, &row) || row.Path != c.path || !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err=%v, want ErrInvalid at %s", name, err, c.path)
		}
	}
	if _, err := canonicalMappings([]Mapping{second, first}); err != nil {
		t.Fatalf("valid set rejected: %v", err)
	}
}
