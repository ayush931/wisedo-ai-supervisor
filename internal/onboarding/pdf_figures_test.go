package onboarding

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// figureObject is a w x h JPEG image XObject (DCT, passed through to the vision model unchanged).
func figureObject(jpg []byte, w, h int) string {
	return fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream", w, h, len(jpg), jpg)
}

// WIS-625 post-merge decision: a typed PDF keeps its typed text, and each raster figure it draws
// is queued for the vision model (prepareDocument's images, titled by page and image) unless it
// is too small to hold text (a logo, an icon: skipped and counted). A figure the page does not
// show whole and plainly is refused; content no renderer here can read (annotations, forms, inline
// images) stays refused; one PDF sends at most maxDocumentImages figures.
func TestPDFFiguresAreReadByTheVisionModel(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	big := scanImage(t, 400, 300, "jpeg")
	figure := figureObject(big, 400, 300)
	logo := figureObject(scanImage(t, 300, 100, "jpeg"), 300, 100)
	icon := figureObject(scanImage(t, 40, 40, "jpeg"), 40, 40)
	draw := " q 300 0 0 225 72 350 cm /Im1 Do Q" // 13.9% of a Letter page, above the typed line
	typed := func(content, resources, extraPage string, extra ...string) []byte {
		return pdfDoc{pages: []string{visibleLine + content}, resources: resources, pageExtra: extraPage, extra: extra}.bytes()
	}
	prepare := func(t *testing.T, data []byte) (preparedDocument, error) {
		t.Helper()
		return prepareDocument(t.Context(), DocumentUpload{Title: "Press SOP", DeclaredType: "application/pdf", Data: data})
	}

	t.Run("a typed page with a large figure keeps its text and queues the figure", func(t *testing.T) {
		got, err := prepare(t, typed(draw, imageResources(6), "", figure))
		if err != nil || got.text != "Visible line\n" || len(got.images) != 1 || got.skipped != 0 {
			t.Fatalf("got %+v %v", got, err)
		}
		img := got.images[0]
		if img.Title != "Press SOP (page 1, image 1)" || img.MediaType != "image/jpeg" || img.Width != 400 || img.Height != 300 || !bytes.Equal(img.Image, big) {
			t.Fatalf("figure %+v", img)
		}
		// Extract alone (no queue) cannot carry the figure: it refuses rather than drop it.
		if text, err := Extract(t.Context(), typed(draw, imageResources(6), "", figure), KindPDF, DefaultLimits); text != "" || !errors.Is(err, ErrPDFNoTextLayer) {
			t.Fatalf("Extract: %q %v", text, err)
		}
	})

	for name, c := range map[string]struct {
		data    []byte
		skipped int
	}{
		"a header logo (1.6% of the page)": {typed(" q 144 0 0 54 72 720 cm /Im1 Do Q", imageResources(6), "", logo), 1},
		"an icon drawn large (40 px)":      {typed(draw, imageResources(6), "", icon), 1},
		// Codex round 19: qualified by what the clip leaves (100 x 50 pt, 1% of the page): too small
		// to be a figure, so neither read nor refused.
		"a figure clipped to a sliver":               {typed(" q 72 300 100 100 re W n"+draw+" Q", imageResources(6), "", figure), 1},
		"the 1 px image of a typed page number page": {pdfDoc{pages: []string{textPage("Typed page"), imagePage}, resources: imageResources(8), extra: []string{imageObject}}.bytes(), 1},
	} {
		t.Run("skipped: "+name, func(t *testing.T) {
			got, err := prepare(t, c.data)
			if err != nil || len(got.images) != 0 || got.skipped != c.skipped || got.text == "" {
				t.Fatalf("got %+v %v", got, err)
			}
		})
	}

	for name, c := range map[string]struct {
		data []byte
		want error
	}{
		"a figure with a box painted over it": {typed(draw+" 1 g 100 350 50 50 re f", imageResources(6), "", figure), ErrPDFScanHiddenContent},
		// Codex, WIS-625 docs rebase: only a scanned page's fax image is decoded and checked by
		// faxPage. A fax figure would reach the model as pdfcpu decodes it, unchecked: refused.
		// (An all-white 256 x 256 Group 4 image: one V0 code, a 1 bit, per row.)
		"a fax-encoded figure": {typed(" q 300 0 0 300 100 400 cm /Im1 Do Q", imageResources(6), "",
			"<< /Type /XObject /Subtype /Image /Width 256 /Height 256 /ColorSpace /DeviceGray /BitsPerComponent 1 /Filter /CCITTFaxDecode /DecodeParms << /K -1 /Columns 256 /Rows 256 >> /Length 32 >>\nstream\n"+strings.Repeat("\xff", 32)+"\nendstream"), ErrPDFScanHiddenContent},
		"a figure partly off the page": {typed(" q 300 0 0 225 500 300 cm /Im1 Do Q", imageResources(6), "", figure), ErrPDFScanHiddenContent},
		// Codex round 19: a figure qualifies by what the clip leaves of it. Clipped to a 150 x 150 pt
		// corner it is still large enough to read, and the model would read all of it.
		"a figure clipped to a corner":      {typed(" q 72 350 150 150 re W n"+draw+" Q", imageResources(6), "", figure), ErrPDFScanHiddenContent},
		"a figure outside the crop box":     {typed(draw, imageResources(6), "/CropBox [0 0 612 400]", figure), ErrPDFScanHiddenContent},
		"a figure under half transparency":  {typed(" /G1 gs"+draw, imageResources(6)+" /ExtGState << /G1 << /ca 0.5 >> >>", "", figure), ErrPDFScanHiddenContent},
		"a rotated figure":                  {typed(" q 0 225 -300 0 400 300 cm /Im1 Do Q", imageResources(6), "", figure), ErrPDFScanHiddenContent},
		"a stamp (vector annotation) still": {typed("", "", "/Annots [<< /Type /Annot /Subtype /Stamp /Rect [0 0 612 792] >>]"), ErrPDFNoTextLayer},
		"a form showing text still":         {typed(" /Fm1 Do", "/XObject << /Fm1 6 0 R >>", "", formObject("/Font << /F1 3 0 R >>", "BT /F1 12 Tf 0 0 Td (Instructions) Tj ET")), ErrPDFNoTextLayer},
		"an inline image still":             {typed(" q 300 0 0 225 72 300 cm BI /W 1 /H 1 /CS /G /BPC 8 ID \x80 EI Q", "", ""), ErrPDFNoTextLayer},
		"hidden text beside a figure still": {typed(draw+" BT /F1 12 Tf 3 Tr 72 100 Td (Hidden claim) Tj ET", imageResources(6), "", figure), ErrUnreadableSource},
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			if got, err := prepare(t, c.data); !errors.Is(err, c.want) {
				t.Fatalf("got %+v %v, want %v", got, err, c.want)
			}
		})
	}

	// Security review: a picture or form drawn after a figure and over it hides part of it.
	t.Run("an image or form drawn over a figure is refused", func(t *testing.T) {
		over := map[string][]byte{
			"a small image": typed(draw+" q 50 0 0 50 100 350 cm /Im2 Do Q", "/XObject << /Im1 6 0 R /Im2 7 0 R >>", "", figure, icon),
		}
		for name, data := range over {
			if got, err := prepare(t, data); !errors.Is(err, ErrPDFScanHiddenContent) {
				t.Errorf("%s: %+v %v, want %v", name, got, err, ErrPDFScanHiddenContent)
			}
		}
		// Codex round 23: a form shown to paint nothing ("q Q") covers nothing, wherever its BBox
		// lies (a form that paints anything leaves the page unread: formShows), so the figure is read.
		empty := map[string][]byte{
			"an empty form": typed(draw+" q 1 0 0 1 100 350 cm /Fm1 Do Q", "/XObject << /Im1 6 0 R /Fm1 7 0 R >>", "", figure, formObject("", "q Q")),
			"a page-size empty form": typed(draw+" /Fm1 Do", "/XObject << /Im1 6 0 R /Fm1 7 0 R >>", "", figure,
				strings.Replace(formObject("", "q Q"), "/BBox [0 0 1 1]", "/BBox [0 0 612 792]", 1)),
		}
		for name, data := range empty {
			if got, err := prepare(t, data); err != nil || len(got.images) != 1 {
				t.Errorf("%s: %+v %v", name, got, err)
			}
		}
		// Drawn beside it, not over it, the figure is read.
		if got, err := prepare(t, typed(draw+" q 50 0 0 50 450 650 cm /Im2 Do Q", "/XObject << /Im1 6 0 R /Im2 7 0 R >>", "", figure, icon)); err != nil || len(got.images) != 1 || got.skipped != 1 {
			t.Fatalf("beside: %+v %v", got, err)
		}
	})

	t.Run("a full-page clip leaves a figure readable", func(t *testing.T) {
		if got, err := prepare(t, typed(" q 0 0 612 792 re W n"+draw+" Q", imageResources(6), "", figure)); err != nil || len(got.images) != 1 {
			t.Fatalf("got %+v %v", got, err)
		}
	})

	t.Run("one PDF reads at most maxDocumentImages figures", func(t *testing.T) {
		pages := make([]string, maxDocumentImages+1)
		for i := range pages {
			pages[i] = visibleLine + draw
		}
		n := len(pages)
		data := pdfDoc{pages: pages, resources: imageResources(4 + 2*n), extra: []string{figure}}.bytes()
		if got, err := prepare(t, data); !errors.Is(err, ErrPDFTooManyImages) {
			t.Fatalf("%d figures: %d %v, want %v", n, len(got.images), err, ErrPDFTooManyImages)
		}
		pages = pages[:maxDocumentImages]
		data = pdfDoc{pages: pages, resources: imageResources(4 + 2*len(pages)), extra: []string{figure}}.bytes()
		if got, err := prepare(t, data); err != nil || len(got.images) != maxDocumentImages || got.images[maxDocumentImages-1].Title != fmt.Sprintf("Press SOP (page %d, image 1)", maxDocumentImages) {
			t.Fatalf("%d figures: %d %v", maxDocumentImages, len(got.images), err)
		}
	})

	// A scanner's searchable PDF: the page image with an invisible (render mode 3) OCR layer. The
	// layer is never stored: the document path sends it to the scan path (no typed text), which
	// returns only the page image for the vision model.
	t.Run("a searchable scan reads the image, never its invisible layer", func(t *testing.T) {
		layer := "BT /F1 12 Tf 3 Tr 72 700 Td (Invisible scanner layer) Tj ET"
		scan := pdfDoc{pages: []string{"q 612 0 0 792 0 0 cm /Im1 Do Q " + layer}, resources: imageResources(6), extra: []string{figure}}.bytes()
		if got, err := prepare(t, scan); !errors.Is(err, ErrPDFNoTextLayer) || got.text != "" {
			t.Fatalf("document path: %+v %v, want %v", got, err, ErrPDFNoTextLayer)
		}
		pages, err := ScanPagesOfPDF(t.Context(), scan)
		if err != nil || len(pages) != 1 || !bytes.Equal(pages[0].Data, big) {
			t.Fatalf("scan path: %d pages %v", len(pages), err)
		}
		// Security review: only text operators may sit in the dropped layer, and outside it only
		// mode 3; anything else there would change how the page shows its image.
		// A cm or a path inside a text object is refused by the content walk itself (Codex round
		// 26: they do not belong in a text object), before the scan check sees the layer.
		for name, c := range map[string]struct {
			layer string
			want  error
		}{
			"a cm inside the text layer":   {"q 612 0 0 792 0 0 cm BT 0.01 0 0 0.01 0 0 cm ET /Im1 Do Q", ErrUnreadableSource},
			"a path inside the text layer": {"q 612 0 0 792 0 0 cm /Im1 Do Q BT 3 Tr 0 0 1 1 re W n (x) Tj ET", ErrUnreadableSource},
		} {
			data := pdfDoc{pages: []string{c.layer}, resources: imageResources(6), extra: []string{figure}}.bytes()
			if pages, err := ScanPagesOfPDF(t.Context(), data); !errors.Is(err, c.want) {
				t.Errorf("%s: %d pages %v, want %v", name, len(pages), err, c.want)
			}
		}
		// Codex round 34: a mode set with no show after it paints nothing (each show's own mode is
		// what parsePDF checks), so this scan is read.
		if pages, err := ScanPagesOfPDF(t.Context(), pdfDoc{pages: []string{"q 612 0 0 792 0 0 cm /Im1 Do Q 1 Tr"}, resources: imageResources(6), extra: []string{figure}}.bytes()); err != nil || len(pages) != 1 {
			t.Errorf("a mode set after the image: %d pages %v", len(pages), err)
		}
		// Hidden-only text on a page of its own, beside typed pages, is dropped, never stored.
		got, err := prepare(t, pdfDoc{pages: []string{visibleLine, "BT /F1 12 Tf 3 Tr 72 500 Td (Hidden claim) Tj ET"}}.bytes())
		if err != nil || got.text != "Visible line\n" || strings.Contains(got.text, "Hidden") {
			t.Fatalf("hidden-only page: %+v %v", got, err)
		}
	})
}
