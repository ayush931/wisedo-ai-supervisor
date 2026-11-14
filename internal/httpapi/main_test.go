package httpapi

import (
	"os"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
)

// TestMain lets this test binary act as the PDF child (WIS-624), which document uploads
// re-execute exactly as cmd/supervisor does in production.
func TestMain(m *testing.M) {
	onboarding.RunPDFChildIfRequested()
	os.Exit(m.Run())
}
