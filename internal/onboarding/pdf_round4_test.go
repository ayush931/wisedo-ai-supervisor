package onboarding

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// Codex round 2 on 1a369da0 (WIS-625 post-merge): text a reader may not see, or that does not say
// what it draws, is refused; each case with a control that normal text of the same shape stores.
func TestPDFTextAReaderSees(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	doc := func(content, resources string, extra ...string) []byte {
		return pdfDoc{pages: []string{content}, resources: resources, extra: extra}.bytes()
	}
	// twoFonts adds font F2 (object 6) beside the fixture's Helvetica F1.
	twoFonts := func(content, f2 string, extra ...string) []byte {
		b := pdfDoc{pages: []string{content}, extra: append([]string{f2}, extra...)}.bytes()
		return rebuildXref(t, bytes.Replace(b, []byte("/Font << /F1 3 0 R >>"), []byte("/Font << /F1 3 0 R /F2 6 0 R >>"), 1))
	}
	// toUnicode is a font (object 6 when it is the first extra) whose ToUnicode CMap (object 7) maps
	// code 0x41 to the UTF-16BE hex dst.
	toUnicode := func(font, dst string) (string, string) {
		cmap := "/CIDInit /ProcSet findresource begin 12 dict begin begincmap /CMapName /X def 1 begincodespacerange <00> <FF> endcodespacerange 1 beginbfchar <41> <" + dst + "> endbfchar endcmap CMapName currentdict /CMap defineresource pop end end"
		return font, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(cmap), cmap)
	}
	gs := func(state string) string { return "/ExtGState << /G1 << " + state + " >> >>" }
	for name, c := range map[string]struct {
		data []byte
		text string
		want error
	}{
		// 1. Word spacing pushes "Hidden" off the page: one placement, the corrected one, decides.
		"a word pushed off the page by word spacing": {data: doc("BT /F1 12 Tf 500 300 Td 1000 Tw (A Hidden) Tj ET", ""), want: ErrPDFScanHiddenContent},
		// 2. A clip hides text.
		"text outside a clip":     {data: doc(visibleLine+" q 0 0 10 10 re W n BT /F1 12 Tf 72 500 Td (Hidden) Tj ET Q", ""), want: ErrPDFScanHiddenContent},
		"text inside a clip":      {data: doc("q 60 280 200 40 re W n "+visibleLine+" Q", ""), text: "Visible line\n"},
		"text under a round clip": {data: doc("q 60 280 m 260 280 l 160 330 l h W n "+visibleLine+" Q", ""), want: ErrPDFScanHiddenContent},
		// 3. Text that does not paint legibly.
		"white text":                       {data: doc("1 g "+visibleLine, ""), want: ErrPDFScanHiddenContent},
		"pale grey text":                   {data: doc("0.8 g "+visibleLine, ""), want: ErrPDFScanHiddenContent},
		"transparent text (ca 0)":          {data: doc("/G1 gs "+visibleLine, gs("/ca 0")), want: ErrPDFScanHiddenContent},
		"text under a soft mask":           {data: doc("/G1 gs "+visibleLine, gs("/SMask << /S /Luminosity /G 6 0 R >>"), formObject("", "q Q")), want: ErrPDFScanHiddenContent},
		"text in a colour space not known": {data: doc("/CS0 cs 0 scn "+visibleLine, "/ColorSpace << /CS0 [/CalGray << /WhitePoint [1 1 1] >>] >>"), want: ErrPDFScanHiddenContent},
		"text at a size of 0":              {data: doc("BT /F1 0 Tf 72 300 Td (Hidden) Tj ET "+visibleLine, ""), want: ErrPDFScanHiddenContent},
		"text scaled below a point":        {data: doc("q 0.05 0 0 0.05 0 0 cm BT /F1 12 Tf 1440 6000 Td (Hidden) Tj ET Q "+visibleLine, ""), want: ErrPDFScanHiddenContent},
		"dark blue text":                   {data: doc("0 0 0.5 rg "+visibleLine, ""), text: "Visible line\n"},
		// 4. ExtGState parameters.
		"a 100 pt line width set by gs":  {data: doc(visibleLine+" /G1 gs 72 250 m 300 250 l S", gs("/LW 100")), want: ErrPDFScanHiddenContent},
		"a transfer function":            {data: doc("/G1 gs "+visibleLine, gs("/TR /Identity")), want: ErrUnreadableSource},
		"an ExtGState font":              {data: doc("/G1 gs "+visibleLine, gs("/Font [3 0 R 12]")), want: ErrUnreadableSource},
		"a plain ExtGState (LW 1, ca 1)": {data: doc("/G1 gs "+visibleLine+" 72 250 m 300 250 l S", gs("/Type /ExtGState /LW 1 /ca 1 /CA 1 /BM /Normal /SMask /None")), text: "Visible line\n"},
		// 5. s closes its path: the closing segment crosses the text.
		"a closing stroke across the text": {data: doc(visibleLine+" 50 302 m 50 400 l 300 400 l s", ""), want: ErrPDFScanHiddenContent},
		"the same path left open":          {data: doc(visibleLine+" 50 302 m 50 400 l 300 400 l S", ""), text: "Visible line\n"},
		// 6. Descenders, and stroked text, reach past the baseline-to-0.7 em box.
		// White, so not an underline (a dark rule as dark as the text passes since round 3).
		"a bar over descenders":        {data: doc("BT /F1 12 Tf 72 300 Td (gypsy) Tj ET 1 g 72 297.6 40 1 re f", ""), want: ErrPDFScanHiddenContent},
		"a bar over wide-stroked text": {data: doc("BT /F1 12 Tf 10 w 1 Tr 72 300 Td (Outline) Tj ET 0 g 72 312 50 2 re f", ""), want: ErrPDFScanHiddenContent},
		// 7. Later text over earlier text.
		"text typed over text":     {data: doc(visibleLine+" BT /F1 12 Tf 72 300 Td (XXXXXXXX) Tj ET", ""), want: ErrPDFScanHiddenContent},
		"text set next to text":    {data: doc("BT /F1 12 Tf 72 300 Td (Visible) Tj ( line) Tj ET", ""), text: "Visible line\n"},
		"two lines at 1.2 leading": {data: doc(textPage("First line", "Second line"), ""), text: "First line\nSecond line\n"},
		// 8. ToUnicode claims text the glyphs do not draw.
		"a standard font whose ToUnicode says Hidden claim": {data: func() []byte {
			f, cmap := toUnicode("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /ToUnicode 6 0 R >>", "00480069006400640065006E00200063006C00610069006D")
			return rebuildXref(t, bytes.Replace(doc("BT /F1 12 Tf 72 300 Td (A) Tj ET", "", cmap), []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"), []byte(f), 1))
		}(), want: ErrPDFNoTextLayer},
		"a standard font whose ToUnicode says B for A": {data: func() []byte {
			f, cmap := toUnicode("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /ToUnicode 6 0 R >>", "0042")
			return rebuildXref(t, bytes.Replace(doc("BT /F1 12 Tf 72 300 Td (A) Tj ET", "", cmap), []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"), []byte(f), 1))
		}(), want: ErrPDFNoTextLayer},
		"an embedded font mapping one code to many characters": {data: func() []byte {
			f, cmap := toUnicode("<< /Type /Font /Subtype /TrueType /BaseFont /ABCDEF+Custom /FirstChar 65 /LastChar 65 /Widths [600] /ToUnicode 6 0 R "+
				"/FontDescriptor << /Type /FontDescriptor /FontName /ABCDEF+Custom /Flags 32 /FontBBox [0 -200 1000 900] /ItalicAngle 0 /Ascent 900 /Descent -200 /CapHeight 700 /StemV 80 >> >>",
				"00480069006400640065006E")
			return rebuildXref(t, bytes.Replace(doc("BT /F1 12 Tf 72 300 Td (A) Tj ET", "", cmap), []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"), []byte(f), 1))
		}(), want: ErrPDFNoTextLayer},
		"an embedded font mapping fi to a ligature": {data: func() []byte {
			f, cmap := toUnicode("<< /Type /Font /Subtype /TrueType /BaseFont /ABCDEF+Custom /FirstChar 65 /LastChar 65 /Widths [600] /ToUnicode 6 0 R "+
				"/FontDescriptor << /Type /FontDescriptor /FontName /ABCDEF+Custom /Flags 32 /FontBBox [0 -200 1000 900] /ItalicAngle 0 /Ascent 900 /Descent -200 /CapHeight 700 /StemV 80 >> >>",
				"00660069")
			return rebuildXref(t, bytes.Replace(doc("BT /F1 12 Tf 72 300 Td (A) Tj ET", "", cmap), []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"), []byte(f), 1))
		}(), text: "fi\n"},
		// 9. Q restores the decoder with the font: the A drawn in Helvetica is stored as A.
		"Q restores the font's decoder": {data: func() []byte {
			f, cmap := toUnicode("<< /Type /Font /Subtype /TrueType /BaseFont /ABCDEF+Custom /FirstChar 65 /LastChar 65 /Widths [600] /ToUnicode 7 0 R "+
				"/FontDescriptor << /Type /FontDescriptor /FontName /ABCDEF+Custom /Flags 32 /FontBBox [0 -200 1000 900] /ItalicAngle 0 /Ascent 900 /Descent -200 /CapHeight 700 /StemV 80 >> >>",
				"005A")
			return twoFonts("BT /F1 12 Tf ET q BT /F2 12 Tf ET Q BT 72 300 Td (A) Tj ET", f, cmap)
		}(), text: "A\n"},
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
