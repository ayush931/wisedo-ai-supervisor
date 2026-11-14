package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
)

// TestProposalErrorConflicts pins the 409 codes for proposal conflicts. Review finding on
// WIS-274: ErrActiveProposal fell through to 503 STORAGE_UNAVAILABLE.
func TestProposalErrorConflicts(t *testing.T) {
	for err, code := range map[error]string{
		approvals.ErrActiveProposal: "PROPOSAL_ACTIVE",
		approvals.ErrStaleBinding:   "PROPOSAL_STALE",
	} {
		w := httptest.NewRecorder()
		proposalError(w, err)
		if w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
			t.Errorf("%v: %d %s", err, w.Code, w.Body.String())
		}
	}
}
