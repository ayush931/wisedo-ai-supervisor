#!/usr/bin/env bash
# Every shipped Go package must carry at least one test, counting the
# integration-tagged files. A package with none is a coverage hole, not a pass.
set -euo pipefail
missing=$(go list -tags=integration -f '{{if not (or .TestGoFiles .XTestGoFiles)}}{{.ImportPath}}{{end}}' ./... | grep . || true)
if [ -n "$missing" ]; then
  echo 'BLOCKED: packages without any test file:' >&2
  echo "$missing" >&2
  exit 1
fi
