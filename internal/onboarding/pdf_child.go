package onboarding

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	ledongthuc "github.com/ledongthuc/pdf"
	pdflog "github.com/pdfcpu/pdfcpu/pkg/log"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// PDFChildFlag is argv[1] of the short-lived child that parses one PDF (WIS-624). A dedicated
// flag (not only an environment marker) makes a binary without the hook fail at once on an
// unknown flag instead of running itself again.
const PDFChildFlag = "--supervisor-internal-extract-pdf"

// minUserUnit and maxUserUnit bound a page's /UserUnit (its default unit in points, 1 when absent).
const (
	minUserUnit = 0.5
	maxUserUnit = 10
)

// Child budgets. The parent also bounds wall time; these hold inside the child.
const (
	pdfChildMemoryBytes = 768 << 20 // total Go runtime memory; the watchdog exits past it
	pdfChildCPUSeconds  = 20
	// MaxPDFPages bounds the pages of one uploaded PDF.
	MaxPDFPages = 1000
)

// pdfChildReady is the child's first stdout line, written before it reads the file. Without it
// the child never started (a binary without the hook, an exec failure): extraction is unavailable.
// With it, a child that then dies was ended by the file it was parsing.
const pdfChildReady = "PDF-CHILD-READY\n"

// pdfChildNoSandbox prefixes the one line a child that could not confine itself writes instead
// of pdfChildReady: which step failed.
const pdfChildNoSandbox = "PDF-CHILD-NO-SANDBOX: "

// Child exit codes the parent interprets; any other code is "extraction unavailable".
const (
	pdfExitOK        = 0
	pdfExitBudget    = 3 // memory watchdog: the file needs more than the parse budget
	pdfExitBadStdin  = 4
	pdfExitBadOutput = 5
	pdfExitNoSandbox = 6 // confineChild failed: the child never started
)

// pdfReply is the child's one JSON line on stdout. Status is "ok" with every page, or "refused"
// with a stable reason; a refusal carries no text.
type pdfReply struct {
	SchemaVersion int       `json:"schema_version"`
	Status        string    `json:"status"`
	Reason        string    `json:"reason,omitempty"`
	Pages         []pdfPage `json:"pages"`
}

// pdfPage is one page in order (WIS-625 post-merge). Status is
//   - "text": typed text, plus the raster figures it draws (Images, read by the vision model) and
//     the number of small images it skips (Skipped);
//   - "image": no typed text, only raster images (a scanned page; an invisible OCR layer on it is
//     dropped, never stored);
//   - "blank": nothing shown;
//   - "unreadable": something the extractor cannot read and no renderer here can: an annotation,
//     an inline image, a form or pattern that shows anything, an unknown operator, or a path or
//     unread text on a page without typed text.
type pdfPage struct {
	Number  int            `json:"number"`
	Status  string         `json:"status"`
	Text    string         `json:"text"`
	Images  []PDFPageImage `json:"images,omitempty"`
	Skipped int            `json:"skipped,omitempty"`
	figures []string       // resource names of the figures, read by attachFigures
	placed  [][2]float64   // each figure's size on the page, in points
	rotate  int            // the page's /Rotate, a quarter-turn multiple in 0..270
}

// Stable refusal reasons, mapped to HTTP codes by the parent (see pdf.go).
const (
	pdfReasonEncrypted  = "PDF_ENCRYPTED"
	pdfReasonActive     = "PDF_ACTIVE_CONTENT"
	pdfReasonExternal   = "PDF_EXTERNAL_REFERENCE"
	pdfReasonNoText     = "PDF_NO_TEXT_LAYER"
	pdfReasonPages      = "PDF_TOO_MANY_PAGES"
	pdfReasonUnreadable = "PDF_UNREADABLE"
	// pdfReasonBrokenRef: a reference whose object is missing, free or of another generation. pdfcpu
	// and ledongthuc resolve by object number alone, a viewer such as PDF.js does not, so what they
	// read is not what is shown (WIS-625 review). It surfaces as UNREADABLE_SOURCE.
	pdfReasonBrokenRef = "PDF_BROKEN_REFERENCE"
	// pdfReasonSyntax: an object body uses a separator PDF does not have (\v, U+0085, U+00A0, a
	// control byte). pdfcpu splits names on any Unicode space, a viewer does not, so "/Image\v"
	// would be an image to pdfcpu and nothing to PDF.js. It surfaces as UNREADABLE_SOURCE.
	pdfReasonSyntax  = "PDF_NONSTANDARD_SYNTAX"
	pdfReasonBadText = "PDF_TEXT_UNREADABLE"
	// pdfReasonHiddenText: text a viewer may not show and the extractor would read: an invisible or
	// malformed render mode, or optional content (WIS-625 post-merge review, hiddenText). It
	// surfaces as UNREADABLE_SOURCE.
	pdfReasonHiddenText = "PDF_HIDDEN_TEXT"
)

// RunPDFChildIfRequested runs the PDF child and exits when argv[1] is PDFChildFlag; otherwise it
// returns at once. cmd/supervisor calls it first in main, and every test package that can upload
// a PDF calls it first in TestMain, so the parent can re-execute its own binary as the child.
func RunPDFChildIfRequested() {
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case PDFChildFlag:
		os.Exit(runPDFChild(os.Stdin, os.Stdout))
	case PDFPagesChildFlag: // WIS-625: a scanned PDF's page images
		os.Exit(runPDFPagesChild(os.Stdin, os.Stdout))
	}
}

// startPDFChild confines the child (confineChild: CPU and hard memory limits, no network, no
// filesystem), announces it started and reads the bounded file. A child that cannot be confined
// exits before the announcement, so the parent reports extraction unavailable and no PDF is parsed
// unconfined.
func startPDFChild(in io.Reader, out io.Writer) ([]byte, int, bool) {
	debug.SetMemoryLimit(pdfChildMemoryBytes / 2)
	if err := confineChild(); err != nil {
		// Our own text (the step and its errno), written before the file is read: the parent logs it.
		_, _ = io.WriteString(out, pdfChildNoSandbox+err.Error()+"\n")
		return nil, pdfExitNoSandbox, false
	}
	go memoryWatchdog(pdfChildMemoryBytes)
	if _, err := io.WriteString(out, pdfChildReady); err != nil {
		return nil, pdfExitBadOutput, false
	}
	data, err := ReadBounded(in, DefaultLimits.MaxFileBytes)
	if err != nil {
		return nil, pdfExitBadStdin, false
	}
	return data, 0, true
}

func runPDFChild(in io.Reader, out io.Writer) int {
	data, code, ok := startPDFChild(in, out)
	if !ok {
		return code
	}
	reply := parsePDF(data)
	if reply.Status == "ok" {
		reply = attachFigures(data, reply)
	}
	reply.SchemaVersion = 1
	if reply.Pages == nil {
		reply.Pages = []pdfPage{}
	}
	if err := json.NewEncoder(out).Encode(reply); err != nil {
		return pdfExitBadOutput
	}
	return pdfExitOK
}

// memoryWatchdog exits the child once the Go runtime holds more than limit bytes. It works on
// every platform (RLIMIT_AS does not on darwin), so a decompression bomb ends here, not in swap.
func memoryWatchdog(limit uint64) {
	sample := []metrics.Sample{{Name: "/memory/classes/total:bytes"}}
	for {
		metrics.Read(sample)
		if sample[0].Value.Kind() == metrics.KindUint64 && sample[0].Value.Uint64() > limit {
			os.Exit(pdfExitBudget)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// parsePDF decides a PDF from its bytes alone: refusals first (pdfcpu reads the whole object
// table), then the text of each page (ledongthuc/pdf). A parser panic is a refusal, never a crash
// that could print document content.
func parsePDF(data []byte) (reply pdfReply) {
	defer func() {
		if recover() != nil {
			reply = pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
		}
	}()
	reason, screened := screenPDF(data)
	if reason != "" {
		return pdfReply{Status: "refused", Reason: reason}
	}
	r, err := ledongthuc.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
	}
	n := r.NumPage()
	if n < 1 || n != len(screened.pages) {
		return pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
	}
	if n > MaxPDFPages {
		return pdfReply{Status: "refused", Reason: pdfReasonPages}
	}
	// Optional content (layers) can be off, or on only for print or a zoom level: what is shown
	// is the viewer's choice, so a document that declares any is refused.
	if !r.Trailer().Key("Root").Key("OCProperties").IsNull() {
		return pdfReply{Status: "refused", Reason: pdfReasonHiddenText}
	}
	pages := make([]pdfPage, 0, n)
	for i := 1; i <= n; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			return pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
		}
		// The extractor must read the page's content from the same streams the screen resolved
		// (WIS-625 Codex round 10): two readers of one file can follow different cross-references.
		// It must also be the same page object (WIS-625 Codex round 11): pdfcpu and ledongthuc walk
		// the page tree differently, and two pages can share their content.
		if nr, gen, ok := valueObject(p.V); !ok || nr != screened.pages[i-1].nr || gen != screened.pages[i-1].gen {
			return pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
		}
		if got, ok := contentOffsets(p.V.Key("Contents")); !ok || !slices.Equal(got, screened.pages[i-1].contents) {
			return pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
		}
		visible, ok := visibleBox(p)
		if !ok {
			return pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
		}
		// /UserUnit scales the page's units to points: every size measured in points (the 1 pt
		// floor, a figure's resolution) is read through it, but only to tighten (min(1, UserUnit)):
		// many viewers ignore it and show the page at one point per unit. Outside 0.5 to 10 it is
		// refused.
		userUnit := 1.0
		if uu := p.V.Key("UserUnit"); !uu.IsNull() {
			if uu.Kind() != ledongthuc.Integer && uu.Kind() != ledongthuc.Real || uu.Float64() < minUserUnit || uu.Float64() > maxUserUnit {
				return pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
			}
			userUnit = min(1, uu.Float64())
		}
		// /Rotate (inherited): an integer quarter turn, or the page is refused. Figures are sent
		// turned as the page shows them (attachFigures).
		rotate := 0
		if r := inherited(p, "Rotate"); !r.IsNull() {
			if r.Kind() != ledongthuc.Integer || r.Int64()%90 != 0 {
				return pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
			}
			rotate = int((r.Int64()%360 + 360) % 360)
		}
		tokens, readable := contentTokens(p.V.Key("Contents"), screened.decoded)
		// The page group's blending colour space: colours are judged through it too, and one whose
		// device conversions are not known (ICCBased, Lab, CMYK...) leaves text unjudgeable
		// (WIS-625 Codex round 24).
		group := p.V.Key("Group")
		// Decided by whether /CS is present, never by its name: an array or a reference to one
		// has no name and must not read as "no group space" (security review of round 24).
		groupSpace, groupKnown := "", true
		cs := group.Key("CS")
		groupHasCS := !cs.IsNull()
		if groupHasCS {
			groupKnown = cs.Kind() == ledongthuc.Name && (cs.Name() == "DeviceGray" || cs.Name() == "DeviceRGB")
			if groupKnown {
				groupSpace = cs.Name()
			}
		}
		pc := walkContent(tokens, visible, p.Resources().Key("ExtGState"), p.Resources().Key("ColorSpace"), userUnit, groupSpace, false)
		// The same walk in float32 must place every paint, clip, draw and text reach within 0.01
		// unit of this one (WIS-625 Codex round 27).
		pc32 := walkContent(tokens, visible, p.Resources().Key("ExtGState"), p.Resources().Key("ColorSpace"), userUnit, groupSpace, true)
		if !samePagePlaces(pc, pc32) {
			return pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
		}
		// A paint only one reading recorded (a sliver samePagePlaces tolerates) cannot erase:
		// the other reading has no such paint (WIS-625 Codex round 32: a white strip 1/512 high at
		// y 65833 under a -65536 translation vanishes in float32).
		// Paired paints carry the other reading's box and clip, for erasedBefore (round 43).
		near := func(x, y [4]float64) bool {
			return math.Abs(x[0]-y[0]) <= 0.01 && math.Abs(x[1]-y[1]) <= 0.01 && math.Abs(x[2]-y[2]) <= 0.01 && math.Abs(x[3]-y[3]) <= 0.01
		}
		for i, pa := range pc.paints {
			found := false
			for j, pb := range pc32.paints {
				if pa.op == pb.op && near(pa.box, pb.box) {
					found = true
					if pa.whole == pb.whole && pa.clipKnown == pb.clipKnown && near(pa.clip, pb.clip) && !pb.paired {
						pc.paints[i].box32, pc.paints[i].clip32, pc.paints[i].paired = pb.box, pb.clip, true
						pc32.paints[j].box32, pc32.paints[j].clip32, pc32.paints[j].paired = pa.box, pa.clip, true
					}
					break
				}
			}
			if !found {
				pc.paints[i].solid = false
			}
		}
		if pc.badOperands || !groupKnown && (pc.shows > 0 || len(pc.draws) > 0 || len(pc.paints) > 0) {
			return pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
		}
		// A knockout group lets a paint of alpha 0 erase what is under it (WIS-625 Codex round 22).
		if knockout(group) {
			return pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
		}
		if pc.hidden {
			return pdfReply{Status: "refused", Reason: pdfReasonHiddenText}
		}
		// Forms are read before any coverage check: one shown to paint nothing covers nothing.
		formShowing := formShows(p, pc.draws, screened.decoded)
		for i := range pc32.draws { // the same forms, proved blank or not
			pc32.draws[i].blank = pc.draws[i].blank
		}
		// The glyphs as the extractor reads them, with the operator that showed each. Its operator
		// sequence must be walkContent's, or the two read the page differently and neither can be
		// trusted.
		var all []opGlyph
		var spans, spans32 []textSpan
		differs, placeable := false, true
		var drawn map[int]bool
		if readable && !pc.inline {
			var ops []string
			all, spans, ops, placeable, drawn = pageGlyphs(p, userUnit)
			differs = !slices.Equal(ops, pc.ops)
			// A reader computing in float32 must place every glyph and box where this one does,
			// within 0.01 unit (WIS-625 Codex round 26); pageGlyphsAt panics on float32 underflow.
			var all32 []opGlyph
			all32, spans32, _, _, _ = pageGlyphsAt(p, userUnit, true)
			if !sameGlyphPlaces(all, all32, spans, spans32) {
				return pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
			}
		}
		// An isolated group composites a non-Normal blend against nothing instead of the page; this
		// walk does not model that (round 22). Blending counts where something was painted under it:
		// a path or shading, a draw that paints (an image, or a form formShows did not prove blank),
		// or a show that drew a code (round 24). An isolated group of Normal paint is an ordinary page.
		blendUsed := pc.blendSet
		for _, d := range pc.draws {
			// Only a draw that shows (not blank, not clipped away, not at alpha 0; round 25).
			_, _, drawShows := drawExtent(p.Resources().Key("XObject"), d)
			blendUsed = blendUsed || d.blended && drawShows
		}
		// A show counts where it paints: a code on the page (not wholly off it; round 26), in a
		// mode that paints. One that could not be placed counts.
		onPage := map[int]bool{}
		for _, sp := range spans {
			if overlaps(sp.box, visible) {
				onPage[sp.op] = true
			}
		}
		for op := range drawn {
			blendUsed = blendUsed || pc.texts[op].blend && pc.texts[op].tr != 3 && (onPage[op] || !placeable)
		}
		if !readable || pc.inline {
			for _, ti := range pc.texts {
				blendUsed = blendUsed || ti.blend
			}
		}
		if i := group.Key("I"); blendUsed && !i.IsNull() && (i.Kind() != ledongthuc.Bool || i.Bool()) {
			return pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
		}
		// shows and invisible count the text-show operators that showed a character code, and those
		// of them in render mode 3 (WIS-625 Codex round 15: an empty "() Tj" or "[] TJ" shows
		// nothing). Codes are counted as shown, not as decoded (round 16: a code mapped to nothing
		// still paints). Without the extractor's reading (content it cannot read), every show counts.
		shows, invisible := pc.shows, pc.invisible
		if readable && !pc.inline {
			shows, invisible = len(drawn), 0
			for op := range drawn {
				if pc.texts[op].tr == 3 {
					invisible++
				}
			}
		}
		// Text in render mode 3 is either the whole page's text (a scanner's OCR layer over its
		// image: dropped, the image is read instead) or hidden among shown text (refused: the
		// extractor cannot tell which glyphs a mode covers).
		if invisible > 0 && invisible != shows {
			return pdfReply{Status: "refused", Reason: pdfReasonHiddenText}
		}
		// Each operator's text, placed by the PDF rules (pageGlyphs), is dropped when it lies wholly
		// outside the visible page, and refuses the page when it is partly outside it, not wholly
		// inside a rectangular clip, under any other clip, not painted in opaque dark ink, or smaller
		// than a point (WIS-625 post-merge review): a reader may not see it.
		dropped := map[int]bool{}
		var shown, shown32 []textSpan
		textHidden := false
		if invisible == 0 {
			for i, sp := range spans {
				ink := pc.texts[sp.op]
				e := ink.halfWidth // a stroked glyph reaches half the line width further
				// Every box text is checked by, as covering paint or as the region covered, reaches
				// the stroke's half width (WIS-625 Codex round 37); tight stays the unstroked ink
				// measure rules are judged against.
				sp.box = [4]float64{sp.box[0] - e, sp.box[1] - e, sp.box[2] + e, sp.box[3] + e}
				sp.under = [4]float64{sp.under[0] - e, sp.under[1] - e, sp.under[2] + e, sp.under[3] + e}
				switch {
				case !overlaps(sp.box, visible):
					dropped[sp.op] = true
					continue
				case !withinStrict(sp.box, visible) || !ink.clipKnown || !withinStrict(sp.box, ink.clip) || !ink.ink || sp.size*userUnit < 1 || sp.overprint || sp.squeezed:
					textHidden = true
				}
				shown = append(shown, sp)
				if i < len(spans32) {
					sp32 := spans32[i]
					e32 := pc32.texts[sp32.op].halfWidth
					sp32.box = [4]float64{sp32.box[0] - e32, sp32.box[1] - e32, sp32.box[2] + e32, sp32.box[3] + e32}
					sp32.under = [4]float64{sp32.under[0] - e32, sp32.under[1] - e32, sp32.under[2] + e32, sp32.under[3] + e32}
					shown32 = append(shown32, sp32)
				}
			}
		}
		// A Default colour space changes what the text's device colour shows as; dark ink must
		// not lie on a dark or unknown background.
		spaces := p.Resources().Key("ColorSpace")
		for _, k := range []string{"DefaultGray", "DefaultRGB", "DefaultCMYK"} {
			if len(shown) > 0 && !spaces.Key(k).IsNull() {
				textHidden = true
			}
		}
		// Legible on what is under it in both readings: a light paint erases a dark one only if it
		// covers it in the float64 and in the float32 reading (WIS-625 Codex round 33).
		textHidden = textHidden || textOnDark(p, pc, shown) || textOnDark(p, pc32, shown32)
		// Simulated bold is stored once: the redraw is left out of the text, not of the checks.
		for op := range boldRedraws(shown) {
			dropped[op] = true
		}
		text := ""
		if invisible == 0 {
			var err error
			if text, err = pageText(all, dropped); err != nil {
				return pdfReply{Status: "refused", Reason: pdfReasonUnreadable}
			}
		}
		spans = shown // a scanner's invisible layer covers nothing
		// No control character but line breaks and tabs is stored: a C1 code (0x80-0x9F) the
		// extractor could not map is refused, never stored as text.
		if !utf8.ValidString(text) || strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 && r != '\n' && r != '\t' || r >= 0x7f && r <= 0x9f }) {
			return pdfReply{Status: "refused", Reason: pdfReasonBadText}
		}
		covered, checkable := textCovered(p, pc, spans)
		figures, placed, skipped, hidden, unknown := pageFigures(p, pc, visible, spans, userUnit)
		hidden = hidden || covered || textHidden
		// A transparency group's blending colour space (/CS) is what the page is composited in: a
		// figure shows converted to it (to grey, lettering whose ink and ground give one grey is
		// gone), while the model reads its own pixels. A figure is read only in its own space (grey
		// in a grey group, RGB in an RGB one) or without a /CS (WIS-625 Codex rounds 23 and 24).
		if groupHasCS {
			xobjects := p.Resources().Key("XObject")
			for _, name := range figures {
				if cs := xobjects.Key(name).Key("ColorSpace"); cs.Kind() != ledongthuc.Name || cs.Name() != groupSpace || !groupKnown {
					hidden = true
				}
			}
		}
		annots := p.V.Key("Annots")
		// Text whose place cannot be computed cannot be checked for paint over it.
		unreadable := !readable || differs || !checkable || shows > invisible && !placeable || pc.inline || pc.unknown || unknown || !annots.IsNull() && (annots.Kind() != ledongthuc.Array || annots.Len() > 0) ||
			len(p.Resources().Key("Pattern").Keys()) > 0 || formShowing
		page := pdfPage{Number: i, Text: text, Status: "text", Skipped: skipped, figures: figures, placed: placed, rotate: rotate}
		if strings.TrimSpace(text) == "" {
			page.Text, page.Status = "", "blank"
			if len(figures) > 0 || skipped > 0 {
				page.Status = "image"
			}
			// Without typed text, a painted path may be lettering and a shown glyph was not read. A
			// show that draws no code (an empty "() Tj") lost nothing (WIS-625 Codex round 13).
			// Text wholly off the page was dropped as unseen (WIS-625 Codex round 15).
			drew := false
			for op := range drawn {
				drew = drew || pc.texts[op].tr != 3 && !dropped[op]
			}
			unreadable = unreadable || pc.painted || drew
		}
		switch {
		case unreadable:
			page = pdfPage{Number: i, Status: "unreadable"}
		case hidden:
			return pdfReply{Status: "refused", Reason: pdfReasonScanHidden}
		}
		pages = append(pages, page)
	}
	return pdfReply{Status: "ok", Pages: pages}
}

// pageText rebuilds a page's lines from positioned glyphs (ledongthuc's Content applies every
// text-positioning operator): glyphs on one baseline form a line, lines run top to bottom and
// glyphs left to right, and a gap wider than a quarter of the font size is a space. Content
// panics on malformed streams; parsePDF turns that into PDF_UNREADABLE.
//
// Only what a viewer can show is kept (WIS-625 post-merge review): parsePDF drops the operators
// whose text lies wholly outside the visible page and refuses a page whose text a reader may not
// see (partly outside it or a clip, under paint or later text, pale, transparent or tiny).
func pageText(all []opGlyph, dropped map[int]bool) (string, error) {
	type line struct {
		y      float64
		glyphs []ledongthuc.Text
	}
	var lines []*line
	for _, og := range all {
		g := og.Text
		// A NaN compares false with every bound and would pass the visibility check below.
		if !finite(g.X) || !finite(g.Y) || !finite(g.FontSize) || !finite(g.W) {
			return "", errors.New("glyph position")
		}
		// The operator's place by the PDF rules decides (parsePDF), not this glyph's position by
		// the extractor's arithmetic, which leaves out word spacing and multi-byte widths.
		if g.S == "" || dropped[og.op] {
			continue
		}
		var at *line
		for _, l := range lines {
			if math.Abs(l.y-g.Y) <= math.Max(1, g.FontSize*0.3) {
				at = l
				break
			}
		}
		if at == nil {
			at = &line{y: g.Y}
			lines = append(lines, at)
		}
		at.glyphs = append(at.glyphs, g)
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].y > lines[j].y })
	var b strings.Builder
	for _, l := range lines {
		sort.SliceStable(l.glyphs, func(i, j int) bool { return l.glyphs[i].X < l.glyphs[j].X })
		var row strings.Builder
		for i, g := range l.glyphs {
			if i > 0 {
				prev := l.glyphs[i-1]
				if g.X-(prev.X+prev.W) > g.FontSize*0.25 && !strings.HasSuffix(row.String(), " ") && !strings.HasPrefix(g.S, " ") {
					row.WriteByte(' ')
				}
			}
			row.WriteString(g.S)
		}
		if text := strings.TrimSpace(row.String()); text != "" {
			b.WriteString(text)
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// inherited is a page entry, or the nearest ancestor's (ISO 32000 7.7.3.4), within 64 levels.
func inherited(p ledongthuc.Page, key string) ledongthuc.Value {
	v := p.V
	for i := 0; i < 64 && !v.IsNull(); i, v = i+1, v.Key("Parent") {
		if r := v.Key(key); !r.IsNull() {
			return r
		}
	}
	return ledongthuc.Value{}
}

// pdfBox reads a page rectangle [llx lly urx ury], normalised; ok is false unless it is four
// finite numbers.
func pdfBox(v ledongthuc.Value) (r [4]float64, ok bool) {
	if v.Kind() != ledongthuc.Array || v.Len() != 4 {
		return r, false
	}
	for i := range r {
		e := v.Index(i)
		if e.Kind() != ledongthuc.Integer && e.Kind() != ledongthuc.Real {
			return r, false
		}
		if r[i] = e.Float64(); !finite(r[i]) {
			return r, false
		}
	}
	return [4]float64{min(r[0], r[2]), min(r[1], r[3]), max(r[0], r[2]), max(r[1], r[3])}, true
}

// shownRenderModes are the text render modes that paint glyphs (fill, stroke, or both, with or
// without adding them to the clip). 3 (neither) and 7 (clip only) are invisible; anything else
// ("3.0", "11", a name) is not a mode a viewer is sure to show.
var shownRenderModes = map[string]bool{"0": true, "1": true, "2": true, "4": true, "5": true, "6": true}

// formShows reports a form XObject in the page's resources that shows anything: the extractor
// never reads a form's content, so its text, paths or images would be lost.
//
// Only forms the page draws (its Do operators, draws) are read, and within a form only those it
// draws in turn: a form left unused in the resources shows nothing (WIS-625 Codex round 22).
//
// Each drawn form shown to paint nothing is marked blank, so it covers nothing (round 23).
func formShows(p ledongthuc.Page, draws []imageDraw, decoded map[int64][32]byte) bool {
	visits := 0
	shows := false
	xobjects := p.Resources().Key("XObject")
	for i, d := range draws {
		if x := xobjects.Key(d.name); x.Key("Subtype").Name() == "Form" {
			if drawsImage(x.Key("Resources"), x, &visits, decoded) {
				shows = true
			} else {
				draws[i].blank = true
			}
		}
	}
	return shows
}

// samePagePlaces reports that the float64 and float32 walks of a page agree on every recorded
// paint and clip box, draw transform and clip, and text clip and reach, within 0.01 unit.
func samePagePlaces(a, b pageContent) bool {
	near := func(x, y []float64) bool {
		for i := range x {
			if math.Abs(x[i]-y[i]) > 0.01 {
				return false
			}
		}
		return true
	}
	if b.badOperands != a.badOperands || len(a.draws) != len(b.draws) || len(a.texts) != len(b.texts) {
		return false
	}
	// Paints are matched by operator within the tolerance. One that only one reading recorded must
	// be a sliver no wider than the tolerance (a clip edge read 0.005 apart; round 28), never a
	// whole-page paint, and painted where the other reading's clip at that operator agrees
	// (security review of round 28).
	matched := func(x, y []paintEvent, yClips map[int]opClip) bool {
		for _, px := range x {
			found := false
			for _, py := range y {
				if px.op == py.op && px.whole == py.whole && px.clipKnown == py.clipKnown && near(px.box[:], py.box[:]) && near(px.clip[:], py.clip[:]) {
					found = true
					break
				}
			}
			if found {
				continue
			}
			other, has := yClips[px.op]
			if px.whole || min(px.box[2]-px.box[0], px.box[3]-px.box[1]) > decisionMargin || !has || other.known != px.clipKnown || !near(other.clip[:], px.clip[:]) {
				return false
			}
		}
		return true
	}
	if !matched(a.paints, b.paints, b.opClips) || !matched(b.paints, a.paints, a.opClips) {
		return false
	}
	for i, da := range a.draws {
		db := b.draws[i]
		if da.clipKnown != db.clipKnown || !near(da.m[:], db.m[:]) || !near(da.clip[:], db.clip[:]) {
			return false
		}
	}
	for op, ta := range a.texts {
		tb, ok := b.texts[op]
		if !ok || ta.clipKnown != tb.clipKnown || !near(ta.clip[:], tb.clip[:]) || !near([]float64{ta.halfWidth}, []float64{tb.halfWidth}) {
			return false
		}
	}
	return true
}

// sameGlyphPlaces reports that two readings of a page's glyphs (float64 and float32) place every
// glyph and every span's boxes within 0.01 unit of each other.
func sameGlyphPlaces(a, b []opGlyph, sa, sb []textSpan) bool {
	near := func(x, y float64) bool { return math.Abs(x-y) <= 0.01 }
	if len(a) != len(b) || len(sa) != len(sb) {
		return false
	}
	for i := range a {
		if !near(a[i].X, b[i].X) || !near(a[i].Y, b[i].Y) {
			return false
		}
	}
	for i := range sa {
		for _, pair := range [][2][4]float64{{sa[i].box, sb[i].box}, {sa[i].tight, sb[i].tight}, {sa[i].under, sb[i].under}} {
			for k := range 4 {
				if !near(pair[0][k], pair[1][k]) {
					return false
				}
			}
		}
	}
	return true
}

// knockout reports a transparency group that knocks out (/K true): there a later paint, even one
// of alpha 0, replaces what an earlier one in the group left, erasing it (WIS-625 Codex round 22).
func knockout(group ledongthuc.Value) bool {
	k := group.Key("K")
	return !k.IsNull() && (k.Kind() != ledongthuc.Bool || k.Bool())
}

const (
	maxImageVisits  = 64
	maxImageContent = 1 << 20
)

// blankOperators show nothing: state, colour, path construction ended without paint (n, clips) and
// text framing. Do is followed through the resources; S, f, B, b and every other painter are absent.
var blankOperators = map[string]bool{}

func init() {
	for _, op := range strings.Fields("q Q cm w J j M d ri i gs m l c v y h re n W W* CS cs SC SCN sc scn G g RG rg K k BX EX MP DP BMC BDC EMC BT ET Tc Tw Tz TL Tf Tr Ts Td TD Tm T* d0 d1 Do") {
		blankOperators[op] = true
	}
}

func drawsImage(resources, contents ledongthuc.Value, visits *int, decoded map[int64][32]byte) bool {
	if *visits++; *visits > maxImageVisits {
		return true
	}
	tokens, ok := contentTokens(contents, decoded)
	if !ok {
		return true
	}
	if slices.Contains(tokens, "d0") || slices.Contains(tokens, "d1") {
		panic(operandMismatch{}) // a Type 3 glyph operator in a form, as in a page (walkContent)
	}
	if knockout(contents.Key("Group")) {
		return true
	}
	for _, t := range tokens {
		if pdfOperator(t) && !blankOperators[t] { // BI, sh, Tj, an unknown operator...
			return true
		}
	}
	if len(resources.Key("Pattern").Keys()) > 0 || len(resources.Key("Shading").Keys()) > 0 {
		return true
	}
	states := resources.Key("ExtGState")
	for _, k := range states.Keys() {
		if m := states.Key(k).Key("SMask"); !m.IsNull() && m.Name() != "None" {
			return true
		}
	}
	// Only what this form draws (its Do operators), not everything in its resources.
	xobjects := resources.Key("XObject")
	for i, t := range tokens {
		if t != "Do" {
			continue
		}
		name, fine := "", false
		if i > 0 && strings.HasPrefix(tokens[i-1], "/") {
			name, fine = decodeName(tokens[i-1][1:])
		}
		if !fine {
			return true
		}
		x := xobjects.Key(name)
		switch x.Key("Subtype").Name() {
		case "Form":
			if drawsImage(x.Key("Resources"), x, visits, decoded) {
				return true
			}
		default: // an image, or anything else
			return true
		}
	}
	return false
}

// contentTokens reads a page's or form's content streams and splits each as a viewer does
// (pdfTokens). ok is false for an entry that is not a stream, a stream over maxImageContent, or
// one that does not tokenise: the caller cannot tell what it shows. Each stream must decode to the
// bytes pdfcpu decoded for it (decoded, by file offset; WIS-625 Codex round 11): otherwise the file
// is refused outright (a streamMismatch panic, which parsePDF turns into UNREADABLE_SOURCE).
func contentTokens(contents ledongthuc.Value, decoded map[int64][32]byte) (tokens []string, ok bool) {
	streams := []ledongthuc.Value{contents}
	if contents.Kind() == ledongthuc.Array {
		streams = streams[:0]
		for i := 0; i < contents.Len(); i++ {
			streams = append(streams, contents.Index(i))
		}
	}
	var joined []byte
	for _, s := range streams {
		if s.Kind() == ledongthuc.Null {
			continue
		}
		if s.Kind() != ledongthuc.Stream {
			return nil, false
		}
		b, err := io.ReadAll(io.LimitReader(s.Reader(), maxImageContent+1))
		if err != nil || len(b) > maxImageContent {
			return nil, false
		}
		if off, ok := streamOffset(s); !ok || decoded[off] != sha256.Sum256(b) {
			panic(streamMismatch{})
		}
		t, ok := pdfTokens(b)
		if !ok {
			return nil, false
		}
		tokens = append(tokens, t...)
		joined = append(joined, b...)
	}
	// The extractor joins the streams with no separator (ledongthuc.Interpret, io.MultiReader); a
	// viewer may insert one. A token or comment that spans two streams ("3 Tr %" then "0 Tr") reads
	// differently in the two, so the page is not read at all (WIS-625 post-merge review).
	if len(streams) > 1 {
		whole, ok := pdfTokens(joined)
		if !ok || !slices.Equal(whole, tokens) {
			return nil, false
		}
	}
	return tokens, true
}

// pdfWhitespace and pdfDelimiter are PDF's own character classes (ISO 32000 7.2.3): \v and U+0085
// are ordinary characters, not separators.
func pdfWhitespace(c byte) bool {
	return c == 0 || c == '\t' || c == '\n' || c == '\f' || c == '\r' || c == ' '
}
func pdfDelimiter(c byte) bool { return strings.IndexByte("()<>[]{}/%", c) >= 0 }

// pdfTokens splits content as a viewer does, for both checks that read page content (the blank
// page check and the plain scan check): whitespace separates, a delimiter ends a token, a name
// keeps its '/', a literal or hex string is the one token "()" or "<>", and a comment is dropped.
// ok is false for an unterminated or unbalanced string.
func pdfTokens(b []byte) (tokens []string, ok bool) {
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case pdfWhitespace(c):
			i++
		case c == '%':
			for i < len(b) && b[i] != '\n' && b[i] != '\r' {
				i++
			}
		case c == '(':
			for depth := 0; ; i++ {
				if i >= len(b) {
					return nil, false
				}
				if b[i] == '\\' {
					i++
				} else if b[i] == '\r' {
					// A raw CR in a literal string is a line end a viewer reads as LF, and the
					// extractor as code 13: the two read different text (WIS-625 Codex round 25).
					return nil, false
				} else if b[i] == '(' {
					depth++
				} else if b[i] == ')' {
					if depth--; depth == 0 {
						break
					}
				}
			}
			i++
			tokens = append(tokens, "()")
		case (c == '<' || c == '>') && i+1 < len(b) && b[i+1] == c:
			tokens = append(tokens, string(b[i:i+2]))
			i += 2
		case c == '<':
			j := bytes.IndexByte(b[i:], '>')
			if j < 0 {
				return nil, false
			}
			i += j + 1
			tokens = append(tokens, "<>")
		case c == ')' || c == '>':
			return nil, false
		case c == '[' || c == ']' || c == '{' || c == '}':
			tokens = append(tokens, string(c))
			i++
		default: // a name ('/' and regular characters) or a regular token
			j := i + 1
			for j < len(b) && !pdfWhitespace(b[j]) && !pdfDelimiter(b[j]) {
				j++
			}
			tokens = append(tokens, string(b[i:j]))
			i = j
		}
	}
	return tokens, true
}

// pdfOperator reports a token that is an operator: not a name, number, string, boolean, null or
// delimiter.
func pdfOperator(t string) bool {
	switch t {
	case "()", "<>", "<<", ">>", "[", "]", "{", "}", "true", "false", "null":
		return false
	}
	return t[0] != '/' && !pdfNumber.MatchString(t)
}

// Names that refuse a PDF wherever they appear as a key or as a name value. Active content runs
// or carries something other than text; external references reach outside the file. The policy
// matches DOCX, where any external relationship (hyperlinks included) is refused.
var (
	pdfActiveNames = map[string]bool{"JS": true, "JavaScript": true, "EmbeddedFile": true, "EmbeddedFiles": true, "FileAttachment": true,
		"Launch": true, "RichMedia": true, "XFA": true, "Sound": true, "Movie": true, "Rendition": true, "SubmitForm": true, "ImportData": true}
	pdfExternalNames = map[string]bool{"URI": true, "GoToR": true, "GoToE": true, "URL": true}
)

// screenPDF reads the document with pdfcpu (offline, no configuration on disk, no link checks)
// and walks every object of the cross-reference table, including those inside object streams.
// It returns a refusal reason, or "" and the document as pdfcpu resolved it (screenedPDF), for the
// extractor's reading to be compared with.
func screenPDF(data []byte) (string, screenedPDF) {
	model.ConfigPath = "disable" // never read or write a pdfcpu config directory
	conf := model.NewDefaultConfiguration()
	conf.Offline, conf.ValidateLinks, conf.ValidationMode = true, false, model.ValidationRelaxed
	pdflog.SetDebugLogger(repairLog{}) // screenPDF runs only in the PDF child process
	repairs := pdfcpuRepairs.Load()
	ctx, err := pdfcpu.Read(bytes.NewReader(data), conf)
	if err != nil {
		if isEncryptionError(err) {
			return pdfReasonEncrypted, screenedPDF{}
		}
		return pdfReasonUnreadable, screenedPDF{}
	}
	if ctx.Encrypt != nil || ctx.E != nil {
		return pdfReasonEncrypted, screenedPDF{}
	}
	// Both readers must resolve every object number to the same place: a direct /Length, /Filter,
	// resource or font can then differ between them only if the bytes there do, which the strict
	// reading below rules out.
	if !sameCrossReferences(data, ctx) {
		return pdfReasonUnreadable, screenedPDF{}
	}
	if ctx.EnsurePageCount() != nil {
		return pdfReasonUnreadable, screenedPDF{}
	}
	if ctx.PageCount > MaxPDFPages {
		return pdfReasonPages, screenedPDF{}
	}
	// A hybrid-reference file (a trailer's /XRefStm) is read one way by readers that know
	// cross-reference streams and another by those that do not (WIS-625 Codex round 10).
	if ctx.Read.Hybrid {
		return pdfReasonUnreadable, screenedPDF{}
	}
	// Each object stream's prologue and every member in it are read first; members maps a stream's
	// object number to the object numbers its prologue lists, in order.
	members := map[int][]int{}
	for nr, entry := range ctx.Table {
		if entry == nil || entry.Free {
			continue
		}
		if osd, ok := entry.Object.(types.ObjectStreamDict); ok {
			if osd.Decode() != nil {
				return pdfReasonUnreadable, screenedPDF{}
			}
			nums, ok := objectStreamSyntax(osd.Content, osd.FirstObjOffset, osd.ObjCount)
			if !ok {
				return pdfReasonSyntax, screenedPDF{}
			}
			// Each listed object that is current here is the member at its index. One listed but
			// superseded by a later update (as an incremental save leaves it) is not current, and
			// no reader resolves it from here (sameCrossReferences; WIS-625 Codex round 11).
			for i, k := range nums {
				e := ctx.Table[k]
				if e != nil && !e.Free && e.ObjectStream != nil && *e.ObjectStream == nr && (e.ObjectStreamInd == nil || *e.ObjectStreamInd != i) {
					return pdfReasonSyntax, screenedPDF{}
				}
			}
			members[nr] = nums
		}
	}
	reason := ""
	for nr, entry := range ctx.Table {
		if entry == nil || entry.Free || entry.Object == nil {
			continue
		}
		// Every body is read strictly, as a viewer reads it, before its parsed form is trusted
		// (pdfSyntax): a plain object from its offset, an object-stream member from its own offset
		// in its stream (checked with the stream, below). A member is decoded here too: left lazy, it
		// would pass the screen unread.
		obj := entry.Object
		if lazy, ok := obj.(types.LazyObjectStreamObject); ok {
			decoded, err := lazy.DecodedObject(context.Background())
			if err != nil {
				return pdfReasonUnreadable, screenedPDF{}
			}
			obj = decoded // nil for a null member: its identity is checked below, its value is not screened
		}
		switch {
		case entry.ObjectStream != nil:
			// The member at the indexed position of its stream must be this object (pdfcpu resolves
			// by index alone, a viewer by the prologue's number).
			nums, ok := members[*entry.ObjectStream]
			if !ok || entry.Offset != nil || entry.ObjectStreamInd == nil || *entry.ObjectStreamInd < 0 || *entry.ObjectStreamInd >= len(nums) || nums[*entry.ObjectStreamInd] != nr {
				return pdfReasonSyntax, screenedPDF{}
			}
		case entry.Offset != nil && *entry.Offset > 0 && *entry.Offset < int64(len(data)):
			if !plainObjectSyntax(data[*entry.Offset:]) {
				return pdfReasonSyntax, screenedPDF{}
			}
			if !streamLengthLands(data, *entry.Offset, obj, ctx.Table) {
				return pdfReasonUnreadable, screenedPDF{}
			}
		default:
			return pdfReasonUnreadable, screenedPDF{}
		}
		if obj == nil {
			continue // a null member, as a plain null object is skipped (WIS-625 Codex round 12)
		}
		if r := screenObject(ctx.Table, obj, 0); r != "" && (reason == "" || r == pdfReasonActive) {
			reason = r // active content outranks an external reference
		}
	}
	if reason != "" {
		return reason, screenedPDF{}
	}
	var out screenedPDF
	if out.pages, reason = pageTree(ctx); reason != "" {
		return reason, screenedPDF{}
	}
	// Every stream the extractor decodes as content (page contents, forms), and every object
	// stream, ToUnicode map or font file it reads, must decode the same in both readers:
	// ledongthuc applies a predictor without /Colors, /BitsPerComponent or /Columns as a viewer
	// does. So no such stream may carry a filter other than FlateDecode or any /DecodeParms; images
	// (decoded by pdfcpu alone) and cross-reference streams (compared above) are exempt.
	out.decoded = map[int64][32]byte{}
	for _, entry := range ctx.Table {
		if entry == nil || entry.Free {
			continue
		}
		var sd types.StreamDict
		switch v := entry.Object.(type) {
		case types.StreamDict:
			sd = v
		case types.ObjectStreamDict:
			sd = v.StreamDict
		default:
			continue
		}
		if t := sd.Dict.Subtype(); t != nil && *t == "Image" {
			continue
		}
		if !plainFlate(sd.Dict) {
			return pdfReasonUnreadable, screenedPDF{}
		}
		if t := sd.Dict.Subtype(); t != nil && *t == "Form" {
			b, ok := decodedStream(sd)
			if !ok {
				return pdfReasonUnreadable, screenedPDF{}
			}
			out.decoded[sd.StreamOffset] = sha256.Sum256(b)
		}
	}
	for i, page := range out.pages {
		d, ref, _, err := ctx.PageDict(i+1, false)
		if err != nil || d == nil || ref == nil || ref.ObjectNumber.Value() != page.nr || ref.GenerationNumber.Value() != page.gen {
			return pdfReasonUnreadable, screenedPDF{}
		}
		contents, err := ctx.Dereference(d["Contents"])
		if err != nil {
			return pdfReasonUnreadable, screenedPDF{}
		}
		refs := types.Array{contents}
		if a, ok := contents.(types.Array); ok {
			refs = a
		}
		for _, ref := range refs {
			o, err := ctx.Dereference(ref)
			if err != nil {
				return pdfReasonUnreadable, screenedPDF{}
			}
			switch v := o.(type) {
			case nil:
			case types.StreamDict:
				b, ok := decodedStream(v)
				if !ok {
					return pdfReasonUnreadable, screenedPDF{}
				}
				out.pages[i].contents = append(out.pages[i].contents, v.StreamOffset)
				out.decoded[v.StreamOffset] = sha256.Sum256(b)
			default:
				return pdfReasonUnreadable, screenedPDF{}
			}
		}
	}
	// A file pdfcpu had to repair (a rebuilt cross-reference table, a missing catalog or object 0,
	// a page-tree node) is not the file another reader sees (WIS-625 Codex round 10 security
	// review). Checked last: pdfcpu repairs the page tree while it walks it (round 11).
	if pdfcpuRepairs.Load() != repairs {
		return pdfReasonUnreadable, screenedPDF{}
	}
	return "", out
}

// screenedPDF is the document as pdfcpu resolved it, for the extractor's reading to be compared
// with: each page's object and content streams, and the SHA-256 of every content and form stream
// pdfcpu decoded, by the file offset of its data.
type screenedPDF struct {
	pages   []screenedPage
	decoded map[int64][32]byte
}

type screenedPage struct {
	nr, gen  int
	contents []int64
}

// pageTree walks the page tree from the catalog's /Pages as both readers must read it: every node
// an indirect dictionary typed /Pages or /Page, visited once, each kid's /Parent the node that lists
// it (so attributes inherited through /Parent are those of the walk), the root without /Parent,
// and each /Count the number of pages below it (ledongthuc selects a page by /Count).
func pageTree(ctx *model.Context) ([]screenedPage, string) {
	root, ok := ctx.RootDict["Pages"].(types.IndirectRef)
	if !ok {
		return nil, pdfReasonUnreadable
	}
	var pages []screenedPage
	seen := map[int]bool{}
	var walk func(ref types.IndirectRef, parent *types.IndirectRef, depth int) (int, bool)
	walk = func(ref types.IndirectRef, parent *types.IndirectRef, depth int) (int, bool) {
		nr := ref.ObjectNumber.Value()
		if depth > 32 || seen[nr] || len(pages) > MaxPDFPages {
			return 0, false
		}
		seen[nr] = true
		o, err := ctx.Dereference(ref)
		d, isDict := o.(types.Dict)
		if err != nil || !isDict || d.Type() == nil {
			return 0, false
		}
		up, hasParent := d["Parent"]
		if parent == nil && hasParent {
			return 0, false
		}
		if parent != nil {
			if p, ok := up.(types.IndirectRef); !ok || p.ObjectNumber != parent.ObjectNumber || p.GenerationNumber != parent.GenerationNumber {
				return 0, false
			}
		}
		switch *d.Type() {
		case "Page":
			if parent == nil {
				return 0, false
			}
			pages = append(pages, screenedPage{nr: nr, gen: ref.GenerationNumber.Value()})
			return 1, true
		case "Pages":
			kids, ok := d["Kids"].(types.Array)
			count, okCount := d["Count"].(types.Integer)
			if !ok || !okCount {
				return 0, false
			}
			total := 0
			for _, k := range kids {
				kid, ok := k.(types.IndirectRef)
				if !ok {
					return 0, false
				}
				n, ok := walk(kid, &ref, depth+1)
				if !ok {
					return 0, false
				}
				total += n
			}
			return total, total == int(count)
		}
		return 0, false
	}
	n, ok := walk(root, nil, 0)
	if !ok || n != ctx.PageCount || n < 1 {
		return nil, pdfReasonUnreadable
	}
	return pages, ""
}

// plainFlate is a stream filter both readers decode alike: none or FlateDecode, no parameters.
func plainFlate(d types.Dict) bool {
	filterArray := false
	switch f := d["Filter"].(type) {
	case nil:
	case types.Name:
		if f != "FlateDecode" {
			return false
		}
	case types.Array:
		if len(f) != 1 || f[0] != types.Name("FlateDecode") {
			return false
		}
		filterArray = true
	default:
		return false
	}
	// /DecodeParms takes the filter's form: beside [/FlateDecode] a one-element array ([null] or
	// [<< >>]; WIS-625 Codex round 38), beside a name a dictionary; either way empty.
	p := d["DecodeParms"]
	if a, isArray := p.(types.Array); isArray {
		if !filterArray || len(a) != 1 {
			return false
		}
		p = a[0]
	}
	switch p := p.(type) {
	case nil:
		return true
	case types.Dict:
		return len(p) == 0
	}
	return false
}

// decodedStream is a plainFlate stream's data as pdfcpu decodes it (zlib, an unexpected end
// ignored as in its flate filter), read at most maxImageContent+1 bytes: a longer stream is never
// read by the extractor (contentTokens), so a decompression bomb is not inflated here either.
func decodedStream(sd types.StreamDict) ([]byte, bool) {
	if sd.Raw == nil {
		return nil, false
	}
	if len(sd.FilterPipeline) == 0 {
		return sd.Raw, true
	}
	if len(sd.FilterPipeline) != 1 || sd.FilterPipeline[0].Name != "FlateDecode" || len(sd.FilterPipeline[0].DecodeParms) != 0 {
		return nil, false
	}
	zr, err := zlib.NewReader(bytes.NewReader(sd.Raw))
	if err != nil {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(zr, maxImageContent+1))
	return b, err == nil || errors.Is(err, io.ErrUnexpectedEOF)
}

// streamOffset is the file offset of a stream's data as ledongthuc read it (the only identity it
// exposes, printed after "@").
func streamOffset(v ledongthuc.Value) (int64, bool) {
	str := v.String()
	off, err := strconv.ParseInt(str[strings.LastIndexByte(str, '@')+1:], 10, 64)
	return off, err == nil
}

// valueObject is the object number and generation ledongthuc read v from (unexported: read by
// reflection, failing closed like sameCrossReferences).
func valueObject(v ledongthuc.Value) (nr, gen int, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	ptr := reflect.ValueOf(v).FieldByName("ptr")
	return int(ptr.FieldByName("id").Uint()), int(ptr.FieldByName("gen").Uint()), ptr.IsValid()
}

// streamMismatch is raised when the extractor decodes a stream to other bytes than pdfcpu.
type streamMismatch struct{}

// sameCrossReferences compares pdfcpu's resolved cross-reference table with ledongthuc's: every
// object pdfcpu holds in use must be in use to ledongthuc too, with the same generation and the
// same file offset or object stream. ledongthuc keeps its table unexported; it is read by reflection, and a change to
// its layout refuses every file (the tests then fail) rather than skipping the check.
func sameCrossReferences(data []byte, ctx *model.Context) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	r, err := ledongthuc.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return false
	}
	table := reflect.ValueOf(r).Elem().FieldByName("xref")
	if !table.IsValid() || table.Kind() != reflect.Slice {
		return false
	}
	type place struct {
		gen    int64
		stream int64 // object stream number, or -1
		offset int64 // file offset of a plain object
	}
	theirs := map[int]place{}
	for i := 1; i < table.Len(); i++ {
		e := table.Index(i)
		ptr := e.FieldByName("ptr")
		inStream, offset := e.FieldByName("inStream").Bool(), e.FieldByName("offset").Int()
		if ptr.FieldByName("id").Uint() != uint64(i) || !inStream && offset == 0 {
			continue // free or absent, as ledongthuc's resolve reads it
		}
		p := place{gen: int64(ptr.FieldByName("gen").Uint()), stream: -1, offset: offset}
		if inStream {
			p.stream, p.offset = int64(e.FieldByName("stream").FieldByName("id").Uint()), 0
		}
		theirs[i] = p
	}
	ours := map[int]place{}
	for nr, e := range ctx.Table {
		if nr == 0 || e == nil || e.Free {
			continue
		}
		p := place{stream: -1}
		if e.Generation != nil {
			p.gen = int64(*e.Generation)
		}
		switch {
		case e.ObjectStream != nil:
			p.stream = int64(*e.ObjectStream)
		case e.Offset != nil:
			p.offset = *e.Offset
		default:
			return false
		}
		ours[nr] = p
	}
	// An object pdfcpu holds free but ledongthuc in use (ledongthuc ignores free entries in a
	// table) is harmless: the screen refuses any reference to it.
	for nr, p := range ours {
		if q, ok := theirs[nr]; !ok || q != p {
			return false
		}
	}
	return true
}

// pdfcpuRepairs counts the repairs pdfcpu reports (it records them only as log lines).
var pdfcpuRepairs atomic.Int64

type repairLog struct{}

func (repairLog) note(line string) {
	// "trailer size" and "trailer ID" are bookkeeping that no reader resolves objects by.
	if strings.HasPrefix(line, "pdfcpu repaired: ") && !strings.HasPrefix(line, "pdfcpu repaired: trailer ") {
		pdfcpuRepairs.Add(1)
	}
}
func (l repairLog) Printf(format string, args ...any) { l.note(fmt.Sprintf(format, args...)) }
func (l repairLog) Println(args ...any)               { l.note(fmt.Sprint(args...)) }
func (l repairLog) Fatalf(format string, args ...any) { l.note(fmt.Sprintf(format, args...)) }
func (l repairLog) Fatalln(args ...any)               { l.note(fmt.Sprint(args...)) }

// contentOffsets is screenPDF's per-page list as the extractor reads /Contents: the file offset of
// each stream's data (the only identity ledongthuc exposes, printed after "@"), nulls skipped.
func contentOffsets(contents ledongthuc.Value) ([]int64, bool) {
	streams := []ledongthuc.Value{contents}
	if contents.Kind() == ledongthuc.Array {
		streams = streams[:0]
		for i := 0; i < contents.Len(); i++ {
			streams = append(streams, contents.Index(i))
		}
	}
	var out []int64
	for _, v := range streams {
		switch v.Kind() {
		case ledongthuc.Null:
		case ledongthuc.Stream:
			off, ok := streamOffset(v)
			if !ok {
				return nil, false
			}
			out = append(out, off)
		default:
			return nil, false
		}
	}
	return out, true
}

// streamLengthLands checks a plain stream object as a viewer reads it: its data starts after
// "stream" and one EOL (CRLF or LF), and the declared /Length (direct, or an indirect integer)
// ends it exactly where an optional EOL and "endstream" follow. pdfcpu falls back to scanning for
// "endstream" when the length is 0 or wrong, so a short /Length could hide data a viewer skips (or
// show data it never reads); the read must also start and end where pdfcpu's did.
func streamLengthLands(data []byte, off int64, obj types.Object, table map[int]*model.XRefTableEntry) bool {
	t, stop, ok := lexPDF(data[off:])
	if !ok || stop != "stream" {
		return true // not a stream
	}
	length := int64(-1)
	for i := 4; i+1 < len(t) && t[i].kind == 'n'; {
		next := pdfValue(t, i+1, 1)
		if next < 0 {
			return false
		}
		if t[i].text == "Length" {
			v := t[i+1].text
			switch {
			case next == i+2 && t[i+1].kind == 'w':
				// A signed integer such as +123 is valid; a negative or non-integer length is not.
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					length = n
				}
			case next == i+4 && t[i+3].text == "R":
				nr, _ := strconv.Atoi(v)
				e := table[nr]
				if e == nil || e.Free || e.Object == nil {
					return false
				}
				o := e.Object
				if lazy, ok := o.(types.LazyObjectStreamObject); ok {
					if o, _ = lazy.DecodedObject(context.Background()); o == nil {
						return false
					}
				}
				n, ok := o.(types.Integer)
				if !ok {
					return false
				}
				length = int64(n)
			}
		}
		i = next
	}
	if length < 0 || len(t) < 4 {
		return false
	}
	at := bytes.Index(data[off+int64(t[len(t)-1].start):], []byte("stream"))
	if at < 0 {
		return false
	}
	start := off + int64(t[len(t)-1].start) + int64(at) + 6
	switch {
	case start > int64(len(data)):
		return false
	case bytes.HasPrefix(data[start:], []byte("\r\n")):
		start += 2
	case bytes.HasPrefix(data[start:], []byte("\n")):
		start++
	default:
		return false
	}
	end := start + length
	if end > int64(len(data)) {
		return false
	}
	rest := data[end:]
	for _, eol := range []string{"\r\n", "\n", "\r"} {
		if bytes.HasPrefix(rest, []byte(eol)) {
			rest = rest[len(eol):]
			break
		}
	}
	if !bytes.HasPrefix(rest, []byte("endstream")) {
		return false
	}
	var sd types.StreamDict
	switch v := obj.(type) {
	case types.StreamDict:
		sd = v
	case types.ObjectStreamDict:
		sd = v.StreamDict
	case types.XRefStreamDict:
		sd = v.StreamDict
	default:
		return false
	}
	return sd.StreamOffset == start && (sd.Raw == nil || int64(len(sd.Raw)) == length)
}

// screenObject checks one object and its direct sub-objects. Indirect references are not
// followed: every object is visited once from the table, so cycles cannot recur. Each reference
// must name an in-use entry of the original cross-reference table with its exact generation.
func screenObject(table map[int]*model.XRefTableEntry, o types.Object, depth int) string {
	if depth > 64 {
		return pdfReasonUnreadable
	}
	var dict types.Dict
	switch v := o.(type) {
	case types.Dict:
		dict = v
	case types.StreamDict:
		dict = v.Dict
	case types.ObjectStreamDict:
		dict = v.Dict
	case types.XRefStreamDict:
		dict = v.Dict
	case types.Array:
		reason := ""
		for _, item := range v {
			if r := screenObject(table, item, depth+1); r != "" && (reason == "" || r == pdfReasonActive) {
				reason = r
			}
		}
		return reason
	case types.Name:
		return nameReason(string(v))
	case types.IndirectRef:
		e := table[v.ObjectNumber.Value()]
		if e == nil || e.Free || e.Generation == nil || *e.Generation != v.GenerationNumber.Value() {
			return pdfReasonBrokenRef
		}
		return ""
	default:
		return ""
	}
	reason := ""
	for key, value := range dict {
		r := nameReason(key)
		if r == "" {
			r = screenObject(table, value, depth+1)
		}
		if r != "" && (reason == "" || r == pdfReasonActive) {
			reason = r
		}
	}
	return reason
}

func nameReason(name string) string {
	switch {
	case pdfActiveNames[name]:
		return pdfReasonActive
	case pdfExternalNames[name]:
		return pdfReasonExternal
	}
	return ""
}

func isEncryptionError(err error) bool {
	s := strings.ToLower(fmt.Sprint(err))
	return strings.Contains(s, "encrypt") || strings.Contains(s, "password")
}
