//go:build linux && amd64 && !cgo

package onboarding

import (
	"encoding/json"
	"testing"
)

// TestPDFChildStaticSandbox is selected by the build (static, CGO_ENABLED=0, as the production
// image), not by the sandbox's own settings, so turning a setting back on fails here instead of
// silently dropping the check. In this build the child may create no thread (Codex review 4: 64
// raw thread clones, each on its own stack and parked in a futex wait, would otherwise survive)
// and may not call mprotect.
func TestPDFChildStaticSandbox(t *testing.T) {
	t.Parallel()
	if pdfChildAllowClone || pdfChildAllowMprotect || pdfChildAllowMapStack || !pdfChildASLimit {
		t.Fatalf("static build sandbox settings: clone %v, mprotect %v, MAP_STACK %v, RLIMIT_AS %v; want false, false, false, true",
			pdfChildAllowClone, pdfChildAllowMprotect, pdfChildAllowMapStack, pdfChildASLimit)
	}
	for _, attempt := range []string{"clone_threads", "mprotect"} {
		in, _ := json.Marshal(probeInput{Mode: "attempt", Attempt: attempt})
		body, err := runPDFChildProcess(t.Context(), in, confineProbeFlag, 1<<20)
		wantSandboxKill(t, attempt, body, err)
	}
}
