package onboarding

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"

	ledongthuc "github.com/ledongthuc/pdf"
)

// pngUp encodes content as PNG "Up" rows of columns bytes (padded with spaces), as ledongthuc
// reads /Predictor 12 whatever /Colors says.
func pngUp(content string, columns int) []byte {
	for len(content)%columns != 0 {
		content += " "
	}
	var raw bytes.Buffer
	prev := make([]byte, columns)
	for i := 0; i < len(content); i += columns {
		raw.WriteByte(2)
		for j := 0; j < columns; j++ {
			raw.WriteByte(content[i+j] - prev[j])
		}
		copy(prev, content[i:i+columns])
	}
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	_, _ = w.Write(raw.Bytes())
	_ = w.Close()
	return z.Bytes()
}

// withContent replaces the first page's content stream object (5) of a pdfDoc file.
func withContent(t *testing.T, doc pdfDoc, dict string, data []byte) []byte {
	t.Helper()
	b := doc.bytes()
	old := fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(doc.pages[0]), doc.pages[0])
	if !bytes.Contains(b, []byte(old)) {
		t.Fatal("fixture: content object not found")
	}
	repl := append(fmt.Appendf(nil, "<< /Length %d %s >>\nstream\n", len(data), dict), data...)
	repl = append(repl, "\nendstream"...)
	return rebuildXref(t, bytes.Replace(b, []byte(old), repl, 1))
}

// Codex round 11 on 1d7fd692 (WIS-625 post-merge). ledongthuc applies a PNG predictor without
// /Colors, so a content stream can decode to readable text for the extractor and to other bytes
// for a viewer; content and form streams may now carry only FlateDecode without parameters (and
// must decode to the bytes pdfcpu decodes). The page tree must be one both readers walk alike:
// pdfcpu repaired an untyped node while ledongthuc selected pages by /Count.
func TestPDFReadersDecodeAndSelectAlike(t *testing.T) {
	requirePDFSandbox(t)
	predicted := pngUp(visibleLine, 8)
	twoPages := pdfDoc{pages: []string{visibleLine, "BT /F1 12 Tf 72 300 Td (Second page) Tj ET"}}.bytes()
	cases := map[string]struct {
		pdf  []byte
		want error
		text string
	}{
		"content with a predictor": {pdf: withContent(t, pdfDoc{pages: []string{visibleLine}}, "/Filter /FlateDecode /DecodeParms << /Predictor 12 /Colors 3 /Columns 8 >>", predicted), want: ErrUnreadableSource},
		"content in ASCIIHex":      {pdf: withContent(t, pdfDoc{pages: []string{visibleLine}}, "/Filter /ASCIIHexDecode", fmt.Appendf(nil, "%x>", visibleLine)), want: ErrUnreadableSource},
		"a form with a predictor": {pdf: pdfDoc{pages: []string{visibleLine + " /Fm1 Do"}, resources: "/XObject << /Fm1 6 0 R >>",
			extra: []string{fmt.Sprintf("<< /Type /XObject /Subtype /Form /BBox [0 0 1 1] /Filter /FlateDecode /DecodeParms << /Predictor 12 /Colors 3 /Columns 4 >> /Length %d >>\nstream\n%s\nendstream", len(pngUp("n", 4)), pngUp("n", 4))}}.bytes(), want: ErrUnreadableSource},
		"plain Flate content is read": {pdf: pdfDoc{pages: []string{visibleLine}, compress: map[int]bool{0: true}}.bytes(), text: "Visible line\n"},
		// The page tree.
		"a /Count short of the pages":          {pdf: bytes.Replace(twoPages, []byte("/Count 2"), []byte("/Count 1"), 1), want: ErrUnreadableSource},
		"a page-tree node without /Type":       {pdf: rebuildXref(t, bytes.Replace(twoPages, []byte("<< /Type /Pages /Kids"), []byte("<< /Kids"), 1)), want: ErrUnreadableSource},
		"a page whose /Parent is not its node": {pdf: rebuildXref(t, bytes.Replace(twoPages, []byte("/Type /Page /Parent 2 0 R"), []byte("/Type /Page /Parent 1 0 R"), 1)), want: ErrUnreadableSource},
		"an ordinary two-page tree":            {pdf: twoPages, text: "Visible line\nSecond page\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Extract(t.Context(), c.pdf, KindPDF, DefaultLimits)
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

// Codex round 11: an incremental save that replaces one object of an object stream leaves the old
// member listed in the old stream; only the current entry is checked, so the file still reads.
func TestPDFIncrementalSaveOfACompressedObject(t *testing.T) {
	requirePDFSandbox(t)
	page := "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /XObject << /Im1 6 0 R >> >> /Contents 5 0 R >>"
	base := objStmScan(page)
	var prev int
	if _, err := fmt.Sscanf(string(base[bytes.LastIndex(base, []byte("startxref\n")):]), "startxref\n%d", &prev); err != nil {
		t.Fatal(err)
	}
	b := bytes.NewBuffer(append([]byte(nil), base...))
	obj4 := b.Len()
	fmt.Fprintf(b, "4 0 obj\n%s\nendobj\n", strings.Replace(page, "/MediaBox", "/Rotate 0 /MediaBox", 1))
	xref := b.Len()
	rows := []byte{1, byte(obj4 >> 24), byte(obj4 >> 16), byte(obj4 >> 8), byte(obj4), 0, 0, 1, byte(xref >> 24), byte(xref >> 16), byte(xref >> 8), byte(xref), 0, 0}
	fmt.Fprintf(b, "10 0 obj\n<< /Type /XRef /Size 11 /W [1 4 2] /Index [4 1 10 1] /Root 1 0 R /Prev %d /Length %d >>\nstream\n%s\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", prev, len(rows), rows, xref)
	if pages, err := ScanPagesOfPDF(t.Context(), b.Bytes()); err != nil || len(pages) != 1 {
		t.Fatalf("an incremental save of a compressed object: %d pages, %v", len(pages), err)
	}
}

// The extractor's read of each content stream is compared with pdfcpu's decode by file offset;
// a stream whose bytes differ refuses the file (a panic parsePDF turns into UNREADABLE_SOURCE).
// No well-formed file is known to decode differently once only plain Flate is allowed, so the
// comparison is driven directly with the screen's own record, then with a wrong one.
func TestPDFContentMustDecodeAsScreened(t *testing.T) {
	b := pdfDoc{pages: []string{visibleLine}, compress: map[int]bool{0: true}}.bytes()
	reason, screened := screenPDF(b)
	if reason != "" {
		t.Fatal(reason)
	}
	r, err := ledongthuc.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	contents := r.Page(1).V.Key("Contents")
	if _, ok := contentTokens(contents, screened.decoded); !ok {
		t.Fatal("the screened content was not read")
	}
	wrong := map[int64][32]byte{}
	for off := range screened.decoded {
		wrong[off] = sha256.Sum256([]byte("other bytes"))
	}
	defer func() {
		if _, ok := recover().(streamMismatch); !ok {
			t.Fatal("content decoded to other bytes than the screen's was read")
		}
	}()
	contentTokens(contents, wrong)
}
