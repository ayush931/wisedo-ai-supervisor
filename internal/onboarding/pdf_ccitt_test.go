package onboarding

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// faxScan is a one-page scanned PDF whose page is a fax-encoded (CCITTFaxDecode) image drawn over
// the whole page, as an office scanner writes it. dict is the image dictionary's extra entries.
func faxScan(t *testing.T, stream []byte, width, height int, dict string) []byte {
	t.Helper()
	return encodedScan(t, "CCITTFaxDecode", stream, width, height, dict)
}

// encodedScan is a one-page scan whose 1-bit grey page image is stored with the given filter.
func encodedScan(t *testing.T, filter string, stream []byte, width, height int, dict string) []byte {
	t.Helper()
	return spacedScan(t, filter, "/ColorSpace /DeviceGray", stream, width, height, dict)
}

// spacedScan is encodedScan with the given colour space entry ("" for none).
func spacedScan(t *testing.T, filter, space string, stream []byte, width, height int, dict string) []byte {
	t.Helper()
	img := fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /BitsPerComponent 1 %s /Filter /%s %s /Length %d >>\nstream\n%s\nendstream",
		width, height, space, filter, dict, len(stream), stream)
	// Objects: 1 catalog, 2 pages, 3 font, 4 page, 5 content, 6 the image.
	return pdfDoc{pages: []string{"q 612 0 0 420 0 186 cm /Im1 Do Q"}, resources: imageResources(6), extra: []string{img}}.bytes()
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// WIS-645: a fax-encoded scanned page (CCITT Group 4, or one-dimensional Group 3) is decoded in
// the PDF child into a PNG of exactly the picture a viewer shows; every parameter whose display
// differs from the decoded bits, and every page over the photo limits, is refused before or
// instead of decoding. pageImages is the child's own function, so this runs without the sandbox.
func TestScanPagesOfFaxPDF(t *testing.T) {
	g4, g3, aligned := fixture(t, "bw-gopher.ccitt_group4"), fixture(t, "bw-gopher.ccitt_group3"), fixture(t, "bw-gopher-aligned.ccitt_group4")
	want, err := png.Decode(bytes.NewReader(fixture(t, "bw-gopher.png")))
	if err != nil {
		t.Fatal(err)
	}
	const w, h = 153, 55
	for _, tc := range []struct {
		name   string
		pdf    []byte
		reason string // "" means the page is read
	}{
		{"group 4", faxScan(t, g4, w, h, "/DecodeParms << /K -1 /Columns 153 /Rows 55 >>"), ""},
		{"one-dimensional group 3", faxScan(t, g3, w, h, "/DecodeParms << /K 0 /Columns 153 /Rows 55 >>"), ""},
		{"byte-aligned group 4", faxScan(t, aligned, w, h, "/DecodeParms << /K -1 /Columns 153 /EncodedByteAlign true >>"), ""},
		{"BlackIs1 shows the bits inverted", faxScan(t, g4, w, h, "/DecodeParms << /K -1 /Columns 153 /BlackIs1 true >>"), pdfReasonScanImage},
		{"two-dimensional group 3 is unsupported", faxScan(t, g3, w, h, "/DecodeParms << /K 1 /Columns 153 >>"), pdfReasonScanImage},
		{"columns that are not the width", faxScan(t, g4, w, h, "/DecodeParms << /K -1 /Columns 152 >>"), pdfReasonScanImage},
		{"rows that are not the height", faxScan(t, g4, w, h, "/DecodeParms << /K -1 /Columns 153 /Rows 54 >>"), pdfReasonScanImage},
		{"no parameters: the default 1728 columns", faxScan(t, g4, w, h, ""), pdfReasonScanImage},
		{"an unknown parameter", faxScan(t, g4, w, h, "/DecodeParms << /K -1 /Columns 153 /EndOfLine true >>"), pdfReasonScanImage},
		// Without its end-of-block marker the picture is still complete (the decoder's own
		// "truncated" fixture), but a stream cut short of its rows is not a whole page.
		{"no end-of-block marker", faxScan(t, fixture(t, "bw-gopher-truncated0.ccitt_group4"), w, h, "/DecodeParms << /K -1 /Columns 153 >>"), ""},
		{"a stream cut short of its rows", faxScan(t, g4[:len(g4)/2], w, h, "/DecodeParms << /K -1 /Columns 153 >>"), pdfReasonScanImage},
		{"a decode array inverts the page", faxScan(t, g4, w, h, "/DecodeParms << /K -1 /Columns 153 >> /Decode [1 0]"), pdfReasonScanHidden},
		// faxPage decodes the bits whatever the colour space says. Without one, the page is the
		// DeviceGray default; a Separation tint that maps every sample to white shows the viewer a
		// blank page while the model would read the bitmap (Codex, WIS-625 docs rebase).
		{"no colour space is DeviceGray", spacedScan(t, "CCITTFaxDecode", "", g4, w, h, "/DecodeParms << /K -1 /Columns 153 >>"), ""},
		{"a Separation tint paints the page white", spacedScan(t, "CCITTFaxDecode", "/ColorSpace [/Separation /Hidden /DeviceGray << /FunctionType 2 /Domain [0 1] /C0 [1] /C1 [1] /N 1 >>]", g4, w, h, "/DecodeParms << /K -1 /Columns 153 >>"), pdfReasonScanHidden},
		// The size is refused from the dictionary: the stream is a 153-pixel-wide picture, so
		// decoding it first would have been refused as unreadable instead.
		// Encodings with no bounded pure-Go decoder stay refused, now after the size check.
		{"JBIG2 is still refused", encodedScan(t, "JBIG2Decode", g4, w, h, ""), pdfReasonScanImage},
		{"JPEG 2000 is still refused", encodedScan(t, "JPXDecode", g4, w, h, ""), pdfReasonScanImage},
		{"wider than a scan job allows", faxScan(t, g4, 20001, h, "/DecodeParms << /K -1 /Columns 20001 >>"), pdfReasonScanLarge},
		{"more than 40 megapixels", faxScan(t, g4, 7000, 7000, "/DecodeParms << /K -1 /Columns 7000 >>"), pdfReasonScanLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := pageImages(tc.pdf)
			if tc.reason != "" {
				if reply.Status != "refused" || reply.Reason != tc.reason {
					t.Fatalf("got %s %s, want refused %s", reply.Status, reply.Reason, tc.reason)
				}
				return
			}
			if reply.Status != "ok" || len(reply.Pages) != 1 || reply.Pages[0].MediaType != "image/png" || reply.Pages[0].Number != 1 {
				t.Fatalf("reply %s %s, %d pages", reply.Status, reply.Reason, len(reply.Pages))
			}
			got, err := png.Decode(bytes.NewReader(reply.Pages[0].Data))
			if err != nil {
				t.Fatal(err)
			}
			if got.Bounds() != image.Rect(0, 0, w, h) {
				t.Fatalf("bounds %v", got.Bounds())
			}
			// Pixel for pixel the picture the reference decoder produces: black stays black.
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					gr, _, _, _ := got.At(x, y).RGBA()
					wr, _, _, _ := want.At(x, y).RGBA()
					if (gr > 0x7fff) != (wr > 0x7fff) {
						t.Fatalf("pixel (%d,%d) differs from the reference", x, y)
					}
				}
			}
		})
	}
}

func TestScanPixelsAllowed(t *testing.T) {
	for _, tc := range []struct {
		w, h int
		ok   bool
	}{{1, 1, true}, {20000, 2000, true}, {6324, 6324, true}, {6325, 6325, false}, {20001, 1, false}, {0, 10, false}, {10, -1, false}} {
		if got := scanPixelsAllowed(tc.w, tc.h); got != tc.ok {
			t.Fatalf("%d × %d: %v, want %v", tc.w, tc.h, got, tc.ok)
		}
	}
}
