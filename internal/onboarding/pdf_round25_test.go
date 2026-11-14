package onboarding

import (
	"errors"
	"testing"
)

// Codex round 20 on cfb87ab8 (WIS-625 post-merge). One stroke measure (strokeHalfWidths) for
// paths, stroked text and its checks: half the declared width through the CTM, at least half a
// physical point, before any join reaches past it.
//   - A 0 w V under ".01 cm" with 10 M: the floor came after the join, so its miter stopped short.
//   - Under /UserUnit 0.5 the floor was half a page unit, a quarter point.
//   - "1 Tr" with "0 w" at 120 Tf under ".01 cm": a one-pixel outline fills the 1.2 pt letters;
//     with the floor the stroke-to-size rule refuses it.
//   - A rectangle under a rotation keeps right angles (a similarity), as under a per-axis scale.
//   - A shading under a clip of no area paints nothing.
func TestPDFOneStrokeMeasureAndShadingClips(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	shading := "<< /ShadingType 2 /ColorSpace /DeviceGray /Coords [0 0 1 0] /Function << /FunctionType 2 /Domain [0 1] /N 1 >> >>"
	for name, c := range map[string]struct {
		doc  pdfDoc
		text string
		want error
	}{
		// The V's vertex is at y 310; a half-point pen mitered at 35 degrees reaches 1.65 pt below.
		"a hairline V mitered into the text": {doc: pdfDoc{pages: []string{visibleLine + " q .01 0 0 .01 0 0 cm 0 G 0 w 10 M 0 j 7200 40000 m 10000 31000 l 12800 40000 l S Q"}}, want: ErrPDFScanHiddenContent},
		"a hairline V with bevel joins":      {doc: pdfDoc{pages: []string{visibleLine + " q .01 0 0 .01 0 0 cm 0 G 0 w 10 M 2 j 7200 40000 m 10000 31000 l 12800 40000 l S Q"}}, text: "Visible line\n"},
		"a thin rule under the text":         {doc: pdfDoc{pages: []string{visibleLine + " q 0 G 0.25 w 72 295 m 129 295 l S Q"}}, text: "Visible line\n"},
		// Half a point is one page unit at UserUnit 0.5: a hairline at y 309.25 reaches 308.25.
		"a hairline under UserUnit 0.5":        {doc: pdfDoc{pages: []string{visibleLine + " q 0 G 0 w 72 309.25 m 129 309.25 l S Q"}, pageExtra: "/UserUnit 0.5"}, want: ErrPDFScanHiddenContent},
		"a hairline under UserUnit 1":          {doc: pdfDoc{pages: []string{visibleLine + " q 0 G 0 w 72 309.25 m 129 309.25 l S Q"}}, text: "Visible line\n"},
		"hairline outlined text a pixel thick": {doc: pdfDoc{pages: []string{visibleLine + " q .01 0 0 .01 0 0 cm 0 w BT /F1 120 Tf 1 Tr 7200 50000 Td (Outlined) Tj ET Q"}}, want: ErrPDFScanHiddenContent},
		// A square turned 45 degrees with its bottom corner at (140, 312): a right-angle corner
		// reaches 0.7 pt, a miter 5 pt (into the line's box).
		"a rotated square near the text":    {doc: pdfDoc{pages: []string{visibleLine + " q 0 G 1 w 10 M 0 j 0.70710678 0.70710678 -0.70710678 0.70710678 140 312 cm 0 0 20 20 re S Q"}}, text: "Visible line\n"},
		"a shading under a zero-width clip": {doc: pdfDoc{pages: []string{visibleLine + " q 100 290 0 25 re W n /Sh1 sh Q"}, resources: "/Shading << /Sh1 6 0 R >>", extra: []string{shading}}, text: "Visible line\n"},
		"a shading clipped onto the text":   {doc: pdfDoc{pages: []string{visibleLine + " q 100 290 10 25 re W n /Sh1 sh Q"}, resources: "/Shading << /Sh1 6 0 R >>", extra: []string{shading}}, want: ErrPDFScanHiddenContent},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Extract(t.Context(), c.doc.bytes(), KindPDF, DefaultLimits)
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
