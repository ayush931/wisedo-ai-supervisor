package onboarding

import (
	"bytes"
	"fmt"
	"testing"
)

// Codex round 38 on 4d8ace58 (WIS-625 post-merge).
//   - A 9.5-point dark bar in the strike-through band at 100 pt covered a hyphen (or an en dash)
//     entirely and was exempt as a strike-through. A rule is exempt only if every glyph it crosses
//     keeps at least 30% of its ink height outside it.
//   - A content stream with /Filter [/FlateDecode] /DecodeParms [null] was refused.
//   - Helvetica's fi glyph (code 0xAE in its built-in StandardEncoding) mapped by ToUnicode to "fi" (two letters) was refused against U+FB01.
//
// errRefused marks a case that must be refused, whatever the reason given.
var errRefused = errorString("refused")

type errorString string

func (e errorString) Error() string { return string(e) }

func TestPDFRulesOverWholeGlyphsFlateParmsAndLigatureLetters(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	bar := " q 0 g 72 322.95 300 9.5 re f Q"
	ligature := func(dst string) []byte {
		cmap := "/CIDInit /ProcSet findresource begin 12 dict begin begincmap /CMapName /L def 1 begincodespacerange <00> <FF> endcodespacerange 1 beginbfchar <AE> <" + dst + "> endbfchar endcmap CMapName currentdict /CMap defineresource pop end end"
		b := pdfDoc{pages: []string{"BT /F1 12 Tf 72 300 Td (\xae) Tj ET"}, extra: []string{fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(cmap), cmap)}}.bytes()
		return rebuildXref(t, bytes.Replace(b, []byte("/BaseFont /Helvetica >>"), []byte("/BaseFont /Helvetica /ToUnicode 6 0 R >>"), 1))
	}
	flate := pdfDoc{pages: []string{visibleLine}, compress: map[int]bool{0: true}}.bytes()
	for name, c := range map[string]struct {
		data []byte
		text string
		want error
	}{
		"a bar over the hyphen of -10":     {data: pdfDoc{pages: []string{"BT /F1 100 Tf 72 300 Td (-10) Tj ET" + bar}}.bytes(), want: errRefused},
		"a bar over an en dash":            {data: pdfDoc{pages: []string{"BT /F1 100 Tf 72 300 Td (\x9610) Tj ET" + bar}}.bytes(), want: errRefused},
		"a strike-through across letters":  {data: pdfDoc{pages: []string{"BT /F1 100 Tf 72 300 Td (Hold) Tj ET" + bar}}.bytes(), text: "Hold\n"},
		"Flate content with [null] parms":  {data: rebuildXref(t, bytes.Replace(flate, []byte("/Filter /FlateDecode"), []byte("/Filter [/FlateDecode] /DecodeParms [null]"), 1)), text: "Visible line\n"},
		"Flate content with [<< >>] parms": {data: rebuildXref(t, bytes.Replace(flate, []byte("/Filter /FlateDecode"), []byte("/Filter [/FlateDecode] /DecodeParms [<< >>]"), 1)), text: "Visible line\n"},
		"the fi glyph read as f and i":     {data: ligature("00660069"), text: "fi\n"},
		"the fi glyph read as f and l":     {data: ligature("0066006C"), want: errRefused},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Extract(t.Context(), c.data, KindPDF, DefaultLimits)
			if c.want != nil {
				if got != "" || err == nil {
					t.Fatalf("got %q %v, want a refusal", got, err)
				}
				return
			}
			if err != nil || got != c.text {
				t.Fatalf("got %q %v, want %q", got, err, c.text)
			}
		})
	}
}
