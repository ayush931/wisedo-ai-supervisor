package onboarding

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

func scanImage(t *testing.T, w, h int, encode string) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(i % 251)
	}
	img.SetGray(0, 0, color.Gray{Y: 255})
	var b bytes.Buffer
	var err error
	if encode == "jpeg" {
		err = jpeg.Encode(&b, img, nil)
	} else {
		err = png.Encode(&b, img)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// scannedPDF is what a scanner writes: one image per page, no text, built by pdfcpu.
func scannedPDF(t *testing.T, images ...[]byte) []byte {
	t.Helper()
	readers := make([]io.Reader, len(images))
	for i, b := range images {
		readers[i] = bytes.NewReader(b)
	}
	model.ConfigPath = "disable"
	var out bytes.Buffer
	if err := api.ImportImages(nil, &out, readers, pdfcpu.DefaultImportConfig(), model.NewDefaultConfiguration()); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// WIS-625 slice B2: a scanned PDF's pages come back as their images, read in the child; anything
// that is not a page-per-picture scan is refused with its own reason.
func TestScanPagesOfPDF(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	photo, lossless := scanImage(t, 320, 200, "jpeg"), scanImage(t, 240, 160, "png")
	pages, err := ScanPagesOfPDF(t.Context(), scannedPDF(t, photo, lossless))
	if err != nil || len(pages) != 2 {
		t.Fatalf("scan: %d pages, %v", len(pages), err)
	}
	if pages[0].Number != 1 || pages[0].MediaType != "image/jpeg" || !bytes.Equal(pages[0].Data, photo) {
		t.Fatalf("page 1: %d %s, the JPEG was not passed through unchanged", pages[0].Number, pages[0].MediaType)
	}
	media, w, h, err := InspectImage(pages[1].Data, pages[1].MediaType)
	if pages[1].Number != 2 || media != "image/png" || w != 240 || h != 160 || err != nil {
		t.Fatalf("page 2: %s %dx%d %v", media, w, h, err)
	}
	many := make([][]byte, MaxScanPages+1)
	for i := range many {
		many[i] = photo
	}
	for name, c := range map[string]struct {
		data []byte
		want error
	}{
		"a typed PDF":                      {pdfDoc{pages: []string{textPage("Press SOP")}}.bytes(), ErrPDFScanHasText},
		"a typed page among scanned pages": {pdfDoc{pages: []string{textPage("Typed page"), fullPageScan}, resources: imageResources(8), extra: []string{imageObject}}.bytes(), ErrPDFScanHasText},
		// WIS-645: typed pages no longer stop the read early, so a scanned page that could hide
		// pixels is refused for that even beside a typed page.
		"a typed page beside a scan drawn smaller than its page": {pdfDoc{pages: []string{textPage("Typed page"), imagePage}, resources: imageResources(8), extra: []string{imageObject}}.bytes(), ErrPDFScanHiddenContent},
		"a page with two images": {pdfDoc{pages: []string{imagePage + " q 100 0 0 100 72 400 cm /Im2 Do Q"}, resources: "/XObject << /Im1 6 0 R /Im2 7 0 R >>",
			extra: []string{imageObject, strings.Replace(imageObject, "\x80", "\x40", 1)}}.bytes(), ErrPDFScanImageUnsupported}, // distinct, or pdfcpu merges them
		"a blank page":        {pdfDoc{pages: []string{""}}.bytes(), ErrPDFScanImageUnsupported},
		"over the page limit": {scannedPDF(t, many...), ErrPDFScanTooManyPages},
		"encrypted":           {encrypted(t, scannedPDF(t, photo)), ErrPDFEncrypted},
		"not a PDF body":      {[]byte("%PDF-1.7\n%%EOF"), ErrUnreadableSource},
	} {
		if got, err := ScanPagesOfPDF(t.Context(), c.data); got != nil || !errors.Is(err, c.want) {
			t.Errorf("%s: %d pages, %v, want %v", name, len(got), err, c.want)
		}
	}
}

// WIS-625 review (#255): OCR reads a page's embedded image, not the rendered page, so a page that
// could hide part of its image (crop, clip, off-page placement, mask, a drawn box, an annotation)
// is refused rather than sending the hidden pixels to the model. A page that shows its whole image
// is read.
func TestScanPagesOfPDFRefusesHiddenContent(t *testing.T) {
	t.Parallel()
	const fullPage = "q 612 0 0 792 0 0 cm /Im1 Do Q" // the image fills the 612 x 792 page
	withResources := func(content, resources string) []byte {
		return pdfDoc{pages: []string{content}, resources: imageResources(6) + " " + resources, extra: []string{imageObject}}.bytes()
	}
	scan := func(content, pageExtra, image string) []byte {
		return pdfDoc{pages: []string{content}, pageExtra: pageExtra, resources: imageResources(6), extra: []string{image}}.bytes()
	}
	gray := scanImage(t, 16, 16, "jpeg")
	jpegObject := func(w, h int, space string) string {
		return fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /%s /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream", w, h, space, len(gray), gray)
	}
	// twoStreams gives the page /Contents [5 0 R 7 0 R], rewritten in place so no offset moves.
	twoStreams := func(first, second string) []byte {
		pad := strings.Repeat(" ", 16)
		second = fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(second), second)
		data := pdfDoc{pages: []string{first}, pageExtra: pad, resources: imageResources(6), extra: []string{imageObject, second}}.bytes()
		from, to := "/Contents 5 0 R "+pad, "/Contents [5 0 R 7 0 R]"
		return bytes.Replace(data, []byte(from), []byte(to+strings.Repeat(" ", len(from)-len(to))), 1)
	}
	image := func(entry string) string { return strings.Replace(imageObject, "/Length 1", entry+" /Length 1", 1) }
	for name, data := range map[string][]byte{
		"the whole image on the page":      scan(fullPage, "", imageObject),
		"a crop box that hides nothing":    scan(fullPage, "/CropBox [0 0 612 792]", imageObject),
		"a JPEG as its dictionary says":    scan(fullPage, "", jpegObject(16, 16, "DeviceGray")),
		"a letter page fitted to the page": scan("q 595 0 0 770 8.5 11 cm /Im1 Do Q", "", imageObject),
	} {
		if pages, err := ScanPagesOfPDF(t.Context(), data); err != nil || len(pages) != 1 {
			t.Errorf("%s: %d pages, %v", name, len(pages), err)
		}
	}
	for name, data := range map[string][]byte{
		"cropped across the image":      scan(fullPage, "/CropBox [0 0 612 650]", imageObject),
		"clipped":                       scan("q 72 600 50 50 re W n "+fullPage+" Q", "", imageObject),
		"a redaction box drawn over":    scan(fullPage+" 0 g 72 600 50 50 re f", "", imageObject),
		"placed partly off the page":    scan("q 2000 0 0 2000 -500 -500 cm /Im1 Do Q", "", imageObject),
		"masked":                        scan(fullPage, "", strings.Replace(imageObject, "/Length 1", "/Mask [0 255] /Length 1", 1)),
		"annotated":                     scan(fullPage, "/Annots [<< /Type /Annot /Subtype /Square /Rect [72 600 172 700] /IC [0 0 0] >>]", imageObject),
		"a drawing past the read bound": scan(fullPage+strings.Repeat(" ", maxScanContent)+"0 g 72 600 50 50 re f", "", imageObject),
		// Security review of the first fix: the image dictionary is an allow-list, and the bytes
		// extracted must be the picture it describes.
		"an entry outside the allow-list": scan(fullPage, "", image("/Intent /Perceptual")),
		"a palette that paints every sample black": scan(fullPage, "", strings.Replace(imageObject, "/ColorSpace /DeviceGray",
			"/ColorSpace [/Indexed /DeviceGray 255 <"+strings.Repeat("00", 256)+">]", 1)),
		"a colour space by reference": pdfDoc{pages: []string{fullPage}, resources: imageResources(6),
			extra: []string{strings.Replace(imageObject, "/ColorSpace /DeviceGray", "/ColorSpace 7 0 R", 1), "/DeviceGray"}}.bytes(),
		"4-bit samples": scan(fullPage, "", strings.Replace(imageObject, "/BitsPerComponent 8", "/BitsPerComponent 4", 1)),
		"a JPEG smaller than its dictionary says": scan(fullPage, "", jpegObject(32, 16, "DeviceGray")),
		"a grey JPEG labelled RGB":                scan(fullPage, "", jpegObject(16, 16, "DeviceRGB")),
		// Security review of the second fix: the content is tokenised as a viewer does. \v is not PDF
		// whitespace, so this page draws nothing in a viewer while OCR would read the image.
		"an operator joined by a vertical tab": scan("q 612 0 0 792 0 0 cm /Im1\vDo Q", "", imageObject),
		"an operator joined by U+0085":         scan("q 612 0 0 792 0 0 cm /Im1\u0085Do Q", "", imageObject),
		// pdfcpu joins content streams with no separator; a token split across two must not pass.
		"a name split across two content streams": twoStreams("q 612 0 0 792 0 0 cm /Im", "1 Do Q"),
		// Codex review of the third fix: placed within the 0.01 pt tolerance but wholly off the
		// page, or too small to see, the image is hidden from the reader and read by OCR.
		"partly off the page": scan("q 612 0 0 792 -100 0 cm /Im1 Do Q", "", imageObject),
		// Codex round 3: a page too small to see holds an image wholly outside it within an absolute
		// tolerance, and the image still covers it.
		"a media box too small to see": bytes.Replace(scan("q 0.01 0 0 0.01 -0.005 -0.005 cm /Im1 Do Q", "", imageObject),
			[]byte("/MediaBox [0 0 612 792]"), []byte("/MediaBox [0 0 .01 .01]"), 1), // same length: no offset moves
		"tiny in a corner":      scan("q 1 0 0 1 0 0 cm /Im1 Do Q", "", imageObject),
		"a quarter of the page": scan("q 306 0 0 396 0 0 cm /Im1 Do Q", "", imageObject),
		// A Default colour space replaces the image's device space on display (a CalGray gamma that
		// turns grey text white), while OCR reads the contrasting extracted pixels.
		"a DefaultGray colour space": withResources(fullPage, "/ColorSpace << /DefaultGray [/CalGray << /WhitePoint [1 1 1] /Gamma 0.01 >>] >>"),
		"a graphics state resource":  withResources(fullPage, "/ExtGState << /G1 << /SMask /None >> >>"),
		"a transparency group":       scan(fullPage, "/Group << /S /Transparency /CS /DeviceRGB >>", imageObject),
		"a decode array":             scan(fullPage, "", image("/Decode [1 0]")),
		"a soft mask":                scan(fullPage, "", image("/SMask 6 0 R")),
		"DCT parameters":             scan(fullPage, "", strings.Replace(jpegObject(16, 16, "DeviceGray"), "/Length", "/DecodeParms << /ColorTransform 0 >> /Length", 1)),
		// The drawn object must be the extracted image itself, in the one object graph both use.
		"a form drawn in place of the extracted image": pdfDoc{pages: []string{"q 612 0 0 792 0 0 cm /Fm1 Do Q"}, resources: "/XObject << /Im1 6 0 R /Fm1 7 0 R >>",
			extra: []string{imageObject, formObject("/XObject << /Im1 6 0 R >>", "/Im1 Do")}}.bytes(),
	} {
		if got, err := ScanPagesOfPDF(t.Context(), data); got != nil || !errors.Is(err, ErrPDFScanHiddenContent) {
			t.Errorf("%s: %d pages, %v, want %v", name, len(got), err, ErrPDFScanHiddenContent)
		}
	}
	// An image the visible page leaves nothing of shows nothing (Codex round 19 discards a draw
	// whose clipped extent is empty): the page holds no image to read, and is refused as such,
	// never read by OCR.
	for name, data := range map[string][]byte{
		"wholly off the page within the tolerance": scan("q 0.001 0 0 0.001 -0.005 -0.005 cm /Im1 Do Q", "", imageObject),
		"a crop box too small to see":              scan("q 0.001 0 0 0.001 -0.005 -0.005 cm /Im1 Do Q", "/CropBox [0 0 0.001 0.001]", imageObject),
	} {
		if got, err := ScanPagesOfPDF(t.Context(), data); got != nil || !errors.Is(err, ErrPDFScanImageUnsupported) {
			t.Errorf("%s: %d pages, %v, want %v", name, len(got), err, ErrPDFScanImageUnsupported)
		}
	}
}

func TestPageTitle(t *testing.T) {
	t.Parallel()
	if got := pageTitle("Receiving log", 2, 3); got != "Receiving log (page 2 of 3)" {
		t.Fatalf("%q", got)
	}
	long := string(bytes.Repeat([]byte("x"), 200))
	if got := pageTitle(long, 12, 20); len([]rune(got)) != 200 || got[len(got)-len(" (page 12 of 20)"):] != " (page 12 of 20)" {
		t.Fatalf("long title %d runes: %q", len([]rune(got)), got[180:])
	}
}

// WIS-625 review round 4: pdfcpu and ledongthuc resolve a reference by object number alone, so a
// reference of the wrong generation (object 6 is generation 0, "6 1 R" names generation 1) was read
// as the image or content while a generation-checking viewer such as PDF.js shows nothing. Every
// such document is refused before either parser reads it, the scan route and the document route
// alike, so a mismatched page is never read by OCR or dropped as blank.
func TestPDFRefusesReferenceOfAnotherGeneration(t *testing.T) {
	t.Parallel()
	const fullPage = "q 612 0 0 792 0 0 cm /Im1 Do Q"
	regen := func(data []byte, ref string) []byte { // same length: no offset moves
		return bytes.ReplaceAll(data, []byte(ref+" 0 R"), []byte(ref+" 1 R"))
	}
	scan := pdfDoc{pages: []string{fullPage}, resources: imageResources(6), extra: []string{imageObject}}.bytes()
	if pages, err := ScanPagesOfPDF(t.Context(), scan); err != nil || len(pages) != 1 {
		t.Fatalf("the generation-0 fixture must be read: %d pages, %v", len(pages), err)
	}
	for name, data := range map[string][]byte{
		"the image named with another generation":   regen(scan, "/Im1 6"),
		"the content named with another generation": regen(scan, "/Contents 5"),
		"a reference to a missing object":           bytes.Replace(scan, []byte("/Im1 6 0 R"), []byte("/Im1 9 0 R"), 1),
	} {
		if got, err := ScanPagesOfPDF(t.Context(), data); got != nil || !errors.Is(err, ErrUnreadableSource) {
			t.Errorf("scan, %s: %d pages, %v, want %v", name, len(got), err, ErrUnreadableSource)
		}
	}
	mixed := pdfDoc{pages: []string{textPage("Typed page"), fullPage}, resources: imageResources(8), extra: []string{imageObject}}.bytes()
	for name, data := range map[string][]byte{
		"a typed page and a scan whose image has another generation":   regen(mixed, "/Im1 8"),
		"a typed page and a scan whose content has another generation": regen(mixed, "/Contents 7"),
	} {
		if got, err := Extract(t.Context(), data, KindPDF, DefaultLimits); got != "" || !errors.Is(err, ErrUnreadableSource) {
			t.Errorf("document, %s: got %q %v, want %v", name, got, err, ErrUnreadableSource)
		}
	}
}

// objStmMember is one object of an object stream: prefix is written before body, and the
// member's offset points at body.
type objStmMember struct {
	num          int
	prefix, body string
	tight        bool // no newline after body: the next member's text follows it directly
}

// objStmScan is a one-page scan whose catalog, page tree, page and font sit in an uncompressed
// object stream (object 7) behind a cross-reference stream (object 8), as modern scanners write
// them; object 5 is the content stream and object 6 the image. page is the page dictionary.
func objStmScan(page string) []byte {
	return objStmDoc(objStmMembers(page), nil)
}

func objStmMembers(page string) []objStmMember {
	return []objStmMember{{num: 1, body: "<< /Type /Catalog /Pages 2 0 R >>"}, {num: 2, body: "<< /Type /Pages /Kids [4 0 R] /Count 1 >>"},
		{num: 3, body: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}, {num: 4, body: page}}
}

// objStmDoc writes the members into object 7; objects 1 to 4, and any numbered 9 or more, are
// listed in the cross-reference stream at their position in members unless index overrides it.
func objStmDoc(members []objStmMember, index map[int]int) []byte {
	var header, body strings.Builder
	at := map[int]int{}
	for i, m := range members {
		body.WriteString(m.prefix)
		fmt.Fprintf(&header, "%d %d ", m.num, body.Len())
		body.WriteString(m.body)
		if !m.tight {
			body.WriteString("\n")
		}
		at[m.num] = i
	}
	for k, v := range index {
		at[k] = v
	}
	stm := header.String() + body.String()
	content := "q 612 0 0 792 0 0 cm /Im1 Do Q"
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n")
	offsets := map[int]int{}
	write := func(nr int, o string) { offsets[nr] = b.Len(); fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", nr, o) }
	write(5, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	write(6, imageObject)
	write(7, fmt.Sprintf("<< /Type /ObjStm /N %d /First %d /Length %d >>\nstream\n%s\nendstream", len(members), header.Len(), len(stm), stm))
	var rows bytes.Buffer
	row := func(kind byte, field2 uint32, field3 uint16) {
		rows.WriteByte(kind)
		rows.Write([]byte{byte(field2 >> 24), byte(field2 >> 16), byte(field2 >> 8), byte(field2)})
		rows.Write([]byte{byte(field3 >> 8), byte(field3)})
	}
	xref := b.Len()
	row(0, 0, 65535)
	for nr := 1; nr <= 4; nr++ {
		row(2, 7, uint16(at[nr]))
	}
	for _, nr := range []int{5, 6, 7} {
		row(1, uint32(offsets[nr]), 0)
	}
	row(1, uint32(xref), 0)
	size := 9
	for ; ; size++ {
		if _, ok := at[size]; !ok {
			break
		}
		row(2, 7, uint16(at[size]))
	}
	fmt.Fprintf(&b, "8 0 obj\n<< /Type /XRef /Size %d /W [1 4 2] /Root 1 0 R /Length %d >>\nstream\n", size, rows.Len())
	b.Write(rows.Bytes())
	fmt.Fprintf(&b, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", xref)
	return b.Bytes()
}

// WIS-625 review round 5: an object inside an object stream was left lazy by pdfcpu and passed the
// screen unread, so a reference of the wrong generation inside it returned; and pdfcpu splits a
// name on any Unicode space, so "/Image\v" (0x0B) or "/Image" + U+0085 was an image to the checks
// and nothing to a viewer. Form feed is PDF whitespace and still reads.
func TestPDFScreenReadsObjectStreamsAndSeparators(t *testing.T) {
	t.Parallel()
	page := func(contents, xobject string) string {
		return "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> /XObject << /Im1 " + xobject + " >> >> /Contents " + contents + " >>"
	}
	if pages, err := ScanPagesOfPDF(t.Context(), objStmScan(page("5 0 R", "6 0 R"))); err != nil || len(pages) != 1 {
		t.Fatalf("a scan in an object stream must be read: %d pages, %v", len(pages), err)
	}
	plain := func(image string) []byte {
		return pdfDoc{pages: []string{"q 612 0 0 792 0 0 cm /Im1 Do Q"}, resources: imageResources(6), extra: []string{image}}.bytes()
	}
	if pages, err := ScanPagesOfPDF(t.Context(), plain(strings.Replace(imageObject, "/Subtype /Image /Width", "/Subtype /Image\f/Width", 1))); err != nil || len(pages) != 1 {
		t.Fatalf("a form feed is PDF whitespace: %d pages, %v", len(pages), err)
	}
	for name, data := range map[string][]byte{
		"the image of another generation, in an object stream":   objStmScan(page("5 0 R", "6 1 R")),
		"the content of another generation, in an object stream": objStmScan(page("5 1 R", "6 0 R")),
		"a vertical tab in a name, in an object stream":          objStmScan(strings.Replace(page("5 0 R", "6 0 R"), "/Im1 6", "/Im1\v6", 1)),
		"a vertical tab ending a name":                           plain(strings.Replace(imageObject, "/Subtype /Image /Width", "/Subtype /Image\v/Width", 1)),
		"U+0085 ending a name":                                   plain(strings.Replace(imageObject, "/Subtype /Image /Width", "/Subtype /Image\u0085/Width", 1)),
	} {
		if got, err := ScanPagesOfPDF(t.Context(), data); got != nil || !errors.Is(err, ErrUnreadableSource) {
			t.Errorf("%s: %d pages, %v, want %v", name, len(got), err, ErrUnreadableSource)
		}
	}
}

// WIS-625 review round 6: three more places where pdfcpu reads a body differently from a viewer.
// A member whose offset follows "% " on the same line was skipped as a comment by the stream-wide
// pass while pdfcpu decoded it from its offset; pdfcpu resolves a member by its xref index without
// checking the prologue's number, so an extra uncropped copy of the page could stand in for the
// cropped page a viewer shows; and pdfcpu drops a null-valued entry and keeps the first of two
// equal keys, a viewer keeps the last.
func TestPDFScreenReadsBodiesAsAViewerDoes(t *testing.T) {
	t.Parallel()
	page := func(extra string) string {
		return "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " + extra + "/Resources << /Font << /F1 3 0 R >> /XObject << /Im1 6 0 R >> >> /Contents 5 0 R >>"
	}
	commented := objStmMembers(strings.Replace(page(""), "/Im1 6", "/Im1\v 6", 1))
	commented[3].prefix = "% "
	withCopy := append(objStmMembers(page("/CropBox [0 0 612 650] ")), objStmMember{num: 9, body: page("")})
	plain := func(image string) []byte {
		return pdfDoc{pages: []string{"q 612 0 0 792 0 0 cm /Im1 Do Q"}, resources: imageResources(6), extra: []string{image}}.bytes()
	}
	// Security review of round 6: a member offset inside another token. Object 3 is "12" with no
	// separator before object 9, "3": the stream read whole holds the one token 123, yet each member
	// alone read as a single value.
	midToken := objStmMembers(strings.Replace(page(""), "/Font << /F1 3 0 R >> ", "", 1))
	midToken = append([]objStmMember{midToken[0], midToken[1], midToken[3]}, objStmMember{num: 3, body: "12", tight: true}, objStmMember{num: 9, body: "3"})
	if pages, err := ScanPagesOfPDF(t.Context(), objStmDoc(objStmMembers(page("")), nil)); err != nil || len(pages) != 1 {
		t.Fatalf("the object-stream fixture must be read: %d pages, %v", len(pages), err)
	}
	for name, data := range map[string][]byte{
		"a member after a comment marker on its line": objStmDoc(commented, nil),
		"a member offset inside another token":        objStmDoc(midToken, nil),
		"the page's index naming an uncropped copy":   objStmDoc(withCopy, map[int]int{4: 4}),
		"a key given twice, the last null":            plain(strings.Replace(imageObject, "/Subtype /Image", "/Subtype /Image /Subtype null", 1)),
		"a key given twice through a name escape":     plain(strings.Replace(imageObject, "/Subtype /Image", "/Subtype /Image /Sub#74ype /Form", 1)),
		"a null-valued entry":                         plain(strings.Replace(imageObject, "/Subtype /Image", "/Subtype /Image /SMask null", 1)),
	} {
		if got, err := ScanPagesOfPDF(t.Context(), data); got != nil || !errors.Is(err, ErrUnreadableSource) {
			t.Errorf("%s: %d pages, %v, want %v", name, len(got), err, ErrUnreadableSource)
		}
	}
}
