package onboarding

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// Codex round 9 on 85aea301 (WIS-625 post-merge): a stream's declared /Length must end exactly
// at "endstream". pdfcpu re-scans for "endstream" when the length is wrong, so a short /Length
// let the checks read a painted-over page that a viewer reads as plain text (or the reverse).
func TestPDFStreamLengthMustLandOnEndstream(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	body := visibleLine + " 1 g 0 0 612 792 re f" // a white page over the line
	doc := func(length string, extra ...string) []byte {
		b := pdfDoc{pages: []string{body}, extra: extra}.bytes()
		b = bytes.Replace(b, fmt.Appendf(nil, "/Length %d >>", len(body)), fmt.Appendf(nil, "/Length %s >>", length), 1)
		return rebuildXref(t, b)
	}
	short := len(visibleLine)
	cases := map[string]struct {
		pdf  []byte
		want error
	}{
		"a direct length short of endstream":      {doc(fmt.Sprint(short)), ErrUnreadableSource},
		"a direct length of zero":                 {doc("0"), ErrUnreadableSource},
		"a direct length past endstream":          {doc(fmt.Sprint(len(body) + 4)), ErrUnreadableSource},
		"an indirect length short of endstream":   {doc("6 0 R", fmt.Sprint(short)), ErrUnreadableSource},
		"an indirect length that is not a number": {doc("6 0 R", "(12)"), ErrUnreadableSource},
		// Codex round 10: a signed non-negative integer is a valid length.
		"an exact signed length is read":   {doc(fmt.Sprintf("+%d", len(body))), ErrPDFScanHiddenContent},
		"a negative length":                {doc("-1"), ErrUnreadableSource},
		"a real-number length":             {doc(fmt.Sprintf("%d.0", len(body))), ErrUnreadableSource},
		"an exact indirect length is read": {doc("6 0 R", fmt.Sprint(len(body))), ErrPDFScanHiddenContent},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Extract(t.Context(), c.pdf, KindPDF, DefaultLimits)
			if got != "" || !errors.Is(err, c.want) {
				t.Fatalf("got %q %v, want %v", got, err, c.want)
			}
		})
	}
}
