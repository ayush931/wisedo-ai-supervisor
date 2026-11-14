package onboarding

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
)

const xlsxType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

func word(body string) string {
	return `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body + `</w:body></w:document>`
}

func workbook(t *testing.T, sheets map[string]string, shared string, extra map[string][]byte) []byte {
	t.Helper()
	files := map[string][]byte{
		"[Content_Types].xml": []byte("<Types/>"),
		"xl/workbook.xml": []byte(`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>` +
			`<sheet name="Routing" sheetId="1" r:id="rId1"/><sheet name="Suppliers" sheetId="2" r:id="rId2"/></sheets></workbook>`),
		"xl/_rels/workbook.xml.rels": []byte(`<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Target="/xl/worksheets/sheet2.xml"/></Relationships>`),
	}
	if shared != "" {
		files["xl/sharedStrings.xml"] = []byte(`<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` + shared + `</sst>`)
	}
	for name, rows := range sheets {
		files[name] = []byte(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` + rows + `</sheetData></worksheet>`)
	}
	for k, v := range extra {
		files[k] = v
	}
	return makeZip(t, files)
}

// TestExtractDocuments (WIS-613): real Word and Excel structure becomes the text a reviewer would
// read, with values exactly as stored, and unsafe or ambiguous packages are refused.
func TestExtractDocuments(t *testing.T) {
	t.Parallel()
	doc := docx(t, map[string][]byte{"word/document.xml": []byte(word(
		`<w:p><w:r><w:t>Receiving SOP</w:t></w:r></w:p>` +
			`<w:p><w:r><w:t xml:space="preserve">Coil is checked at </w:t></w:r><w:r><w:t>Gate 2.</w:t></w:r></w:p>` +
			`<w:p/>` +
			`<w:tbl><w:tr><w:tc><w:p><w:r><w:t>Step</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Cycle (s)</w:t></w:r></w:p></w:tc></w:tr>` +
			`<w:tr><w:tc><w:p><w:r><w:t>Press</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>42</w:t></w:r></w:p></w:tc></w:tr></w:tbl>` +
			`<w:p><w:r><w:t>Ship to Apex</w:t><w:tab/><w:t>daily</w:t></w:r></w:p>`))})
	kind, err := Inspect(doc, docxType, DefaultLimits)
	if err != nil || kind != KindDOCX {
		t.Fatalf("docx inspect: %q %v", kind, err)
	}
	got, err := Extract(t.Context(), doc, kind, DefaultLimits)
	want := "Receiving SOP\nCoil is checked at Gate 2.\n\nStep\tCycle (s)\nPress\t42\n\nShip to Apex\tdaily\n"
	if err != nil || got != want {
		t.Fatalf("docx text:\n%q\nwant\n%q (%v)", got, want, err)
	}

	book := workbook(t, map[string]string{
		"xl/worksheets/sheet1.xml": `<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>` +
			`<row r="2"><c r="A2" t="inlineStr"><is><t>Press</t></is></c><c r="B2"><v>42.5</v></c></row>` +
			`<row r="3"><c r="A3" t="str"><f>A2&amp;"!"</f><v>Press!</v></c><c r="B3"><f>B2*2</f><v>85</v></c></row>` +
			`<row r="4"><c r="A4"/></row>`,
		"xl/worksheets/sheet2.xml": `<row r="1"><c r="A1" t="s"><v>2</v></c><c r="B1"><v>45658</v></c></row>`,
	}, `<si><t>Operation</t></si><si><r><t>Cycle </t></r><r><t>(s)</t></r></si><si><t>Bharat Steel</t></si>`, nil)
	if kind, err = Inspect(book, xlsxType, DefaultLimits); err != nil || kind != KindXLSX {
		t.Fatalf("xlsx inspect: %q %v", kind, err)
	}
	got, err = Extract(t.Context(), book, kind, DefaultLimits)
	want = "# Routing\nOperation\tCycle (s)\nPress\t42.5\nPress!\t85\n\n# Suppliers\nBharat Steel\t45658\n" // a date stays its serial number
	if err != nil || got != want {
		t.Fatalf("xlsx text:\n%q\nwant\n%q (%v)", got, want, err)
	}

	csv := []byte("supplier,material,lead_days\r\nBharat Steel,Steel coil,14\r\n")
	if kind, err = Inspect(csv, "text/csv", DefaultLimits); err != nil || kind != KindText {
		t.Fatalf("csv: %q %v", kind, err)
	}

	for name, c := range map[string]struct {
		data     []byte
		declared string
		err      error
	}{
		"macro workbook":         {workbook(t, nil, "", map[string][]byte{"xl/vbaProject.bin": []byte("x")}), xlsxType, ErrUnreadableSource},
		"external workbook link": {workbook(t, nil, "", map[string][]byte{"xl/_rels/workbook.xml.rels": []byte(`<Relationships><Relationship Id="r9" Target="http://x.invalid/data" TargetMode="External"/></Relationships>`)}), xlsxType, ErrUnreadableSource},
		"word declared as excel": {doc, xlsxType, ErrUnsupportedMedia},
		"both document and book": {workbook(t, nil, "", map[string][]byte{"word/document.xml": []byte(word(""))}), "", ErrUnsupportedMedia},
		"executable as csv":      {append([]byte("MZ"), make([]byte, 64)...), "text/csv", ErrUnsupportedMedia},
	} {
		if _, err := Inspect(c.data, c.declared, DefaultLimits); !errors.Is(err, c.err) {
			t.Errorf("%s: %v, want %v", name, err, c.err)
		}
	}

	// A shared-string index outside the table is corrupt input, not a blank cell.
	broken := workbook(t, map[string]string{"xl/worksheets/sheet1.xml": `<row><c t="s"><v>7</v></c></row>`, "xl/worksheets/sheet2.xml": ``}, `<si><t>only</t></si>`, nil)
	if _, err := Extract(t.Context(), broken, KindXLSX, DefaultLimits); !errors.Is(err, ErrUnreadableSource) {
		t.Fatalf("bad shared index: %v", err)
	}
	// Extracted text over the bound is refused, not truncated.
	small := DefaultLimits
	small.MaxExtractedText = 20
	if _, err := Extract(t.Context(), doc, KindDOCX, small); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized text: %v", err)
	}
}

// TestExtractRefusesAmplification (WIS-613): a small package cannot make extraction do or hold far
// more than its bounds allow, whether by repeating one large shared string across a row or by
// listing the same worksheet many times.
func TestExtractRefusesAmplification(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 1<<20)
	row := strings.Repeat(`<c t="s"><v>0</v></c>`, 2000)
	fanout := workbook(t, map[string]string{"xl/worksheets/sheet1.xml": `<row>` + row + `</row>`, "xl/worksheets/sheet2.xml": ``}, `<si><t>`+big+`</t></si>`, nil)
	if _, err := Extract(t.Context(), fanout, KindXLSX, DefaultLimits); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("shared-string fan-out: %v", err)
	}

	sheets := strings.Repeat(`<sheet name="s" r:id="rId1"/>`, 3)
	repeated := workbook(t, map[string]string{"xl/worksheets/sheet1.xml": `<row><c><v>1</v></c></row>`}, "", map[string][]byte{
		"xl/workbook.xml": []byte(`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>` + sheets + `</sheets></workbook>`),
	})
	if _, err := Extract(t.Context(), repeated, KindXLSX, DefaultLimits); !errors.Is(err, ErrUnreadableSource) {
		t.Fatalf("repeated worksheet: %v", err)
	}

	// Every part read from one package shares one decompression budget.
	tight := DefaultLimits
	tight.MaxDecompressedBytes = 600
	book := workbook(t, map[string]string{
		"xl/worksheets/sheet1.xml": `<row><c><v>` + strings.Repeat("1", 200) + `</v></c></row>`,
		"xl/worksheets/sheet2.xml": `<row><c><v>` + strings.Repeat("2", 200) + `</v></c></row>`,
	}, "", nil)
	if _, err := Extract(t.Context(), book, KindXLSX, tight); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("shared budget: %v", err)
	}

	escape := workbook(t, map[string]string{"xl/worksheets/sheet2.xml": ``}, "", map[string][]byte{
		"xl/_rels/workbook.xml.rels": []byte(`<Relationships><Relationship Id="rId1" Target="../word/document.xml"/><Relationship Id="rId2" Target="/xl/worksheets/sheet2.xml"/></Relationships>`),
		"word/document.xml":          []byte(word(`<w:p><w:r><w:t>not a sheet</w:t></w:r></w:p>`)),
	})
	if _, err := Extract(t.Context(), escape, KindXLSX, DefaultLimits); !errors.Is(err, ErrUnreadableSource) {
		t.Fatalf("target outside xl/: %v", err)
	}
}

// TestExtractKeepsLayout (WIS-613): an empty Excel cell keeps its column, a line break inside a cell
// stays on its row, and Word text-box content is read once, not again from its fallback copy.
func TestExtractKeepsLayout(t *testing.T) {
	t.Parallel()
	book := workbook(t, map[string]string{
		"xl/worksheets/sheet1.xml": `<row r="1"><c r="A1"><v>a</v></c><c r="C1"><v>c</v></c></row>` +
			`<row r="2"><c r="B2" t="inlineStr"><is><t>two
lines</t></is></c></row>`,
		"xl/worksheets/sheet2.xml": ``,
	}, "", nil)
	got, err := Extract(t.Context(), book, KindXLSX, DefaultLimits)
	if want := "# Routing\na\t\tc\n\ttwo lines\n\n# Suppliers\n"; err != nil || got != want {
		t.Fatalf("sparse cells:\n%q\nwant\n%q (%v)", got, want, err)
	}
	backwards := workbook(t, map[string]string{"xl/worksheets/sheet1.xml": `<row><c r="C1"><v>1</v></c><c r="A1"><v>2</v></c></row>`, "xl/worksheets/sheet2.xml": ``}, "", nil)
	if _, err := Extract(t.Context(), backwards, KindXLSX, DefaultLimits); !errors.Is(err, ErrUnreadableSource) {
		t.Fatalf("backwards reference: %v", err)
	}

	box := `<w:p><w:r><w:t>Outer </w:t></w:r><mc:AlternateContent xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006">` +
		`<mc:Choice><w:txbxContent><w:p><w:r><w:t>In box</w:t></w:r></w:p></w:txbxContent></mc:Choice>` +
		`<mc:Fallback><w:txbxContent><w:p><w:r><w:t>In box</w:t></w:r></w:p></w:txbxContent></mc:Fallback>` +
		`</mc:AlternateContent><w:r><w:t>text</w:t></w:r></w:p>`
	doc := docx(t, map[string][]byte{"word/document.xml": []byte(word(box))})
	got, err = Extract(t.Context(), doc, KindDOCX, DefaultLimits)
	if want := "In box\nOuter text\n"; err != nil || got != want {
		t.Fatalf("text box:\n%q\nwant\n%q (%v)", got, want, err)
	}
}

// TestDocumentSpans (WIS-613): consecutive lines are packed into passages of at most
// MaxDocumentSpanRunes, a line is split only when it alone exceeds the bound, passages carry no
// surrounding whitespace, and the same text always gives the same spans.
func TestDocumentSpans(t *testing.T) {
	t.Parallel()
	passages := func(text string) []string {
		runes := []rune(text)
		var out []string
		for _, s := range DocumentSpans(text) {
			out = append(out, string(runes[s.Start:s.End]))
		}
		return out
	}
	text := "  Receiving SOP\nCoil checked at Gate 2.\n\n\n# Routing\nPress\t42\n"
	if got, want := passages(text), []string{"Receiving SOP\nCoil checked at Gate 2.\n\n\n# Routing\nPress\t42"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("spans %q, want %q", got, want)
	}
	if a, b := DocumentSpans(text), DocumentSpans(text); len(a) != 1 || len(b) != 1 || a[0] != b[0] {
		t.Fatal("spans are not deterministic")
	}

	// A passage closes before the line that would take it over the bound; that line starts the next.
	line := strings.Repeat("ü", 999)
	got := passages(line + "\n" + line + "\n" + line + "\n")
	if len(got) != 2 || got[0] != line+"\n"+line || got[1] != line {
		t.Fatalf("packing at the bound: %d passages", len(got))
	}
	// Only a line longer than the bound is split, at the bound, in runes; its remainder packs with
	// the lines after it.
	long := strings.Repeat("é", MaxDocumentSpanRunes+5)
	if got = passages("intro\n" + long + "\nend"); len(got) != 3 || got[0] != "intro" || got[1] != long[:2*MaxDocumentSpanRunes] || got[2] != "ééééé\nend" {
		t.Fatalf("long line: %d passages", len(got))
	}

	for _, blank := range []string{"", "\n \n\t\n", "\r\n"} {
		if DocumentSpans(blank) != nil {
			t.Fatalf("blank text %q has spans", blank)
		}
	}
}

// TestDocumentSpanCountIsBounded (WIS-613): whatever the line layout, the passages of a full evidence
// source stay within evidence.MaxDocumentSpans, so one upload is one bounded transaction.
func TestDocumentSpanCountIsBounded(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"one character per paragraph": strings.Repeat("a\n\n", evidence.MaxTextBytes/3),
		"lines just over half a span": strings.Repeat(strings.Repeat("x", MaxDocumentSpanRunes/2+1)+"\n", evidence.MaxTextBytes/(MaxDocumentSpanRunes/2+2)),
		"one unbroken line":           strings.Repeat("y", evidence.MaxTextBytes),
	} {
		spans := DocumentSpans(text)
		if len(spans) == 0 || len(spans) > evidence.MaxDocumentSpans {
			t.Fatalf("%s: %d spans, bound %d", name, len(spans), evidence.MaxDocumentSpans)
		}
		runes := []rune(text)
		for i, s := range spans {
			if s.End-s.Start < 1 || s.End-s.Start > MaxDocumentSpanRunes || (i > 0 && s.Start < spans[i-1].End) {
				t.Fatalf("%s: span %d %+v is empty, oversized or overlapping", name, i, s)
			}
			if p := string(runes[s.Start:s.End]); strings.TrimSpace(p) != p || !utf8.ValidString(p) {
				t.Fatalf("%s: span %d is not a clean passage", name, i)
			}
		}
	}
}

// TestUploadDocumentRefusesBeforeWriting (WIS-613): every refusal is decided from the bytes alone,
// in prepareDocument, which has no evidence service to write to, so a refused upload writes nothing.
func TestUploadDocumentRefusesBeforeWriting(t *testing.T) {
	t.Parallel()
	macro := workbook(t, nil, "", map[string][]byte{"xl/vbaProject.bin": []byte("x")})
	external := docx(t, map[string][]byte{"word/_rels/document.xml.rels": []byte(`<Relationship Target="http://x.invalid" TargetMode="External"/>`)})
	empty := docx(t, map[string][]byte{"word/document.xml": []byte(word(`<w:p/><w:p><w:r><w:t xml:space="preserve">   </w:t></w:r></w:p>`))})
	for name, c := range map[string]struct {
		data     []byte
		declared string
		err      error
	}{
		"pdf without a readable body": {[]byte("%PDF-1.7\n1 0 obj\n%%EOF"), "application/pdf", ErrUnreadableSource},
		"executable declared as word": {append([]byte("MZ"), make([]byte, 64)...), docxType, ErrUnsupportedMedia},
		"text declared as word":       {[]byte("Step 1"), docxType, ErrUnsupportedMedia},
		"word declared as csv":        {docx(t, nil), "text/csv", ErrUnsupportedMedia},
		"macro-enabled workbook":      {macro, xlsxType, ErrUnreadableSource},
		"externally linked document":  {external, docxType, ErrUnreadableSource},
		"document with no text":       {empty, docxType, ErrUnreadableSource},
		"blank text file":             {[]byte(" \r\n\t\n"), "text/plain", ErrUnreadableSource},
		"text larger than one source": {[]byte(strings.Repeat("row,1\n", evidence.MaxTextBytes/6+1)), "text/csv", ErrTooLarge},
	} {
		_, err := prepareDocument(t.Context(), DocumentUpload{Title: "t", DeclaredType: c.declared, Data: c.data})
		if !errors.Is(err, c.err) {
			t.Errorf("%s: %v, want %v", name, err, c.err)
		}
	}
}

// Audit defect (WIS-613 follow-up): every nested <w:p> copied the open paragraph out and back,
// so one long run followed by many empty nested paragraphs inside a table (which write no output
// and so never meet the output cap) cost a copy of up to MaxExtractedText each: a 4 KB upload ran
// for seconds of CPU, and the 100 MiB budget allowed minutes. Opening and closing a paragraph
// must cost the same whatever the open text holds. A 1 MiB run and 2000 nested paragraphs copied
// about 2 GB before the fix; the bound here is far below that and far above the honest cost.
func TestExtractNestedParagraphsCostNoCopies(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString(`<w:tbl><w:tr><w:tc><w:p><w:r><w:t>`)
	b.WriteString(strings.Repeat("x", 1<<20))
	b.WriteString(`</w:t></w:r>`)
	for range 2000 {
		b.WriteString(`<w:p/>`)
	}
	b.WriteString(`</w:p></w:tc></w:tr></w:tbl>`)
	doc := docx(t, map[string][]byte{"word/document.xml": []byte(word(b.String()))})
	limits := DefaultLimits
	limits.MaxExtractedText = 2 << 20
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := Extract(t.Context(), doc, KindDOCX, limits)
	runtime.ReadMemStats(&after)
	if err != nil || len(got) != 1<<20+2 { // the cell text, then the table's closing blank line
		t.Fatalf("extract: %d bytes, %v", len(got), err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 256<<20 {
		t.Fatalf("extraction allocated %d MiB for 2000 nested paragraphs", alloc>>20)
	}
}

// Paragraphs nested deeper than any real text box are refused, not walked.
func TestExtractRefusesDeepParagraphNesting(t *testing.T) {
	t.Parallel()
	deep := strings.Repeat(`<w:p>`, maxParagraphDepth+1) + strings.Repeat(`</w:p>`, maxParagraphDepth+1)
	ok := strings.Repeat(`<w:p>`, maxParagraphDepth) + `<w:r><w:t>x</w:t></w:r>` + strings.Repeat(`</w:p>`, maxParagraphDepth)
	if _, err := Extract(t.Context(), docx(t, map[string][]byte{"word/document.xml": []byte(word(deep))}), KindDOCX, DefaultLimits); !errors.Is(err, ErrUnreadableSource) {
		t.Fatalf("over-deep nesting: %v", err)
	}
	if got, err := Extract(t.Context(), docx(t, map[string][]byte{"word/document.xml": []byte(word(ok))}), KindDOCX, DefaultLimits); err != nil || !strings.Contains(got, "x") {
		t.Fatalf("nesting at the bound: %q %v", got, err)
	}
}

// Audit defects (WIS-613): an Excel phonetic guide (<rPh>) was read as cell text, so 漢字 with its
// reading became "漢字カンジ"; and a tab or line break written inside a Word table cell's text split
// the row or shifted its columns, though XLSX cells already normalize them.
func TestExtractDropsPhoneticGuidesAndKeepsWordCellsWhole(t *testing.T) {
	t.Parallel()
	book := workbook(t, map[string]string{
		"xl/worksheets/sheet1.xml": `<row><c r="A1" t="s"><v>0</v></c><c r="B1" t="inlineStr"><is><t>鋼</t><rPh sb="0" eb="1"><t>ハガネ</t></rPh></is></c></row>`,
		"xl/worksheets/sheet2.xml": ``,
	}, `<si><t>漢字</t><rPh sb="0" eb="2"><t>カンジ</t></rPh></si>`, nil)
	got, err := Extract(t.Context(), book, KindXLSX, DefaultLimits)
	if want := "# Routing\n漢字\t鋼\n\n# Suppliers\n"; err != nil || got != want {
		t.Fatalf("phonetic guide:\n%q\nwant\n%q (%v)", got, want, err)
	}
	table := `<w:tbl><w:tr><w:tc><w:p><w:r><w:t xml:space="preserve">Press	line
one</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>42 s</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`
	got, err = Extract(t.Context(), docx(t, map[string][]byte{"word/document.xml": []byte(word(table))}), KindDOCX, DefaultLimits)
	if want := "Press line one\t42 s\n\n"; err != nil || got != want {
		t.Fatalf("word cell:\n%q\nwant\n%q (%v)", got, want, err)
	}
}
