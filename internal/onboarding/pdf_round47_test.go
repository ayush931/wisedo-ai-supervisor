package onboarding

import (
	"errors"
	"testing"
)

// Codex round 40 on 2edff941 (WIS-625 post-merge): an underline stroked before the text reached its
// descender cells and was taken for a dark background, though the same rule drawn after the text
// is an underline. A rule under the text that passes ruleBand (underline or strike band, at most
// 0.1 em, as dark as the text, every crossed glyph keeping 30% of its ink outside, one per band)
// is a decoration whether drawn before or after.
func TestPDFRulesDrawnBeforeTheText(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	for name, c := range map[string]struct {
		page string
		text string
		want error
	}{
		"an underline drawn before the text": {page: "q 0 G 0.5 w 72 298 m 100 298 l S Q BT /F1 12 Tf 72 300 Td (Test) Tj ET", text: "Test\n"},
		"a thick dark bar before the text":   {page: "q 0 g 72 296 30 10 re f Q BT /F1 12 Tf 72 300 Td (Test) Tj ET", want: ErrPDFScanHiddenContent},
		"a bar under a whole minus":          {page: "q 0 g 72 322.95 300 9.5 re f Q BT /F1 100 Tf 72 300 Td (-10) Tj ET", want: ErrPDFScanHiddenContent},
		// Security review: a strike rule before the text and one after it, each passing alone,
		// together cover the minus of -10 (one rule per band in all, before and after).
		"a strike rule on each side of a minus": {page: "q 0 g 72 323 300 5 re f Q BT /F1 100 Tf 72 300 Td (-10) Tj ET q 0 g 72 327.5 300 5 re f Q", want: ErrPDFScanHiddenContent},
		"two underlines before the text":        {page: "q 0 G 0.5 w 72 298 m 100 298 l S 72 297 m 100 297 l S Q BT /F1 12 Tf 72 300 Td (Test) Tj ET", want: ErrPDFScanHiddenContent},
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
