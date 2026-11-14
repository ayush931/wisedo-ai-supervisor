package onboarding

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Security review of 37f7c920 (WIS-625 post-merge): rules exempt only as one underline and one
// strike-through, text cells overlap with no vertical tolerance and include a stroke, a C1 code is
// trusted only for WinAnsi's own glyph and no control character is stored.
func TestPDFRulesCellsAndControls(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	doc := func(content string, extra ...string) []byte {
		return pdfDoc{pages: []string{content}, extra: extra}.bytes()
	}
	var stacked strings.Builder
	for y := 300.0; y < 309; y += 1 {
		fmt.Fprintf(&stacked, " 0 g 72 %.1f 60 0.8 re f", y)
	}
	embedded := func(dst string) []byte {
		cmap := "/CIDInit /ProcSet findresource begin 12 dict begin begincmap /CMapName /X def 1 begincodespacerange <00> <FF> endcodespacerange 1 beginbfchar <41> <" + dst + "> endbfchar endcmap CMapName currentdict /CMap defineresource pop end end"
		f := "<< /Type /Font /Subtype /TrueType /BaseFont /ABCDEF+Custom /FirstChar 65 /LastChar 65 /Widths [600] /ToUnicode 6 0 R " +
			"/FontDescriptor << /Type /FontDescriptor /FontName /ABCDEF+Custom /Flags 32 /FontBBox [0 -200 1000 900] /ItalicAngle 0 /Ascent 900 /Descent -200 /CapHeight 700 /StemV 80 >> >>"
		b := doc("BT /F1 12 Tf 72 300 Td (A) Tj ET", fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(cmap), cmap))
		return rebuildXref(t, bytes.Replace(b, []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"), []byte(f), 1))
	}
	for name, c := range map[string]struct {
		data []byte
		text string
		want error
	}{
		// 1. Rules.
		"nine thin black rules stacked over the text": {data: doc(visibleLine + stacked.String()), want: ErrPDFScanHiddenContent},
		"two underlines":                    {data: doc(visibleLine + " 0 g 72 298 60 0.6 re f 0 g 72 299 60 0.6 re f"), want: ErrPDFScanHiddenContent},
		"a thin rule across the x-height":   {data: doc(visibleLine + " 0 g 72 306 60 0.8 re f"), want: ErrPDFScanHiddenContent},
		"an underline and a strike-through": {data: doc(visibleLine + " 0 g 72 298 60 0.6 re f 0 g 72 303 60 0.8 re f"), text: "Visible line\n"},
		// 3. Text cells: no vertical tolerance, and a stroked one reaches its half line width.
		"lines closer than their glyphs":   {data: doc("BT /F1 12 Tf 72 300 Td (gypsy) Tj ET BT /F1 12 Tf 72 291 Td (Next line) Tj ET"), want: ErrPDFScanHiddenContent}, // descenders over capitals
		"wide-stroked text set against it": {data: doc(visibleLine + " BT /F1 12 Tf 10 w 1 Tr 132 300 Td (Next) Tj ET"), want: ErrPDFScanHiddenContent},
		"two lines 14 pt apart":            {data: doc(textPage("First line") + " BT /F1 12 Tf 72 706 Td (Second line) Tj ET"), text: "First line\nSecond line\n"},
		// 5. No control character is stored.
		"a code mapped to a control character": {data: embedded("0007"), want: ErrPDFTextUnreadable},
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
	// 4. A raw C1 code is accepted for WinAnsi's own glyph at that code only.
	t.Run("a C1 code for another glyph than WinAnsi's", func(t *testing.T) {
		own := fontMetrics{names: map[int]string{0x93: "quotedblleft"}}
		other := fontMetrics{names: map[int]string{0x93: "quotedblright"}}
		if _, honest := own.codeText(latin1{}, "\x93", 0x93); !honest {
			t.Fatal("WinAnsi's own glyph at 0x93 was refused")
		}
		if _, honest := other.codeText(latin1{}, "\x93", 0x93); honest {
			t.Fatal("0x93 read as itself was accepted for quotedblright")
		}
	})
}

// latin1 decodes each byte as the character of that code, as the extractor's /Differences decoder
// does for a code it has no name for.
type latin1 struct{}

func (latin1) Decode(raw string) string {
	r := make([]rune, len(raw))
	for i := range raw {
		r[i] = rune(raw[i])
	}
	return string(r)
}
