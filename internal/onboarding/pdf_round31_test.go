package onboarding

import (
	"bytes"
	"errors"
	"testing"
)

// Codex round 25 on ac6149db (WIS-625 post-merge).
//   - Translations of 1073741824, 1000 and -1073741824 sum to 1000 in float64 and 1024 in a
//     float32 reader (MuPDF): text on the page here is off it there. Every operand, and every
//     term and component of the CTM and text matrices, is bounded at 1e5 units (maxCoordinate).
//   - A raw CR in a literal string is LF to a viewer and code 13 to the extractor: with
//     /Differences [10 /space 13 /one] the page shows "1 " and the extractor read "11". Content
//     holding one is not read.
//   - A blended draw clipped away entirely counted as blended paint on an isolated page.
func TestPDFHugeNumbersRawCRAndClippedBlendedDraws(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	states := "/ExtGState << /Mul << /BM /Multiply >> >>"
	isolated := "/Group << /S /Transparency /I true >>"
	diff := func(content string) []byte {
		b := pdfDoc{pages: []string{content}}.bytes()
		return rebuildXref(t, bytes.Replace(b, []byte("/BaseFont /Helvetica >>"), []byte("/BaseFont /Helvetica /Encoding << /Type /Encoding /BaseEncoding /WinAnsiEncoding /Differences [10 /space 13 /one] >> >>"), 1))
	}
	for name, c := range map[string]struct {
		data []byte
		text string
		want error
	}{
		"translations that cancel in float64 only": {data: pdfDoc{pages: []string{"q 1 0 0 1 1073741824 0 cm 1 0 0 1 1000 0 cm 1 0 0 1 -1073741824 0 cm BT /F1 12 Tf -900 300 Td (Hidden) Tj ET Q " + visibleLine}}.bytes(), want: ErrUnreadableSource},
		"a CTM scaled past the bound":              {data: pdfDoc{pages: []string{"q 1000 0 0 1000 0 0 cm 1000 0 0 1000 0 0 cm 0.000001 0 0 0.000001 0 0 cm Q " + visibleLine}}.bytes(), want: ErrUnreadableSource},
		"a text position moved past the bound":     {data: pdfDoc{pages: []string{"BT /F1 12 Tf 99999 0 Td 99999 0 Td -99999 0 Td -99926 300 Td (Hidden) Tj ET " + visibleLine}}.bytes(), want: ErrUnreadableSource},
		"a raw CR in a shown string":               {data: diff("BT /F1 12 Tf 72 300 Td (Visible\rline) Tj ET"), want: ErrPDFNoTextLayer},
		// "\\r" is code 13 to both, glyph one: the page shows the "1" that is stored.
		"an escaped CR in a shown string":             {data: diff("BT /F1 12 Tf 72 300 Td (Visible line\\r) Tj ET"), text: "Visible line1\n"},
		"ordinary coordinates":                        {data: pdfDoc{pages: []string{"q 1 0 0 1 100 0 cm 1 0 0 1 -100 0 cm Q " + visibleLine}}.bytes(), text: "Visible line\n"},
		"a blended image clipped away, isolated page": {data: pdfDoc{pages: []string{"q 0 0 10 10 re W n /Mul gs 300 0 0 300 50 200 cm /Im1 Do Q " + visibleLine}, resources: states + " " + imageResources(6), pageExtra: isolated, extra: []string{imageObject}}.bytes(), text: "Visible line\n"},
		"a blended image shown, isolated page":        {data: pdfDoc{pages: []string{"q /Mul gs 50 0 0 50 400 600 cm /Im1 Do Q " + visibleLine}, resources: states + " " + imageResources(6), pageExtra: isolated, extra: []string{imageObject}}.bytes(), want: ErrUnreadableSource},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Extract(t.Context(), c.data, KindPDF, DefaultLimits)
			if c.want != nil {
				if got != "" || !errors.Is(err, c.want) {
					t.Fatalf("got %q %v, want %v", got, err, c.want)
				}
				return
			}
			if err != nil || got != c.text {
				t.Fatalf("got %q %v, want %q", got, err, c.text)
			}
		})
	}
}
