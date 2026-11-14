package strictjson

import (
	"errors"
	"strings"
	"testing"
)

func TestDecodeRejectsAmbiguousOrIncompleteJSON(t *testing.T) {
	type nested struct {
		Key string `json:"key"`
	}
	type body struct {
		Name   string   `json:"name"`
		Nested []nested `json:"nested"`
	}
	var got body
	if err := Decode([]byte(`{"name":"ok","nested":[{"key":"v"}]}`), &got, 1024); err != nil || got.Name != "ok" {
		t.Fatalf("valid object: %+v, %v", got, err)
	}
	for _, input := range []string{
		`{"name":"a","name":"b"}`,
		`{"name":"a","nested":[{"key":"a","key":"b"}]}`,
		`{"unknown":true}`,
		`{"name":"a"} {}`,
		`{"name":"\ud800"}`,
		`{"name":"\udc00"}`,
		`{"name":"\ud800x"}`,
	} {
		if err := Decode([]byte(input), &got, 1024); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted %s", input)
		}
	}
	if err := Decode([]byte(`{"name":"\ud83d\ude42"}`), &got, 1024); err != nil {
		t.Fatalf("rejected valid surrogate pair: %v", err)
	}
	if err := Decode([]byte(`{"name":"ok"}`), &got, 2); !errors.Is(err, ErrInvalid) {
		t.Fatal("accepted oversized input")
	}
	invalidUTF8 := []byte(`{"name":"ok"}`)
	invalidUTF8[len(invalidUTF8)-2] = 0xff
	if err := Decode(invalidUTF8, &got, 1024); !errors.Is(err, ErrInvalid) {
		t.Fatal("accepted invalid UTF-8")
	}
	if err := Decode([]byte(strings.Repeat(" ", 1024)), &got, 0); !errors.Is(err, ErrInvalid) {
		t.Fatal("accepted disabled byte bound")
	}
}

// encoding/json matches field names case-insensitively; strict decoding does not.
func TestDecodeRejectsCaseVariantNames(t *testing.T) {
	type body struct {
		Token    string `json:"token"`
		TenantID string `json:"tenant_id,omitempty"`
		Nested   struct {
			Role string `json:"role"`
		} `json:"nested"`
	}
	for name, doc := range map[string]string{
		"case-variant duplicate": `{"token":"a","TOKEN":"b"}`,
		"case-variant field":     `{"Token":"a"}`,
		"forged tenant spelling": `{"token":"a","Tenant_ID":"x"}`,
		"nested case variant":    `{"token":"a","nested":{"ROLE":"owner"}}`,
		"exact duplicate":        `{"token":"a","token":"b"}`,
	} {
		var v body
		if err := Decode([]byte(doc), &v, 1024); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %+v", name, v)
		}
	}
	var v body
	if err := Decode([]byte(`{"token":"a","tenant_id":"t","nested":{"role":"owner"}}`), &v, 1024); err != nil || v.Token != "a" || v.Nested.Role != "owner" {
		t.Fatalf("exact names rejected: %v %+v", err, v)
	}
	if err := Decode([]byte(`{"token":"a"}`), v, 1024); !errors.Is(err, ErrInvalid) {
		t.Fatal("a non-pointer destination was accepted")
	}
}
