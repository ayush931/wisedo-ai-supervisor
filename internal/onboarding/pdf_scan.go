package onboarding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"regexp"
	"strconv"

	"golang.org/x/image/draw"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

// WIS-625 slice B2: a scanned PDF (every page a picture, no text layer) is read page by page by
// the vision model. Go cannot rasterise a PDF page, so a page is read through the one image it
// embeds: the PDF child, in its page-images mode, screens the file exactly as for text, then
// returns each page's single image. A JPEG is passed through unchanged; a Flate (lossless) image
// is re-encoded as PNG by pdfcpu. A fax-encoded (CCITT), JBIG2 or JPEG 2000 page, a page with more
// or fewer than one image, and a PDF with any text page are refused with their own codes; nothing
// is substituted. Because the model reads the embedded image, not the rendered page, a page must
// show all of its image and nothing else (plainScanPage); anything that could hide part of it is
// refused (PDF_SCAN_HIDDEN_CONTENT).

// PDFPagesChildFlag is argv[1] of the child in page-images mode.
const PDFPagesChildFlag = "--supervisor-internal-pdf-page-images"

// MaxScanPages bounds one scanned PDF: each page is one budgeted model call.
const MaxScanPages = 20

// maxScanOutput bounds the child's reply: base64 of MaxScanPages pages, each at most
// agent.MaxImageBytes, is over 100 MiB, so a scan whose pages are that large is refused instead.
const maxScanOutput = 40 << 20

var (
	ErrPDFScanHasText          = errors.New("PDF_SCAN_HAS_TEXT")
	ErrPDFScanImageUnsupported = errors.New("PDF_SCAN_IMAGE_UNSUPPORTED")
	ErrPDFScanTooManyPages     = errors.New("PDF_SCAN_TOO_MANY_PAGES")
	ErrPDFScanHiddenContent    = errors.New("PDF_SCAN_HIDDEN_CONTENT")
)

const (
	pdfReasonScanText   = "PDF_SCAN_HAS_TEXT"
	pdfReasonScanImage  = "PDF_SCAN_IMAGE_UNSUPPORTED"
	pdfReasonScanPages  = "PDF_SCAN_TOO_MANY_PAGES"
	pdfReasonScanLarge  = "PDF_SCAN_PAGE_TOO_LARGE"
	pdfReasonScanHidden = "PDF_SCAN_HIDDEN_CONTENT"
	// pdfReasonTooManyImages: a typed PDF with more figures than one upload may send to the model.
	pdfReasonTooManyImages = "PDF_TOO_MANY_IMAGES"
)

// pdfPagesReply is the child's one JSON line in page-images mode.
type pdfPagesReply struct {
	SchemaVersion int            `json:"schema_version"`
	Status        string         `json:"status"`
	Reason        string         `json:"reason,omitempty"`
	Pages         []PDFPageImage `json:"pages"`
	// Typed are the pages with a text layer, read in the same parse (WIS-645): a PDF may mix them
	// with scanned pages. Every page is in exactly one of Pages and Typed.
	Typed []pdfPage `json:"typed,omitempty"`
}

// PDFPageImage is one scanned page's image, in page order.
type PDFPageImage struct {
	Number    int    `json:"number"`
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
	// Original is the SHA-256 of a figure's image as embedded, when Data is that image scaled to
	// how the page shows it (figureAsShown); empty for a scanned page, sent as embedded.
	Original string `json:"original,omitempty" contract:"sha256"`
}

// figureDPI and scanDPI are the resolutions a figure and a scanned page are sent to the vision
// model at, of their size on the page; a page is read for OCR, so finer.
const (
	figureDPI = 150
	scanDPI   = 300
)

// figureAsShown scales an image down to figureDPI of its placed size (points), with the same
// deterministic filter every time, as PNG; an image no larger than that is returned as it is.
func figureAsShown(data []byte, media string, placed [2]float64) ([]byte, string, error) {
	return imageAsShown(data, media, placed, figureDPI)
}

// rotateImage turns an image clockwise by a quarter-turn multiple (0, 90, 180 or 270): a JPEG as
// a JPEG at quality 92 (a PNG of a photographed page can pass the size limit), a PNG as a PNG
// (lossless stays lossless); 0 returns it as it is. Both encoders are deterministic.
func rotateImage(data []byte, media string, degrees int) ([]byte, string, error) {
	if degrees == 0 {
		return data, media, nil
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	var dst *image.RGBA
	if degrees == 180 {
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
	} else {
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := src.At(b.Min.X+x, b.Min.Y+y)
			switch degrees {
			case 90: // clockwise: the left column becomes the top row
				dst.Set(h-1-y, x, c)
			case 180:
				dst.Set(w-1-x, h-1-y, c)
			case 270:
				dst.Set(y, w-1-x, c)
			default:
				return nil, "", errors.New("rotation")
			}
		}
	}
	var out bytes.Buffer
	if media == "image/jpeg" {
		if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: rotatedJPEGQuality}); err != nil {
			return nil, "", err
		}
		return out.Bytes(), "image/jpeg", nil
	}
	if err := png.Encode(&out, dst); err != nil {
		return nil, "", err
	}
	return out.Bytes(), "image/png", nil
}

// rotatedJPEGQuality is the quality a turned or resized JPEG is encoded at.
const rotatedJPEGQuality = 92

// imageAsShown is figureAsShown at dpi.
func imageAsShown(data []byte, media string, placed [2]float64, dpi float64) ([]byte, string, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	w := min(cfg.Width, max(1, int(math.Ceil(placed[0]/72*dpi))))
	h := min(cfg.Height, max(1, int(math.Ceil(placed[1]/72*dpi))))
	if w == cfg.Width && h == cfg.Height {
		return data, media, nil
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	var out bytes.Buffer
	if media == "image/jpeg" { // a JPEG stays JPEG: as PNG a resized photo can outgrow the limit
		if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: rotatedJPEGQuality}); err != nil {
			return nil, "", err
		}
		return out.Bytes(), "image/jpeg", nil
	}
	if err := png.Encode(&out, dst); err != nil {
		return nil, "", err
	}
	return out.Bytes(), "image/png", nil
}

// pageImages decides a scanned PDF from its bytes alone: the same screening and page reading as
// for text (so an encrypted, active or linked file is refused for the same reasons), then one
// image per page. A parser panic is a refusal, never a crash that could print document content.
func pageImages(data []byte) (reply pdfPagesReply) {
	defer func() {
		if recover() != nil {
			reply = pdfPagesReply{Status: "refused", Reason: pdfReasonUnreadable}
		}
	}()
	text := parsePDF(data)
	if text.Status != "ok" {
		return pdfPagesReply{Status: "refused", Reason: text.Reason}
	}
	// A typed page is kept as text and a scanned page read as an image (WIS-645); a PDF without a
	// scanned page belongs to /source-documents, and a blank page holds nothing to read. A typed
	// page with figures is refused here: this path keeps a typed page's text only, and its pictures
	// must not be dropped unread (rule 06); /source-documents reads them (WIS-625).
	scanned := 0
	for _, p := range text.Pages {
		switch p.Status {
		case "image":
			scanned++
		case "text":
			if len(p.figures) > 0 {
				return pdfPagesReply{Status: "refused", Reason: pdfReasonScanText}
			}
		case "unreadable": // drawn over, annotated, or more than pictures
			return pdfPagesReply{Status: "refused", Reason: pdfReasonScanHidden}
		default: // a blank page holds no image to read
			return pdfPagesReply{Status: "refused", Reason: pdfReasonScanImage}
		}
	}
	if scanned == 0 {
		return pdfPagesReply{Status: "refused", Reason: pdfReasonScanText}
	}
	if scanned > MaxScanPages {
		return pdfPagesReply{Status: "refused", Reason: pdfReasonScanPages}
	}
	model.ConfigPath = "disable"
	conf := model.NewDefaultConfiguration()
	conf.Offline, conf.ValidateLinks, conf.ValidationMode, conf.Cmd = true, false, model.ValidationRelaxed, model.EXTRACTIMAGES
	// Keep each page's resources and content as written: optimising drops resources the content
	// never names, such as a DefaultGray colour space a viewer still applies, before the check sees them.
	conf.OptimizeResourceDicts, conf.OptimizeDuplicateContentStreams = false, false
	// One object graph for both the extraction and the plainness check (as api.ExtractImagesRaw
	// reads it), so the image checked is the image extracted: no second parser can disagree.
	ctx, err := api.ReadValidateAndOptimize(bytes.NewReader(data), conf)
	if err != nil || ctx.PageCount != len(text.Pages) {
		return pdfPagesReply{Status: "refused", Reason: pdfReasonScanImage}
	}
	pages := make([]PDFPageImage, 0, scanned)
	var typed []pdfPage
	for nr := 1; nr <= len(text.Pages); nr++ {
		if p := text.Pages[nr-1]; p.Status == "text" {
			typed = append(typed, p)
			continue
		}
		// The page's one image is read from its dictionary first (stub mode decodes nothing): its
		// declared size is held to the photo limits before any decoder runs (WIS-645), and a
		// fax-encoded page is decoded here into exactly that size instead of by pdfcpu.
		stubs, err := pdfcpu.ExtractPageImages(ctx, nr, true)
		if err != nil || len(stubs) != 1 {
			return pdfPagesReply{Status: "refused", Reason: pdfReasonScanImage}
		}
		var stub model.Image
		for _, img := range stubs {
			stub = img
		}
		if !scanPixelsAllowed(stub.Width, stub.Height) {
			return pdfPagesReply{Status: "refused", Reason: pdfReasonScanLarge}
		}
		var (
			objNr int
			fax   bool
			media string
			b     []byte
		)
		if stub.Filter == "CCITTFaxDecode" {
			var ok, large bool
			b, ok, large = faxPage(ctx, stub.ObjNr, stub.Width, stub.Height)
			switch {
			case large:
				return pdfPagesReply{Status: "refused", Reason: pdfReasonScanLarge}
			case !ok:
				return pdfPagesReply{Status: "refused", Reason: pdfReasonScanImage}
			}
			objNr, media, fax = stub.ObjNr, "image/png", true
		} else {
			images, err := pdfcpu.ExtractPageImages(ctx, nr, false)
			if err != nil || len(images) != 1 {
				return pdfPagesReply{Status: "refused", Reason: pdfReasonScanImage}
			}
			for n, img := range images {
				objNr, media = n, map[string]string{"jpg": "image/jpeg", "png": "image/png"}[img.FileType]
				if media == "" || img.Reader == nil {
					return pdfPagesReply{Status: "refused", Reason: pdfReasonScanImage}
				}
				if b, err = io.ReadAll(io.LimitReader(img, agent.MaxImageBytes+1)); err != nil {
					return pdfPagesReply{Status: "refused", Reason: pdfReasonScanImage}
				}
			}
			if len(b) > agent.MaxImageBytes {
				return pdfPagesReply{Status: "refused", Reason: pdfReasonScanLarge}
			}
		}
		placed, rotate, plain := plainScanPage(ctx, nr, objNr, media, b, fax)
		if !plain {
			return pdfPagesReply{Status: "refused", Reason: pdfReasonScanHidden}
		}
		// The vision model gets the page at scanDPI of its size on the page, not finer: an
		// ordinary letter or A4 scan at 300 dpi is sent unchanged. Identity stays the embedded
		// image's hash (for a fax page, the hash of its decoded PNG).
		sum := sha256.Sum256(b)
		shown, shownMedia, err := imageAsShown(b, media, placed, scanDPI)
		if err == nil {
			shown, shownMedia, err = rotateImage(shown, shownMedia, rotate)
		}
		if err != nil {
			return pdfPagesReply{Status: "refused", Reason: pdfReasonScanImage}
		}
		if len(shown) > agent.MaxImageBytes {
			return pdfPagesReply{Status: "refused", Reason: pdfReasonScanLarge}
		}
		pages = append(pages, PDFPageImage{Number: nr, MediaType: shownMedia, Data: shown, Original: hex.EncodeToString(sum[:])})
	}
	return pdfPagesReply{Status: "ok", Pages: pages, Typed: typed}
}

// scanLayerOperators are the operators a searchable scan's invisible text layer may use.
var scanLayerOperators = map[string]bool{"Tf": true, "Td": true, "TD": true, "Tm": true, "T*": true, "Tj": true, "TJ": true, "'": true, "\"": true,
	"Tc": true, "Tw": true, "Tz": true, "TL": true, "Ts": true, "Tr": true}

// maxScanContent bounds a scanned page's content: the one drawing it allows is far shorter.
const maxScanContent = 4096

var (
	pdfNumber = regexp.MustCompile(`^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)$`)
	// The only image dictionary entries a plain scan needs; any other (a mask, decode array,
	// optional content, alternates, a palette by reference...) could change what the page shows.
	// Interpolate only smooths scaling, and pdfcpu and many scanners write it.
	scanImageKeys = map[string]bool{"Type": true, "Subtype": true, "Width": true, "Height": true, "ColorSpace": true, "BitsPerComponent": true, "Filter": true,
		"DecodeParms": true, "Length": true, "Interpolate": true}
	// Page entries that cannot change what the page shows (Annots, Group, AA and the rest refuse it).
	scanPageKeys = map[string]bool{"Type": true, "Parent": true, "MediaBox": true, "CropBox": true, "BleedBox": true, "TrimBox": true, "ArtBox": true,
		"Resources": true, "Contents": true, "Rotate": true, "UserUnit": true, "Metadata": true, "StructParents": true, "Tabs": true, "LastModified": true, "PieceInfo": true, "Annots": true}
	// Resources a plain scan may carry: its image, and fonts and procedure sets nothing in its
	// content can use. ColorSpace (DefaultGray/RGB/CMYK), ExtGState, Pattern, Shading and Properties refuse.
	scanResourceKeys = map[string]bool{"XObject": true, "Font": true, "ProcSet": true}
	scanColorSpaces  = map[string]int{"DeviceGray": 1, "DeviceRGB": 3, "DeviceCMYK": 4}
	jpegComponents   = map[color.Model]int{color.GrayModel: 1, color.YCbCrModel: 3, color.RGBAModel: 3, color.CMYKModel: 4} // RGBA: an Adobe JPEG stored as RGB
)

// plainScanPage reports that page nr shows all of its one image, object objNr (whose extracted
// bytes are data), and nothing else (WIS-625 review): OCR reads the embedded image, so a crop
// box, a clip, an image placed partly off the page, a mask, a palette, a drawn redaction box or an
// annotation would send pixels the page hides to the model. Nothing is rendered: the content must
// be exactly `[q] a 0 0 d e f cm /Name Do [Q]`, where /Name is objNr itself, an allow-listed
// 8-bit Device{Gray,RGB,CMYK} image whose dictionary agrees with the extracted bytes, placed
// inside the visible page area (MediaBox within CropBox). Anything else refuses the page.
func plainScanPage(ctx *model.Context, nr, objNr int, media string, data []byte, fax bool) (placed [2]float64, rotate int, plain bool) {
	ok := scanPageIsPlain(ctx, nr, objNr, media, data, fax, &placed)
	if !ok {
		return placed, 0, false
	}
	// /Rotate (inherited and dereferenced by pdfcpu): the page is shown turned clockwise by it, so
	// the image is turned the same way before it is read. Only a whole quarter turn, written as an
	// integer on the page itself, is accepted.
	d, _, inh, err := ctx.PageDict(nr, false)
	if err != nil || inh == nil || inh.Rotate%90 != 0 {
		return placed, 0, false
	}
	if r, found := d.Find("Rotate"); found {
		if resolved, err := ctx.Dereference(r); err != nil {
			return placed, 0, false
		} else if _, integer := resolved.(types.Integer); !integer {
			return placed, 0, false
		}
	}
	return placed, ((inh.Rotate % 360) + 360) % 360, true
}

// scanPageIsPlain is plainScanPage's check; it sets placed to the image's size on the page in
// points (UserUnit applied only to make it smaller).
func scanPageIsPlain(ctx *model.Context, nr, objNr int, media string, data []byte, fax bool, placed *[2]float64) bool {
	d, _, inh, err := ctx.PageDict(nr, false)
	if err != nil || d == nil || inh == nil || inh.MediaBox == nil {
		return false
	}
	// An empty annotations array shows nothing (WIS-625 Codex round 32); any annotation refuses.
	if a, found := d.Find("Annots"); found {
		resolved, err := ctx.Dereference(a)
		if arr, ok := resolved.(types.Array); err != nil || resolved != nil && (!ok || len(arr) != 0) {
			return false // a reference to null is no annotations too (round 33)
		}
	}
	visible := box(inh.MediaBox)
	if inh.CropBox != nil {
		c := box(inh.CropBox)
		visible = [4]float64{max(visible[0], c[0]), max(visible[1], c[1]), min(visible[2], c[2]), min(visible[3], c[3])}
	}
	// One content stream: pdfcpu joins several without a separator, so a token split across them
	// would read differently here than in a viewer. A plain scan never needs more than one.
	if c, err := ctx.Dereference(d["Contents"]); err != nil {
		return false
	} else if a, ok := c.(types.Array); ok && len(a) != 1 {
		return false
	}
	// The content is bounded as other content is (maxImageContent); the drawing left once the
	// searchable layer is set aside is bounded at maxScanContent below (WIS-625 Codex round 32).
	content, err := ctx.PageContent(d, nr)
	if err != nil || len(content) > maxImageContent {
		return false
	}
	// Tokenised as a viewer does (pdfTokens): "/Im1\vDo" is one name there, and draws nothing.
	all, ok := pdfTokens(content)
	if !ok {
		return false
	}
	// A searchable scan's text layer draws nothing: parsePDF made this an "image" page only if every
	// glyph it shows is in render mode 3. It is left out of the check and never read (WIS-625
	// post-merge), but only a BT ... ET object holding text operators alone, with Tr only in mode 3,
	// and outside text objects only "3 Tr": anything else there (cm, q, Do, a path, a clip, another
	// mode) would change how the page shows its image, and refuses it.
	var f []string
	for i := 0; i < len(all); i++ {
		switch {
		case all[i] == "BT":
			for i++; i < len(all) && all[i] != "ET"; i++ {
				// Any mode may be set: each show's own mode is what parsePDF checks (every glyph
				// shown in mode 3); a mode set after the last show paints nothing (round 34).
				if t := all[i]; pdfOperator(t) && (!scanLayerOperators[t] || t == "Tr" && !paintMode(all[i-1])) {
					return false
				}
			}
			if i == len(all) {
				return false
			}
		case all[i] == "Tr":
			return false // a mode with no operand
		case i+1 < len(all) && all[i+1] == "Tr":
			if !paintMode(all[i]) {
				return false
			}
			i++
		default:
			f = append(f, all[i])
		}
	}
	// Text-state operators outside a text object paint nothing; each is set aside with exactly its
	// own operands, read as a viewer does (an operand stack cleared at every operator) and checked
	// against the content walk's operand signatures. Surplus or missing operands refuse
	// (WIS-625 Codex round 35, and its security review: "1 2 /F1 12 Tf" left "1 2" behind).
	var kept, operands []string
	for _, t := range f {
		if !pdfOperator(t) {
			operands = append(operands, t)
			continue
		}
		switch t {
		case "Tf", "Tc", "Tw", "Tz", "TL", "Ts":
			if !operandsMatch(t, tokenKinds(operands)) {
				return false
			}
		default:
			kept = append(append(kept, operands...), t)
		}
		operands = operands[:0]
	}
	f = append(kept, operands...)
	// q ... Q pairs holding nothing (left by a layer set aside, as in "q BT ... ET Q") change
	// nothing; they are dropped, nested ones too (WIS-625 Codex round 33).
	for again := true; again; {
		again = false
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "q" && f[i+1] == "Q" {
				f = append(f[:i:i], f[i+2:]...)
				again = true
				break
			}
		}
	}
	drawing := 0
	for _, t := range f {
		drawing += len(t) + 1
	}
	if drawing > maxScanContent {
		return false
	}
	// Balanced q ... Q wrappers around the one image draw, at any depth, are dropped (WIS-625
	// Codex round 34). One cm places it: a placement composed of several was computed here in
	// float64 only, past the float32 cross-check and bounds (security review of round 34).
	for len(f) >= 2 && f[0] == "q" && f[len(f)-1] == "Q" {
		f = f[1 : len(f)-1]
	}
	if len(f) != 9 || f[6] != "cm" || f[8] != "Do" || f[7][0] != '/' {
		return false
	}
	// The image's name is read decoded (#xx), as walkContent and the resource keys read it:
	// "/Im#31" is /Im1 (WIS-625 Codex round 15).
	imageName, ok := decodeName(f[7][1:])
	if !ok || imageName == "" {
		return false
	}
	var m [6]float64
	for i := range m {
		v, err := strconv.ParseFloat(f[i], 64)
		if !pdfNumber.MatchString(f[i]) || err != nil || !finite(v) || math.Abs(v) > maxCoordinate {
			return false // an overflow, a NaN or a number past the bound is refused, never compared
		}
		m[i] = v
	}
	// The visible page is at least 72 pt (one inch) on each side: smaller than any paper or label a
	// scan is made from, and a reader cannot see a page smaller than that, so its image is hidden.
	// The image is axis-aligned, upright and contained in it, up to 0.01% of the page's side for
	// rounded coordinates, and fills at least half of it. A scan fills its page; fitted to a page of
	// another shape it still covers half unless its aspect ratio differs from the page's by more than
	// 2:1. A smaller picture is not a page scan, and a tiny one is invisible while OCR reads all of it.
	const minSide, relTolerance, minCover = 72.0, 1e-4, 0.5
	// /UserUnit (points per unit, 1 when absent) scales the one-inch floor; outside 0.5 to 10 the
	// page is refused, as for text.
	userUnit := 1.0
	if uu, found := d.Find("UserUnit"); found {
		// It may be an indirect reference, like any value.
		resolved, err := ctx.Dereference(uu)
		if err != nil {
			return false
		}
		switch v := resolved.(type) {
		case types.Integer:
			userUnit = float64(v.Value())
		case types.Float:
			userUnit = v.Value()
		default:
			return false
		}
		if !(userUnit >= minUserUnit && userUnit <= maxUserUnit) {
			return false
		}
		userUnit = min(1, userUnit) // only to tighten: many viewers ignore it
	}
	// Every test below is written to accept only when it holds, so a NaN anywhere refuses.
	w, h := visible[2]-visible[0], visible[3]-visible[1]
	if !(w*userUnit >= minSide && h*userUnit >= minSide) {
		return false
	}
	// The rounding tolerance is capped at maxRounding: scaled with the page alone, a tall page let
	// rows of the scan lie off it and still reach OCR (WIS-625 Codex round 35).
	tx, ty := min(relTolerance*w, maxRounding), min(relTolerance*h, maxRounding)
	placedOK := m[0] > 0 && m[1] == 0 && m[2] == 0 && m[3] > 0 &&
		m[4] >= visible[0]-tx && m[5] >= visible[1]-ty && m[4]+m[0] <= visible[2]+tx && m[5]+m[3] <= visible[3]+ty && m[0]*m[3] >= minCover*w*h
	if !placedOK {
		return false
	}
	// The page and its resources hold nothing that changes how the image shows: no Default*
	// colour space (it replaces the image's device space on display), group, pattern or state.
	for k := range d {
		if !scanPageKeys[k] {
			return false
		}
	}
	for k := range inh.Resources {
		if !scanResourceKeys[k] {
			return false
		}
	}
	xobjects, err := ctx.DereferenceDict(inh.Resources["XObject"])
	if err != nil {
		return false
	}
	ref, ok := xobjects[imageName].(types.IndirectRef)
	if !ok || ref.ObjectNumber.Value() != objNr {
		return false // the drawn object is not the image extracted
	}
	sd, _, err := ctx.DereferenceStreamDict(ref)
	if err != nil || sd == nil {
		return false
	}
	// Placed in another shape than its pixels by more than 2:1, the page shows the scan distorted
	// while the model reads it undistorted, as for a figure (WIS-625 Codex round 31: a 99 x 3300
	// scan stretched 26:1 across a page).
	pw, ph := sd.IntEntry("Width"), sd.IntEntry("Height")
	if pw == nil || ph == nil || *pw <= 0 || *ph <= 0 {
		return false
	}
	if aspect := (m[0] / m[3]) / (float64(*pw) / float64(*ph)); !(aspect <= 2 && aspect >= 0.5) {
		return false
	}
	*placed = [2]float64{m[0] * userUnit, m[3] * userUnit}
	return plainImage(sd, media, data, fax)
}

// plainImage reports that an image XObject is an allow-listed 8-bit Device{Gray,RGB,CMYK} picture
// with nothing that changes how it shows (mask, decode, palette, alternates), and that data, the
// extracted bytes, is the picture its dictionary describes.
func plainImage(sd *types.StreamDict, media string, data []byte, faxDecoded bool) bool {
	var err error
	for k := range sd.Dict {
		if !scanImageKeys[k] {
			return false
		}
	}
	space, ok := sd.Dict["ColorSpace"].(types.Name)
	components := scanColorSpaces[string(space)]
	width, height, bpc := sd.Dict.IntEntry("Width"), sd.Dict.IntEntry("Height"), sd.Dict.IntEntry("BitsPerComponent")
	// One filter, as a name or an array of exactly that name ([/DCTDecode] is /DCTDecode; WIS-625
	// Codex round 36); a chain refuses. /DecodeParms takes the same form as /Filter.
	filter, filterArray := types.Name(""), false
	switch f := sd.Dict["Filter"].(type) {
	case nil:
	case types.Name:
		filter = f
	case types.Array:
		if len(f) != 1 {
			return false
		}
		name, ok := f[0].(types.Name)
		if !ok {
			return false
		}
		filter, filterArray = name, true
	default:
		return false
	}
	// A fax page (WIS-645) is a 1-bit DeviceGray picture, as CCITTFaxDecode defines it, and only
	// when the caller decoded it with faxPage, which checked its parameters and produced the bytes
	// compared below; a figure decoded by pdfcpu is not one. faxPage reads only a plain
	// /CCITTFaxDecode name and ignores the colour space, so the colour space must be absent or
	// exactly /DeviceGray: a /Separation or other array (a tint that paints it white) would show
	// the viewer something other than the bitmap the model reads (WIS-625 Codex, docs rebase).
	_, hasSpace := sd.Dict["ColorSpace"]
	fax := faxDecoded && !filterArray && filter == "CCITTFaxDecode" && media == "image/png" && bpc != nil && *bpc == 1 && (!hasSpace || ok && space == "DeviceGray")
	if fax {
		ok, components = true, 1
	} else if bpc == nil || *bpc != 8 {
		return false
	}
	if sd.Dict.NameEntry("Subtype") == nil || *sd.Dict.NameEntry("Subtype") != "Image" || !ok || components == 0 || width == nil || height == nil {
		return false
	}
	if t := sd.Dict.NameEntry("Type"); t != nil && *t != "XObject" {
		return false
	}
	if _, found := sd.Dict["Filter"]; found && filter != "DCTDecode" && filter != "FlateDecode" && !fax {
		return false
	}
	if parms, found := sd.Dict["DecodeParms"]; found {
		if a, isArray := parms.(types.Array); isArray != filterArray || isArray && len(a) != 1 {
			return false
		} else if isArray {
			parms = a[0]
		}
		// DCT parameters (ColorTransform) change the colours shown, not the bytes read: only a
		// null or an empty dictionary is accepted with DCTDecode (WIS-625 Codex round 37).
		if d, isDict := parms.(types.Dict); filter != "FlateDecode" && !fax && !(parms == nil || isDict && len(d) == 0) {
			return false
		}
	}
	// The extracted bytes must be the picture the dictionary describes.
	var cfg image.Config
	if media == "image/jpeg" {
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil || jpegComponents[cfg.ColorModel] != components {
			return false
		}
	} else if cfg, err = png.DecodeConfig(bytes.NewReader(data)); err != nil {
		return false
	}
	return cfg.Width == *width && cfg.Height == *height
}

// box is a rectangle as [llx lly urx ury], normalised.
// paintMode reports a render-mode operand from 0 to 3 (an integer): modes 4 to 7 add to the clip,
// which a plain scan never needs (security review of round 34).
func paintMode(t string) bool {
	for _, m := range []float64{0, 1, 2, 3} {
		if numberIs(t, m) {
			return true
		}
	}
	return false
}

func box(r *types.Rectangle) [4]float64 {
	return [4]float64{min(r.LL.X, r.UR.X), min(r.LL.Y, r.UR.Y), max(r.LL.X, r.UR.X), max(r.LL.Y, r.UR.Y)}
}

func runPDFPagesChild(in io.Reader, out io.Writer) int {
	data, code, ok := startPDFChild(in, out)
	if !ok {
		return code
	}
	reply := pageImages(data)
	reply.SchemaVersion = 1
	if reply.Pages == nil {
		reply.Pages = []PDFPageImage{}
	}
	if err := json.NewEncoder(out).Encode(reply); err != nil {
		return pdfExitBadOutput
	}
	return pdfExitOK
}

// PDFScan is a scanned or mixed PDF read in one parse (WIS-645): its scanned pages' images and its
// typed pages' text, each in page order with its own page number, together covering pages
// 1..PageCount exactly once.
type PDFScan struct {
	Pages     []PDFPageImage
	Typed     []PDFTypedPage
	PageCount int
}

// PDFTypedPage is one typed page's extracted text.
type PDFTypedPage struct {
	Number int
	Text   string
}

// ScanPagesOfPDF reads a PDF whose every page is scanned (WIS-625): a typed page refuses it with
// ErrPDFScanHasText, as before mixed PDFs were read.
func ScanPagesOfPDF(ctx context.Context, data []byte) ([]PDFPageImage, error) {
	scan, err := ReadPagesOfPDF(ctx, data)
	if err != nil {
		return nil, err
	}
	if len(scan.Typed) > 0 {
		return nil, ErrPDFScanHasText
	}
	return scan.Pages, nil
}

// ReadPagesOfPDF runs the child in page-images mode once and checks its reply (scanReply).
func ReadPagesOfPDF(ctx context.Context, data []byte) (PDFScan, error) {
	body, err := runPDFChildProcess(ctx, data, PDFPagesChildFlag, maxScanOutput)
	if err != nil {
		return PDFScan{}, err
	}
	var reply pdfPagesReply
	if strictjson.Decode(body, &reply, len(body)+1) != nil || reply.SchemaVersion != 1 {
		return PDFScan{}, ErrExtractionUnavailable
	}
	return scanReply(reply)
}

// scanReply maps a refusal to its error, and otherwise accepts only a reply whose scanned and typed
// pages together are pages 1..n, each exactly once and in order, with at least one scanned page.
// Anything else is ErrExtractionUnavailable: the child never answers that way.
func scanReply(reply pdfPagesReply) (PDFScan, error) {
	if reply.Status == "refused" {
		switch reply.Reason {
		case pdfReasonScanText:
			return PDFScan{}, ErrPDFScanHasText
		case pdfReasonScanImage:
			return PDFScan{}, ErrPDFScanImageUnsupported
		case pdfReasonScanPages, pdfReasonPages:
			return PDFScan{}, ErrPDFScanTooManyPages
		case pdfReasonScanLarge:
			return PDFScan{}, ErrTooLarge
		case pdfReasonScanHidden:
			return PDFScan{}, ErrPDFScanHiddenContent
		case pdfReasonEncrypted:
			return PDFScan{}, ErrPDFEncrypted
		case pdfReasonActive:
			return PDFScan{}, ErrPDFActiveContent
		case pdfReasonExternal:
			return PDFScan{}, ErrPDFExternalReference
		default:
			return PDFScan{}, ErrUnreadableSource
		}
	}
	if reply.Status != "ok" || len(reply.Pages) == 0 || len(reply.Pages) > MaxScanPages {
		return PDFScan{}, ErrExtractionUnavailable
	}
	out := PDFScan{Pages: reply.Pages, PageCount: len(reply.Pages) + len(reply.Typed)}
	i, j := 0, 0
	for n := 1; n <= out.PageCount; n++ {
		switch {
		case i < len(reply.Pages) && reply.Pages[i].Number == n:
			i++
		case j < len(reply.Typed) && reply.Typed[j].Number == n && reply.Typed[j].Status == "text":
			out.Typed = append(out.Typed, PDFTypedPage{Number: n, Text: reply.Typed[j].Text})
			j++
		default:
			return PDFScan{}, ErrExtractionUnavailable
		}
	}
	return out, nil
}

// attachFigures reads the bytes of every figure parsePDF chose (WIS-625 post-merge), with the same
// pdfcpu object graph and image extraction as a scanned page: a JPEG unchanged, a lossless image
// as PNG, anything else refused (PDF_SCAN_IMAGE_UNSUPPORTED). More than maxDocumentImages figures
// is refused before any is read, so one PDF never fans out into more model calls than a scan.
func attachFigures(data []byte, reply pdfReply) pdfReply {
	refuse := func(reason string) pdfReply { return pdfReply{Status: "refused", Reason: reason} }
	count := 0
	for _, p := range reply.Pages {
		count += len(p.figures)
	}
	if count == 0 {
		return reply
	}
	if count > maxDocumentImages {
		return refuse(pdfReasonTooManyImages)
	}
	model.ConfigPath = "disable"
	conf := model.NewDefaultConfiguration()
	conf.Offline, conf.ValidateLinks, conf.ValidationMode, conf.Cmd = true, false, model.ValidationRelaxed, model.EXTRACTIMAGES
	conf.OptimizeResourceDicts, conf.OptimizeDuplicateContentStreams = false, false
	ctx, err := api.ReadValidateAndOptimize(bytes.NewReader(data), conf)
	if err != nil || ctx.PageCount != len(reply.Pages) {
		return refuse(pdfReasonScanImage)
	}
	for i := range reply.Pages {
		page := &reply.Pages[i]
		if len(page.figures) == 0 {
			continue
		}
		_, _, inh, err := ctx.PageDict(page.Number, false)
		if err != nil || inh == nil {
			return refuse(pdfReasonScanImage)
		}
		xobjects, err := ctx.DereferenceDict(inh.Resources["XObject"])
		if err != nil {
			return refuse(pdfReasonScanImage)
		}
		for k, name := range page.figures {
			ref, ok := xobjects[name].(types.IndirectRef)
			if !ok {
				return refuse(pdfReasonScanImage)
			}
			sd, _, err := ctx.DereferenceStreamDict(ref)
			if err != nil || sd == nil {
				return refuse(pdfReasonScanImage)
			}
			img, err := pdfcpu.ExtractImage(ctx, sd, false, name, ref.ObjectNumber.Value(), false)
			if err != nil || img == nil || img.Reader == nil {
				return refuse(pdfReasonScanImage)
			}
			media := map[string]string{"jpg": "image/jpeg", "png": "image/png"}[img.FileType]
			if media == "" {
				return refuse(pdfReasonScanImage)
			}
			b, err := io.ReadAll(io.LimitReader(img, agent.MaxImageBytes+1))
			if err != nil {
				return refuse(pdfReasonScanImage)
			}
			if len(b) > agent.MaxImageBytes {
				return refuse(pdfReasonScanLarge)
			}
			if !plainImage(sd, media, b, false) { // a fax figure was not decoded by faxPage: refused
				return refuse(pdfReasonScanHidden)
			}
			// The vision model gets what the page shows: the figure at figureDPI of its size on the
			// page, not finer (a 6000 px image in an inch would show text a reader cannot see). The
			// original's hash stays the figure's identity, so a replay matches.
			sum := sha256.Sum256(b)
			shown, shownMedia, err := figureAsShown(b, media, page.placed[k])
			if err == nil {
				shown, shownMedia, err = rotateImage(shown, shownMedia, page.rotate) // as the page is shown
			}
			if err != nil {
				return refuse(pdfReasonScanImage)
			}
			if len(shown) > agent.MaxImageBytes {
				return refuse(pdfReasonScanLarge)
			}
			page.Images = append(page.Images, PDFPageImage{Number: k + 1, MediaType: shownMedia, Data: shown, Original: hex.EncodeToString(sum[:])})
		}
	}
	return reply
}
