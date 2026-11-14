package onboarding

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

const docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

func makeZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(content)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func docx(t *testing.T, extra map[string][]byte) []byte {
	files := map[string][]byte{"[Content_Types].xml": []byte("<Types/>"), "word/document.xml": []byte("<w:document/>")}
	for k, v := range extra {
		files[k] = v
	}
	return makeZip(t, files)
}

func TestReadBoundedCountsActualBytes(t *testing.T) {
	t.Parallel()
	if _, err := ReadBounded(strings.NewReader(strings.Repeat("a", 11)), 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over limit: %v", err)
	}
	if got, err := ReadBounded(strings.NewReader(strings.Repeat("a", 10)), 10); err != nil || len(got) != 10 {
		t.Fatalf("exact limit: %d %v", len(got), err)
	}
	if _, err := ReadBounded(iotest.ErrReader(errors.New("boom")), 10); !errors.Is(err, ErrUnreadableSource) {
		t.Fatalf("read error: %v", err)
	}
}

func TestInspect(t *testing.T) {
	t.Parallel()
	l := DefaultLimits
	bomb := makeZip(t, map[string][]byte{"[Content_Types].xml": []byte("<Types/>"), "word/document.xml": bytes.Repeat([]byte{0}, 4<<20)})
	small := l
	small.MaxDecompressedBytes = 1 << 20
	external := docx(t, map[string][]byte{"word/_rels/document.xml.rels": []byte(`<Relationship Target="http://x.invalid" TargetMode="External"/>`)})
	internal := docx(t, map[string][]byte{"word/_rels/document.xml.rels": []byte(`<Relationship Target="styles.xml"/>`)})
	cases := []struct {
		name     string
		data     []byte
		declared string
		limits   Limits
		kind     Kind
		err      error
	}{
		{"utf8 text", []byte("Step 1: inspect"), "text/plain; charset=utf-8", l, KindText, nil},
		{"markdown", []byte("# SOP"), "text/markdown", l, KindText, nil},
		{"undeclared text", []byte("hello"), "", l, KindText, nil},
		{"empty", nil, "text/plain", l, "", ErrUnreadableSource},
		{"nul byte", []byte("a\x00b"), "text/plain", l, "", ErrUnsupportedMedia},
		{"invalid utf8", []byte{0xff, 0xfe, 'a'}, "text/plain", l, "", ErrUnsupportedMedia},
		{"text over extract cap", bytes.Repeat([]byte("a"), 11), "text/plain", Limits{MaxFileBytes: 100, MaxExtractedText: 10}, "", ErrTooLarge},
		{"file over cap", bytes.Repeat([]byte("a"), 11), "", Limits{MaxFileBytes: 10, MaxExtractedText: 10}, "", ErrTooLarge},
		{"unsupported declared type", []byte("a"), "image/png", l, "", ErrUnsupportedMedia},
		{"declared pdf but text", []byte("a"), "application/pdf", l, "", ErrUnsupportedMedia},
		{"windows executable", []byte("MZ\x90\x00"), "text/plain", l, "", ErrUnsupportedMedia},
		{"elf", []byte("\x7fELF\x02"), "", l, "", ErrUnsupportedMedia},
		{"script", []byte("#!/bin/sh\nrm -rf /"), "", l, "", ErrUnsupportedMedia},
		{"pdf", []byte("%PDF-1.7\n1 0 obj\n%%EOF"), "application/pdf", l, KindPDF, nil},
		{"encrypted pdf", []byte("%PDF-1.7\ntrailer<</Encrypt 5 0 R>>\n%%EOF"), "application/pdf", l, "", ErrPDFEncrypted},
		{"truncated pdf", []byte("%PDF-1.7\n1 0 obj"), "", l, "", ErrUnreadableSource},
		{"docx", docx(t, nil), docxType, l, KindDOCX, nil},
		{"docx internal relationship", internal, "", l, KindDOCX, nil},
		{"docx external relationship", external, docxType, l, "", ErrUnreadableSource},
		{"docx decompression bomb", bomb, docxType, small, "", ErrTooLarge},
		{"plain zip archive", makeZip(t, map[string][]byte{"a.txt": []byte("x")}), "", l, "", ErrUnsupportedMedia},
		{"zip path traversal", docx(t, map[string][]byte{"../evil": []byte("x")}), "", l, "", ErrUnreadableSource},
		{"zip too many entries", docx(t, map[string][]byte{"a": nil, "b": nil, "c": nil}), "", Limits{MaxFileBytes: 1 << 20, MaxDecompressedBytes: 1 << 20, MaxZipEntries: 2}, "", ErrTooLarge},
		{"corrupt zip", []byte("PK\x03\x04garbage"), "", l, "", ErrUnreadableSource},
		// Audit defect (WIS-613): macros were refused only by the part name vbaProject.bin. The
		// package's own declarations (content types and relationship types) name macro content
		// whatever the part is called.
		{"macro-enabled main content type", docx(t, map[string][]byte{"[Content_Types].xml": []byte(`<Types><Override PartName="/word/document.xml" ContentType="application/vnd.ms-word.document.macroEnabled.main+xml"/></Types>`)}), "", l, "", ErrUnreadableSource},
		{"Excel 4.0 macro sheet content type", docx(t, map[string][]byte{"[Content_Types].xml": []byte(`<Types><Override PartName="/xl/macrosheets/sheet1.xml" ContentType="application/vnd.ms-excel.macrosheet+xml"/></Types>`)}), "", l, "", ErrUnreadableSource},
		{"VBA project under another part name", docx(t, map[string][]byte{"word/_rels/document.xml.rels": []byte(`<Relationships><Relationship Id="r1" Type="http://schemas.microsoft.com/office/2006/relationships/vbaProject" Target="code.bin"/></Relationships>`)}), "", l, "", ErrUnreadableSource},
		{"macro sheet part name", docx(t, map[string][]byte{"xl/macrosheets/sheet1.xml": []byte("<x/>")}), "", l, "", ErrUnreadableSource},
		{"control: ordinary content types", docx(t, map[string][]byte{"[Content_Types].xml": []byte(`<Types><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`)}), "", l, KindDOCX, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, err := Inspect(c.data, c.declared, c.limits)
			if !errors.Is(err, c.err) || kind != c.kind {
				t.Fatalf("kind=%q err=%v want kind=%q err=%v", kind, err, c.kind, c.err)
			}
		})
	}
}

func TestExtractFailsExplicitlyForUnparsedKinds(t *testing.T) {
	t.Parallel()
	if got, err := Extract(t.Context(), []byte("plain"), KindText, DefaultLimits); err != nil || got != "plain" {
		t.Fatalf("text: %q %v", got, err)
	}
	// A PDF without a body is refused as unreadable by the PDF child (WIS-624), never given
	// fabricated text.
	if got, err := Extract(t.Context(), []byte("%PDF-1.7\n%%EOF"), KindPDF, DefaultLimits); got != "" || !errors.Is(err, ErrUnreadableSource) {
		t.Fatalf("pdf must fail explicitly, got %q %v", got, err)
	}
}

// Spellings that a byte scan for targetmode="external" missed. Each must be refused.
func TestInspectRejectsHiddenExternalRelationships(t *testing.T) {
	t.Parallel()
	rels := func(name, body string) []byte {
		return docx(t, map[string][]byte{name: []byte(body)})
	}
	const part = "word/_rels/document.xml.rels"
	for name, data := range map[string][]byte{
		"single quotes":              rels(part, `<Relationships><Relationship Id="r1" Target="http://x.invalid" TargetMode='External'/></Relationships>`),
		"spaces around equals":       rels(part, `<Relationships><Relationship Id="r1" Target="http://x.invalid" TargetMode = "External"/></Relationships>`),
		"character reference":        rels(part, `<Relationships><Relationship Id="r1" Target="http://x.invalid" TargetMode="&#69;xternal"/></Relationships>`),
		"upper-case part name":       rels("word/_rels/document.xml.RELS", `<Relationships><Relationship Id="r1" Target="http://x.invalid" TargetMode="External"/></Relationships>`),
		"scheme without target mode": rels(part, `<Relationships><Relationship Id="r1" Target="file:///etc/passwd"/></Relationships>`),
		"UNC target":                 rels(part, `<Relationships><Relationship Id="r1" Target="\\host\share\x"/></Relationships>`),
		"protocol-relative target":   rels(part, `<Relationships><Relationship Id="r1" Target="//host/x"/></Relationships>`),
		"unparseable relationships":  rels(part, `<Relationships><Relationship`),
		"non-UTF-8 declaration":      rels(part, `<?xml version="1.0" encoding="UTF-16"?><Relationships/>`),
		"macro project":              docx(t, map[string][]byte{"word/vbaProject.bin": []byte("x")}),
	} {
		if kind, err := Inspect(data, docxType, DefaultLimits); !errors.Is(err, ErrUnreadableSource) {
			t.Errorf("%s: kind=%q err=%v", name, kind, err)
		}
	}
	// Ordinary internal relationships and an explicit Internal mode still pass.
	ok := rels(part, `<Relationships><Relationship Id="r1" Target="styles.xml"/><Relationship Id="r2" Target="media/image1.png" TargetMode="Internal"/></Relationships>`)
	if kind, err := Inspect(ok, docxType, DefaultLimits); err != nil || kind != KindDOCX {
		t.Fatalf("internal relationships: kind=%q err=%v", kind, err)
	}
}

func TestInspectDecodesPDFNameEscapes(t *testing.T) {
	t.Parallel()
	escaped := []byte("%PDF-1.7\ntrailer<</Encr#79pt 5 0 R>>\n%%EOF")
	if _, err := Inspect(escaped, "application/pdf", DefaultLimits); !errors.Is(err, ErrPDFEncrypted) {
		t.Fatalf("escaped /Encrypt accepted: %v", err)
	}
	plain := []byte("%PDF-1.7\n1 0 obj<</Name /A#20B>>\n%%EOF")
	if kind, err := Inspect(plain, "application/pdf", DefaultLimits); err != nil || kind != KindPDF {
		t.Fatalf("escaped ordinary name: kind=%q err=%v", kind, err)
	}
}

// Audit gap (WIS-613): the existing bomb case only reaches the honest-header check. A header that
// declares a few bytes but inflates to megabytes is refused in both inspection and extraction:
// archive/zip stops reading past an entry's declared size (a corrupt archive, UNREADABLE_SOURCE),
// so inflation is bounded by the declared sizes, whose total the budget already caps.
func TestDecompressionBudgetHoldsAgainstLyingHeaders(t *testing.T) {
	t.Parallel()
	var deflated bytes.Buffer
	fw, err := flate.NewWriter(&deflated, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(bytes.Repeat([]byte("<w:p/>"), 1<<19)) // 3 MiB inflated
	fw.Close()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string][]byte{"[Content_Types].xml": []byte("<Types/>")} {
		w, _ := zw.Create(name)
		w.Write(body)
	}
	w, err := zw.CreateRaw(&zip.FileHeader{Name: "word/document.xml", Method: zip.Deflate, CompressedSize64: uint64(deflated.Len()), UncompressedSize64: 10, CRC32: 0})
	if err != nil {
		t.Fatal(err)
	}
	w.Write(deflated.Bytes())
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	small := DefaultLimits
	small.MaxDecompressedBytes = 1 << 20
	if _, err = Inspect(buf.Bytes(), docxType, small); !errors.Is(err, ErrUnreadableSource) {
		t.Fatalf("inspect a lying header: %v", err)
	}
	if _, err = Extract(t.Context(), buf.Bytes(), KindDOCX, small); !errors.Is(err, ErrUnreadableSource) {
		t.Fatalf("extract a lying header: %v", err)
	}
}

// An encrypted Office document is an OLE compound file, not a ZIP package; it is refused and never
// parsed (today by the binary-content check, pinned here so the refusal cannot silently lapse).
func TestEncryptedOfficeDocumentIsRefused(t *testing.T) {
	t.Parallel()
	cfb := append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, make([]byte, 512)...)
	for _, declared := range []string{docxType, xlsxType, ""} {
		if kind, err := Inspect(cfb, declared, DefaultLimits); err == nil || kind != "" {
			t.Fatalf("encrypted OOXML declared %q was accepted as %q", declared, kind)
		}
	}
}
