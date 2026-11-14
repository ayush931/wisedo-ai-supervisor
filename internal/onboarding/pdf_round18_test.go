package onboarding

import (
	"errors"
	"testing"
)

// Codex round 13 on 0e433266 (WIS-625 post-merge). Operand values were compared as token text:
// "+0 j" was not the miter join "0", so a miter's spike over the text fell outside the computed
// paint, and "+0 Tr" or "00 Tr" was refused. Integer operands are read as numbers and range
// checked (outside the range refuses). BX ... EX sections ignore unknown operators, as the
// specification requires, and must balance. An empty "() Tj" on an otherwise blank page draws
// nothing and loses nothing.
func TestPDFOperandValuesAreNumbers(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	// A "v" stroke whose miter spike at (100, 330) reaches down to about y 303, into the line;
	// a bevel or the sqrt 2 reach stops at about y 316.
	miter := func(join string) string {
		return visibleLine + " q 0 g 20 w 10 M " + join + " j 72 400 m 100 330 l 128 400 l S Q"
	}
	cases := map[string]struct {
		pages []string
		text  string
		want  error
	}{
		"a miter join written 0":           {pages: []string{miter("0")}, want: ErrPDFScanHiddenContent},
		"a miter join written +0":          {pages: []string{miter("+0")}, want: ErrPDFScanHiddenContent},
		"a miter join written 0.0":         {pages: []string{miter("0.0")}, want: ErrPDFScanHiddenContent},
		"a line join of 3":                 {pages: []string{"q 3 j Q " + visibleLine}, want: ErrUnreadableSource},
		"a line cap of 0.5":                {pages: []string{"q 0.5 J Q " + visibleLine}, want: ErrUnreadableSource},
		"render mode +0":                   {pages: []string{"BT /F1 12 Tf +0 Tr 72 300 Td (Visible line) Tj ET"}, text: "Visible line\n"},
		"render mode 00":                   {pages: []string{"BT /F1 12 Tf 00 Tr 72 300 Td (Visible line) Tj ET"}, text: "Visible line\n"},
		"render mode 8":                    {pages: []string{"BT /F1 12 Tf 8 Tr 72 300 Td (Visible line) Tj ET"}, want: ErrUnreadableSource},
		"render mode 1.5":                  {pages: []string{"BT /F1 12 Tf 1.5 Tr 72 300 Td (Visible line) Tj ET"}, want: ErrUnreadableSource},
		"vendor hints in BX ... EX":        {pages: []string{"BX /Vendor 1 Zz /Hint Yy EX " + visibleLine}, text: "Visible line\n"},
		"nested BX ... EX":                 {pages: []string{"BX BX 1 Zz EX EX " + visibleLine}, text: "Visible line\n"},
		"an unbalanced BX":                 {pages: []string{"BX 1 Zz " + visibleLine}, want: ErrUnreadableSource},
		"an EX without BX":                 {pages: []string{visibleLine + " EX"}, want: ErrUnreadableSource},
		"a known operator in BX, mistyped": {pages: []string{"BX q (9) w Q EX " + visibleLine}, want: ErrUnreadableSource},
		"an empty show on a blank page":    {pages: []string{visibleLine, "BT /F1 12 Tf 72 300 Td () Tj ET"}, text: "Visible line\n"},
		"a shown code on a blank page":     {pages: []string{visibleLine, "BT /F1 12 Tf 72 300 Td ( ) Tj ET"}, want: ErrPDFNoTextLayer},
	}
	for name, c := range cases {
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
}
