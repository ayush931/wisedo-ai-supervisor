package onboarding

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// testICCProfiles stand in for listed standard profiles in this test binary, which the PDF child
// also runs as: their hashes join iccProfiles in both.
var testICCProfiles = map[int]string{1: "test profile, 1 component", 3: "test profile, 3 components", 4: "test profile, 4 components"}

func init() {
	for n, body := range testICCProfiles {
		sum := sha256.Sum256([]byte(body))
		iccProfiles[hex.EncodeToString(sum[:])] = int64(n)
	}
}

// Security review of fdc2135b and false-refusal cuts (WIS-625 post-merge): text is placed by where
// its ink may reach, across operators too; apostrophes, WinAnsi's 128-159, ICCBased colour,
// simulated bold and underlines no longer refuse ordinary documents, while the hiding forms of each
// still do.
func TestPDFInkAcrossOperatorsAndOrdinaryMarks(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	doc := func(content, resources string, extra ...string) []byte {
		return pdfDoc{pages: []string{content}, resources: resources, extra: extra}.bytes()
	}
	font := func(content, fontObj string, extra ...string) []byte {
		return rebuildXref(t, bytes.Replace(doc(content, "", extra...), []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"), []byte(fontObj), 1))
	}
	ones := "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /FirstChar 32 /LastChar 126 /Widths [" + strings.TrimSpace(strings.Repeat("1 ", 95)) + "] >>"
	// icc is an ICC profile stream: a listed test profile (testICCProfiles), or an unlisted one.
	icc := func(n int, alt string, listed bool) string {
		body := "unlisted profile"
		if listed {
			body = testICCProfiles[n]
		}
		return fmt.Sprintf("<< /N %d %s /Length %d >>\nstream\n%s\nendstream", n, alt, len(body), body)
	}
	toUnicode := func(dst string) string {
		cmap := "/CIDInit /ProcSet findresource begin 12 dict begin begincmap /CMapName /X def 1 begincodespacerange <00> <FF> endcodespacerange 1 beginbfchar <27> <" + dst + "> endbfchar endcmap CMapName currentdict /CMap defineresource pop end end"
		return fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(cmap), cmap)
	}
	for name, c := range map[string]struct {
		data []byte
		text string
		want error
	}{
		// 1. Ink past the pen: a dark box under where H is drawn, past its 1/1000 em declared advance.
		"dark paint under ink past the pen": {data: font("0 g 76 300 4 6 re f BT /F1 12 Tf 72 300 Td (H) Tj ET", ones), want: ErrPDFScanHiddenContent},
		// 2. Two operators stacked by tiny declared widths, one code each.
		"two operators stacked by declared widths": {data: font("BT /F1 12 Tf 72 300 Td (H) Tj (W) Tj ET", ones), want: ErrPDFScanHiddenContent},
		// 3. Apostrophes and WinAnsi's 128-159.
		"an apostrophe in StandardEncoding": {data: doc("BT /F1 12 Tf 72 300 Td (It's) Tj ET", ""), text: "It's\n"},
		"WinAnsi's euro":                    {data: font("BT /F1 12 Tf 72 300 Td (\x80 5) Tj ET", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"), text: "€ 5\n"},
		"WinAnsi quotes under /Differences": {data: font("BT /F1 12 Tf 72 300 Td (\x93Quote\x94) Tj ET",
			"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding << /BaseEncoding /WinAnsiEncoding /Differences [65 /A] >> >>"), text: "\u201cQuote\u201d\n"}, // the glyphs' characters, not the C1 codes
		"an apostrophe ToUnicode maps to an opening quote": {data: font("BT /F1 12 Tf 72 300 Td (It's) Tj ET",
			"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /ToUnicode 6 0 R >>", toUnicode("2018")), want: ErrPDFNoTextLayer},
		// 4. ICCBased colour.
		"black text in an ICC RGB space":            {data: doc("/CS0 cs 0 0 0 scn "+visibleLine, "/ColorSpace << /CS0 [/ICCBased 6 0 R] >>", icc(3, "", true)), text: "Visible line\n"},
		"black text, ICC with a matching alternate": {data: doc("/CS0 cs 0 scn "+visibleLine, "/ColorSpace << /CS0 [/ICCBased 6 0 R] >>", icc(1, "/Alternate /DeviceGray", true)), text: "Visible line\n"},
		"white text in an ICC RGB space":            {data: doc("/CS0 cs 1 1 1 scn "+visibleLine, "/ColorSpace << /CS0 [/ICCBased 6 0 R] >>", icc(3, "", true)), want: ErrPDFScanHiddenContent},
		// Security review of 37f7c920: an ICC profile not on the list may map black to white.
		"black text in an unlisted ICC profile": {data: doc("/CS0 cs 0 0 0 scn "+visibleLine, "/ColorSpace << /CS0 [/ICCBased 6 0 R] >>", icc(3, "", false)), want: ErrPDFScanHiddenContent},
		"text in an Indexed space":              {data: doc("/CS0 cs 0 scn "+visibleLine, "/ColorSpace << /CS0 [/Indexed /DeviceRGB 1 <000000FFFFFF>] >>"), want: ErrPDFScanHiddenContent},
		// 5. Simulated bold: the same text again 0.3 pt along.
		"simulated bold":              {data: doc(visibleLine+" BT /F1 12 Tf 72.3 300 Td (Visible line) Tj ET", ""), text: "Visible line\n"}, // stored once
		"other text at a bold offset": {data: doc(visibleLine+" BT /F1 12 Tf 72.3 300 Td (Hidden text) Tj ET", ""), want: ErrPDFScanHiddenContent},
		// 6. Underlines and strike-through.
		"a black underline":      {data: doc(visibleLine+" 0 g 72 298 60 0.6 re f", ""), text: "Visible line\n"},
		"a black strike-through": {data: doc(visibleLine+" 0 g 72 303 60 0.8 re f", ""), text: "Visible line\n"},
		"a stroked underline":    {data: doc(visibleLine+" 0 G 0.5 w 72 298 m 130 298 l S", ""), text: "Visible line\n"},
		"a white strike-through": {data: doc(visibleLine+" 1 g 72 303 60 0.8 re f", ""), want: ErrPDFScanHiddenContent},
		"a thick black bar":      {data: doc(visibleLine+" 0 g 72 301 60 4 re f", ""), want: ErrPDFScanHiddenContent},
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
