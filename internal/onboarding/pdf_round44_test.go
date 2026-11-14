package onboarding

import (
	"errors"
	"strings"
	"testing"
)

// Codex round 37 on fd6f5624 (WIS-625 post-merge).
//   - Stroked text was checked against what lies under it by its unstroked box: a 10 w outline
//     "T" (2 Tr) beside a dark bar reaches over it. The region under text is widened by the
//     stroke's half width, as the text's box already was.
//   - Two redraws of one original rejected each other: three 40 pt "1"s at x 100, 100.5 and 101
//     form one bold group, exempt within itself.
//   - A no-op /DecodeParms with DCTDecode ([null] or <<>>) refused a scan.
func TestPDFStrokedRegionsBoldGroupsAndNoOpDCTParameters(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	for name, c := range map[string]struct {
		page string
		text string
		want error
	}{
		"an outlined T beside a dark bar": {page: "q 0 g 195 300 4.5 60 re f Q BT /F1 100 Tf 0 g 0 G 10 w 2 Tr 200 300 Td (T) Tj ET", want: ErrPDFScanHiddenContent},
		"an outlined T clear of the bar":  {page: "q 0 g 150 300 4.5 60 re f Q BT /F1 100 Tf 0 g 0 G 10 w 2 Tr 200 300 Td (T) Tj ET", text: "T\n"},
		"a bold group of three":           {page: "BT /F1 40 Tf 100 400 Td (1) Tj ET BT /F1 40 Tf 100.5 400 Td (1) Tj ET BT /F1 40 Tf 101 400 Td (1) Tj ET", text: "1\n"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Extract(t.Context(), pdfDoc{pages: []string{c.page}}.bytes(), KindPDF, DefaultLimits)
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
	jpeg := figureObject(scanImage(t, 1000, 1000, "jpeg"), 1000, 1000)
	scan := "q 612 0 0 792 0 0 cm /Im1 Do Q"
	for name, c := range map[string]struct {
		image string
		pages int
	}{
		"DCT with [null] parameters":      {image: strings.Replace(jpeg, "/Filter /DCTDecode", "/Filter [/DCTDecode] /DecodeParms [null]", 1), pages: 1},
		"DCT with empty parameters":       {image: strings.Replace(jpeg, "/Filter /DCTDecode", "/Filter /DCTDecode /DecodeParms << >>", 1), pages: 1},
		"DCT with a colour transform":     {image: strings.Replace(jpeg, "/Filter /DCTDecode", "/Filter /DCTDecode /DecodeParms << /ColorTransform 1 >>", 1)},
		"an array filter with dict parms": {image: strings.Replace(jpeg, "/Filter /DCTDecode", "/Filter [/DCTDecode] /DecodeParms << >>", 1)},
	} {
		t.Run(name, func(t *testing.T) {
			pages, err := ScanPagesOfPDF(t.Context(), pdfDoc{pages: []string{scan}, resources: imageResources(6), extra: []string{c.image}}.bytes())
			if c.pages == 0 {
				if pages != nil || err == nil {
					t.Fatalf("%d pages %v", len(pages), err)
				}
				return
			}
			if err != nil || len(pages) != c.pages {
				t.Fatalf("%d pages %v", len(pages), err)
			}
		})
	}
}
