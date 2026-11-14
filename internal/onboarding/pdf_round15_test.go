package onboarding

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// incrementalUpdate appends a section to a pdfDoc file replacing object 5 (the first page's
// content) with content. hybrid hides the replacement in an /XRefStm stream, as a hybrid-reference
// file does; otherwise it is listed in an ordinary cross-reference table.
func incrementalUpdate(t *testing.T, base []byte, content string, hybrid bool) []byte {
	t.Helper()
	at := bytes.LastIndex(base, []byte("startxref\n"))
	var prev int
	if _, err := fmt.Sscanf(string(base[at:]), "startxref\n%d", &prev); err != nil {
		t.Fatal(err)
	}
	b := bytes.NewBuffer(append([]byte(nil), base...))
	obj5 := b.Len()
	fmt.Fprintf(b, "5 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(content), content)
	if !hybrid {
		xref := b.Len()
		fmt.Fprintf(b, "xref\n0 1\n0000000000 65535 f \n5 1\n%010d 00000 n \ntrailer\n<< /Size 7 /Root 1 0 R /Prev %d >>\nstartxref\n%d\n%%%%EOF\n", obj5, prev, xref)
		return b.Bytes()
	}
	row := []byte{1, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(row[1:5], uint32(obj5))
	stm := b.Len()
	fmt.Fprintf(b, "7 0 obj\n<< /Type /XRef /Size 8 /W [1 4 2] /Index [5 1] /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(row), row)
	xref := b.Len()
	fmt.Fprintf(b, "xref\n0 1\n0000000000 65535 f \ntrailer\n<< /Size 8 /Root 1 0 R /Prev %d /XRefStm %d >>\nstartxref\n%d\n%%%%EOF\n", prev, stm, xref)
	return b.Bytes()
}

// lengthUnset is a page whose content stream has an indirect /Length (object 6), and an update
// that lists object 6 as in use at offset 0.
func lengthUnset(t *testing.T) []byte {
	t.Helper()
	body := visibleLine
	b := pdfDoc{pages: []string{body}, extra: []string{fmt.Sprint(len(body))}}.bytes()
	b = rebuildXref(t, bytes.Replace(b, fmt.Appendf(nil, "/Length %d >>", len(body)), []byte("/Length 6 0 R >>"), 1))
	var prev int
	if _, err := fmt.Sscanf(string(b[bytes.LastIndex(b, []byte("startxref\n")):]), "startxref\n%d", &prev); err != nil {
		t.Fatal(err)
	}
	return fmt.Appendf(b, "xref\n6 1\n0000000000 00000 n \ntrailer\n<< /Size 7 /Root 1 0 R /Prev %d >>\nstartxref\n%d\n%%%%EOF\n", prev, len(b))
}

// Codex round 10 on 75a10b00 (WIS-625 post-merge): pdfcpu reads a hybrid file's /XRefStm,
// ledongthuc does not, so the screen checked the replacement page and the extractor read the
// original. A hybrid file is refused, and the extractor must read each page's content from the
// streams the screen resolved. An ordinary incremental update is still read, as updated.
func TestPDFReadersAgreeOnTheObjectGraph(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	base := pdfDoc{pages: []string{visibleLine}}.bytes()
	replaced := "BT /F1 12 Tf 72 300 Td (Replaced line) Tj ET"
	cases := map[string]struct {
		pdf  []byte
		text string
		want error
	}{
		"a replacement page hidden in /XRefStm": {pdf: incrementalUpdate(t, base, replaced, true), want: ErrUnreadableSource},
		// Both readers skip an update's in-use entry at offset 0 and keep the older object, so the
		// indirect /Length resolves alike and the file is read.
		"an update entry at offset 0 for an indirect /Length": {pdf: lengthUnset(t), text: "Visible line\n"},
		// pdfcpu repairs an in-use object 0 silently; a repaired file is not the file a viewer reads.
		"a file pdfcpu repaired":         {pdf: bytes.Replace(base, []byte("0000000000 65535 f \n"), []byte("0000000000 00000 n \n"), 1), want: ErrUnreadableSource},
		"an ordinary incremental update": {pdf: incrementalUpdate(t, base, replaced, false), text: "Replaced line\n"},
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

// Security review of round 10: the screen compares pdfcpu's whole cross-reference table with
// ledongthuc's (read by reflection), not only the page content streams, so an indirect /Length,
// font or resource cannot resolve differently. Driven directly: no well-formed file is known to
// make the two disagree once hybrid files are refused, so pdfcpu's view is moved by one object.
func TestPDFCrossReferenceTablesMustAgree(t *testing.T) {
	t.Parallel()
	b := pdfDoc{pages: []string{visibleLine}, compress: map[int]bool{0: true}}.bytes()
	read := func() *model.Context {
		model.ConfigPath = "disable"
		conf := model.NewDefaultConfiguration()
		conf.Offline, conf.ValidateLinks, conf.ValidationMode = true, false, model.ValidationRelaxed
		ctx, err := pdfcpu.Read(bytes.NewReader(b), conf)
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	if !sameCrossReferences(b, read()) {
		t.Fatal("an ordinary file: the readers' tables differ")
	}
	moved := read()
	off := *moved.Table[5].Offset + 1
	moved.Table[5].Offset = &off
	if sameCrossReferences(b, moved) {
		t.Fatal("object 5 at another offset to pdfcpu was not noticed")
	}
	gone := read()
	delete(gone.Table, 3) // ledongthuc still lists the font; pdfcpu does not: harmless, see sameCrossReferences
	if !sameCrossReferences(b, gone) {
		t.Fatal("an object only ledongthuc holds was refused")
	}
	extra := read()
	extra.Table[9] = extra.Table[5] // in use to pdfcpu only
	if sameCrossReferences(b, extra) {
		t.Fatal("an object only pdfcpu holds was not noticed")
	}
}
