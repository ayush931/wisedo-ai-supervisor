package onboarding

import (
	"errors"
	"testing"
)

// pdfRuleCases runs pages of one span of text with rules near it: text is what each must read,
// or want the error it must be refused with.
func pdfRuleCases(t *testing.T, cases map[string]struct {
	page string
	text string
	want error
}) {
	t.Helper()
	requirePDFSandbox(t)
	for name, c := range cases {
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

const ruleText = " BT /F1 12 Tf 72 300 Td (Test) Tj ET"

// Codex round 43 on 6dda50b5 (WIS-625 post-merge): a rule repainted white over exactly its own
// box, then redrawn, was counted twice; erasure asked for a margin beyond the rule. A fill that
// contains the rule exactly in both readings erases it.
func TestPDFExactRepaintErasesRule(t *testing.T) {
	t.Parallel()
	pdfRuleCases(t, map[string]struct {
		page string
		text string
		want error
	}{
		"black, white, black over one box": {page: "0 g 72 298 28 0.6 re f 1 g 72 298 28 0.6 re f 0 g 72 298 28 0.6 re f" + ruleText, text: "Test\n"},
		// The redraw lower down is a second rule unless the first is wholly erased.
		"an exact repaint, then a rule lower": {page: "0 g 72 298.5 28 0.6 re f 1 g 72 298.5 28 0.6 re f 0 g 72 297 28 0.6 re f" + ruleText, text: "Test\n"},
		"a white fill short of the rule":      {page: "0 g 72 298.5 28 0.6 re f 1 g 72 298.5 20 0.6 re f 0 g 72 297 28 0.6 re f" + ruleText, want: ErrPDFScanHiddenContent},
		"a white fill a sliver too low":       {page: "0 g 72 298.5 28 0.6 re f 1 g 72 298.49 28 0.6 re f 0 g 72 297 28 0.6 re f" + ruleText, want: ErrPDFScanHiddenContent},
	})
}

// Codex round 43 on 6dda50b5: an underline drawn as two adjoining fills was refused as two rules.
// Pieces at the same height that touch or overlap are one rule, checked as a whole.
func TestPDFAdjoiningRulePiecesAreOneRule(t *testing.T) {
	t.Parallel()
	pdfRuleCases(t, map[string]struct {
		page string
		text string
		want error
	}{
		"two adjoining pieces after the text":  {page: ruleText + " 72 298 14 0.6 re f 86 298 14 0.6 re f", text: "Test\n"},
		"two adjoining pieces before the text": {page: "72 298 14 0.6 re f 86 298 14 0.6 re f" + ruleText, text: "Test\n"},
		"one piece before, one after":          {page: "72 298 14 0.6 re f" + ruleText + " 86 298 14 0.6 re f", text: "Test\n"},
		"two stacked pieces at different y":    {page: ruleText + " 72 298 14 0.6 re f 86 297 14 0.6 re f", want: ErrPDFScanHiddenContent},
		"two stacked pieces before the text":   {page: "72 298 14 0.6 re f 86 297 14 0.6 re f" + ruleText, want: ErrPDFScanHiddenContent},
		"two pieces with a gap":                {page: ruleText + " 72 298 10 0.6 re f 86 298 14 0.6 re f", want: ErrPDFScanHiddenContent},
		"a repeated rule over itself":          {page: "72 298 28 0.6 re f" + ruleText + " 72 298 28 0.6 re f", text: "Test\n"},
		"a partly erased rule, then one lower": {page: "72 298.5 28 0.6 re f 1 g 85 298.5 20 0.6 re f 0 g 72 297 28 0.6 re f" + ruleText, want: ErrPDFScanHiddenContent},
		"a thick bar in two thin pieces":       {page: ruleText + " 72 298 28 0.6 re f 72 298.6 28 0.6 re f", want: ErrPDFScanHiddenContent},
	})
}
