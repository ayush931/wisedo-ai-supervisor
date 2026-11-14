package onboarding

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Security review on the round-2 commits (WIS-625 post-merge): declared metrics too small to
// trust, dashed outline text, Default colour spaces under text, overprint within one operator, and
// text on a dark or unknown background. Each with a control of normal text of the same shape.
func TestPDFTextInkAndMetrics(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	doc := func(content, resources string, extra ...string) []byte {
		return pdfDoc{pages: []string{content}, resources: resources, extra: extra}.bytes()
	}
	font := func(content, fontObj string, extra ...string) []byte {
		return rebuildXref(t, bytes.Replace(doc(content, "", extra...), []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"), []byte(fontObj), 1))
	}
	widths := "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /FirstChar 32 /LastChar 126 /Widths [" + strings.TrimSpace(strings.Repeat("600 ", 95)) + "] >>"
	ones := strings.TrimSpace(strings.Repeat("1 ", 95))
	tinyBox := "/FontDescriptor << /Type /FontDescriptor /FontName /X /Flags 32 /FontBBox [0 0 1 1] /ItalicAngle 0 /Ascent 1 /Descent 0 /CapHeight 1 /StemV 80 >>"
	covered := "BT /F1 12 Tf 72 300 Td (Hidden) Tj ET 1 g 90 300 4 6 re f"
	figure := figureObject(scanImage(t, 400, 300, "jpeg"), 400, 300)
	for name, c := range map[string]struct {
		data []byte
		text string
		want error
	}{
		// Declared metrics a viewer does not draw by.
		"an embedded font with a 1/1000 em box and widths": {data: font(covered,
			"<< /Type /Font /Subtype /TrueType /BaseFont /ABCDEF+Custom /FirstChar 32 /LastChar 126 /Widths ["+ones+"] "+strings.Replace(tinyBox, "/StemV 80", "/StemV 80 /FontFile2 6 0 R", 1)+" >>",
			"<< /Length 1 >>\nstream\nx\nendstream"),
			want: ErrPDFNoTextLayer},
		// The pen moves by the declared 1/1000 em while Adobe's glyphs are drawn: the words pile up.
		"Helvetica declaring widths of 1, two words in one Tj": {data: font("BT /F1 12 Tf 72 300 Td (Hidden words) Tj ET",
			"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /FirstChar 32 /LastChar 126 /Widths ["+ones+"] >>"), want: ErrPDFScanHiddenContent},
		"Helvetica declaring a 1/1000 em box and widths": {data: font(covered,
			"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /FirstChar 32 /LastChar 126 /Widths ["+ones+"] "+tinyBox+" >>"), want: ErrPDFScanHiddenContent},
		// Dashed outline text.
		"outline text with a sparse dash":     {data: doc("[1 20] 0 d BT /F1 12 Tf 1 Tr 72 300 Td (Dashed) Tj ET", ""), want: ErrPDFScanHiddenContent},
		"outline text dashed by an ExtGState": {data: doc("/G1 gs BT /F1 12 Tf 1 Tr 72 300 Td (Dashed) Tj ET", "/ExtGState << /G1 << /D [[1 20] 0] >> >>"), want: ErrPDFScanHiddenContent},
		"outline text with a solid line":      {data: doc("[] 0 d BT /F1 12 Tf 1 Tr 72 300 Td (Outline) Tj ET", ""), text: "Outline\n"},
		// A Default colour space under text.
		"text under DefaultGray": {data: doc(visibleLine, "/ColorSpace << /DefaultGray [/CalGray << /WhitePoint [1 1 1] /Gamma 0.1 >>] >>"), want: ErrPDFScanHiddenContent},
		// Overprint within one operator.
		"a TJ number moving back over earlier text":  {data: doc("BT /F1 12 Tf 72 300 Td [(Visible) 3000 (XXXX)] TJ ET", ""), want: ErrPDFScanHiddenContent},
		"negative character spacing stacking glyphs": {data: doc("BT /F1 12 Tf -8 Tc 72 300 Td (AAAA) Tj ET", ""), want: ErrPDFScanHiddenContent},
		"ordinary kerning":                           {data: font("BT /F1 12 Tf 72 300 Td [(A) 80 (V)] TJ ET", widths), text: "AV\n"},
		// Text on a dark or unknown background.
		"black text on a black box":        {data: doc("0 g 60 290 200 25 re f "+visibleLine, ""), want: ErrPDFScanHiddenContent},
		"text over an image":               {data: doc("q 300 0 0 225 50 250 cm /Im1 Do Q "+visibleLine, imageResources(6), figure), want: ErrPDFScanHiddenContent},
		"text over a shading":              {data: doc("/Sh1 sh "+visibleLine, "/Shading << /Sh1 6 0 R >>", "<< /ShadingType 2 /ColorSpace /DeviceGray /Coords [0 0 1 0] /Function << /FunctionType 2 /Domain [0 1] /C0 [0] /C1 [0] /N 1 >> >>"), want: ErrPDFScanHiddenContent},
		"text over an unknown colour":      {data: doc("/CS0 cs 0.9 scn 60 290 200 25 re f 0 g "+visibleLine, "/ColorSpace << /CS0 [/CalGray << /WhitePoint [1 1 1] >>] >>"), want: ErrPDFScanHiddenContent},
		"black text on light-grey shading": {data: doc("0.85 0.85 0.85 rg 60 290 200 25 re f 0 g "+visibleLine, ""), text: "Visible line\n"},
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
