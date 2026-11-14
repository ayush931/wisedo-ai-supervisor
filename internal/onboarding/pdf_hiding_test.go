package onboarding

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Codex review of f8dcdffd (WIS-625 post-merge): ways a page could hide what is stored or read,
// each refused, with a control that a normal page of the same shape is still accepted.
func TestPDFPageHidesNothingRound3(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	figure := figureObject(scanImage(t, 400, 300, "jpeg"), 400, 300)
	draw := " q 300 0 0 225 72 350 cm /Im1 Do Q" // the figure, clear of visibleLine
	doc := func(content, resources string, extra ...string) pdfDoc {
		return pdfDoc{pages: []string{content}, resources: resources, extra: extra}
	}
	// twoStreams makes the page's content the array [first second].
	twoStreams := func(first, second string) []byte {
		b := pdfDoc{pages: []string{first}, extra: []string{fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(second), second)}}.bytes()
		return rebuildXref(t, bytes.Replace(b, []byte("/Contents 5 0 R"), []byte("/Contents [5 0 R 6 0 R]"), 1))
	}
	// withFont replaces the fixture's Helvetica (no /Widths) with font.
	withFont := func(d pdfDoc, font string) []byte {
		return rebuildXref(t, bytes.Replace(d.bytes(), []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"), []byte(font), 1))
	}
	widths := "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /FirstChar 32 /LastChar 126 /Widths [" + strings.TrimSpace(strings.Repeat("700 ", 95)) + "] >>" // at least Adobe's Helvetica widths for A, B and space
	for name, c := range map[string]struct {
		data []byte
		text string
		want error
	}{
		// Security review of pdf_glyphs.go: text is placed by its whole rendering matrix, with word
		// spacing and each code's width, so a paint over its real place is found.
		"a box over rotated text":                 {data: doc(visibleLine+" BT /F1 12 Tf 0 1 -1 0 300 300 Tm (Rotated claim) Tj ET 1 g 290 340 12 20 re f", "").bytes(), want: ErrPDFScanHiddenContent},
		"a box over a word moved by word spacing": {data: withFont(doc("BT /F1 12 Tf 72 500 Td 200 Tw (A B) Tj ET 1 g 280 498 20 14 re f", ""), widths), want: ErrPDFScanHiddenContent},
		"word spacing with nothing over it":       {data: withFont(doc("BT /F1 12 Tf 72 500 Td 200 Tw (A B) Tj ET", ""), widths), text: "A B\n"},
		"a box only touching the text":            {data: doc("BT /F1 10 Tf 72 300 Td (Touched) Tj ET 0 g 80 307 10 5 re f", "").bytes(), want: ErrPDFScanHiddenContent}, // 0.7 em of 10 pt: the text's top is 307
		"a Type0 font with a CMap other than Identity-H": {data: withFont(doc(visibleLine, ""),
			"<< /Type /Font /Subtype /Type0 /BaseFont /X /Encoding /UniJIS-UCS2-H /DescendantFonts [<< /Type /Font /Subtype /CIDFontType2 /BaseFont /X /CIDSystemInfo << /Registry (Adobe) /Ordering (Japan1) /Supplement 6 >> >>] >>"), want: ErrPDFNoTextLayer},
		"a Type3 font": {data: withFont(doc(visibleLine, ""),
			"<< /Type /Font /Subtype /Type3 /FontBBox [0 0 1000 1000] /FontMatrix [0.001 0 0 0.001 0 0] /CharProcs << >> /Encoding << /Differences [] >> /FirstChar 32 /LastChar 32 /Widths [500] >>"), want: ErrPDFNoTextLayer},
		// 1. Text with something opaque painted over it later.
		"a box filled over typed text":      {data: doc(visibleLine+" 1 g 70 298 200 14 re f", "").bytes(), want: ErrPDFScanHiddenContent},
		"an image drawn over typed text":    {data: doc(visibleLine+" q 300 0 0 225 72 280 cm /Im1 Do Q", imageResources(6), figure).bytes(), want: ErrPDFScanHiddenContent},
		"cell shading drawn under the text": {data: doc("0.9 g 66 296 300 16 re f 0 g "+visibleLine, "").bytes(), text: "Visible line\n"},
		// 2. A comment at the end of one content stream swallows the start of the next when the
		// extractor joins them; a viewer may not.
		"a comment spanning two content streams": {data: twoStreams("BT /F1 12 Tf 72 300 Td 3 Tr %", "0 Tr\n(Hidden claim) Tj ET"), want: ErrPDFNoTextLayer},
		"two streams split between operators":    {data: twoStreams("BT /F1 12 Tf 72 300 Td", "\n(Visible line) Tj ET"), text: "Visible line\n"},
		// 3. Render modes 4-6 add the glyphs to the clip at ET.
		"a mode-4 text clip over a figure": {data: doc(visibleLine+" BT /F1 1 Tf 4 Tr 80 360 Td (x) Tj ET"+draw, imageResources(6), figure).bytes(), want: ErrUnreadableSource},
		"mode 5 alone":                     {data: doc(visibleLine+" BT /F1 12 Tf 5 Tr 72 500 Td (Clip) Tj ET", "").bytes(), want: ErrUnreadableSource},
		// 4. A path built under a rotation, then the CTM restored, then used as a clip: its outline
		// is a diamond, not its bounding box.
		"a rotated clip around a figure": {data: doc(visibleLine+" q q 0.7071 0.7071 -0.7071 0.7071 300 400 cm 0 0 100 100 re Q W n q 100 0 0 100 250 410 cm /Im1 Do Q Q", imageResources(6), figure).bytes(),
			want: ErrPDFScanHiddenContent},
		// 5. A wide stroke near a figure, and text shown over a figure afterwards.
		"a 100 pt stroke beside a figure": {data: doc(visibleLine+draw+" 100 w 72 620 m 372 620 l S", imageResources(6), figure).bytes(), want: ErrPDFScanHiddenContent},
		"a thin rule beside a figure":     {data: doc(visibleLine+draw+" 1 w 72 620 m 372 620 l S", imageResources(6), figure).bytes(), text: "Visible line\n"},
		"a label typed over a figure":     {data: doc(visibleLine+draw+" BT /F1 12 Tf 100 400 Td (Label) Tj ET", imageResources(6), figure).bytes(), want: ErrPDFScanHiddenContent},
		// 6. A Default colour space replaces how the figure's device colours show.
		"a figure under DefaultGray": {data: doc(visibleLine+draw, imageResources(6)+" /ColorSpace << /DefaultGray [/CalGray << /WhitePoint [1 1 1] /Gamma 2.2 >>] >>", figure).bytes(),
			want: ErrPDFScanHiddenContent},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := prepareDocument(t.Context(), DocumentUpload{Title: "Press SOP", DeclaredType: "application/pdf", Data: c.data})
			if c.want != nil {
				if !errors.Is(err, c.want) {
					t.Fatalf("got %q %v, want %v", got.text, err, c.want)
				}
				return
			}
			if err != nil || got.text != c.text {
				t.Fatalf("got %q %v, want %q", got.text, err, c.text)
			}
		})
	}
}
