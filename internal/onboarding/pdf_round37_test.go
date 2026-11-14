package onboarding

import (
	"errors"
	"testing"
)

// Codex round 31 on 486068f1 (WIS-625 post-merge).
//   - A 99 x 3300 scan stretched across a page skipped the aspect check (its short side is under
//     the figure threshold, and plainScanPage had none): the page shows it distorted 26:1, the
//     model reads it undistorted. A scan placed in another shape than its pixels by more than 2:1
//     is refused, as a figure is.
//   - A dark mark just below the line (within the margin, not under it) erased by an exact white
//     repaint was refused: its region under the text is empty. A light paint that contains the
//     whole mark exactly erased it.
func TestPDFDistortedNarrowScansAndErasedMarksBesideText(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	full := "q 612 0 0 792 0 0 cm /Im1 Do Q"
	for name, c := range map[string]struct {
		image string
		pages int
		want  error
	}{
		"a 99 x 3300 scan stretched over the page": {image: figureObject(scanImage(t, 99, 3300, "jpeg"), 99, 3300), want: ErrPDFScanHiddenContent},
		"a scan in its own shape":                  {image: figureObject(scanImage(t, 773, 1000, "jpeg"), 773, 1000), pages: 1},
	} {
		t.Run(name, func(t *testing.T) {
			pages, err := ScanPagesOfPDF(t.Context(), pdfDoc{pages: []string{full}, resources: imageResources(6), extra: []string{c.image}}.bytes())
			if c.want != nil {
				if pages != nil || !errors.Is(err, c.want) {
					t.Fatalf("%d pages %v, want %v", len(pages), err, c.want)
				}
				return
			}
			if err != nil || len(pages) != c.pages {
				t.Fatalf("%d pages %v", len(pages), err)
			}
		})
	}
	// The line's box starts about y 297.52: a mark up to 297.5 lies within the margin of it.
	for name, c := range map[string]struct {
		page string
		text string
		want error
	}{
		"a mark within the margin, erased exactly": {page: "q 0 g 72 294.5 50 3 re f 1 g 72 294.5 50 3 re f Q " + visibleLine, text: "Visible line\n"},
		"a mark within the margin, not erased":     {page: "q 0 g 72 294.5 50 3 re f Q " + visibleLine, want: ErrPDFScanHiddenContent},
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
}
