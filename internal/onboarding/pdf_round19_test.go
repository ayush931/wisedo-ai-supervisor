package onboarding

import (
	"errors"
	"testing"
)

// Codex round 14 on 71b7a74e (WIS-625 post-merge).
//   - Inside BX ... EX an unknown word is ignored, but PDF.js splits "TrZz" into Tr and Zz: a
//     word that a known operator begins (or with a character vendor operators do not use)
//     refuses the file there.
//   - A TJ that shows no code ("[] TJ", "[()] TJ", "[-250] TJ") still drew the line break the
//     extractor adds, and a blank page holding one refused the upload.
//   - The searchable-scan check compared the layer's mode to the text "3": "+3 Tr" and "03 Tr"
//     were refused.
func TestPDFCompatibilityWordsEmptyArraysAndScanModes(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	for name, c := range map[string]struct {
		pages []string
		text  string
		want  error
	}{
		"a mode hidden in a vendor word":          {pages: []string{"BX 3 TrZz EX " + visibleLine}, want: ErrUnreadableSource},
		"a word with a character vendors avoid":   {pages: []string{"BX 1 Zz# EX " + visibleLine}, want: ErrUnreadableSource},
		"ordinary vendor hints":                   {pages: []string{"BX /Vendor 1 Zz /Hint Yy EX " + visibleLine}, text: "Visible line\n"},
		"an empty TJ on a blank page":             {pages: []string{visibleLine, "BT /F1 12 Tf 72 300 Td [] TJ ET"}, text: "Visible line\n"},
		"a TJ of an empty string on a blank page": {pages: []string{visibleLine, "BT /F1 12 Tf 72 300 Td [()] TJ ET"}, text: "Visible line\n"},
		"a TJ of numbers on a blank page":         {pages: []string{visibleLine, "BT /F1 12 Tf 72 300 Td [-250 100] TJ ET"}, text: "Visible line\n"},
		"a TJ that shows a code on a blank page":  {pages: []string{visibleLine, "BT /F1 12 Tf 72 300 Td [( )] TJ ET"}, want: ErrPDFNoTextLayer},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Extract(t.Context(), pdfDoc{pages: c.pages}.bytes(), KindPDF, DefaultLimits)
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
	figure := figureObject(scanImage(t, 1000, 1000, "jpeg"), 1000, 1000)
	for _, mode := range []string{"3", "+3", "03", "3.0"} {
		t.Run("a searchable scan whose layer is in mode "+mode, func(t *testing.T) {
			layer := "BT /F1 12 Tf " + mode + " Tr 72 700 Td (Invisible scanner layer) Tj ET"
			scan := pdfDoc{pages: []string{"q 612 0 0 792 0 0 cm /Im1 Do Q " + layer}, resources: imageResources(6), extra: []string{figure}}.bytes()
			if pages, err := ScanPagesOfPDF(t.Context(), scan); err != nil || len(pages) != 1 {
				t.Fatalf("%d pages %v", len(pages), err)
			}
			outside := pdfDoc{pages: []string{"q 612 0 0 792 0 0 cm /Im1 Do Q " + mode + " Tr"}, resources: imageResources(6), extra: []string{figure}}.bytes()
			if pages, err := ScanPagesOfPDF(t.Context(), outside); err != nil || len(pages) != 1 {
				t.Fatalf("mode %s outside text: %d pages %v", mode, len(pages), err)
			}
		})
	}
}
