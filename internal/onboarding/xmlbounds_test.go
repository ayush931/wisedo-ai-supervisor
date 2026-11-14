package onboarding

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// allocated reports the bytes f allocates, so a test can tell a bounded parse from one whose work
// grows with the attacker's input without reading the wall clock (rule 10).
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// allocBound is far under what the unbounded code allocated (265+ MiB for the 4 MiB deep parts, 18
// GiB for the padded sheet) and leaves room for -race instrumentation in CI. Fixtures are built
// before measuring, so only the parse is counted.
const allocBound = 64 << 20

// TestWIS694DeepXMLIsRefusedWithinABound (security audit run 1, finding 1): a deeply nested part
// is refused once nesting passes maxXMLDepth, before memory grows with it, on every token loop
// over an uploaded part, Skip included. A part nested just under the bound is still read.
func TestWIS694DeepXMLIsRefusedWithinABound(t *testing.T) {
	deep := strings.Repeat("<a>", 4<<20/3)
	nested := func(depth int) string { return strings.Repeat("<a>", depth) + strings.Repeat("</a>", depth) }
	const mcNSDecl = `xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006"`
	inspect := func(files map[string][]byte) func() error {
		data := docx(t, files)
		return func() error { _, err := Inspect(data, docxType, DefaultLimits); return err }
	}
	extractDOCX := func(body string) func() error {
		data := docx(t, map[string][]byte{"word/document.xml": []byte(word(body))})
		return func() error { _, err := Extract(context.Background(), data, KindDOCX, DefaultLimits); return err }
	}
	extractXLSX := func(sheet, shared string) func() error {
		data := workbook(t, map[string]string{"xl/worksheets/sheet1.xml": sheet, "xl/worksheets/sheet2.xml": ""}, shared, nil)
		return func() error { _, err := Extract(context.Background(), data, KindXLSX, DefaultLimits); return err }
	}
	for _, c := range []struct {
		name string
		run  func() error
		want error
	}{
		{"a nested relationship part", inspect(map[string][]byte{"_rels/.rels": []byte(deep)}), ErrUnreadableSource},
		{"nested content types", inspect(map[string][]byte{"[Content_Types].xml": []byte("<Types>" + deep)}), ErrUnreadableSource},
		{"a nested document body", extractDOCX(deep), ErrUnreadableSource},
		{"nesting inside a skipped fallback", extractDOCX(`<mc:AlternateContent ` + mcNSDecl + `><mc:Fallback>` + deep), ErrUnreadableSource},
		{"nested shared strings", extractXLSX(`<row r="1"><c r="A1"><v>1</v></c></row>`, deep), ErrUnreadableSource},
		{"a nested worksheet", extractXLSX(deep, ""), ErrUnreadableSource},
		// Controls: nesting under the bound is read as before.
		{"a relationship part nested 60 deep", inspect(map[string][]byte{"_rels/.rels": []byte(nested(60))}), nil},
		{"a document body nested 60 deep", extractDOCX(nested(60)), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var err error
			n := allocated(func() { err = c.run() })
			if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if n > allocBound {
				t.Fatalf("allocated %d MiB, want under %d MiB", n>>20, allocBound>>20)
			}
		})
	}
}

// TestWIS694SharedStringCountIsBounded (security audit run 1, finding 1): every empty "<si/>" was
// kept, so 16 MiB of them held 200 MiB. A table over maxSharedStrings entries is refused; one at
// the bound is read.
func TestWIS694SharedStringCountIsBounded(t *testing.T) {
	sheets := map[string]string{"xl/worksheets/sheet1.xml": `<row r="1"><c r="A1" t="s"><v>0</v></c></row>`, "xl/worksheets/sheet2.xml": ""}
	over := workbook(t, sheets, strings.Repeat("<si/>", maxSharedStrings+1), nil)
	if _, err := Extract(context.Background(), over, KindXLSX, DefaultLimits); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the bound: %v, want %v", err, ErrTooLarge)
	}
	at := workbook(t, sheets, strings.Repeat("<si/>", maxSharedStrings), nil)
	if _, err := Extract(context.Background(), at, KindXLSX, DefaultLimits); err != nil {
		t.Fatalf("at the bound: %v", err)
	}
}

// TestWIS694ColumnPaddingIsCharged (security audit run 1, finding 2): a reference such as "XFD1"
// padded 16383 empty cells per row that no budget counted, a second of CPU per few thousand rows.
// Empty cells are now written only before a later cell with text, so a sheet of empty far
// references costs what a near one does and extracts the same; far references with text are
// charged to the output budget; empty cells between values keep their columns.
func TestWIS694ColumnPaddingIsCharged(t *testing.T) {
	book := func(rows string) []byte {
		return workbook(t, map[string]string{"xl/worksheets/sheet1.xml": rows, "xl/worksheets/sheet2.xml": ""}, "", nil)
	}
	extract := func(rows string) (string, error) {
		return Extract(context.Background(), book(rows), KindXLSX, DefaultLimits)
	}
	var far, near string
	var farErr, nearErr error
	farBook := book(strings.Repeat(`<row><c r="XFD1"/></row>`, 16000))
	n := allocated(func() { far, farErr = Extract(context.Background(), farBook, KindXLSX, DefaultLimits) })
	near, nearErr = extract(strings.Repeat(`<row><c r="A1"/></row>`, 16000))
	if farErr != nil || nearErr != nil || far != near {
		t.Fatalf("empty far cells: %q %v, want the near result %q %v", far, farErr, near, nearErr)
	}
	if n > allocBound {
		t.Fatalf("empty far cells allocated %d MiB, want under %d MiB", n>>20, allocBound>>20)
	}
	var err error
	withText := book(strings.Repeat(`<row><c r="XFD1" t="inlineStr"><is><t>x</t></is></c></row>`, 16000))
	n = allocated(func() { _, err = Extract(context.Background(), withText, KindXLSX, DefaultLimits) })
	if !errors.Is(err, ErrTooLarge) || n > allocBound {
		t.Fatalf("far cells with text: %v after %d MiB, want %v within %d MiB", err, n>>20, ErrTooLarge, allocBound>>20)
	}
	got, err := extract(`<row><c r="A1"><v>1</v></c><c r="C1"><v>3</v></c><c r="F1"/></row><row><c r="B2"><v>b</v></c></row>`)
	if err != nil || !strings.Contains(got, "1\t\t3\n\tb\n") {
		t.Fatalf("columns between values: %q %v", got, err)
	}
}
