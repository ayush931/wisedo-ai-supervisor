package onboarding

import (
	"errors"
	"testing"
)

// Codex round 21 on 9071cb1e (WIS-625 post-merge).
//   - A hairline under a shear ("1 0 10 1 cm 0 w"): renderers widen a sub-pixel pen in user space
//     until it is a pixel across its narrowest direction, so it grows along the major axis (PDF.js
//     draws about 7 pt to each side); the bound stopped at 0.7 pt.
//   - A clip built under a quarter turn is a page-aligned rectangle, not an unknown clip.
//   - A clip of no area stays empty whatever is intersected with it next.
//   - A paint of alpha 0 paints nothing.
func TestPDFShearedHairlinesQuarterTurnClipsEmptyClipsAndAlphaZero(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	states := "/ExtGState << /Clear << /ca 0 /CA 0 >> /Half << /ca 0.5 /CA 0.5 >> >>"
	for name, c := range map[string]struct {
		doc  pdfDoc
		text string
		want error
	}{
		// A vertical hairline at x 132 on the page, 2.7 pt past the line's end: under the shear its
		// pen reaches far across; unsheared, half a point.
		"a sheared hairline beside the text": {doc: pdfDoc{pages: []string{visibleLine + " q 1 G 0 w 1 0 10 1 0 0 cm -2818 295 m -2968 310 l S Q"}}, want: ErrPDFScanHiddenContent},
		"an unsheared hairline beside it":    {doc: pdfDoc{pages: []string{visibleLine + " q 1 G 0 w 132 295 m 132 310 l S Q"}}, text: "Visible line\n"},
		"a clip under a quarter turn":        {doc: pdfDoc{pages: []string{"q 0 1 -1 0 612 0 cm 0 0 792 612 re W n Q " + visibleLine}}, text: "Visible line\n"},
		"text inside a quarter-turned clip":  {doc: pdfDoc{pages: []string{"q 0 1 -1 0 612 0 cm 0 0 792 612 re W n " + visibleLine + " Q"}}, text: "Visible line\n"},
		"an empty clip, then a triangle":     {doc: pdfDoc{pages: []string{visibleLine + " q 100 290 0 25 re W n 0 0 m 600 0 l 0 700 l h W n 0 g 60 290 200 25 re f Q"}}, text: "Visible line\n"},
		"a triangle clip alone":              {doc: pdfDoc{pages: []string{visibleLine + " q 0 0 m 600 0 l 0 700 l h W n 0 g 60 290 200 25 re f Q"}}, want: ErrPDFScanHiddenContent},
		"a fill of alpha 0 over the text":    {doc: pdfDoc{pages: []string{visibleLine + " q /Clear gs 0 g 60 290 200 25 re f Q"}, resources: states}, text: "Visible line\n"},
		"a stroke of alpha 0 over the text":  {doc: pdfDoc{pages: []string{visibleLine + " q /Clear gs 0 G 5 w 60 302 m 260 302 l S Q"}, resources: states}, text: "Visible line\n"},
		"a fill of alpha 0.5 over the text":  {doc: pdfDoc{pages: []string{visibleLine + " q /Half gs 0 g 60 290 200 25 re f Q"}, resources: states}, want: ErrPDFScanHiddenContent},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Extract(t.Context(), c.doc.bytes(), KindPDF, DefaultLimits)
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
