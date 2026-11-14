package canonical

import (
	"errors"
	"testing"
)

const (
	tenant = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
	site   = "1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a"
	other  = "2a2a2a2a-2a2a-4a2a-8a2a-2a2a2a2a2a2a"
)

type body struct {
	B     string   `json:"b"`
	A     int64    `json:"a"`
	Items []string `json:"items"`
	Gone  *string  `json:"gone"`
}

func TestEnvelopeIsSchemaOrderedAndScoped(t *testing.T) {
	encoded, digest, err := Envelope("Example", 1, tenant, site, body{B: "x", A: 2, Items: []string{"1"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":"Example","schema_version":1,"tenant_id":"` + tenant + `","site_id":"` + site + `","body":{"b":"x","a":2,"items":["1"],"gone":null}}`
	if string(encoded) != want {
		t.Fatalf("got %s\nwant %s", encoded, want)
	}
	_, again, _ := Envelope("Example", 1, tenant, site, body{B: "x", A: 2, Items: []string{"1"}})
	if again != digest || len(digest) != 64 {
		t.Fatal("identical input changed digest")
	}
	for name, change := range map[string]func() (string, error){
		"tenant": func() (string, error) {
			_, d, e := Envelope("Example", 1, other, site, body{B: "x", A: 2})
			return d, e
		},
		"site": func() (string, error) {
			_, d, e := Envelope("Example", 1, tenant, other, body{B: "x", A: 2})
			return d, e
		},
		"version": func() (string, error) {
			_, d, e := Envelope("Example", 2, tenant, site, body{B: "x", A: 2})
			return d, e
		},
		"schema": func() (string, error) { _, d, e := Envelope("Other", 1, tenant, site, body{B: "x", A: 2}); return d, e },
	} {
		if d, e := change(); e != nil || d == digest {
			t.Errorf("%s did not change the digest: %v", name, e)
		}
	}
}

func TestEnvelopeRejectsFloatsAndBadScope(t *testing.T) {
	if _, _, err := Envelope("Example", 1, tenant, site, map[string]float64{"x": 1.5}); !errors.Is(err, ErrNotCanonical) {
		t.Fatalf("float accepted: %v", err)
	}
	if _, _, err := Envelope("Example", 1, tenant, site, []any{1e21}); !errors.Is(err, ErrNotCanonical) {
		t.Fatalf("exponent float accepted: %v", err)
	}
	for name, args := range map[string][3]string{
		"empty schema": {"", tenant, site},
		"padded":       {" Example", tenant, site},
		"bad tenant":   {"Example", "TENANT", site},
		"nil site":     {"Example", tenant, "00000000-0000-0000-0000-000000000000"},
	} {
		if _, _, err := Envelope(args[0], 1, args[1], args[2], nil); !errors.Is(err, ErrNotCanonical) {
			t.Errorf("%s accepted", name)
		}
	}
	if _, _, err := Envelope("Example", 0, tenant, site, nil); !errors.Is(err, ErrNotCanonical) {
		t.Fatal("version 0 accepted")
	}
}

func TestDigestIsSchemaOrderedAndRejectsFloats(t *testing.T) {
	encoded, digest, err := Digest("SetupRequest", 1, body{B: "x", A: 2, Items: []string{"1"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"schema":"SetupRequest","schema_version":1,"body":{"b":"x","a":2,"items":["1"],"gone":null}}` || len(digest) != 64 {
		t.Fatalf("unexpected digest envelope %s %s", encoded, digest)
	}
	_, again, err := Digest("SetupRequest", 1, body{B: "x", A: 2, Items: []string{"1"}})
	if err != nil || again != digest {
		t.Fatalf("same request changed digest: %v", err)
	}
	if _, _, err = Digest("SetupRequest", 1, map[string]float64{"x": 1.5}); !errors.Is(err, ErrNotCanonical) {
		t.Fatalf("float accepted: %v", err)
	}
}
