package onboarding

import (
	"errors"
	"testing"
)

// Codex round 42 on 1b4418ae (WIS-625 post-merge): textOnDark still counted an underline erased
// before the text, so a replacement underline (also before the text) made two in one band and the
// page was refused. Both rule counters now share ruleBefore.
func TestPDFErasedRuleThenReplacementBeforeText(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	text := " BT /F1 12 Tf 72 300 Td (Test) Tj ET"
	rule := "q 0 G 0.5 w 72 298 m 100 298 l S Q "
	low := "q 0 G 0.5 w 72 297 m 100 297 l S Q " // a rule below it, never joined with it (joinRules)
	for name, c := range map[string]struct {
		page string
		text string
		want error
	}{
		"an erased underline, then a replacement": {page: low + "q 1 g 70 296 34 4 re f Q " + rule + text, text: "Test\n"},
		"two underlines before the text":          {page: rule + "q 0 G 0.5 w 72 297 m 100 297 l S Q" + text, want: ErrPDFScanHiddenContent},
		"a partly erased underline, then another": {page: low + "q 1 g 85 296 20 4 re f Q " + rule + text, want: ErrPDFScanHiddenContent},
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
