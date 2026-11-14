package onboarding

import (
	"errors"
	"testing"
)

// Codex round 5 on 7cdebed7 (WIS-625 post-merge): stroked text thick enough to fill its letters,
// strokes that make glyphs touch within an operator, and figures squashed into a strip.
func TestPDFThickStrokesAndSquashedFigures(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	doc := func(content, resources string, extra ...string) []byte {
		return pdfDoc{pages: []string{content}, resources: resources, extra: extra}.bytes()
	}
	figure := figureObject(scanImage(t, 400, 300, "jpeg"), 400, 300)
	for name, c := range map[string]struct {
		data   []byte
		text   string
		images int
		want   error
	}{
		"fill-and-stroke text with a 2 pt stroke at 12 pt": {data: doc("0 g 0 G BT /F1 12 Tf 2 w 2 Tr 72 300 Td (Thick) Tj ET", ""), want: ErrPDFScanHiddenContent},
		"outline text whose strokes meet":                  {data: doc("0 G BT /F1 12 Tf 1 w 1 Tr -1.5 Tc 72 300 Td (AAAA) Tj ET", ""), want: ErrPDFScanHiddenContent},
		"the same letters filled":                          {data: doc("0 g BT /F1 12 Tf -1.5 Tc 72 300 Td (AAAA) Tj ET", ""), text: "AAAA\n"},
		"a figure squashed into a strip":                   {data: doc(visibleLine+" q 500 0 0 20 72 350 cm /Im1 Do Q", imageResources(6), figure), want: ErrPDFScanHiddenContent},
		"a figure in its own shape":                        {data: doc(visibleLine+" q 300 0 0 225 72 350 cm /Im1 Do Q", imageResources(6), figure), text: "Visible line\n", images: 1},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := prepareDocument(t.Context(), DocumentUpload{Title: "Press SOP", DeclaredType: "application/pdf", Data: c.data})
			if c.want != nil {
				if !errors.Is(err, c.want) {
					t.Fatalf("got %q %v, want %v", got.text, err, c.want)
				}
				return
			}
			if err != nil || got.text != c.text || len(got.images) != c.images {
				t.Fatalf("got %q, %d images, %v; want %q, %d", got.text, len(got.images), err, c.text, c.images)
			}
		})
	}
}
