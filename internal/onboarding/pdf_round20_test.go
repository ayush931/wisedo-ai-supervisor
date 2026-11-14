package onboarding

import (
	"errors"
	"testing"
)

// Codex round 15 on 14b8c067 (WIS-625 post-merge).
//   - d0 and d1 belong in Type 3 glyph procedures; PDF.js clips what follows d1 to its box, so in
//     a page or form they could hide text that was read. They refuse the file.
//   - The mixed-render-mode refusal counted empty shows; it counts shows that drew a code.
//   - Text wholly off the page, already dropped as unseen, counted as lost text on a blank page.
//   - The scan path read "/Im#31" as an invalid name instead of /Im1.
func TestPDFType3OperatorsEmptyShowsOffPageTextAndEscapedScanNames(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	figure := figureObject(scanImage(t, 1000, 1000, "jpeg"), 1000, 1000)
	scanPage := "q 612 0 0 792 0 0 cm /Im1 Do Q"
	layer := " BT /F1 12 Tf 3 Tr 72 700 Td (Invisible scanner layer) Tj ET"
	for name, c := range map[string]struct {
		doc  pdfDoc
		text string
		want error
	}{
		"d1 before the text":             {doc: pdfDoc{pages: []string{"0 0 0 0 100 100 d1 " + visibleLine}}, want: ErrUnreadableSource},
		"d0 in a page":                   {doc: pdfDoc{pages: []string{"0 0 d0 " + visibleLine}}, want: ErrUnreadableSource},
		"d1 in a form":                   {doc: pdfDoc{pages: []string{visibleLine + " /Fm1 Do"}, resources: "/XObject << /Fm1 6 0 R >>", extra: []string{formObject("", "0 0 0 0 1 1 d1")}}, want: ErrUnreadableSource},
		"an empty mode-3 TJ beside text": {doc: pdfDoc{pages: []string{visibleLine + " BT /F1 12 Tf 3 Tr 72 500 Td [] TJ ET"}}, text: "Visible line\n"},
		// The page is a scan with its invisible layer: no typed text, so the scan path reads it.
		"an empty mode-0 show before a scanner layer": {doc: pdfDoc{pages: []string{"BT /F1 12 Tf 72 300 Td () Tj ET " + scanPage + layer}, resources: imageResources(6), extra: []string{figure}}, want: ErrPDFNoTextLayer},
		"text wholly off a blank page":                {doc: pdfDoc{pages: []string{visibleLine, "BT /F1 12 Tf -500 300 Td (Off the page) Tj ET"}}, text: "Visible line\n"},
		"text partly off a blank page":                {doc: pdfDoc{pages: []string{visibleLine, "BT /F1 12 Tf -20 300 Td (Off the page) Tj ET"}}, want: ErrPDFScanHiddenContent},
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
	for name, page := range map[string]string{
		"an escaped image name":                       "q 612 0 0 792 0 0 cm /Im#31 Do Q",
		"an empty mode-0 show before a scanner layer": "BT /F1 12 Tf 72 300 Td () Tj ET " + scanPage + layer,
	} {
		t.Run("scan path: "+name, func(t *testing.T) {
			pages, err := ScanPagesOfPDF(t.Context(), pdfDoc{pages: []string{page}, resources: imageResources(6), extra: []string{figure}}.bytes())
			if err != nil || len(pages) != 1 {
				t.Fatalf("%d pages %v", len(pages), err)
			}
		})
	}
}
