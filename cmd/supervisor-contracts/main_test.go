package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReplacesTreeAndRemovesStaleFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "contracts")
	if err := os.MkdirAll(filepath.Join(root, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "schemas", "Stale.schema.json")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := write(root, map[string][]byte{"schemas/A.schema.json": []byte("a"), "openapi.json": []byte("o")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file survived: %v", err)
	}
	for name, want := range map[string]string{"schemas/A.schema.json": "a", "openapi.json": "o"} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
	}
}
