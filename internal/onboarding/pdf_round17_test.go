package onboarding

import (
	"bytes"
	"errors"
	"testing"

	ledongthuc "github.com/ledongthuc/pdf"
)

// Codex round 12 on 871ffe49 (WIS-625 post-merge): operands were read without their types.
// "1 0 0 1 (10000) 300 Tm" placed the text at x 0 for the checks (Float64 of a string) and at
// x 10000, off the page, for PDF.js. Every interpreted operator's operands must now be of its
// types, or the file is UNREADABLE_SOURCE.
func TestPDFOperandsMustBeOfTheirTypes(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	cases := map[string]string{
		"a string as a Tm coordinate":   "BT /F1 12 Tf 1 0 0 1 (10000) 300 Tm (Hidden claim) Tj ET",
		"a string as a Td offset":       "BT /F1 12 Tf 72 300 Td (9000) 0 Td (Hidden claim) Tj ET",
		"a string as a cm entry":        "q 1 0 0 1 (9000) 0 cm BT /F1 12 Tf 72 300 Td (Hidden claim) Tj ET Q",
		"a number as the Tf font":       "BT 1 12 Tf 72 300 Td (Hidden claim) Tj ET",
		"a name inside a TJ array":      "BT /F1 12 Tf 72 300 Td [(Hidden) /X (claim)] TJ ET",
		"a missing operand":             "BT /F1 12 Tf 72 Td (Hidden claim) Tj ET",
		"a boolean as a Tc spacing":     "BT /F1 12 Tf true Tc 72 300 Td (Hidden claim) Tj ET",
		"a string as a line width":      "q (50) w 0 g 72 280 m 300 280 l S Q " + visibleLine,
		"a name as a fill grey":         "q /A g 72 280 200 30 re f Q " + visibleLine,
		"a number as the Do name":       "q 1 Do Q " + visibleLine,
		"an array where Tj takes text":  "BT /F1 12 Tf 72 300 Td [(Hidden claim)] Tj ET",
		"a string as a rectangle width": "q 1 g 72 280 (200) 30 re f Q " + visibleLine,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Extract(t.Context(), pdfDoc{pages: []string{content}}.bytes(), KindPDF, DefaultLimits)
			if got != "" || !errors.Is(err, ErrUnreadableSource) {
				t.Fatalf("got %q %v, want %v", got, err, ErrUnreadableSource)
			}
		})
	}
	t.Run("operands of their types are read", func(t *testing.T) {
		content := "q 1 0 0 1 0 0 cm 0.5 w 0 0 0 RG 72 290 m 300 290 l S Q BT /F1 12 Tf 0 Tc 0 Tw 100 Tz 14 TL 0 Ts 0 Tr 1 0 0 1 72 300 Tm [(Visible line)] TJ ET"
		got, err := Extract(t.Context(), pdfDoc{pages: []string{content}}.bytes(), KindPDF, DefaultLimits)
		if err != nil || got != "Visible line\n" {
			t.Fatalf("got %q %v", got, err)
		}
	})
}

// The glyph pass checks the extractor's own operands too (walkContent refuses first in a file,
// so it is driven directly).
func TestPDFGlyphPassChecksOperandTypes(t *testing.T) {
	t.Parallel()
	b := pdfDoc{pages: []string{"BT /F1 12 Tf 1 0 0 1 (10000) 300 Tm (Hidden claim) Tj ET"}}.bytes()
	r, err := ledongthuc.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, ok := recover().(operandMismatch); !ok {
			t.Fatal("a string Tm operand was read by the glyph pass")
		}
	}()
	pageGlyphs(r.Page(1), 1)
}

// Codex round 12: a null member of an object stream decoded to nothing and refused the file, while
// a plain null object is skipped. A scan with an unused null member still reads.
func TestPDFNullObjectStreamMemberIsRead(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	page := "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /XObject << /Im1 6 0 R >> >> /Contents 5 0 R >>"
	doc := objStmDoc(append(objStmMembers(page), objStmMember{num: 9, body: "null"}), nil)
	if pages, err := ScanPagesOfPDF(t.Context(), doc); err != nil || len(pages) != 1 {
		t.Fatalf("a null object-stream member: %d pages, %v", len(pages), err)
	}
	// Security review of the fix: a null member still has its identity checked. Object 10 is
	// indexed at the null member's position (4) but the stream lists that member as 9.
	misindexed := objStmDoc(append(objStmMembers(page), objStmMember{num: 9, body: "null"}), map[int]int{10: 4})
	if pages, err := ScanPagesOfPDF(t.Context(), misindexed); pages != nil || !errors.Is(err, ErrUnreadableSource) {
		t.Fatalf("a null member read under another number: %d pages, %v, want %v", len(pages), err, ErrUnreadableSource)
	}
}
