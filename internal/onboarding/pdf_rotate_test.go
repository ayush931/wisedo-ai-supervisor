package onboarding

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// A page's /Rotate turns how it is shown: a scanned page and a typed page's figures are sent to the
// vision model turned the same way (clockwise), and a rotation that is not a quarter turn refuses.
func TestPDFRotatedPagesAreReadAsShown(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	// halves is a 300x200 picture, white on the left, black on the right.
	var halves bytes.Buffer
	img := image.NewGray(image.Rect(0, 0, 300, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 300; x++ {
			if x < 150 {
				img.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	if err := jpeg.Encode(&halves, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	scanPage := func(rotate string) []byte {
		b := pdfDoc{pages: []string{"q 300 0 0 200 0 0 cm /Im1 Do Q"}, resources: imageResources(6), pageExtra: rotate, extra: []string{figureObject(halves.Bytes(), 300, 200)}}.bytes()
		return rebuildXref(t, bytes.Replace(b, []byte("/MediaBox [0 0 612 792]"), []byte("/MediaBox [0 0 300 200]"), 1))
	}
	t.Run("a scanned page rotated 90 degrees", func(t *testing.T) {
		pages, err := ScanPagesOfPDF(t.Context(), scanPage("/Rotate 90"))
		if err != nil || len(pages) != 1 {
			t.Fatalf("got %d pages %v", len(pages), err)
		}
		got, _, err := image.Decode(bytes.NewReader(pages[0].Data))
		if err != nil || pages[0].MediaType != "image/jpeg" {
			t.Fatalf("page image is not the turned JPEG: %s %v", pages[0].MediaType, err)
		}
		if got.Bounds().Dx() != 200 || got.Bounds().Dy() != 300 {
			t.Fatalf("page image %v, want 200x300", got.Bounds())
		}
		// Turned clockwise, the white left half is on top.
		top, _, _, _ := got.At(100, 30).RGBA()
		bottom, _, _, _ := got.At(100, 270).RGBA()
		if top < 0xe000 || bottom > 0x2000 {
			t.Fatalf("top %#x bottom %#x: not turned clockwise", top, bottom)
		}
	})
	t.Run("a page rotated 45 degrees", func(t *testing.T) {
		if _, err := ScanPagesOfPDF(t.Context(), scanPage("/Rotate 45")); !errors.Is(err, ErrUnreadableSource) {
			t.Fatalf("got %v, want %v", err, ErrUnreadableSource)
		}
	})
	t.Run("a figure on a typed page rotated 90 degrees", func(t *testing.T) {
		big := scanImage(t, 400, 300, "jpeg")
		doc := pdfDoc{pages: []string{visibleLine + " q 300 0 0 225 72 350 cm /Im1 Do Q"}, resources: imageResources(6), pageExtra: "/Rotate 90", extra: []string{figureObject(big, 400, 300)}}
		got, err := prepareDocument(t.Context(), DocumentUpload{Title: "SOP", DeclaredType: "application/pdf", Data: doc.bytes()})
		if err != nil || len(got.images) != 1 || got.images[0].Width != 300 || got.images[0].Height != 400 {
			t.Fatalf("got %+v %v, want the figure turned to 300x400", got.images, err)
		}
	})
}

// Codex round 8: a turned JPEG is encoded as a JPEG, not a PNG, so a large photographed page stays
// under the 4 MiB limit. Before, the same page turned into a PNG of over 4 MiB and was refused.
func TestPDFRotatedLargeScanStaysUnderTheLimit(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	// A 1800 x 1800 grey page of fine noise: a large JPEG, and a far larger PNG.
	img := image.NewGray(image.Rect(0, 0, 1800, 1800))
	seed := uint32(1)
	for i := range img.Pix {
		seed = seed*1664525 + 1013904223
		img.Pix[i] = uint8(seed >> 24)
	}
	var page bytes.Buffer
	if err := jpeg.Encode(&page, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	if page.Len() > 4<<20 {
		t.Fatalf("fixture %d bytes, over the limit itself", page.Len())
	}
	b := pdfDoc{pages: []string{"q 432 0 0 432 0 0 cm /Im1 Do Q"}, resources: imageResources(6), pageExtra: "/Rotate 270", extra: []string{figureObject(page.Bytes(), 1800, 1800)}}.bytes()
	b = rebuildXref(t, bytes.Replace(b, []byte("/MediaBox [0 0 612 792]"), []byte("/MediaBox [0 0 432 432]"), 1))
	pages, err := ScanPagesOfPDF(t.Context(), b)
	if err != nil || len(pages) != 1 || pages[0].MediaType != "image/jpeg" || len(pages[0].Data) > 4<<20 {
		t.Fatalf("got %d pages %v (fixture %d bytes)", len(pages), err, page.Len())
	}
}

// Codex round 9 on 85aea301: a scan larger than its placement is resized before it is turned. The
// resize re-encoded a JPEG as PNG, so a photo of fine detail near the 4 MiB limit came out far
// over it. A resized JPEG stays JPEG through both steps.
func TestPDFResizedThenRotatedScanStaysJPEG(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	img := image.NewGray(image.Rect(0, 0, 2400, 2400))
	seed := uint32(7)
	for i := range img.Pix {
		seed = seed*1664525 + 1013904223
		img.Pix[i] = uint8(seed >> 24)
	}
	var page bytes.Buffer
	if err := jpeg.Encode(&page, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	if page.Len() > 4<<20 || page.Len() < 3<<20 {
		t.Fatalf("fixture %d bytes, want near the 4 MiB limit", page.Len())
	}
	// 432 pt at 300 dpi is 1800 px: the 2400 px scan is resized, then turned.
	b := pdfDoc{pages: []string{"q 432 0 0 432 0 0 cm /Im1 Do Q"}, resources: imageResources(6), pageExtra: "/Rotate 90", extra: []string{figureObject(page.Bytes(), 2400, 2400)}}.bytes()
	b = rebuildXref(t, bytes.Replace(b, []byte("/MediaBox [0 0 612 792]"), []byte("/MediaBox [0 0 432 432]"), 1))
	pages, err := ScanPagesOfPDF(t.Context(), b)
	if err != nil || len(pages) != 1 || pages[0].MediaType != "image/jpeg" || len(pages[0].Data) > 4<<20 {
		t.Fatalf("got %d pages %v", len(pages), err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(pages[0].Data))
	if err != nil || cfg.Width != 1800 || cfg.Height != 1800 {
		t.Fatalf("got %+v %v, want an 1800 x 1800 JPEG", cfg, err)
	}
}
