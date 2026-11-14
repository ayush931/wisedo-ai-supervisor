package onboarding

import (
	"errors"
	"testing"
)

// Codex round 41 on b5a110e4 (WIS-625 post-merge): an underline drawn before the text and then
// wholly repainted white (still before the text) shows nothing, but it was counted with the
// underline drawn after the text, two in one band, and the page was refused. A rule a later opaque
// light fill wholly covers (with the decision margin) before the text is not counted.
func TestPDFErasedRulesAreNotCounted(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	// The earlier rule sits 1 unit below the later one, so the two are never one rule (joinRules).
	text := " BT /F1 12 Tf 72 300 Td (Test) Tj ET"
	after := " q 0 G 0.5 w 72 298 m 100 298 l S Q"
	for name, c := range map[string]struct {
		page string
		text string
		want error
	}{
		"an erased underline, then one after":       {page: "q 0 G 0.5 w 72 297 m 100 297 l S Q q 1 g 70 296 34 4 re f Q" + text + after, text: "Test\n"},
		"a partly erased underline, then one after": {page: "q 0 G 0.5 w 72 297 m 100 297 l S Q q 1 g 85 296 20 4 re f Q" + text + after, want: ErrPDFScanHiddenContent},
		"two underlines after the text":             {page: text + " q 0 G 0.5 w 72 297 m 100 297 l S Q" + after, want: ErrPDFScanHiddenContent},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Extract(t.Context(), pdfDoc{pages: []string{c.page}}.bytes(), KindPDF, DefaultLimits)
			if c.want != nil {
				if got != "" || !errors.Is(err, c.want) {
					t.Fatalf("got %q %v, want %v", got, err, c.want)
				}
				return
			}
			if err != nil || got != c.text {
				t.Fatalf("got %q %v, want %q", got, err, c.text)
			}
		})
	}
}
