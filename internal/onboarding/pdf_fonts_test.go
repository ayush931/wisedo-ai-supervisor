package onboarding

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Security review of the overpaint check's font metrics (WIS-625 post-merge): a code covers its
// advance or its font box, whichever is wider; a code with no positive width, or one only
// MissingWidth would place, cannot be placed; only a non-embedded standard font may go without
// /Widths, and then Adobe's widths place it (no full-em estimate). An Identity-H Type0 font with a
// ToUnicode CMap still uploads.
func TestPDFFontMetricsPlaceText(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	font := func(content, fontObj string, extra ...string) []byte {
		b := pdfDoc{pages: []string{content}, extra: extra}.bytes()
		return rebuildXref(t, bytes.Replace(b, []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"), []byte(fontObj), 1))
	}
	widths := func(w string) string {
		return "<< /Type /Font /Subtype /Type1 /BaseFont /Custom /FirstChar 32 /LastChar 126 /Widths [" + strings.TrimSpace(strings.Repeat(w+" ", 95)) +
			"] /FontDescriptor << /Type /FontDescriptor /FontName /Custom /Flags 32 /FontBBox [0 0 1000 1000] /ItalicAngle 0 /Ascent 900 /Descent -200 /CapHeight 700 /StemV 80 >> >>"
	}
	hidden := "BT /F1 12 Tf 72 300 Td (Hidden) Tj ET 1 g 76 300 4 8 re f"
	cmap := "/CIDInit /ProcSet findresource begin 12 dict begin begincmap /CMapName /Adobe-Identity-UCS def 1 begincodespacerange <0000> <FFFF> endcodespacerange " +
		"2 beginbfchar <0001> <0041> <0002> <0042> endbfchar endcmap CMapName currentdict /CMap defineresource pop end end"
	for name, c := range map[string]struct {
		data []byte
		text string
		want error
	}{
		// 1. A glyph is drawn over its font box, not only its advance.
		// Since the next security review a code under a tenth of an em cannot be placed at all.
		"a box over glyphs with 1/1000 em advances": {data: font(hidden, widths("1")), want: ErrPDFNoTextLayer},
		"a zero width":     {data: font("BT /F1 12 Tf 72 300 Td (Zero) Tj ET", widths("0")), want: ErrPDFNoTextLayer},
		"a negative width": {data: font("BT /F1 12 Tf 72 300 Td (Back) Tj ET", widths("-500")), want: ErrPDFNoTextLayer},
		"a code outside /Widths (MissingWidth)": {data: font("BT /F1 12 Tf 72 300 Td (AB) Tj ET",
			"<< /Type /Font /Subtype /Type1 /BaseFont /Custom /FirstChar 65 /LastChar 65 /Widths [500] /FontDescriptor << /Type /FontDescriptor /FontName /Custom /Flags 32 /FontBBox [0 0 1000 1000] /ItalicAngle 0 /Ascent 900 /Descent -200 /CapHeight 700 /StemV 80 /MissingWidth 500 >> >>"),
			want: ErrPDFNoTextLayer},
		// 2. Only a non-embedded standard font goes without /Widths.
		"a non-standard font without /Widths": {data: font(visibleLine, "<< /Type /Font /Subtype /Type1 /BaseFont /Custom >>"), want: ErrPDFNoTextLayer},
		"an embedded standard font without /Widths": {data: font(visibleLine,
			"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /FontDescriptor << /Type /FontDescriptor /FontName /Helvetica /Flags 32 /FontBBox [0 0 1000 1000] /ItalicAngle 0 /Ascent 900 /Descent -200 /CapHeight 700 /StemV 80 /FontFile 6 0 R >> >>",
			"<< /Length 1 >>\nstream\nx\nendstream"), want: ErrPDFNoTextLayer},
		"a standard font in MacRomanEncoding without /Widths": {data: font(visibleLine, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /MacRomanEncoding >>"), want: ErrPDFNoTextLayer},
		"a subset-named standard font, WinAnsi with /Differences": {data: font(visibleLine,
			"<< /Type /Font /Subtype /Type1 /BaseFont /ABCDEF+Helvetica /Encoding << /BaseEncoding /WinAnsiEncoding /Differences [65 /B] >> >>"), text: "Visible line\n"},
		// 3. Adobe's Helvetica widths end "Visible line" near x 135, so a box at 140 is clear of it
		// (the earlier full-em estimate ran to x 216 and refused it).
		"a box just after Helvetica text": {data: font(visibleLine+" 0 g 140 300 10 8 re f", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"), text: "Visible line\n"},
		// 4. An Identity-H Type0 font with a ToUnicode CMap.
		"an Identity-H font with ToUnicode": {data: font("BT /F1 12 Tf 72 300 Td <00010002> Tj ET",
			"<< /Type /Font /Subtype /Type0 /BaseFont /ABCDEF+Arial /Encoding /Identity-H /ToUnicode 6 0 R /DescendantFonts [<< /Type /Font /Subtype /CIDFontType2 /BaseFont /ABCDEF+Arial "+
				"/CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /FontDescriptor << /Type /FontDescriptor /FontName /ABCDEF+Arial /Flags 32 /FontBBox [-200 -200 1000 900] "+
				"/ItalicAngle 0 /Ascent 900 /Descent -200 /CapHeight 700 /StemV 80 >> /DW 1000 /W [1 [600 650]] >>] >>",
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(cmap), cmap)), text: "AB\n"},
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
