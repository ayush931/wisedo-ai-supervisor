// Command supervisor-contracts regenerates the committed contract artifacts.
// It writes only under the contracts directory and removes stale files so the
// tree always equals the generator output; `git diff` then proves drift.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/contracts"
)

func main() {
	root := "contracts"
	if len(os.Args) == 2 {
		root = os.Args[1]
	} else if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: supervisor-contracts [dir]")
		os.Exit(2)
	}
	if err := write(root, contracts.Files()); err != nil {
		fmt.Fprintln(os.Stderr, "supervisor-contracts:", err)
		os.Exit(1)
	}
}

func write(root string, files map[string][]byte) error {
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	for name, data := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
