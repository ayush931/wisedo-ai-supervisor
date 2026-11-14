package onboarding

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

// Codex round 6 on 3ae23410 (WIS-625 post-merge): /UserUnit, bold redraws of other glyphs, and
// figures sent finer than the page shows them.
func TestPDFUserUnitBoldGeometryAndFigureResolution(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	tiny := "BT /F1 1.5 Tf 72 500 Td (Small print) Tj ET " + visibleLine // 1.5 units
	for name, c := range map[string]struct {
		doc  pdfDoc
		text string
		want error
	}{
		"a UserUnit out of range":            {doc: pdfDoc{pages: []string{visibleLine}, pageExtra: "/UserUnit 0.05"}, want: ErrUnreadableSource},
		"1.5-unit text at a UserUnit of 0.5": {doc: pdfDoc{pages: []string{tiny}, pageExtra: "/UserUnit 0.5"}, want: ErrPDFScanHiddenContent},
		"1.5-unit text at the default unit":  {doc: pdfDoc{pages: []string{tiny}}, text: "Small print\nVisible line\n"},
		// Security review of 8f7b013c: a UserUnit above 1 never loosens the floor (viewers may ignore it).
		"0.5-unit text at a UserUnit of 4":       {doc: pdfDoc{pages: []string{"BT /F1 0.5 Tf 72 500 Td (Small print) Tj ET " + visibleLine}, pageExtra: "/UserUnit 4"}, want: ErrPDFScanHiddenContent},
		"a redraw at another size over the text": {doc: pdfDoc{pages: []string{visibleLine + " BT /F1 12.5 Tf 72.3 300 Td (Visible line) Tj ET"}}, want: ErrPDFScanHiddenContent},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := prepareDocument(t.Context(), DocumentUpload{Title: "Press SOP", DeclaredType: "application/pdf", Data: c.doc.bytes()})
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
	t.Run("a large image in a small place is sent as the page shows it", func(t *testing.T) {
		big := scanImage(t, 2000, 2000, "jpeg")
		doc := pdfDoc{pages: []string{visibleLine + " q 150 0 0 150 72 350 cm /Im1 Do Q"}, resources: imageResources(6), extra: []string{figureObject(big, 2000, 2000)}}
		got, err := prepareDocument(t.Context(), DocumentUpload{Title: "Press SOP", DeclaredType: "application/pdf", Data: doc.bytes()})
		if err != nil || len(got.images) != 1 {
			t.Fatalf("got %+v %v", got, err)
		}
		img, sum := got.images[0], sha256.Sum256(big)
		// 150 pt at 150 dpi is 313 px; the identity stays the embedded image's, so a replay matches.
		if img.MediaType != "image/jpeg" || img.Width != 313 || img.Height != 313 || img.OriginalSHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("figure %s %dx%d %s", img.MediaType, img.Width, img.Height, img.OriginalSHA256)
		}
		again, err := prepareDocument(t.Context(), DocumentUpload{Title: "Press SOP", DeclaredType: "application/pdf", Data: doc.bytes()})
		if err != nil || string(again.images[0].Image) != string(img.Image) {
			t.Fatal("the scaled figure is not the same bytes twice")
		}
	})
}
