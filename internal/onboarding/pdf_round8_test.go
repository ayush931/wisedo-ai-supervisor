package onboarding

import (
	"bytes"
	"errors"
	"testing"
)

// Codex round 3 on fdc2135b (WIS-625 post-merge): escaped names, mode-2 strokes, singular text
// matrices, glyphs past the ascent, and a dark background painted over in light.
func TestPDFNamesStrokesMatricesAndBackgrounds(t *testing.T) {
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
		// 1. Names are read decoded, as the resource keys are.
		"a figure drawn by an escaped name":       {data: doc(visibleLine+" q 300 0 0 225 72 350 cm /Im#31 Do Q", imageResources(6), figure), text: "Visible line\n", images: 1},
		"an escaped name for a transparent state": {data: doc("/G#31 gs "+visibleLine, "/ExtGState << /G1 << /ca 0 >> >>"), want: ErrPDFScanHiddenContent},
		"two resource keys that decode alike":     {data: doc(visibleLine, "/XObject << /Im1 6 0 R /Im#31 6 0 R >>", figure), want: ErrUnreadableSource},
		// 2. Mode 2 strokes over its fill.
		"a wide white stroke over black text": {data: doc("0 g 1 G 20 w BT /F1 12 Tf 2 Tr 72 300 Td (Hidden) Tj ET", ""), want: ErrPDFScanHiddenContent},
		"fill and stroke both black":          {data: doc("0 g 0 G 0.3 w BT /F1 12 Tf 2 Tr 72 300 Td (Bold) Tj ET", ""), text: "Bold\n"},
		// 3. A singular text matrix squashes text to a line.
		"a singular text matrix": {data: doc(visibleLine+" BT /F1 12 Tf 1 1 1 1 72 500 Tm (Hidden) Tj ET", ""), want: ErrPDFScanHiddenContent},
		// 5. An accent rises past the ascent: Adobe's box of Eacute reaches 0.929 em.
		"a box over an accent above the ascent": {data: func() []byte {
			b := doc("BT /F1 12 Tf 72 300 Td (\xc9t\xe9) Tj ET 1 g 72 309.5 8 1.4 re f", "")
			return rebuildXref(t, bytes.Replace(b, []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"), []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"), 1))
		}(), want: ErrPDFScanHiddenContent},
		// 6. A dark fill painted over in light before the text no longer shows; half painted over does.
		"a dark box repainted light":      {data: doc("0 g 60 290 200 25 re f 1 g 60 290 200 25 re f 0 g "+visibleLine, ""), text: "Visible line\n"},
		"a dark box half repainted light": {data: doc("0 g 60 290 200 25 re f 1 g 60 290 40 25 re f 0 g "+visibleLine, ""), want: ErrPDFScanHiddenContent}, // light under "Vis" only
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
