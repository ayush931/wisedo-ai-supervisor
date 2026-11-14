package onboarding

import (
	"bytes"
	"errors"
	"testing"
)

// Security review of the round-3 commits (WIS-625 post-merge): a light repaint only counts where
// its clip lets it paint, a font box must straddle the baseline, and a projecting cap on a slant
// reaches past half the line width.
func TestPDFRepaintBoxesAndCaps(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	doc := func(content string, extra ...string) []byte {
		return pdfDoc{pages: []string{content}, extra: extra}.bytes()
	}
	offBaseline := "<< /Type /Font /Subtype /TrueType /BaseFont /ABCDEF+Custom /FirstChar 32 /LastChar 126 /Widths [" + repeatWidths("600") + "] " +
		"/FontDescriptor << /Type /FontDescriptor /FontName /ABCDEF+Custom /Flags 32 /FontBBox [0 100 1000 900] /ItalicAngle 0 /Ascent 900 /Descent 0 /CapHeight 700 /StemV 80 /FontFile2 6 0 R >> >>"
	diagonal := " 0 G 10 w 40 250 m 68 291.5 l S" // ends 6 pt below the text: within the √2 reach, not the half width
	for name, c := range map[string]struct {
		data []byte
		text string
		want error
	}{
		"a dark box repainted light only inside a small clip": {data: doc("0 g 60 290 200 25 re f q 0 0 10 10 re W n 1 g 60 290 200 25 re f Q 0 g " + visibleLine), want: ErrPDFScanHiddenContent},
		"a font box above the baseline": {data: rebuildXref(t, bytes.Replace(doc("BT /F1 12 Tf 72 300 Td (Text) Tj ET", "<< /Length 1 >>\nstream\nx\nendstream"),
			[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"), []byte(offBaseline), 1)), want: ErrPDFNoTextLayer},
		"a slanted stroke with projecting caps just below the text": {data: doc(visibleLine + " 2 J" + diagonal), want: ErrPDFScanHiddenContent},
		"the same stroke with butt caps":                            {data: doc(visibleLine + " 0 J" + diagonal), text: "Visible line\n"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := prepareDocument(t.Context(), DocumentUpload{Title: "Press SOP", DeclaredType: "application/pdf", Data: c.data})
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
}

func repeatWidths(w string) string {
	out := w
	for i := 1; i < 95; i++ {
		out += " " + w
	}
	return out
}
