package onboarding

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// rgbFigure is a w x h RGB JPEG image XObject of coloured noise.
func rgbFigure(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		img.Set(i%w, i/w, color.RGBA{R: uint8(i % 251), G: uint8(i % 241), B: uint8(i % 233), A: 255})
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream", w, h, b.Len(), b.Bytes())
}

// Codex round 24 on 8d91385e (WIS-625 post-merge).
//   - A grey page group shows RGB colours as 0.3 R + 0.59 G + 0.11 B, not as their luminance:
//     text in "1 .4 1 rg" on "0 1 .51 rg" passes in RGB (0.571 on 0.752) and is one grey shown.
//     Colours are judged through the group's space too; a space with no known conversion refuses
//     a page with text.
//   - An empty form drawn under a blend, or an empty show, counted as blending on an isolated page.
//   - A grey figure in a grey group is shown as it is and is read; a conversion is refused.
func TestPDFGroupSpaceColoursBlendedEmptyDrawsAndSameSpaceFigures(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	grey := "/Group << /S /Transparency /CS /DeviceGray >>"
	isolated := "/Group << /S /Transparency /I true >>"
	states := "/ExtGState << /Mul << /BM /Multiply >> >>"
	pinkOnGreen := "q 0 1 .51 rg 60 290 200 25 re f Q BT /F1 12 Tf 1 .4 1 rg 72 300 Td (Visible line) Tj ET"
	for name, c := range map[string]struct {
		doc  pdfDoc
		text string
		want error
	}{
		"text one grey with its ground in a grey group": {doc: pdfDoc{pages: []string{pinkOnGreen}, pageExtra: grey}, want: ErrPDFScanHiddenContent},
		"the same colours without a group space":        {doc: pdfDoc{pages: []string{pinkOnGreen}}, text: "Visible line\n"},
		"black text in a grey group":                    {doc: pdfDoc{pages: []string{visibleLine}, pageExtra: grey}, text: "Visible line\n"},
		"text in an ICC-based group":                    {doc: pdfDoc{pages: []string{visibleLine}, pageExtra: "/Group << /S /Transparency /CS [/ICCBased 6 0 R] >>", extra: []string{"<< /N 3 /Length 0 >>\nstream\n\nendstream"}}, want: ErrUnreadableSource},
		"an empty form drawn blended, isolated page": {doc: pdfDoc{pages: []string{"q /Mul gs /Fm1 Do Q " + visibleLine}, resources: states + " /XObject << /Fm1 6 0 R >>", pageExtra: isolated,
			extra: []string{formObject("", "q Q")}}, text: "Visible line\n"},
		"an empty show blended, isolated page": {doc: pdfDoc{pages: []string{"q /Mul gs BT /F1 12 Tf 72 500 Td () Tj ET Q " + visibleLine}, resources: states, pageExtra: isolated}, text: "Visible line\n"},
		"a blended show, isolated page":        {doc: pdfDoc{pages: []string{visibleLine + " q /Mul gs BT /F1 12 Tf 72 500 Td (Blended) Tj ET Q"}, resources: states, pageExtra: isolated}, want: ErrUnreadableSource},
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
	draw := " q 300 0 0 300 150 400 cm /Im1 Do Q"
	for name, c := range map[string]struct {
		figure, group string
		images        int
		want          error
	}{
		"a grey figure in a grey group": {figure: figureObject(scanImage(t, 1000, 1000, "jpeg"), 1000, 1000), group: grey, images: 1},
		"an RGB figure in a grey group": {figure: rgbFigure(t, 1000, 1000), group: grey, want: ErrPDFScanHiddenContent},
		"an RGB figure in an RGB group": {figure: rgbFigure(t, 1000, 1000), group: "/Group << /S /Transparency /CS /DeviceRGB >>", images: 1},
	} {
		t.Run(name, func(t *testing.T) {
			doc := pdfDoc{pages: []string{visibleLine + draw}, resources: imageResources(6), pageExtra: c.group, extra: []string{c.figure}}.bytes()
			got, err := prepareDocument(t.Context(), DocumentUpload{Title: "SOP", DeclaredType: "application/pdf", Data: doc})
			if c.want != nil {
				if !errors.Is(err, c.want) {
					t.Fatalf("got %d images %v, want %v", len(got.images), err, c.want)
				}
				return
			}
			if err != nil || len(got.images) != c.images {
				t.Fatalf("got %q, %d images, %v", got.text, len(got.images), err)
			}
		})
	}
	// Security review: a group /CS that is not a name (an ICCBased array, directly or by reference)
	// must not read as no group space. The figure is on a page of its own, beside a typed page, so
	// no text on its page refuses it first. Object 8 is the figure, 9 an ICC profile, 10 the array.
	icc := "<< /N 3 /Length 0 >>\nstream\n\nendstream"
	for name, cs := range map[string]string{
		"a figure in an ICC-based group":       "[/ICCBased 9 0 R]",
		"a figure in a group /CS by reference": "10 0 R",
	} {
		t.Run(name, func(t *testing.T) {
			b := pdfDoc{pages: []string{visibleLine, draw}, resources: imageResources(8), extra: []string{rgbFigure(t, 1000, 1000), icc, "[/ICCBased 9 0 R]"}}.bytes()
			b = rebuildXref(t, bytes.Replace(b, []byte("/Contents 7 0 R"), []byte("/Contents 7 0 R /Group << /S /Transparency /CS "+cs+" >>"), 1))
			got, err := prepareDocument(t.Context(), DocumentUpload{Title: "SOP", DeclaredType: "application/pdf", Data: b})
			if !errors.Is(err, ErrUnreadableSource) || len(got.images) != 0 {
				t.Fatalf("got %q, %d images, %v, want %v", got.text, len(got.images), err, ErrUnreadableSource)
			}
		})
	}
	// Control: the same page without a group /CS sends its figure.
	b := pdfDoc{pages: []string{visibleLine, draw}, resources: imageResources(8), extra: []string{rgbFigure(t, 1000, 1000)}}.bytes()
	if got, err := prepareDocument(t.Context(), DocumentUpload{Title: "SOP", DeclaredType: "application/pdf", Data: b}); err != nil || len(got.images) != 1 {
		t.Fatalf("control: %q, %d images, %v", got.text, len(got.images), err)
	}
}
