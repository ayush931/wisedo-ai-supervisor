package onboarding

import (
	"errors"
	"testing"
)

// Security review of 8ea02ffb (WIS-625 post-merge): under a singular CTM the sheared-pen bound
// computed Inf * 0 = NaN, and a NaN box compares false everywhere, so the stroke covered nothing.
// A singular transform now gives an infinite reach, and any non-finite geometry refuses the file.
func TestPDFSingularTransformRefuses(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	for name, c := range map[string]struct {
		page string
		text string
		want error
	}{
		"a hairline under a singular CTM over the text": {page: visibleLine + " q 1 1 1 1 0 0 cm 0 G 0 w 150 150 m 155 155 l S Q", want: ErrUnreadableSource},
		// "1 0 1 0 0 300 cm" flattens everything onto y 300, through the line: the old bound was
		// NaN across y (Inf * 0), so this white stroke over the text covered nothing.
		"a white stroke flattened onto the text": {page: visibleLine + " q 1 0 1 0 0 300 cm 1 G 0 w 80 0 m 120 0 l S Q", want: ErrUnreadableSource},
		"outlined text under a singular CTM":     {page: visibleLine + " q 1 1 1 1 0 0 cm 0 w BT /F1 12 Tf 1 Tr 10 10 Td (Flat) Tj ET Q", want: ErrUnreadableSource},
		"a hairline under an ordinary CTM":       {page: visibleLine + " q 0 G 0 w 72 200 m 129 200 l S Q", text: "Visible line\n"},
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
