package onboarding

import (
	"errors"
	"strings"
	"testing"
)

// Codex round 23 on 710d5ec6 (WIS-625 post-merge).
//   - A page group's blending colour space (/CS /DeviceGray) converts a figure as shown, while
//     the model reads the original pixels: with figures, only no /CS or DeviceRGB is read.
//   - Blending counted when a blend mode was set ("q /Mul gs Q"), not when something was painted
//     under it, so an isolated page of ordinary text was refused.
//   - A form that paints nothing ("q Q") covered the text under its BBox.
func TestPDFGroupColourSpacesBlendUseAndEmptyForms(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	// An RGB figure (round 24 fixed the fixture: a grey one in a grey group is shown as it is).
	figure := rgbFigure(t, 1000, 1000)
	draw := " q 300 0 0 300 150 400 cm /Im1 Do Q"
	isolated := "/Group << /S /Transparency /I true >>"
	states := "/ExtGState << /Mul << /BM /Multiply >> /Nor << /BM /Normal >> >>"
	for name, c := range map[string]struct {
		pageExtra string
		images    int
		want      error
	}{
		"a figure in a grey page group":   {pageExtra: "/Group << /S /Transparency /I true /CS /DeviceGray >>", want: ErrPDFScanHiddenContent},
		"a figure in an RGB page group":   {pageExtra: "/Group << /S /Transparency /I true /CS /DeviceRGB >>", images: 1},
		"a figure in a group without /CS": {pageExtra: isolated, images: 1},
	} {
		t.Run(name, func(t *testing.T) {
			doc := pdfDoc{pages: []string{visibleLine + draw}, resources: imageResources(6), pageExtra: c.pageExtra, extra: []string{figure}}.bytes()
			got, err := prepareDocument(t.Context(), DocumentUpload{Title: "SOP", DeclaredType: "application/pdf", Data: doc})
			if c.want != nil {
				if !errors.Is(err, c.want) {
					t.Fatalf("got %d images %v, want %v", len(got.images), err, c.want)
				}
				return
			}
			if err != nil || len(got.images) != c.images || got.text != "Visible line\n" {
				t.Fatalf("got %q, %d images, %v", got.text, len(got.images), err)
			}
		})
	}
	emptyPageForm := strings.Replace(formObject("", "q Q"), "/BBox [0 0 1 1]", "/BBox [0 0 612 792]", 1)
	for name, c := range map[string]struct {
		doc  pdfDoc
		text string
		want error
	}{
		"text in a grey page group":                {doc: pdfDoc{pages: []string{visibleLine}, pageExtra: "/Group << /S /Transparency /I true /CS /DeviceGray >>"}, text: "Visible line\n"},
		"a blend set and restored, isolated page":  {doc: pdfDoc{pages: []string{"q /Mul gs Q " + visibleLine}, resources: states, pageExtra: isolated}, text: "Visible line\n"},
		"a blend set and reset, isolated page":     {doc: pdfDoc{pages: []string{"/Mul gs /Nor gs " + visibleLine}, resources: states, pageExtra: isolated}, text: "Visible line\n"},
		"a blended paint, isolated page":           {doc: pdfDoc{pages: []string{visibleLine + " q /Mul gs 0 g 300 600 50 50 re f Q"}, resources: states, pageExtra: isolated}, want: ErrUnreadableSource},
		"an empty form whose BBox covers the text": {doc: pdfDoc{pages: []string{visibleLine + " /Fm1 Do"}, resources: "/XObject << /Fm1 6 0 R >>", extra: []string{emptyPageForm}}, text: "Visible line\n"},
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
