package onboarding

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// TestMain lets this test binary act as the PDF child (WIS-624): extractPDF re-executes the
// running binary with PDFChildFlag, exactly as cmd/supervisor does in production.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		if probe := testChildProbes[os.Args[1]]; probe != nil {
			os.Exit(probe())
		}
	}
	RunPDFChildIfRequested()
	// The PDF tests run in parallel, one child each: allow as many children as tests run at once,
	// or a test would wait past pdfSlotWait and fail EXTRACTION_BUSY. The slot tests run serially
	// and fill whatever capacity this is (cap(pdfSlots)).
	flag.Parse()
	if n := flag.Lookup("test.parallel").Value.(flag.Getter).Get().(int); n > cap(pdfSlots) {
		pdfSlots = make(chan struct{}, n)
	}
	os.Exit(m.Run())
}

// testChildProbes are test-only child modes (they never ship), keyed by argv[1].
var testChildProbes = map[string]func() int{}

// requirePDFSandbox reports BLOCKED (rule 09) where the PDF child cannot confine itself: there
// every PDF is EXTRACTION_UNAVAILABLE by design, so the PDF tests cannot run.
func requirePDFSandbox(t *testing.T) {
	t.Helper()
	if !PDFSandboxSupported {
		t.Fatalf("BLOCKED: the PDF parser sandbox needs linux/amd64, this is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

// pdfDoc builds a small, reviewable PDF: a catalog (plus catalogExtra), one page per content
// stream (plus pageExtra on every page), Helvetica as F1, and any extra objects numbered after
// them. Offsets in the cross-reference table are computed, so the file is well formed.
type pdfDoc struct {
	pages        []string // content streams; "" is a page with no content
	catalogExtra string
	pageExtra    string
	resources    string // extra entries inside each page's /Resources
	extra        []string
	compress     map[int]bool // page index -> Flate-compress its content
}

func (d pdfDoc) bytes() []byte {
	n := len(d.pages)
	// 1 catalog, 2 pages, 3 font, then per page: page object and content stream, then extras.
	objs := []string{
		fmt.Sprintf("<< /Type /Catalog /Pages 2 0 R %s >>", d.catalogExtra),
		"", // filled below
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	kids := []string{}
	for i, content := range d.pages {
		page := 4 + 2*i
		kids = append(kids, fmt.Sprintf("%d 0 R", page))
		objs = append(objs, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> %s >> /Contents %d 0 R %s >>", d.resources, page+1, d.pageExtra))
		body, filter := []byte(content), ""
		if d.compress[i] {
			var b bytes.Buffer
			w := zlib.NewWriter(&b)
			_, _ = w.Write(body)
			_ = w.Close()
			body, filter = b.Bytes(), " /Filter /FlateDecode"
		}
		objs = append(objs, fmt.Sprintf("<< /Length %d%s >>\nstream\n%s\nendstream", len(body), filter, body))
	}
	objs[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n)
	objs = append(objs, d.extra...)
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func textPage(lines ...string) string {
	var b strings.Builder
	b.WriteString("BT /F1 12 Tf 72 720 Td ")
	for i, l := range lines {
		if i > 0 {
			b.WriteString("0 -20 Td ")
		}
		fmt.Fprintf(&b, "(%s) Tj ", l)
	}
	b.WriteString("ET")
	return b.String()
}

const imagePage = "q 100 0 0 100 72 600 cm /Im1 Do Q"

// imageResources and imageObject give a page a 1x1 grey image XObject (object number set by
// the caller's position in extra).
func imageResources(obj int) string { return fmt.Sprintf("/XObject << /Im1 %d 0 R >>", obj) }

const imageObject = "<< /Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceGray /BitsPerComponent 8 /Length 1 >>\nstream\n\x80\nendstream"

// formObject is a form XObject with the given resource entries and content stream.
func formObject(resources, content string) string {
	return fmt.Sprintf("<< /Type /XObject /Subtype /Form /BBox [0 0 1 1] /Resources << %s >> /Length %d >>\nstream\n%s\nendstream", resources, len(content), content)
}

func encrypted(t *testing.T, plain []byte) []byte {
	t.Helper()
	model.ConfigPath = "disable"
	var out bytes.Buffer
	if err := api.Encrypt(bytes.NewReader(plain), &out, model.NewAESConfiguration("user-pw", "owner-pw", 256)); err != nil {
		t.Fatalf("encrypt fixture: %v", err)
	}
	return out.Bytes()
}

// TestPDFExtraction (WIS-624): a born-digital PDF is extracted in the child, page by page, one
// line per row; each refusal is the file's own reason, decided before anything is written.
func TestPDFExtraction(t *testing.T) {
	requirePDFSandbox(t)
	good := pdfDoc{pages: []string{
		textPage("Steel coil comes from Bharat Steel.", "Lead time is 14 days."),
		"",
		textPage("The press takes 42 seconds a part."),
	}}.bytes()
	text, err := Extract(t.Context(), good, KindPDF, DefaultLimits)
	if err != nil {
		t.Fatalf("good PDF: %v", err)
	}
	if want := "Steel coil comes from Bharat Steel.\nLead time is 14 days.\nThe press takes 42 seconds a part.\n"; text != want {
		t.Fatalf("good PDF text %q, want %q", text, want)
	}
	prepared, err := prepareDocument(t.Context(), DocumentUpload{Title: "SOP", DeclaredType: "application/pdf", Data: good})
	if err != nil || prepared.kind != KindPDF || len(prepared.spans) != 1 || prepared.text != text {
		t.Fatalf("prepared PDF: %+v %v", prepared, err)
	}

	// A large decompressed string: the child's memory watchdog ends it, whatever the platform.
	var bomb bytes.Buffer
	w, _ := zlib.NewWriterLevel(&bomb, flate.BestSpeed)
	_, _ = w.Write([]byte("BT /F1 12 Tf 72 720 Td ("))
	chunk := bytes.Repeat([]byte("A"), 1<<20)
	for i := 0; i < 1100; i++ {
		_, _ = w.Write(chunk)
	}
	_, _ = w.Write([]byte(") Tj ET"))
	_ = w.Close()
	bombDoc := pdfDoc{pages: []string{""}}.bytes()
	bombDoc = bytes.Replace(bombDoc, []byte("<< /Length 0 >>\nstream\n\nendstream"), append([]byte(fmt.Sprintf("<< /Length %d /Filter /FlateDecode >>\nstream\n", bomb.Len())), append(bomb.Bytes(), []byte("\nendstream")...)...), 1)
	bombDoc = rebuildXref(t, bombDoc)

	many := pdfDoc{pages: make([]string, MaxPDFPages+1)}
	for i := range many.pages {
		many.pages[i] = textPage("p")
	}

	for name, c := range map[string]struct {
		data []byte
		want error
	}{
		"encrypted (AES-256, by pdfcpu)": {encrypted(t, good), ErrPDFEncrypted},
		"JavaScript open action":         {pdfDoc{pages: []string{textPage("Hello")}, catalogExtra: "/OpenAction << /S /JavaScript /JS (app.alert(1)) >>"}.bytes(), ErrPDFActiveContent},
		"embedded file":                  {pdfDoc{pages: []string{textPage("Hello")}, catalogExtra: "/Names << /EmbeddedFiles << /Names [(a.txt) 6 0 R] >> >>", extra: []string{"<< /Type /Filespec /F (a.txt) /EF << /F 7 0 R >> >>", "<< /Type /EmbeddedFile /Length 1 >>\nstream\nx\nendstream"}}.bytes(), ErrPDFActiveContent},
		"launch action":                  {pdfDoc{pages: []string{textPage("Hello")}, pageExtra: "/AA << /O << /S /Launch /F (cmd.exe) >> >>"}.bytes(), ErrPDFActiveContent},
		"hyperlink (as DOCX)":            {pdfDoc{pages: []string{textPage("Hello")}, pageExtra: "/Annots [<< /Type /Annot /Subtype /Link /Rect [0 0 1 1] /A << /S /URI /URI (http://x.invalid) >> >>]"}.bytes(), ErrPDFExternalReference},
		"remote go-to":                   {pdfDoc{pages: []string{textPage("Hello")}, catalogExtra: "/OpenAction << /S /GoToR /F (other.pdf) /D [0 /Fit] >>"}.bytes(), ErrPDFExternalReference},
		"scanned (image only)":           {pdfDoc{pages: []string{imagePage}, resources: imageResources(6), extra: []string{imageObject}}.bytes(), ErrPDFNoTextLayer},
		// "one scanned page among text pages": since the WIS-625 post-merge decision its image goes to
		// the vision model, or is skipped and counted when tiny: TestPDFFiguresAreReadByTheVisionModel.
		"no text at all": {pdfDoc{pages: []string{"", ""}}.bytes(), ErrPDFNoTextLayer},
		// WIS-625 review (#252): a picture inside a form, or drawn inline, is still a picture; before
		// the fix such a page read as blank and the upload kept only the typed page's text.
		"a scan wrapped in a form among text pages": {pdfDoc{pages: []string{textPage("Typed page"), "q 612 0 0 792 0 0 cm /Fm1 Do Q"}, resources: "/XObject << /Fm1 8 0 R >>",
			extra: []string{formObject("/XObject << /Im1 9 0 R >>", "/Im1 Do"), imageObject}}.bytes(), ErrPDFNoTextLayer},
		"a form that draws itself": {pdfDoc{pages: []string{textPage("Typed page"), "/Fm1 Do"}, resources: "/XObject << /Fm1 8 0 R >>",
			extra: []string{formObject("/XObject << /Fm1 8 0 R >>", "/Fm1 Do")}}.bytes(), ErrPDFNoTextLayer},
		"an inline image among text pages": {pdfDoc{pages: []string{textPage("Typed page"), "q 100 0 0 100 72 600 cm BI /W 1 /H 1 /CS /G /BPC 8 ID \x80 EI Q"}}.bytes(), ErrPDFNoTextLayer},
		// Codex review of the third fix: an inline image written with delimiters only is still an
		// image, and content that cannot be read as plain vector drawing is never called blank.
		"a delimiter-separated inline image among text pages": {pdfDoc{pages: []string{textPage("Typed page"), "q 100 0 0 100 72 600 cm BI/W 1/H 1/CS/G/BPC 8 ID \x80 EI Q"}}.bytes(), ErrPDFNoTextLayer},
		"operators joined by U+0085 among text pages":         {pdfDoc{pages: []string{textPage("Typed page"), "q 1 0 0 1 0 0 cm\u0085Q"}}.bytes(), ErrPDFNoTextLayer},
		"a shading among text pages":                          {pdfDoc{pages: []string{textPage("Typed page"), "/Sh1 sh"}, resources: "/Shading << /Sh1 8 0 R >>", extra: []string{"<< /ShadingType 2 /ColorSpace /DeviceGray /Coords [0 0 1 0] /Function << /FunctionType 2 /Domain [0 1] /N 1 >> >>"}}.bytes(), ErrPDFNoTextLayer},
		"an unknown operator among text pages":                {pdfDoc{pages: []string{textPage("Typed page"), "q 1 0 0 1 0 0 cm Zz Q"}}.bytes(), ErrPDFNoTextLayer},
		// Codex round 3: lettering drawn as filled outlines, and a scan shown only by an annotation's
		// appearance stream, are not blank pages; before the fix both were dropped silently.
		"outlined lettering among text pages": {pdfDoc{pages: []string{textPage("Typed page"), "q 0 g 72 700 m 80 720 l 88 700 l h f 92 700 m 92 720 l 100 720 l 100 700 l h f Q"}}.bytes(), ErrPDFNoTextLayer},
		"a scan shown by an annotation among text pages": {pdfDoc{pages: []string{textPage("Typed page"), ""}, pageExtra: "/Annots [<< /Type /Annot /Subtype /Stamp /Rect [0 0 612 792] /AP << /N 8 0 R >> >>]",
			extra: []string{formObject("/XObject << /Im1 9 0 R >>", "q 612 0 0 792 0 0 cm /Im1 Do Q"), imageObject}}.bytes(), ErrPDFNoTextLayer},
		"too many pages": {many.bytes(), ErrPDFTooManyPages},
		// A page's content is read at most 1 MiB (contentTokens) before the extractor reads it
		// (WIS-625 post-merge), so a decompression bomb in it is refused without being inflated.
		// The memory ceiling itself is TestPDFChildConfinement's "alloc" case.
		"decompression bomb": {bombDoc, ErrPDFNoTextLayer},
		"not a PDF body":     {[]byte("%PDF-1.7\n%%EOF"), ErrUnreadableSource},
	} {
		t.Run(name, func(t *testing.T) {
			if errors.Is(c.want, ErrPDFParseBudget) {
				// The assertion below needs the memory watchdog to fire; on a host saturated by a full
				// parallel integration run the 20 s clock could otherwise end the child first.
				saved := pdfChildWallTime
				pdfChildWallTime = 2 * time.Minute
				t.Cleanup(func() { pdfChildWallTime = saved })
			}
			start := time.Now()
			got, err := Extract(t.Context(), c.data, KindPDF, DefaultLimits)
			if got != "" || !errors.Is(err, c.want) {
				t.Fatalf("got %q %v, want %v", got, err, c.want)
			}
			// The bomb must end on the child's memory budget (the watchdog, or the runtime failing an
			// allocation after the child started), not on the CPU or wall-clock limit.
			if errors.Is(c.want, ErrPDFParseBudget) && !strings.HasSuffix(err.Error(), "(memory)") && !strings.HasSuffix(err.Error(), "(child failure)") {
				t.Fatalf("budget refusal after %s was %q: not the memory watchdog", time.Since(start), err)
			}
		})
	}
}

// A text-less page whose content shows nothing (state, colour, a path ended with n or used as a
// clip) is blank and dropped; one that paints anything is not (Codex round 3, fail closed): a
// ruled-lines page now refuses the document with PDF_NO_TEXT_LAYER rather than being dropped.
func TestPDFExtractionBlankPageShowsNothing(t *testing.T) {
	t.Parallel()
	data := pdfDoc{pages: []string{textPage("Typed page"), "q 0 g 1 w 72 72 m 540 72 l n 0 0 612 792 re W n Q % nothing painted"}}.bytes()
	if got, err := Extract(t.Context(), data, KindPDF, DefaultLimits); err != nil || !strings.Contains(got, "Typed page") {
		t.Fatalf("blank page: got %q %v", got, err)
	}
	ruled := pdfDoc{pages: []string{textPage("Typed page"), "q 0 g 1 w 72 72 m 540 72 l S Q % a rule"}}.bytes()
	if got, err := Extract(t.Context(), ruled, KindPDF, DefaultLimits); got != "" || !errors.Is(err, ErrPDFNoTextLayer) {
		t.Fatalf("ruled page: got %q %v, want %v", got, err, ErrPDFNoTextLayer)
	}
}

// rebuildXref recomputes the cross-reference offsets after a fixture's bytes were edited.
func rebuildXref(t *testing.T, b []byte) []byte {
	t.Helper()
	body := b[:bytes.Index(b, []byte("xref\n"))]
	var offsets []int
	for i := 1; ; i++ {
		at := bytes.Index(body, []byte(fmt.Sprintf("\n%d 0 obj\n", i)))
		if at < 0 {
			break
		}
		offsets = append(offsets, at+1)
	}
	var out bytes.Buffer
	out.Write(body)
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return out.Bytes()
}

// The child slots bound concurrent parses; a full set is EXTRACTION_BUSY (retryable), not a hang.
func TestPDFExtractionBusyWhenSlotsTaken(t *testing.T) {
	for i := 0; i < cap(pdfSlots); i++ {
		pdfSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(pdfSlots); i++ {
			<-pdfSlots
		}
	}()
	start := time.Now()
	if _, err := extractPDF(t.Context(), []byte("%PDF-1.7\n%%EOF"), DefaultLimits); !errors.Is(err, ErrExtractionBusy) {
		t.Fatalf("busy: %v", err)
	}
	if waited := time.Since(start); waited < pdfSlotWait || waited > pdfSlotWait+time.Second {
		t.Fatalf("waited %s for a slot", waited)
	}
}

// WIS-624 post-merge review: a caller that gives up while waiting for a slot stops waiting at once
// with its own error. Before the fix the wait ignored the request and ended only after
// pdfSlotWait, as EXTRACTION_BUSY.
func TestPDFSlotWaitEndsWithTheCaller(t *testing.T) {
	for i := 0; i < cap(pdfSlots); i++ {
		pdfSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(pdfSlots); i++ {
			<-pdfSlots
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if _, err := extractPDF(ctx, []byte("%PDF-1.7\n%%EOF"), DefaultLimits); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait: %v, want context.Canceled", err)
	}
	if waited := time.Since(start); waited >= pdfSlotWait {
		t.Fatalf("waited %s for a slot after the caller gave up", waited)
	}
}

// A binary that is not child-capable (no hook) is EXTRACTION_UNAVAILABLE, never a guessed text.
func TestPDFExtractionUnavailableWithoutChild(t *testing.T) {
	defer func(old func() (string, error)) { pdfExecutable = old }(pdfExecutable)
	pdfExecutable = func() (string, error) { return "/usr/bin/false", nil }
	good := pdfDoc{pages: []string{textPage("Hello")}}.bytes()
	if got, err := extractPDF(t.Context(), good, DefaultLimits); got.text != "" || !errors.Is(err, ErrExtractionUnavailable) {
		t.Fatalf("no child: %q %v", got.text, err)
	}
}

// pdfCase is one extraction expectation: the stored text, or a refusal.
type pdfCase struct {
	doc  pdfDoc
	text string
	want error
}

func runPDFCases(t *testing.T, cases map[string]pdfCase) {
	t.Helper()
	requirePDFSandbox(t)
	for name, c := range cases {
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

const visibleLine = "BT /F1 12 Tf 72 300 Td (Visible line) Tj ET"

// WIS-625 post-merge review, finding 2: a page whose text was read but that also shows an image,
// a form (whose text the extractor never reads), an inline image or an annotation is incomplete.
// Before the fix only a page with no text was checked, so a scan with a typed page number, or a
// heading over instructions in a form, stored just the heading. It now refuses the file as
// PDF_NO_TEXT_LAYER. Painted paths (table rules) on a text page stay accepted.
func TestPDFTextPageShowsNothingUnread(t *testing.T) {
	t.Parallel()
	runPDFCases(t, map[string]pdfCase{
		// "a scanned page with a typed page number" now reads its image: TestPDFFiguresAreReadByTheVisionModel.
		"instructions inside a form under a heading": {doc: pdfDoc{pages: []string{visibleLine + " /Fm1 Do"}, resources: "/XObject << /Fm1 6 0 R >>",
			extra: []string{formObject("/Font << /F1 3 0 R >>", "BT /F1 12 Tf 0 0 Td (Instructions) Tj ET")}}, want: ErrPDFNoTextLayer},
		"an inline image under a heading": {doc: pdfDoc{pages: []string{visibleLine + " q 100 0 0 100 72 600 cm BI /W 1 /H 1 /CS /G /BPC 8 ID \x80 EI Q"}}, want: ErrPDFNoTextLayer},
		"a stamp on a typed page": {doc: pdfDoc{pages: []string{visibleLine}, pageExtra: "/Annots [<< /Type /Annot /Subtype /Stamp /Rect [0 0 612 792] /AP << /N 6 0 R >> >>]",
			extra: []string{formObject("/Font << /F1 3 0 R >>", "BT /F1 12 Tf 0 0 Td (Approved) Tj ET")}}, want: ErrPDFNoTextLayer},
		// Control: ruled lines and a cell border drawn after the text, clear of it, are not unread
		// text and cover nothing.
		"table rules on a typed page": {doc: pdfDoc{pages: []string{visibleLine + " q 1 w 66 290 m 540 290 l S 66 280 474 34 re S Q"}}, text: "Visible line\n"},
	})
}

// WIS-625 post-merge review, finding 1: text a viewer does not show is not evidence. Before the
// fix every decoded glyph was kept. Glyphs wholly outside the visible page (MediaBox within
// CropBox) are now dropped, and text drawn invisibly (render mode 3, or 7: clip only) refuses the
// file, since the extractor cannot tell which glyphs it covers. Overpainting, clipping paths and
// white-on-white text are not detected (see pageText).
func TestPDFTextPageHidesNothing(t *testing.T) {
	t.Parallel()
	runPDFCases(t, map[string]pdfCase{
		"text above the crop box":  {doc: pdfDoc{pages: []string{textPage("Hidden above the crop") + " " + visibleLine}, pageExtra: "/CropBox [0 0 612 400]"}, text: "Visible line\n"},
		"text off the media box":   {doc: pdfDoc{pages: []string{"BT /F1 12 Tf 700 300 Td (Off the page) Tj ET " + visibleLine}}, text: "Visible line\n"},
		"text below the media box": {doc: pdfDoc{pages: []string{"BT /F1 12 Tf 72 -40 Td (Under the page) Tj ET " + visibleLine}}, text: "Visible line\n"},
		// Codex round 2: text partly outside the visible page refuses (a reader sees only part of it);
		// text wholly outside it is dropped, as above.
		"text straddling the crop edge": {doc: pdfDoc{pages: []string{"BT /F1 12 Tf 72 295 Td (Edge) Tj ET"}, pageExtra: "/CropBox [0 300 612 792]"}, want: ErrPDFScanHiddenContent},
		"a malformed crop box":          {doc: pdfDoc{pages: []string{visibleLine}, pageExtra: "/CropBox [0 0 (a) 400]"}, want: ErrUnreadableSource},
		"invisible text (Tr 3)":         {doc: pdfDoc{pages: []string{visibleLine + " BT /F1 12 Tf 3 Tr 72 500 Td (Hidden claim) Tj ET"}}, want: ErrUnreadableSource},
		"clip-only text (Tr 7)":         {doc: pdfDoc{pages: []string{visibleLine + " BT /F1 12 Tf 7 Tr 72 500 Td (Hidden claim) Tj ET"}}, want: ErrUnreadableSource},
	})
}

// WIS-625 security review of the hidden-text checks: a render mode other than a plain 0-7
// integer, text inside optional content (a layer that can be off), and a font whose declared
// widths stretch an off-page glyph over the visible page must not become evidence.
func TestPDFHiddenTextBypasses(t *testing.T) {
	t.Parallel()
	ocg := "<< /Type /OCG /Name (Layer) >>"
	runPDFCases(t, map[string]pdfCase{
		"render mode 11":        {doc: pdfDoc{pages: []string{visibleLine + " BT /F1 12 Tf 11 Tr 72 500 Td (Hidden claim) Tj ET"}}, want: ErrUnreadableSource},
		"render mode 3.0":       {doc: pdfDoc{pages: []string{visibleLine + " BT /F1 12 Tf 3.0 Tr 72 500 Td (Hidden claim) Tj ET"}}, want: ErrUnreadableSource},
		"render mode 1 (shown)": {doc: pdfDoc{pages: []string{visibleLine + " BT /F1 12 Tf 1 Tr 72 500 Td (Outlined) Tj ET"}}, text: "Outlined\nVisible line\n"},
		"text in an optional-content section": {doc: pdfDoc{pages: []string{visibleLine + " /OC /oc1 BDC BT /F1 12 Tf 72 500 Td (Layer text) Tj ET EMC"},
			resources: "/Properties << /oc1 6 0 R >>", extra: []string{ocg}}, want: ErrUnreadableSource},
		"a document with optional content": {doc: pdfDoc{pages: []string{visibleLine}, catalogExtra: "/OCProperties << /OCGs [6 0 R] /D << /OFF [6 0 R] >> >>", extra: []string{ocg}}, want: ErrUnreadableSource},
	})
	t.Run("an off-page glyph stretched by its declared width", func(t *testing.T) {
		widths := strings.TrimSpace(strings.Repeat("1000000 ", 95))
		doc := pdfDoc{pages: []string{"BT /F1 12 Tf -5000 300 Td (H) Tj ET BT /F1 12 Tf 72 300 Td (V) Tj ET"}}.bytes()
		doc = rebuildXref(t, bytes.Replace(doc, []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
			[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /FirstChar 32 /LastChar 126 /Widths ["+widths+"] >>"), 1))
		// Its glyph may be drawn over its advance across the page, so it is not dropped as off-page:
		// the page is refused, and "H" is never stored.
		if got, err := Extract(t.Context(), doc, KindPDF, DefaultLimits); got != "" || !errors.Is(err, ErrPDFScanHiddenContent) {
			t.Fatalf("got %q %v, want %v", got, err, ErrPDFScanHiddenContent)
		}
	})
}

// WIS-625 security review, second round: a render mode written without a separator ("3Tr", which
// PDF.js reads as "3 Tr"), any other number-like token that is not a number, and a glyph whose
// position or size is not a finite number (NaN compares false with every bound) must not pass.
func TestPDFHiddenTextTokenAndNaNBypasses(t *testing.T) {
	t.Parallel()
	huge := "1" + strings.Repeat("0", 200) + ".0"
	runPDFCases(t, map[string]pdfCase{
		"render mode joined to its operator": {doc: pdfDoc{pages: []string{visibleLine + " BT /F1 12 Tf 3Tr 72 500 Td (Hidden claim) Tj ET"}}, want: ErrUnreadableSource},
		"render mode after a joined number":  {doc: pdfDoc{pages: []string{visibleLine + " BT /F1 12 Tf 0 3Tr 72 500 Td (Hidden claim) Tj ET"}}, want: ErrUnreadableSource},
		"an operator ending in Tr":           {doc: pdfDoc{pages: []string{visibleLine + " BT /F1 12 Tf 3 xTr 72 500 Td (Hidden claim) Tj ET"}}, want: ErrUnreadableSource},
		"a glyph at a non-finite position": {doc: pdfDoc{pages: []string{visibleLine + " q " + huge + " 0 0 " + huge + " 0 0 cm " + huge + " 0 0 " + huge + " 0 0 cm BT /F1 12 Tf (Hidden claim) Tj ET Q"}},
			want: ErrUnreadableSource},
	})
}
