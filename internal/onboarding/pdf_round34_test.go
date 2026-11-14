package onboarding

import (
	"errors"
	"testing"
)

// Codex round 28 on 77336548 (WIS-625 post-merge).
//   - The float32 shadow rounded each matrix dot product once; a float32 reader rounds every
//     multiply and every add. Products and box corners are now computed in float32 arithmetic.
//   - A difference within the 0.01 tolerance could still flip a coverage decision: overlap tests
//     widen by 2 x 0.01 and text containment shrinks by it, so such a difference can only make the
//     decision more cautious.
//   - The page boxes were not rounded in the float32 walk: a paint just outside a fractional
//     CropBox left a sliver in one reading, and an ordinary page was refused. The boxes are
//     rounded, and a paint only one reading records is tolerated when no wider than the margin.
func TestPDFFloat32ArithmeticDecisionMarginsAndCropBoxes(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	for name, c := range map[string]struct {
		doc  pdfDoc
		text string
		want error
	}{
		// The line's box ends between y 308.61 and 308.62; a white fill from 308.62 is clear of it
		// by less than the margin.
		"a white fill within the margin of the text": {doc: pdfDoc{pages: []string{visibleLine + " q 1 g 72 308.62 50 5 re f Q"}}, want: ErrPDFScanHiddenContent},
		"a white fill clear of the margin":           {doc: pdfDoc{pages: []string{visibleLine + " q 1 g 72 308.66 50 5 re f Q"}}, text: "Visible line\n"},
		"a paint just outside a fractional CropBox":  {doc: pdfDoc{pages: []string{visibleLine + " q 0 g 595.27579 400 10 10 re f Q"}, pageExtra: "/CropBox [0 0 595.2758 842]"}, text: "Visible line\n"},
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

// cellBox32 rounds each multiply and each add: for these inputs that gives 2162.1689453125, where
// rounding the float64 dot product once gives 2162.169189453125. Within maxCoordinate no page
// was found where the two differ by more than the tolerance, so the arithmetic is checked here.
func TestPDFCellBox32RoundsEveryOperation(t *testing.T) {
	t.Parallel()
	m := textMatrix{{-324.2062683105469, 0, 0}, {-380.0841369628906, 0, 0}, {637.0361328125, 0, 1}}
	box, ok := cellBox32(-0.3850962817668915, -0.3850962817668915, -3.6841378211975098, -3.6841378211975098, m)
	if !ok || box[0] != 2162.1689453125 {
		t.Fatalf("got %v %v, want x 2162.1689453125", box, ok)
	}
}

// Security review of round 28: a paint only one reading records is tolerated only as a sliver
// under the same clip as the other reading's at that operator, never as a whole-page paint.
func TestPDFUnmatchedPaintsAreSliversOnly(t *testing.T) {
	t.Parallel()
	clip := opClip{clip: [4]float64{0, 0, 612, 792}, known: true}
	base := pageContent{texts: map[int]textInk{}, opClips: map[int]opClip{3: clip}}
	with := func(pe paintEvent) pageContent {
		pc := base
		pc.paints = []paintEvent{pe}
		return pc
	}
	sliver := paintEvent{op: 3, box: [4]float64{595.2757, 400, 595.2758, 410}, clip: clip.clip, clipKnown: true}
	if !samePagePlaces(with(sliver), base) {
		t.Fatal("a sliver under the same clip was refused")
	}
	if samePagePlaces(with(paintEvent{op: 3, whole: true}), base) {
		t.Fatal("a whole-page paint only one reading recorded was accepted")
	}
	other := sliver
	other.clipKnown = false
	if samePagePlaces(with(other), base) {
		t.Fatal("a sliver under another clip state was accepted")
	}
	wide := sliver
	wide.box[2] += 1
	if samePagePlaces(with(wide), base) {
		t.Fatal("a paint wider than the margin only one reading recorded was accepted")
	}
}
