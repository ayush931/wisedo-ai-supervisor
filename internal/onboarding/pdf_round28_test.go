package onboarding

import (
	"errors"
	"strings"
	"testing"
)

// Codex round 22 on 8ea02ffb (WIS-625 post-merge).
//   - In a knockout transparency group (/K true) a paint of alpha 0 erases what is under it; an
//     isolated group (/I true) composites a non-Normal blend against nothing. A page with either
//     is refused; alpha-0 paint is skipped only outside them.
//   - An image or a shading drawn at alpha 0 still counted as covering the text.
//   - A form in the resources that the page never draws refused the page.
func TestPDFTransparencyGroupsAlphaZeroDrawsAndUnusedForms(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	states := "/ExtGState << /Clear << /ca 0 /CA 0 >> /Half << /ca 0.5 >> /Mul << /BM /Multiply >> >>"
	shading := "<< /ShadingType 2 /ColorSpace /DeviceGray /Coords [0 0 1 0] /Function << /FunctionType 2 /Domain [0 1] /N 1 >> >>"
	erase := " q /Clear gs 0 g 60 290 200 25 re f Q"
	textForm := formObject("/Font << /F1 3 0 R >>", "BT /F1 12 Tf 0 0 Td (Unused) Tj ET")
	for name, c := range map[string]struct {
		doc  pdfDoc
		text string
		want error
	}{
		"alpha 0 over text in a knockout group": {doc: pdfDoc{pages: []string{visibleLine + erase}, resources: states, pageExtra: "/Group << /S /Transparency /K true >>"}, want: ErrUnreadableSource},
		"alpha 0 over text in a plain group":    {doc: pdfDoc{pages: []string{visibleLine + erase}, resources: states, pageExtra: "/Group << /S /Transparency >>"}, text: "Visible line\n"},
		"a blend in an isolated group":          {doc: pdfDoc{pages: []string{visibleLine + " q /Mul gs 0 g 300 600 50 50 re f Q"}, resources: states, pageExtra: "/Group << /S /Transparency /I true >>"}, want: ErrUnreadableSource},
		"normal paint in an isolated group":     {doc: pdfDoc{pages: []string{visibleLine + " q 0 g 300 600 50 50 re f Q"}, resources: states, pageExtra: "/Group << /S /Transparency /I true >>"}, text: "Visible line\n"},
		"an image at alpha 0 over the text":     {doc: pdfDoc{pages: []string{visibleLine + " q /Clear gs 300 0 0 300 50 200 cm /Im1 Do Q"}, resources: states + " " + imageResources(6), extra: []string{imageObject}}, text: "Visible line\n"},
		"an image at alpha 0.5 over the text":   {doc: pdfDoc{pages: []string{visibleLine + " q /Half gs 300 0 0 300 50 200 cm /Im1 Do Q"}, resources: states + " " + imageResources(6), extra: []string{imageObject}}, want: ErrPDFScanHiddenContent},
		"a shading at alpha 0 over the text":    {doc: pdfDoc{pages: []string{visibleLine + " q /Clear gs 60 290 200 25 re W n /Sh1 sh Q"}, resources: states + " /Shading << /Sh1 6 0 R >>", extra: []string{shading}}, text: "Visible line\n"},
		"a form the page never draws":           {doc: pdfDoc{pages: []string{visibleLine}, resources: "/XObject << /Fm1 6 0 R >>", extra: []string{textForm}}, text: "Visible line\n"},
		"a drawn form's unused nested form": {doc: pdfDoc{pages: []string{visibleLine + " /Fm1 Do"}, resources: "/XObject << /Fm1 6 0 R >>",
			extra: []string{formObject("/XObject << /Fm2 7 0 R >>", "q Q"), textForm}}, text: "Visible line\n"},
		"a drawn form in a knockout group": {doc: pdfDoc{pages: []string{visibleLine + " /Fm1 Do"}, resources: "/XObject << /Fm1 6 0 R >>",
			extra: []string{strings.Replace(formObject("", "q Q"), "/Subtype /Form", "/Subtype /Form /Group << /S /Transparency /K true >>", 1)}}, want: ErrPDFNoTextLayer},
		"a form the page draws": {doc: pdfDoc{pages: []string{visibleLine + " /Fm1 Do"}, resources: "/XObject << /Fm1 6 0 R >>", extra: []string{textForm}}, want: ErrPDFNoTextLayer},
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
