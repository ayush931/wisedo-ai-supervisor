package onboarding

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Codex round 4 on 1d7b7a5a (WIS-625 post-merge).
func TestPDFSeparatorsColoursScalesAndRedraws(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	doc := func(content, resources string, extra ...string) []byte {
		return pdfDoc{pages: []string{content}, resources: resources, extra: extra}.bytes()
	}
	icc := func(n int, more string) string {
		body := testICCProfiles[n]
		return fmt.Sprintf("<< /N %d %s /Length %d >>\nstream\n%s\nendstream", n, more, len(body), body)
	}
	cmap := "/CIDInit /ProcSet findresource begin 12 dict begin begincmap /CMapName /X def 1 begincodespacerange <00> <FF> endcodespacerange 2 beginbfchar <41> <0041> <0A> <005A> endbfchar endcmap CMapName currentdict /CMap defineresource pop end end"
	custom := "<< /Type /Font /Subtype /TrueType /BaseFont /ABCDEF+Custom /FirstChar 32 /LastChar 126 /Widths [" + strings.TrimSpace(strings.Repeat("600 ", 95)) + "] /ToUnicode 6 0 R " +
		"/FontDescriptor << /Type /FontDescriptor /FontName /ABCDEF+Custom /Flags 32 /FontBBox [0 -200 1000 900] /ItalicAngle 0 /Ascent 900 /Descent -200 /CapHeight 700 /StemV 80 >> >>"
	var chain, many strings.Builder
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&chain, " BT /F1 12 Tf %.2f 300 Td (Visible line) Tj ET", 72+0.48*float64(i)) // each 0.04 em past the last
		many.WriteString(" " + visibleLine)
	}
	winAnsi := func(content string) []byte {
		return rebuildXref(t, bytes.Replace(doc(content, ""), []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
			[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"), 1))
	}
	for name, c := range map[string]struct {
		data []byte
		text string
		want error
	}{
		// 1. The line break after a TJ is not decoded through the font.
		"a TJ whose font maps the line break to Z": {data: rebuildXref(t, bytes.Replace(doc("BT /F1 12 Tf 72 300 Td [(A)] TJ ET", "",
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(cmap), cmap)), []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"), []byte(custom), 1)), text: "A\n"},
		// 2. The initial colour of a CMYK space is white; Range must be the device's.
		// Codex round 5: DeviceCMYK starts black (0 0 0 1, ISO 32000-1 Table 74); only an ICC space
		// starts at all zeros, white for CMYK.
		"text after selecting DeviceCMYK alone":         {data: doc("/DeviceCMYK cs "+visibleLine, ""), text: "Visible line\n"},
		"black text on a DeviceCMYK initial-colour box": {data: doc("/DeviceCMYK cs 60 290 200 25 re f 0 g "+visibleLine, ""), want: ErrPDFScanHiddenContent},
		"text after selecting an ICC CMYK alone":        {data: doc("/CS0 cs "+visibleLine, "/ColorSpace << /CS0 [/ICCBased 6 0 R] >>", icc(4, "")), want: ErrPDFScanHiddenContent},
		"black DeviceCMYK text":                         {data: doc("/DeviceCMYK cs 0 0 0 1 scn "+visibleLine, ""), text: "Visible line\n"},
		"an ICC gray space with another Range":          {data: doc("/CS0 cs 0 scn "+visibleLine, "/ColorSpace << /CS0 [/ICCBased 6 0 R] >>", icc(1, "/Range [0 2]")), want: ErrPDFScanHiddenContent},
		// 3. Text squeezed to a tenth of its width.
		"text at 10 Tz": {data: doc("BT /F1 12 Tf 10 Tz 72 300 Td (Hidden) Tj ET "+visibleLine, ""), want: ErrPDFScanHiddenContent},
		// Security review of 3858a06c: the same squeeze through the text matrix or the CTM.
		"text squeezed by Tm":                   {data: doc("BT /F1 12 Tf 0.1 0 0 1 72 500 Tm (Hidden) Tj ET "+visibleLine, ""), want: ErrPDFScanHiddenContent},
		"text squeezed by cm":                   {data: doc("q 0.1 0 0 1 0 0 cm BT /F1 12 Tf 720 500 Td (Hidden) Tj ET Q "+visibleLine, ""), want: ErrPDFScanHiddenContent},
		"text slanted by Tm (synthetic italic)": {data: doc("BT /F1 12 Tf 1 0 0.2 1 72 300 Tm (Slanted) Tj ET", ""), text: "Slanted\n"},
		// 4. Simulated bold is measured against the original, at most four times.
		"a chain of redraws walking away": {data: doc(chain.String(), ""), want: ErrPDFScanHiddenContent},
		"five redraws in place":           {data: doc(many.String(), ""), want: ErrPDFScanHiddenContent},
		"one redraw (simulated bold)":     {data: doc(visibleLine+" BT /F1 12 Tf 72.3 300 Td (Visible line) Tj ET", ""), text: "Visible line\n"},
		// 5. A sheared pen reaches further than half its width.
		"a sheared stroke beside the text": {data: doc(visibleLine+" q 1 0 1 0.2 0 0 cm 0 G 20 w -1480 1500 m -1480 1540 l S Q", ""), want: ErrPDFScanHiddenContent},
		// 6. An accent on the next line at 12 pt leading does not reach a capital above it.
		"H over an E acute at 12 pt leading": {data: winAnsi("BT /F1 12 Tf 72 312 Td (H) Tj ET BT /F1 12 Tf 72 300 Td (\xc9) Tj ET"), text: "H\nÉ\n"},
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
