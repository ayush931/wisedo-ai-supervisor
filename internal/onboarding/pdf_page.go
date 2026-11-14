package onboarding

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	ledongthuc "github.com/ledongthuc/pdf"
)

// WIS-625 post-merge decision: a typed PDF keeps its typed text, and each raster figure it draws
// is read by the vision model into its own model-extracted source. A page is classified from its
// content stream, read as a viewer reads it (pdfTokens), with the graphics state a viewer keeps
// for what matters here: the transformation matrix (where an image lands), the text render mode
// (whether text shows) and rectangular clips (what part of an image shows).

// Figure thresholds. An image is read only when it is large in pixels and on the page: at least
// minFigureSide pixels on its shorter side (below that a line of text scanned at 150 dpi is under
// 20 px tall, so it holds at most a word or two: an icon, a logo, a bullet) and at least
// minFigureCover of the visible page (a 2 in x 0.75 in header logo on Letter is 1.6%, a 2 in x 2 in
// figure 4.3%). A smaller image is skipped and counted (images_skipped), never silently dropped.
const (
	minFigureSide  = 100
	minFigureCover = 0.02
	// maxDocumentImages bounds the figures read from one PDF: each is one budgeted model call, as a
	// scanned PDF's page is (MaxScanPages).
	maxDocumentImages = MaxScanPages
)

// pageContent is what a page's own content stream does.
type pageContent struct {
	shows, invisible int  // text-show operators, and those in render mode 3
	painted          bool // a path painted, or a shading
	inline           bool // an inline image (BI)
	unknown          bool // an operator outside PDF's set, or malformed operands
	badOperands      bool // an operator whose operands are not its types (operandsMatch): refused
	blendSet         bool // a path or shading was painted under a blend mode other than Normal
	hidden           bool // text a viewer may not show: modes 4-7 or no mode, /OC, a token read two ways
	draws            []imageDraw
	paints           []paintEvent
	ops              []string        // every operator, in order, as walkContent read it
	texts            map[int]textInk // the state each text-show operator paints with
	opClips          map[int]opClip  // the clip each painting operator paints under, recorded or not
}

// opClip is the clip a painting operator paints under.
type opClip struct {
	clip  [4]float64
	known bool
}

// textInk is what a text-show operator paints with: its render mode, whether it paints opaque and
// dark enough to be read (ink), its stroke's half width on the page, and the clip it is under.
type textInk struct {
	tr        int
	ink       bool
	lum       float64 // the luminance of the colour it paints with
	lumG      float64 // and through the page group's colour space
	blend     bool    // shown under a blend mode other than Normal
	halfWidth float64
	clip      [4]float64
	clipKnown bool
}

// colour is a fill or stroke colour as far as legibility goes: its luminance, when it is in a
// device colour space (ok); any other space is not known.
type colour struct {
	space string
	lum   float64
	lumG  float64 // its luminance as shown through the page group's colour space (= lum without one)
	ok    bool
}

// groupLuminance is a device colour's luminance after conversion to the page group's blending
// space by ISO 32000-1's device conversions (10.4): to DeviceGray 0.3 R + 0.59 G + 0.11 B, or
// 1 - min(1, 0.3 C + 0.59 M + 0.11 Y + K); to DeviceRGB a grey as itself, CMYK as
// 1 - min(1, C + K) and so on. Without a group space it is the device luminance (WIS-625 Codex
// round 24: grey conversion does not keep luminance, so two colours apart in RGB can meet in grey).
func groupLuminance(space string, v []float64, group string) (float64, bool) {
	lum, ok := luminance(space, v)
	if !ok || group == "" {
		return lum, ok
	}
	switch {
	case group == "DeviceGray" && space == "DeviceGray":
		return v[0], true
	case group == "DeviceGray" && space == "DeviceRGB":
		return 0.3*v[0] + 0.59*v[1] + 0.11*v[2], true
	case group == "DeviceGray" && space == "DeviceCMYK":
		return 1 - min(1, 0.3*v[0]+0.59*v[1]+0.11*v[2]+v[3]), true
	case group == "DeviceRGB" && space == "DeviceCMYK":
		return luminance("DeviceRGB", []float64{1 - min(1, v[0]+v[3]), 1 - min(1, v[1]+v[3]), 1 - min(1, v[2]+v[3])})
	case group == "DeviceRGB":
		return lum, true
	}
	return 0, false
}

// darkInk is the luminance below which text counts as legible ink: black and dark colours on a
// white page. Lighter text (white on white, pale grey) may be invisible and is refused.
const darkInk = 0.6

// extGStateKeys are the ExtGState entries walkContent models or that cannot change what is
// painted on screen when they hold the values it accepts. Any other (TR, TR2, BG, UCR, HT, Font
// and the rest) refuses the page.
var extGStateKeys = map[string]bool{"Type": true, "LW": true, "LC": true, "LJ": true, "ML": true, "D": true, "RI": true, "FL": true, "SM": true, "SA": true,
	"OP": true, "op": true, "OPM": true, "ca": true, "CA": true, "BM": true, "SMask": true, "AIS": true, "TK": true}

// imageDraw is one `/Name Do`, operator op: the unit square mapped by m, under the clip then in
// effect.
type imageDraw struct {
	name      string
	op        int
	m         [6]float64
	clip      [4]float64
	clipKnown bool    // false after a clip that is not one axis-aligned rectangle
	blended   bool    // a blend mode or soft mask in force at the draw
	alpha     float64 // the fill (non-stroking) alpha in force at the draw
	blank     bool    // a form shown to paint nothing (parsePDF, by drawsImage)
}

// paintEvent is one opaque paint, operator op, over box (page space): a filled or stroked path
// (stroke widened by half its line width, and to the miter limit at joins) or a shading over the
// clip (whole: the clip is unknown, so the whole page). Do draws are in draws.
type paintEvent struct {
	op    int
	box   [4]float64
	whole bool
	light bool // an opaque, unblended paint in a known device colour of luminance at least lightPaint
	// thick is how far the paint reaches across its narrow side (a stroke: its segment plus the
	// line width; a fill: its box), and lum its colour when known (lumKnown), for rules.
	thick     float64
	lum       float64
	lumG      float64
	lumKnown  bool
	solid     bool // a filled rectangle, opaque and unblended: it paints all of box within clip
	clip      [4]float64
	clipKnown bool
	// box32 and clip32 are the same paint in the other reading (float32 for a float64 paint and
	// the reverse), when parsePDF paired it (paired).
	box32, clip32 [4]float64
	paired        bool
}

// lightPaint is the luminance a paint under text must reach for the text to stay legible: light
// cell shading passes, a dark fill, an image or a shading under text does not.
const lightPaint = 0.75

type gstate struct {
	ctm                    [6]float64
	tr                     int // text render mode, 0 to 7
	clip                   [4]float64
	clipKnown              bool
	lineWidth, mlim        float64
	join, cap              int // line join and cap styles, 0 to 2
	fill, stroke           colour
	fillAlpha, strokeAlpha float64
	blend, mask            bool // a blend mode other than Normal or Compatible; a soft mask other than None
	dashed                 bool // a dash array that is not empty: a stroke shows only in part
}

// blended reports a blend mode or soft mask in force: what a paint shows depends on what is under it.
func (g gstate) blended() bool { return g.blend || g.mask }

// deviceSpace resolves a colour space operand to the device space its colours are judged in: a
// device space itself, or an ICCBased space whose profile is a listed standard one (iccProfiles)
// with 1, 3 or 4 components, through its /Alternate device space or, without one, the device
// space its component count implies. Anything else
// (Indexed, Separation, DeviceN, Pattern, a calibrated space) is not judged ("").
func deviceSpace(name string, spaces ledongthuc.Value) string {
	name = strings.TrimPrefix(name, "/")
	if name == "DeviceGray" || name == "DeviceRGB" || name == "DeviceCMYK" {
		return name
	}
	cs := spaces.Key(name)
	if cs.Kind() != ledongthuc.Array || cs.Len() != 2 || cs.Index(0).Name() != "ICCBased" || cs.Index(1).Kind() != ledongthuc.Stream {
		return ""
	}
	icc := cs.Index(1)
	implied := map[int64]string{1: "DeviceGray", 3: "DeviceRGB", 4: "DeviceCMYK"}[icc.Key("N").Int64()]
	// Only a standard profile, by its hash, is judged as its device space (iccProfiles).
	b, err := io.ReadAll(io.LimitReader(icc.Reader(), maxICCProfile+1))
	sum := sha256.Sum256(b)
	if n, ok := iccProfiles[hex.EncodeToString(sum[:])]; err != nil || len(b) > maxICCProfile || !ok || n != icc.Key("N").Int64() {
		return ""
	}
	if alt := icc.Key("Alternate"); !alt.IsNull() {
		if alt.Kind() != ledongthuc.Name || alt.Name() != implied {
			return "" // an alternate of another kind is not a simple mapping
		}
	}
	// Components range over 0..1 as in the device space, or the mapping is not simple.
	if r := icc.Key("Range"); !r.IsNull() {
		if r.Kind() != ledongthuc.Array || int64(r.Len()) != 2*icc.Key("N").Int64() {
			return ""
		}
		for i := 0; i < r.Len(); i++ {
			if r.Index(i).Float64() != float64(i%2) || r.Index(i).Kind() != ledongthuc.Integer && r.Index(i).Kind() != ledongthuc.Real {
				return ""
			}
		}
	}
	return implied
}

// luminance of a device colour from its components (0..1).
func luminance(space string, v []float64) (float64, bool) {
	for _, c := range v {
		if c < 0 || c > 1 {
			return 0, false
		}
	}
	switch {
	case space == "DeviceGray" && len(v) == 1:
		return v[0], true
	case space == "DeviceRGB" && len(v) == 3:
		return 0.2126*v[0] + 0.7152*v[1] + 0.0722*v[2], true
	case space == "DeviceCMYK" && len(v) == 4:
		r, g, b := (1-v[0])*(1-v[3]), (1-v[1])*(1-v[3]), (1-v[2])*(1-v[3])
		return 0.2126*r + 0.7152*g + 0.0722*b, true
	}
	return 0, false
}

// pdfOperators are every content operator PDF defines that this walk accepts. BI ends the walk.
var pdfOperators = map[string]bool{}

func init() {
	for _, op := range strings.Fields("q Q cm w J j M d ri i gs m l c v y h re n W W* S s f F f* B B* b b* sh CS cs SC SCN sc scn G g RG rg K k BX EX MP DP BMC BDC EMC BT ET Tc Tw Tz TL Tf Tr Ts Td TD Tm T* Tj TJ ' \" d0 d1 Do BI") {
		pdfOperators[op] = true
	}
}

// noOperands are the operators that take none: operands before one are not what a viewer reads.
var noOperands = map[string]bool{}

func init() {
	for _, op := range strings.Fields("q Q BT ET W W* n S s f F f* B B* b b* h EMC BX EX T* BI") {
		noOperands[op] = true
	}
}

// walkContent reads a page's tokens. visible is the visible page, the initial clip; states is the
// page's ExtGState resources.
//
// group is the page group's blending colour space ("DeviceGray" or "DeviceRGB"; "" for none):
// colours are judged as shown through it as well as in device space.
//
// With f32 every operand and every computed coordinate (CTM, path points, stroke reach, paint and
// clip boxes) is rounded to float32 at each step, as a reader computing in float32 (MuPDF) holds
// it; parsePDF compares that walk with the float64 one (WIS-625 Codex round 27).
func walkContent(tokens []string, visible [4]float64, states, spaces ledongthuc.Value, userUnit float64, group string, f32 bool) (pc pageContent) {
	f := func(v float64) float64 { return v }
	if f32 {
		f = func(v float64) float64 { return float64(float32(v)) }
		// The page's boxes as a float32 reader holds them too (round 28).
		visible = [4]float64{f(visible[0]), f(visible[1]), f(visible[2]), f(visible[3])}
	}
	black := colour{space: "DeviceGray", ok: true}
	g := gstate{ctm: [6]float64{1, 0, 0, 1, 0, 0}, tr: 0, clip: visible, clipKnown: true, lineWidth: 1, mlim: 10, join: 0, cap: 0,
		fill: black, stroke: black, fillAlpha: 1, strokeAlpha: 1}
	pc.texts = map[int]textInk{}
	pc.opClips = map[int]opClip{}
	var stack []gstate
	var operands []string
	inText := false     // within BT ... ET
	compat := 0         // BX ... EX depth
	var path [4]float64 // bounding box of the current path, in page space
	hasPath, rectOnly, pathRects, segments, clipNext, pathAxis := false, true, 0, 0, false, true
	var pathCTM [6]float64
	pathMixed := false
	// A stroke paints only along the path: each segment's box, in page space.
	var segs [][4]float64
	var cur, start [2]float64
	point := func(x, y float64) [2]float64 {
		return [2]float64{f(f(f(g.ctm[0]*x)+f(g.ctm[2]*y)) + g.ctm[4]), f(f(f(g.ctm[1]*x)+f(g.ctm[3]*y)) + g.ctm[5])}
	}
	seg := func(pts ...[2]float64) {
		b := [4]float64{pts[0][0], pts[0][1], pts[0][0], pts[0][1]}
		for _, q := range pts[1:] {
			b = [4]float64{min(b[0], q[0]), min(b[1], q[1]), max(b[2], q[0]), max(b[3], q[1])}
		}
		segs = append(segs, b)
	}
	nums := func(n int) ([]float64, bool) {
		if len(operands) != n {
			return nil, false
		}
		out := make([]float64, n)
		for i, t := range operands {
			if !pdfNumber.MatchString(t) {
				return nil, false
			}
			v, _ := strconv.ParseFloat(t, 64)
			out[i] = f(v)
		}
		return out, true
	}
	// integer reads one integer-valued operand in [lo, hi] (WIS-625 Codex round 13: "+0" and "00"
	// are 0, compared as numbers, never as token text); anything else refuses the file.
	integer := func(lo, hi int) (int, bool) {
		v, ok := nums(1)
		if !ok || v[0] != math.Trunc(v[0]) || v[0] < float64(lo) || v[0] > float64(hi) {
			pc.badOperands = true
			return 0, false
		}
		return int(v[0]), true
	}
	addPoint := func(x, y float64) {
		if !axisAligned(g.ctm) {
			pathAxis = false // built under a rotation or shear: its outline is not its bounding box
		}
		// The transform each point was built under: a path keeps its geometry through Q, and a
		// rectangle built under a shear has corners that are not right angles.
		if !hasPath {
			pathCTM = g.ctm
		} else if g.ctm != pathCTM {
			pathMixed = true
		}
		q := point(x, y)
		px, py := q[0], q[1]
		if !hasPath {
			path, hasPath = [4]float64{px, py, px, py}, true
			return
		}
		path = [4]float64{min(path[0], px), min(path[1], py), max(path[2], px), max(path[3], py)}
	}
	endPath := func() {
		if clipNext {
			// A clip of no area stays so, whatever is intersected with it next (WIS-625 Codex
			// round 21): nothing inside it shows.
			empty := g.clipKnown && !(g.clip[0] < g.clip[2] && g.clip[1] < g.clip[3])
			switch {
			case empty:
			case hasPath && rectOnly && pathRects == 1 && pathAxis:
				g.clip = [4]float64{max(g.clip[0], path[0]), max(g.clip[1], path[1]), min(g.clip[2], path[2]), min(g.clip[3], path[3])}
			default:
				g.clipKnown = false
			}
		}
		hasPath, rectOnly, pathRects, segments, clipNext, pathAxis, pathMixed = false, true, 0, 0, false, true, false
		segs = segs[:0]
	}
	for _, t := range tokens {
		// A name is read decoded (#xx), as the resource keys it looks up are: "/Im#31" is /Im1,
		// and "/O#43" is /OC.
		if t[0] == '/' {
			name, fine := decodeName(t[1:])
			if !fine {
				pc.unknown = true
			}
			t = "/" + name
		}
		switch {
		case t == "/OC":
			pc.hidden = true // optional content: a viewer may switch it off
		case t != "Tr" && strings.HasSuffix(t, "Tr"),
			strings.IndexByte("0123456789+-.", t[0]) >= 0 && !pdfNumber.MatchString(t):
			pc.hidden = true // "3Tr" is "3 Tr" to PDF.js
		}
		if !pdfOperator(t) {
			operands = append(operands, t)
			continue
		}
		op := len(pc.ops)
		pc.ops = append(pc.ops, t)
		// A text object holds only text, colour, general graphics state and marked-content
		// operators (ISO 32000-1, 9.4, Figure 9): q, Q and cm inside one are kept with the text
		// matrices by one reader and apart from them by another (WIS-625 Codex round 26), and
		// paths, XObjects, shadings and inline images do not belong there. BT ... ET must balance.
		switch {
		case t == "BT":
			if inText {
				pc.badOperands = true
			}
			inText = true
		case t == "ET":
			if !inText {
				pc.badOperands = true
			}
			inText = false
		case inText && notInText[t]:
			pc.badOperands = true
		case !inText && textOnly[t]:
			// Showing or positioning text outside a text object: the extractor keeps the text
			// matrices across it, another reader does not (WIS-625 Codex round 27).
			pc.badOperands = true
		}
		// Inside BX ... EX a reader ignores an operator it does not know, with its operands
		// (ISO 32000-1, 8.2); known ones are read as everywhere. Unbalanced sections refuse.
		switch {
		case t == "BX":
			compat++
		case t == "EX":
			if compat--; compat < 0 {
				pc.badOperands = true
			}
		case !pdfOperators[t] && compat > 0 && splitByViewer(t):
			// PDF.js reads "TrZz" as Tr then Zz (it extends an operator only while a known one
			// begins so): inside BX ... EX, where this walk ignores an unknown word, that would hide
			// a real operator (WIS-625 Codex round 14). Outside a section any unknown word already
			// leaves the page unread (pc.unknown), which refuses the upload.
			pc.badOperands = true
		case !pdfOperators[t] && compat > 0:
			operands = operands[:0]
			continue
		}
		if !operandsMatch(t, tokenKinds(operands)) {
			pc.badOperands = true
		}
		// No number a page shows by is larger than maxCoordinate (WIS-625 Codex round 25).
		for _, o := range operands {
			if v, err := strconv.ParseFloat(o, 64); err == nil && pdfNumber.MatchString(o) && math.Abs(v) > maxCoordinate {
				pc.badOperands = true
			}
		}
		if !pdfOperators[t] || noOperands[t] && len(operands) > 0 {
			pc.unknown = true // "/Im1\vDo Q" is a name before Q, and draws nothing a viewer shows
		}
		switch t {
		case "d0", "d1":
			// Only a Type 3 glyph procedure may hold these, and PDF.js clips what follows d1 to its
			// box: in a page or form they could hide text this walk reads (WIS-625 Codex round 15).
			pc.badOperands = true
		case "q":
			stack = append(stack, g)
		case "Q":
			if len(stack) == 0 {
				pc.unknown = true
			} else {
				g, stack = stack[len(stack)-1], stack[:len(stack)-1]
			}
		case "cm":
			v, ok := nums(6)
			if !ok {
				pc.unknown = true
				break
			}
			c := g.ctm
			for _, p := range []float64{v[0] * c[0], v[1] * c[2], v[0] * c[1], v[1] * c[3], v[2] * c[0], v[3] * c[2], v[2] * c[1], v[3] * c[3], v[4] * c[0], v[5] * c[2], v[4] * c[1], v[5] * c[3]} {
				if math.Abs(p) > maxCoordinate {
					pc.badOperands = true
				}
			}
			g.ctm = [6]float64{f(f(v[0]*c[0]) + f(v[1]*c[2])), f(f(v[0]*c[1]) + f(v[1]*c[3])), f(f(v[2]*c[0]) + f(v[3]*c[2])), f(f(v[2]*c[1]) + f(v[3]*c[3])),
				f(f(f(v[4]*c[0])+f(v[5]*c[2])) + c[4]), f(f(f(v[4]*c[1])+f(v[5]*c[3])) + c[5])}
			// A transform that shrinks area below 1e-6 is refused before a float32 reader's
			// products underflow (round 26); the float32 walk compares everything else (round 27).
			if math.Abs(g.ctm[0]*g.ctm[3]-g.ctm[1]*g.ctm[2]) < minDeterminant {
				pc.badOperands = true
			}
			for _, f := range g.ctm {
				if math.Abs(f) > maxCoordinate {
					pc.badOperands = true
				}
			}
			for _, f := range g.ctm {
				if !finite(f) {
					pc.hidden = true
				}
			}
		case "w", "M":
			v, ok := nums(1)
			if !ok || v[0] < 0 {
				pc.unknown = true
			} else if t == "w" {
				g.lineWidth = v[0]
			} else {
				g.mlim = v[0]
			}
		case "d":
			// "[ ] phase" is solid; anything in the array dashes the stroke.
			g.dashed = !(len(operands) == 3 && operands[0] == "[" && operands[1] == "]")
		case "j", "J":
			if v, ok := integer(0, 2); ok && t == "j" {
				g.join = v
			} else if ok {
				g.cap = v
			}
		case "g", "G", "rg", "RG", "k", "K":
			space := map[string]string{"g": "DeviceGray", "rg": "DeviceRGB", "k": "DeviceCMYK"}[strings.ToLower(t)]
			v, _ := nums(len(operands))
			lum, fine := luminance(space, v)
			lumG, fineG := groupLuminance(space, v, group)
			c := colour{space: space, lum: lum, lumG: lumG, ok: fine && fineG && len(v) == len(operands)}
			if t == strings.ToLower(t) {
				g.fill = c
			} else {
				g.stroke = c
			}
		case "cs", "CS":
			c := colour{}
			if len(operands) == 1 {
				if space := deviceSpace(operands[0], spaces); space != "" {
					// The initial colour (ISO 32000-1, Table 74): black in DeviceGray, DeviceRGB and
					// DeviceCMYK (0 0 0 1); every component 0 in an ICCBased space, which is white
					// for 4 components.
					initial := map[string][]float64{"DeviceGray": {0}, "DeviceRGB": {0, 0, 0}, "DeviceCMYK": {0, 0, 0, 1}}[space]
					if strings.TrimPrefix(operands[0], "/") != space {
						initial = make([]float64, len(initial))
					}
					lum, _ := luminance(space, initial)
					lumG, fineG := groupLuminance(space, initial, group)
					c = colour{space: space, lum: lum, lumG: lumG, ok: fineG}
				}
			}
			if t == "cs" {
				g.fill = c
			} else {
				g.stroke = c
			}
		case "sc", "scn", "SC", "SCN":
			c := &g.fill
			if t == "SC" || t == "SCN" {
				c = &g.stroke
			}
			v, fine := nums(len(operands))
			lum, known := luminance(c.space, v)
			lumG, knownG := groupLuminance(c.space, v, group)
			*c = colour{space: c.space, lum: lum, lumG: lumG, ok: c.ok && fine && known && knownG}
		case "gs":
			if len(operands) != 1 || operands[0][0] != '/' {
				pc.unknown = true
				break
			}
			st := states.Key(operands[0][1:])
			if st.Kind() != ledongthuc.Dict {
				pc.unknown = true
				break
			}
			for _, key := range st.Keys() {
				v := st.Key(key)
				number := func() (float64, bool) {
					if v.Kind() != ledongthuc.Integer && v.Kind() != ledongthuc.Real || !finite(v.Float64()) {
						return 0, false
					}
					return v.Float64(), true
				}
				switch key {
				case "LW", "ML", "LJ", "LC", "ca", "CA":
					n, fine := number()
					switch {
					case !fine || n < 0:
						pc.unknown = true
					case (key == "LJ" || key == "LC") && (n != math.Trunc(n) || n > 2):
						pc.badOperands = true
					case key == "LW":
						g.lineWidth = n
					case key == "ML":
						g.mlim = n
					case key == "LJ":
						g.join = int(n)
					case key == "LC":
						g.cap = int(n)
					case key == "ca":
						g.fillAlpha = n
					default:
						g.strokeAlpha = n
					}
				case "BM":
					// Each state sets the mode: a later /Normal clears an earlier /Multiply
					// (WIS-625 Codex round 16). An array of modes is not judged.
					g.blend = v.Kind() != ledongthuc.Name || v.Name() != "Normal" && v.Name() != "Compatible"
				case "SMask":
					g.mask = v.Kind() != ledongthuc.Name || v.Name() != "None"
				case "D":
					if v.Kind() != ledongthuc.Array || v.Len() != 2 || v.Index(0).Kind() != ledongthuc.Array {
						pc.unknown = true
					} else {
						g.dashed = v.Index(0).Len() > 0
					}
				case "AIS", "OP", "op":
					if v.Kind() != ledongthuc.Bool || v.Bool() {
						pc.hidden = true // alpha as shape, or overprint: what shows depends on more than this walk
					}
				default:
					if !extGStateKeys[key] {
						pc.hidden = true // a transfer function, halftone, black generation, font...
					}
				}
			}
		case "Tr":
			// 0-2 show text, 3 shows nothing (a scanner's layer: decided by the caller); 4-6 add the
			// glyphs to the clip at ET, which this walk does not track, and 7 is clip only. A value
			// outside 0 to 7, or not an integer, refuses the file (integer).
			if v, ok := integer(0, 7); ok {
				g.tr = v
				if v > 3 {
					pc.hidden = true
				}
			}
		case "Tj", "TJ", "'", "\"":
			pc.shows++
			if g.tr == 3 {
				pc.invisible++
			}
			// Legible: painted opaque, unblended, in a dark device colour (the stroke for mode 1,
			// either for mode 2).
			// Dark in device space and as shown through the page group's space, both.
			fillInk := g.fill.ok && g.fill.lum < darkInk && g.fill.lumG < darkInk && g.fillAlpha == 1
			strokeInk := g.stroke.ok && g.stroke.lum < darkInk && g.stroke.lumG < darkInk && g.strokeAlpha == 1 && !g.dashed // a dashed outline shows only in part
			// Mode 2 fills, then strokes over the fill: a pale stroke would paint the fill out, so both
			// must be ink.
			ink := !g.blended() && (g.tr == 0 && fillInk || g.tr == 1 && strokeInk || g.tr == 2 && fillInk && strokeInk)
			half := 0.0
			if g.tr == 1 || g.tr == 2 {
				_, _, h := strokeHalfWidths(g.lineWidth, g.ctm, userUnit)
				half = f(h * math.Sqrt2)
			}
			lum, lumG := g.fill.lum, g.fill.lumG
			if g.tr == 1 {
				lum, lumG = g.stroke.lum, g.stroke.lumG
			} else if g.tr == 2 {
				lum, lumG = max(g.fill.lum, g.stroke.lum), max(g.fill.lumG, g.stroke.lumG)
			}
			pc.texts[op] = textInk{tr: g.tr, ink: ink, lum: lum, lumG: lumG, blend: g.blend, halfWidth: half, clip: g.clip, clipKnown: g.clipKnown}
		case "Do":
			if len(operands) != 1 || operands[0][0] != '/' {
				pc.unknown = true
				break
			}
			pc.draws = append(pc.draws, imageDraw{name: operands[0][1:], op: op, m: g.ctm, clip: g.clip, clipKnown: g.clipKnown, blended: g.blended(), alpha: g.fillAlpha})
		case "BI":
			pc.inline = true
			return pc // its data follows; nothing after it is read
		case "m", "l":
			if v, ok := nums(2); ok {
				addPoint(v[0], v[1])
				q := point(v[0], v[1])
				if t == "l" {
					seg(cur, q)
				} else {
					start = q
				}
				cur = q
			} else {
				pc.unknown = true
			}
			rectOnly = false
			segments++
		case "c", "v", "y":
			n := 6
			if t != "c" {
				n = 4
			}
			if v, ok := nums(n); ok {
				pts := [][2]float64{cur}
				for i := 0; i < n; i += 2 {
					addPoint(v[i], v[i+1])
					pts = append(pts, point(v[i], v[i+1]))
				}
				seg(pts...) // a Bezier curve lies within its control points
				cur = pts[len(pts)-1]
			} else {
				pc.unknown = true
			}
			rectOnly = false
			segments += 2 // a curve bends: treat it as a join
		case "re":
			if v, ok := nums(4); ok {
				// The far corner is x + w and y + h as the reader adds them: in float32 that sum
				// is rounded before the transform (WIS-625 Codex round 34: a 0.003-high strip at
				// y 65536 has no height there).
				x1, y1 := f(v[0]+v[2]), f(v[1]+v[3])
				addPoint(v[0], v[1])
				addPoint(x1, v[1])
				addPoint(v[0], y1)
				addPoint(x1, y1)
				pathRects++
				a, b, c, d := point(v[0], v[1]), point(x1, v[1]), point(x1, y1), point(v[0], y1)
				seg(a, b)
				seg(b, c)
				seg(c, d)
				seg(d, a)
				cur, start = a, a
			} else {
				pc.unknown = true
			}
		case "h":
			seg(cur, start)
			cur = start
		case "W", "W*":
			clipNext = true
		case "n":
			endPath()
		case "S", "s", "f", "F", "f*", "B", "B*", "b", "b*":
			pc.opClips[op] = opClip{clip: g.clip, known: g.clipKnown}
			if t == "s" || t == "b" || t == "b*" { // close the subpath first, as h does
				seg(cur, start)
			}
			// A paint shows only within a known rectangular clip: its box is cut to it, and a paint
			// clipped away entirely paints nothing (WIS-625 Codex round 17). Under an unknown clip
			// the whole box stays.
			clipped := func(b [4]float64) ([4]float64, bool) {
				if !g.clipKnown {
					return b, true
				}
				c := [4]float64{max(b[0], g.clip[0]), max(b[1], g.clip[1]), min(b[2], g.clip[2]), min(b[3], g.clip[3])}
				return c, c[0] < c[2] && c[1] < c[3] // a zero-area remainder paints nothing (round 19)
			}
			if hasPath {
				fillLight := g.fill.ok && g.fill.lum >= lightPaint && g.fill.lumG >= lightPaint && g.fillAlpha == 1 && !g.blended()
				strokeLight := g.stroke.ok && g.stroke.lum >= lightPaint && g.stroke.lumG >= lightPaint && g.strokeAlpha == 1 && !g.blended()
				// A paint of alpha 0 under normal blending paints nothing (round 21); fill and stroke
				// each by their own alpha.
				fillShows := g.fillAlpha != 0 // alpha 0 paints nothing under any blend (round 26)
				strokeShows := g.strokeAlpha != 0
				if box, shows := clipped(path); shows && fillShows && t != "S" && t != "s" { // filled: the whole path
					pc.painted = true // only a paint the clip leaves shows (WIS-625 Codex round 18)
					pc.blendSet = pc.blendSet || g.blend
					pc.paints = append(pc.paints, paintEvent{op: op, box: box, light: fillLight, thick: min(box[2]-box[0], box[3]-box[1]),
						lum: g.fill.lum, lumG: g.fill.lumG, lumKnown: g.fill.ok && g.fillAlpha == 1 && !g.blended(), solid: rectOnly && pathRects == 1 && pathAxis && g.fillAlpha == 1 && !g.blended(), clip: g.clip, clipKnown: g.clipKnown})
				}
				if t != "f" && t != "F" && t != "f*" { // stroked: each segment, widened by half the
					// line width scaled by the CTM, and at a miter join up to the miter limit (a
					// rectangle's right-angle corners reach sqrt 2).
					// The pen's circle (or square) through the CTM reaches at most half the width times
					// its largest singular value.
					reach := math.Sqrt2
					// A miter join reaches up to the miter limit, except at a right angle: one rectangle,
					// built and stroked under one transform that keeps right angles (a similarity). A
					// rectangle built under a shear, or under another transform than the stroke's (a
					// path survives Q), has sharper corners (WIS-625 Codex round 18).
					// An axis-aligned transform (a scale per axis, or a quarter turn) keeps the corners
					// of an axis-aligned rectangle right angles; a similarity is not needed (round 19).
					// A similarity (a uniform scale with a rotation) keeps them too (round 20).
					rightAngles := pathRects == 1 && segments == 0 && !pathMixed && pathCTM == g.ctm && (axisAligned(g.ctm) || similarity(g.ctm))
					if g.join == 0 && !rightAngles && (segments > 2 || pathRects > 0) {
						reach = max(g.mlim, reach)
					}
					// One straight segment has no join: under a similarity (no shear, uniform scale), with
					// a butt or round cap or lying along an axis, it reaches half its width; a projecting
					// square cap on a slant, or a sheared pen, reaches further.
					if len(segs) == 1 && pathRects == 0 && similarity(g.ctm) && (g.cap == 0 || g.cap == 1 || segs[0][0] == segs[0][2] || segs[0][1] == segs[0][3]) {
						reach = 1
					}
					// The pen's reach on each page axis: a circle of that radius through the CTM extends
					// hypot(a, c) across and hypot(b, d) up. A renderer draws any stroke at least a device
					// pixel wide whatever its width and CTM (a 0 w hairline under ".01 0 0 .01 0 0 cm"
					// included), so each side reaches at least half a point: one pixel at 72 dpi
					// (WIS-625 Codex round 19).
					// The floor applies to the pen before any join reaches past it (round 20).
					hx, hy, h := strokeHalfWidths(g.lineWidth, g.ctm, userUnit)
					ex, ey, width := f(hx*reach), f(hy*reach), 2*h
					for _, b := range segs {
						box, shows := clipped([4]float64{f(b[0] - ex), f(b[1] - ey), f(b[2] + ex), f(b[3] + ey)})
						if !shows || !strokeShows {
							continue
						}
						pc.painted = true
						pc.blendSet = pc.blendSet || g.blend
						pc.paints = append(pc.paints, paintEvent{op: op, box: box, light: strokeLight,
							thick: min(b[2]-b[0], b[3]-b[1]) + width, lum: g.stroke.lum, lumG: g.stroke.lumG, lumKnown: g.stroke.ok && g.strokeAlpha == 1 && !g.blended() && !g.dashed, clip: g.clip, clipKnown: g.clipKnown})
					}
				}
			}
			endPath()
		case "sh":
			pc.opClips[op] = opClip{clip: g.clip, known: g.clipKnown}
			// A shading fills the clip; a clip of no area leaves it nothing (round 20).
			// At fill alpha 0 under normal blending it paints nothing (round 22).
			if (!g.clipKnown || g.clip[0] < g.clip[2] && g.clip[1] < g.clip[3]) && g.fillAlpha != 0 {
				pc.painted = true
				pc.blendSet = pc.blendSet || g.blend
				pc.paints = append(pc.paints, paintEvent{op: op, box: g.clip, whole: !g.clipKnown, clip: g.clip, clipKnown: g.clipKnown})
			}
		}
		operands = operands[:0]
	}
	if compat != 0 || inText {
		pc.badOperands = true
	}
	// Every box, clip, transform and reach the checks decide by must be a number: a NaN compares
	// false everywhere and would let paint or text pass unchecked (security review of round 21).
	finiteAll := func(vs ...float64) bool {
		for _, v := range vs {
			if !finite(v) {
				return false
			}
		}
		return true
	}
	for _, pe := range pc.paints {
		if !finiteAll(pe.box[:]...) || !finiteAll(pe.clip[:]...) || !finiteAll(pe.thick) {
			pc.badOperands = true
		}
	}
	for _, d := range pc.draws {
		if !finiteAll(d.m[:]...) || !finiteAll(d.clip[:]...) {
			pc.badOperands = true
		}
	}
	for _, ti := range pc.texts {
		if !finiteAll(ti.halfWidth) || !finiteAll(ti.clip[:]...) {
			pc.badOperands = true
		}
	}
	if len(operands) > 0 {
		pc.unknown = true // operands with no operator: "/Im1\vDo" is one name, and does nothing a viewer shows
	}
	return pc
}

// splitByViewer reports an operator word a viewer may not read as one unknown operator: one that a
// known operator begins, or with a character outside those vendor operators use.
func splitByViewer(t string) bool {
	for _, c := range t {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '*' || c == '\'' || c == '"') {
			return true
		}
	}
	for op := range pdfOperators {
		if len(op) < len(t) && strings.HasPrefix(t, op) {
			return true
		}
	}
	return false
}

// numberIs reports a numeric operand token whose value is v ("+3" and "03" are 3): operands are
// compared as numbers, never as text (WIS-625 Codex round 13).
func numberIs(t string, v float64) bool {
	if !pdfNumber.MatchString(t) {
		return false
	}
	f, err := strconv.ParseFloat(t, 64)
	return err == nil && f == v
}

// operandSignatures are the operand types of every operator the walk or the glyph pass
// interprets, as kinds (operandKind): N a number, / a name, S a string, A an array, D a
// dictionary. A reader that takes another type (PDF.js reads "(10000)" as 10000 where Float64
// reads 0) may place or paint what neither check saw, so a mismatch refuses the file (WIS-625
// Codex round 12). Array elements are checked by arrayElements. Operators that take no operands
// are noOperands' (an operand before one is a token read two ways: hidden, not this refusal).
var operandSignatures = map[string]*regexp.Regexp{}

var arrayElements = map[string]*regexp.Regexp{"TJ": regexp.MustCompile(`^[SN]*$`), "d": regexp.MustCompile(`^N*$`)}

func init() {
	for sig, ops := range map[string]string{
		`^N$`:                "w M J j i Tc Tw Tz TL Ts Tr g G",
		`^NN$`:               "m l Td TD",
		`^NNN$`:              "rg RG",
		`^NNNN$`:             "re v y k K",
		`^NNNNNN$`:           "cm Tm c",
		`^/$`:                "Do gs ri cs CS sh MP BMC",
		`^/N$`:               "Tf",
		`^/[/D]$`:            "BDC DP",
		`^S$`:                "Tj '",
		`^NNS$`:              "\"",
		`^A$`:                "TJ",
		`^AN$`:               "d",
		`^N{1,4}$`:           "sc SC",
		`^(N{1,4}|N{0,4}/)$`: "scn SCN",
	} {
		for _, op := range strings.Fields(ops) {
			operandSignatures[op] = regexp.MustCompile(sig)
		}
	}
}

// operandKinds are an operator's operands as kinds, and each array's elements.
type operandKinds struct {
	kinds  string
	arrays []string
}

// tokenKinds reads walkContent's operand tokens as kinds: a nested array, or a word that is not a
// number (true, null), is X, which no signature takes.
func tokenKinds(operands []string) operandKinds {
	var k operandKinds
	for i := 0; i < len(operands); i++ {
		switch t := operands[i]; {
		case t == "[":
			var elems strings.Builder
			for i++; i < len(operands) && operands[i] != "]"; i++ {
				elems.WriteByte(tokenKind(operands[i]))
			}
			if i >= len(operands) {
				k.kinds += "X"
				return k
			}
			k.kinds += "A"
			k.arrays = append(k.arrays, elems.String())
		case t == "<<":
			depth := 1
			for i++; i < len(operands) && depth > 0; i++ {
				if operands[i] == "<<" {
					depth++
				} else if operands[i] == ">>" {
					depth--
				}
			}
			i--
			k.kinds += "D"
			if depth != 0 {
				k.kinds += "X"
			}
		default:
			k.kinds += string(tokenKind(t))
		}
	}
	return k
}

func tokenKind(t string) byte {
	switch {
	case t == "()" || t == "<>":
		return 'S'
	case t[0] == '/':
		return '/'
	case pdfNumber.MatchString(t):
		return 'N'
	}
	return 'X'
}

// valueKinds reads the extractor's operands (ledongthuc values) as kinds.
func valueKinds(args []ledongthuc.Value) operandKinds {
	var k operandKinds
	kind := func(v ledongthuc.Value) byte {
		switch v.Kind() {
		case ledongthuc.Integer, ledongthuc.Real:
			return 'N'
		case ledongthuc.Name:
			return '/'
		case ledongthuc.String:
			return 'S'
		case ledongthuc.Array:
			return 'A'
		case ledongthuc.Dict:
			return 'D'
		}
		return 'X'
	}
	for _, v := range args {
		c := kind(v)
		k.kinds += string(c)
		if c == 'A' {
			var elems strings.Builder
			for i := 0; i < v.Len(); i++ {
				e := kind(v.Index(i))
				if e == 'A' || e == 'D' {
					e = 'X'
				}
				elems.WriteByte(e)
			}
			k.arrays = append(k.arrays, elems.String())
		}
	}
	return k
}

// operandsMatch reports whether op's operands are of its types; an operator outside the table is
// not judged here (walkContent marks it unknown).
func operandsMatch(op string, k operandKinds) bool {
	sig, ok := operandSignatures[op]
	if !ok {
		return true
	}
	if !sig.MatchString(k.kinds) {
		return false
	}
	for _, a := range k.arrays {
		if e, ok := arrayElements[op]; !ok || !e.MatchString(a) {
			return false
		}
	}
	return true
}

// textOnDark reports text drawn over something it may not be legible on (Codex round 2 review):
// an earlier paint under its advance (descent to ascent) that is not a light opaque fill or
// stroke in a known colour, or an earlier image, form or shading. Light cell shading passes.
func textOnDark(p ledongthuc.Page, pc pageContent, spans []textSpan) bool {
	xobjects := p.Resources().Key("XObject")
	// repainted reports that a light, solid fill drawn after op and before the text paints over all
	// of region: what op left there no longer shows.
	// Only where its rectangular clip lets it paint; a soft mask, blend or transparency is not solid,
	// and optional content refuses the page before this (walkContent).
	// mark is the whole dark paint (whole: its extent is unknown): a light paint that contains all
	// of it exactly erased it, wherever the margins put it (WIS-625 Codex round 31).
	repainted := func(op, before int, region, mark [4]float64, whole bool) bool {
		for _, pe := range pc.paints {
			if pe.op > op && pe.op < before && pe.light && pe.solid && pe.clipKnown {
				light := clipTo(pe.box, pe.clip)
				if !whole && mark[0] >= light[0] && mark[1] >= light[1] && mark[2] <= light[2] && mark[3] <= light[3] || withinShrunk(region, light) {
					return true
				}
			}
		}
		return false
	}
	for _, sp := range spans {
		// An underline or strike-through drawn before the text is a decoration, as one drawn after
		// it is (ruleBand, at most one in each band; WIS-625 Codex round 40).
		// Pieces that line up are one rule (joinRules); the first rule in each band is exempt.
		var pieces [3][]paintEvent
		var at [3][]int
		for i, pe := range pc.paints {
			if band := ruleBefore(pc, pe, sp); band != 0 {
				pieces[band], at[band] = append(pieces[band], pe), append(at[band], i)
			}
		}
		exempt := map[int]bool{}
		for band := 1; band <= 2; band++ {
			if _, members, ok := joinRules(band, pieces[band], sp, pc.texts[sp.op]); ok && len(members) > 0 {
				for _, m := range members[0] {
					exempt[at[band][m]] = true
				}
			}
		}
		for i, pe := range pc.paints {
			if pe.op < sp.op && !pe.light && (pe.whole || touchesGlyph(sp, pe.box, pc.texts[sp.op].halfWidth)) {
				if exempt[i] {
					continue
				}
				region := sp.under
				if !pe.whole {
					region = clipTo(sp.under, pe.box)
				}
				if !repainted(pe.op, sp.op, region, pe.box, pe.whole) {
					return true
				}
			}
		}
		for _, d := range pc.draws {
			if d.op < sp.op {
				if box, whole, shows := drawExtent(xobjects, d); shows && (whole || touchesGlyph(sp, box, pc.texts[sp.op].halfWidth)) {
					region := sp.under
					if !whole {
						region = clipTo(sp.under, box)
					}
					if !repainted(d.op, sp.op, region, box, whole) {
						return true
					}
				}
			}
		}
	}
	return false
}

// ruleBand reports the band a paint over upright text lies in when it is an underline or a
// strike-through, which cannot hide the text: a horizontal mark at most a tenth of an em thick, in
// a known colour as dark as the text or darker, wholly in the underline band (below the baseline,
// within a quarter em) or the strike-through band (0.2 to 0.45 em above it). 0 is no band.
func ruleBand(pe paintEvent, sp textSpan, ink textInk) int {
	b := pe.box
	if pe.whole || !sp.upright || !pe.lumKnown || pe.lum > ink.lum || pe.lumG > ink.lumG || b[3]-b[1] > 0.1*sp.em || b[2]-b[0] <= b[3]-b[1] {
		return 0
	}
	band := 0
	switch {
	case b[3] <= sp.baseline && b[1] >= sp.baseline-0.25*sp.em:
		band = 1
	case b[1] >= sp.baseline+0.2*sp.em && b[3] <= sp.baseline+0.45*sp.em:
		band = 2
	}
	// A rule hides nothing only if every glyph it crosses keeps at least 30% of its ink height
	// outside it: a 9.5-point bar can cover a hyphen or a dash entirely within the strike band
	// (WIS-625 Codex round 38).
	for _, g := range sp.inks {
		if band == 0 || !overlaps(g, b) {
			continue
		}
		h := g[3] - g[1]
		outside := max(0, min(g[3], b[1]-decisionMargin)-g[1]) + max(0, g[3]-max(g[1], b[3]+decisionMargin))
		if outside < 0.3*h {
			return 0
		}
	}
	return band
}

// boldRedraws are the text operators that redraw an earlier operator's same text within 0.05 em
// (simulated bold): they hide nothing, and their text is stored once (parsePDF drops the redraw).
//
// Each redraw is measured against the original it repeats, never another redraw (a chain of small
// steps would walk the text anywhere), and an original has at most maxBoldRedraws.
//
// It maps each redraw's operator to its own original's: the exemption covers that pair only
// (WIS-625 Codex round 36: by text alone a large "1" redrawn over its original also covered a
// small unrelated "1" nearby).
func boldRedraws(spans []textSpan) map[int]int {
	const maxBoldRedraws = 4
	out := map[int]int{}
	var originals []int // indexes into spans
	count := map[int]int{}
	for i, sp := range spans {
		redraw := false
		for _, o := range originals {
			orig := spans[o]
			// The same glyphs at the same size and shape: the same font, codes and text, and a
			// rendering matrix with singular values within 2%, moved by at most 0.05 em.
			if orig.op < sp.op && sp.text == orig.text && sp.codes == orig.codes && sp.font == orig.font && count[o] < maxBoldRedraws &&
				sameMatrix(sp.matrix, orig.matrix) && samePens(sp, orig) &&
				math.Abs(sp.tight[0]-orig.tight[0]) <= 0.05*orig.size && math.Abs(sp.tight[1]-orig.tight[1]) <= 0.05*orig.size {
				count[o]++
				out[sp.op], redraw = orig.op, true
				break
			}
		}
		if !redraw {
			originals = append(originals, i)
		}
	}
	return out
}

// sameMatrix reports two rendering matrices equal up to translation: each linear term within 2% of
// the larger term, so the same size, sign and orientation (not mirrored or rotated).
func sameMatrix(a, b [4]float64) bool {
	scale := max(math.Abs(b[0]), math.Abs(b[1]), math.Abs(b[2]), math.Abs(b[3]))
	for i := range a {
		if math.Abs(a[i]-b[i]) > 0.02*scale {
			return false
		}
	}
	return true
}

// samePens reports each code of a redraw within 0.05 em of the original's code.
func samePens(sp, orig textSpan) bool {
	if len(sp.pens) != len(orig.pens) {
		return false
	}
	for i := range sp.pens {
		if math.Abs(sp.pens[i][0]-orig.pens[i][0]) > 0.05*orig.size || math.Abs(sp.pens[i][1]-orig.pens[i][1]) > 0.05*orig.size {
			return false
		}
	}
	return true
}

// maxCoverageChecks bounds the glyph-by-paint comparisons on one page; past it the page cannot be
// checked in the time budget and is refused.
const maxCoverageChecks = 20_000_000

// maxSingular is the largest singular value of m's linear part: the most it stretches any length.
func maxSingular(m [6]float64) float64 {
	a, b, c, d := m[0], m[1], m[2], m[3]
	sq := a*a + b*b + c*c + d*d
	det := a*d - b*c
	return math.Sqrt((sq + math.Sqrt(max(0, sq*sq-4*det*det))) / 2)
}

// similarity reports m's linear part is a rotation (or reflection) times a uniform scale.
func similarity(m [6]float64) bool {
	a, b, c, d := m[0], m[1], m[2], m[3]
	eps := 1e-9 * max(math.Abs(a), math.Abs(b), math.Abs(c), math.Abs(d), 1e-300)
	return math.Abs(a-d) <= eps && math.Abs(b+c) <= eps || math.Abs(a+d) <= eps && math.Abs(b-c) <= eps
}

// minDeterminant is the least area scale a CTM may have: below it a float32 reader's products
// underflow (WIS-625 Codex round 26).
const minDeterminant = 1e-6

// textOnly are the text-showing and text-positioning operators, which only a text object may hold;
// text-state operators (Tf, Tc, Tw, Tz, TL, Tr, Ts) are allowed outside one.
var textOnly = map[string]bool{"Tj": true, "TJ": true, "'": true, "\"": true, "Td": true, "TD": true, "Tm": true, "T*": true}

// notInText are the operators a text object may not hold (ISO 32000-1, Figure 9).
var notInText = map[string]bool{}

func init() {
	for _, op := range strings.Fields("q Q cm m l c v y h re S s f F f* B B* b b* n W W* sh Do BI d0 d1") {
		notInText[op] = true
	}
}

// maxCoordinate bounds every number a page is drawn by: each operand, each term and component of
// the CTM and of the text matrices (1e5 units, about 35 m). Within it, a reader computing in
// float32 (MuPDF) lands within about 0.01 unit of these float64 sums; beyond it, translations of
// 1073741824, 1000 and -1073741824 give 1000 here and 1024 there, so text on the page here is off
// it there (WIS-625 Codex round 25). No real page comes near it.
const maxCoordinate = 1e5

// minStrokeHalf is the least a stroke reaches on each side: half a physical point, so one device
// pixel at 72 dpi, which renderers draw for any thinner stroke.
const minStrokeHalf = 0.5

// strokeHalfWidths is the one measure of a stroke's pen on the page (WIS-625 Codex rounds 19 and
// 20), for paths, stroked text and its legibility and overprint checks: half the declared width
// through the CTM, across (hx: a circle through the CTM reaches hypot(a, c)), up (hy: hypot(b, d))
// and at most (h: the largest singular value), each at least minStrokeHalf physical points. A
// page unit is userUnit points (userUnit at most 1), so the floor is minStrokeHalf/userUnit units.
// Joins and caps reach past this; the floor is applied first.
func strokeHalfWidths(lineWidth float64, ctm [6]float64, userUnit float64) (hx, hy, h float64) {
	floor := minStrokeHalf / userUnit
	hw := 0.5 * lineWidth
	hx, hy, h = max(hw*math.Hypot(ctm[0], ctm[2]), floor), max(hw*math.Hypot(ctm[1], ctm[3]), floor), max(hw*maxSingular(ctm), floor)
	// Under a shear or a non-uniform scale, a pen thinner than a pixel across its narrowest
	// direction is widened by the renderer in user space until it is a pixel there, so it grows
	// along the transform's major axis (PDF.js draws "1 0 10 1 cm 0 w" about 7 pt to each side;
	// WIS-625 Codex round 21). The pen is taken as a circle of floor/minSV user units through the
	// CTM: no cap, so a nearly degenerate transform reaches far and refuses nearby text.
	if big, small := maxSingular(ctm), minSingular(ctm); !similarity(ctm) && hw*small < floor {
		// A singular CTM has no such pen: infinite, never NaN (Inf * 0), and the walk refuses
		// any non-finite geometry (security review of round 21).
		if !(small > 0) || !finite(small) {
			inf := math.Inf(1)
			return inf, inf, inf
		}
		r := floor / small
		hx, hy, h = max(hx, r*math.Hypot(ctm[0], ctm[2])), max(hy, r*math.Hypot(ctm[1], ctm[3])), max(h, r*big)
	}
	return hx, hy, h
}

// minSingular is the smaller singular value of m's linear part: |det| over the larger one.
func minSingular(m [6]float64) float64 {
	big := maxSingular(m)
	if big == 0 {
		return 0
	}
	return math.Abs(m[0]*m[3]-m[1]*m[2]) / big
}

// axisAligned reports a transform that maps the page axes onto the page axes: a scale on each
// axis (b = c = 0) or a quarter turn with one (a = d = 0).
func axisAligned(m [6]float64) bool { return m[1] == 0 && m[2] == 0 || m[0] == 0 && m[3] == 0 }

// clipTo is the part of a inside b.
func clipTo(a, b [4]float64) [4]float64 {
	return [4]float64{max(a[0], b[0]), max(a[1], b[1]), min(a[2], b[2]), min(a[3], b[3])}
}

// decodeName decodes a PDF name's #xx escapes (ISO 32000 7.3.5); fine is false for a bad one.
func decodeName(raw string) (string, bool) {
	if !strings.Contains(raw, "#") {
		return raw, true
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '#' {
			b.WriteByte(raw[i])
			continue
		}
		if i+2 >= len(raw) {
			return "", false
		}
		v, err := strconv.ParseUint(raw[i+1:i+3], 16, 8)
		if err != nil || v == 0 {
			return "", false
		}
		b.WriteByte(byte(v))
		i += 2
	}
	return b.String(), true
}

// drawExtent is what a Do paints over: an image's unit square, a form's BBox through its Matrix,
// or (a form whose extent cannot be read) the whole page; within a known rectangular clip, only
// what lies inside it, and nothing (shows false) when the clip leaves none (WIS-625 Codex round 18).
func drawExtent(xobjects ledongthuc.Value, d imageDraw) (box [4]float64, whole, shows bool) {
	// Drawn at alpha 0 under normal blending, it paints nothing (round 22; a knockout group,
	// where it would erase what is under it, refuses the page in parsePDF).
	// At alpha 0 a draw paints nothing under any blend mode (a knockout group, where it would
	// erase, is refused; round 26), and a form that paints nothing covers nothing (round 23).
	if d.alpha == 0 || d.blank {
		return box, false, false
	}
	x := xobjects.Key(d.name)
	box = drawnBox(d.m)
	if x.Key("Subtype").Name() == "Form" {
		var ok bool
		box, ok = formBox(x, d.m)
		whole = !ok
	}
	if !d.clipKnown {
		return box, whole, true
	}
	if whole {
		return d.clip, false, true
	}
	c := [4]float64{max(box[0], d.clip[0]), max(box[1], d.clip[1]), min(box[2], d.clip[2]), min(box[3], d.clip[3])}
	return c, false, c[0] < c[2] && c[1] < c[3]
}

// textCovered reports a glyph that something painted later lies over (WIS-625 post-merge
// review): a filled or stroked path, a shading, an image or a form. A viewer shows the paint, the
// extractor still reads the glyph, so the text is refused rather than stored. A paint before the
// text (cell shading, a letterhead) does not cover it. ok is false when the page is too large to
// check.
func textCovered(p ledongthuc.Page, pc pageContent, spans []textSpan) (covered, ok bool) {
	xobjects := p.Resources().Key("XObject")
	paints := append([]paintEvent(nil), pc.paints...)
	for _, d := range pc.draws {
		if box, whole, shows := drawExtent(xobjects, d); shows {
			paints = append(paints, paintEvent{op: d.op, box: box, whole: whole})
		}
	}
	if len(spans)*(len(paints)+len(spans)) > maxCoverageChecks {
		return false, false
	}
	bold := boldRedraws(spans)
	checks := 0
	for si, sp := range spans {
		ink := pc.texts[sp.op]
		// Paint later over it covers it, but for at most one underline and one strike-through,
		// which do not overlap each other and together cover at most 0.15 em of its height.
		var rules [3][]paintEvent
		for _, pe := range paints {
			if pe.op <= sp.op || !pe.whole && !touchesGlyph(sp, pe.box, pc.texts[sp.op].halfWidth) {
				continue
			}
			band := ruleBand(pe, sp, ink)
			if band == 0 {
				return true, true
			}
			rules[band] = append(rules[band], pe)
		}
		// Rules drawn before the text count with those drawn after: one per band in all, their
		// heights together, and each crossed glyph keeping 30% of its ink outside all of them
		// (security review of round 40: a strike rule on each side together hid a minus).
		for _, pe := range pc.paints {
			if band := ruleBefore(pc, pe, sp); band != 0 {
				rules[band] = append(rules[band], pe)
			}
		}
		for band := 1; band <= 2; band++ {
			joined, _, ok := joinRules(band, rules[band], sp, ink)
			if !ok {
				return true, true
			}
			rules[band] = joined
		}
		for _, g := range sp.inks {
			var spans [][2]float64 // the glyph's heights the rules crossing it cover
			for _, band := range rules[1:] {
				for _, pe := range band {
					if overlaps(g, pe.box) {
						spans = append(spans, [2]float64{max(g[1], pe.box[1]-decisionMargin), min(g[3], pe.box[3]+decisionMargin)})
					}
				}
			}
			if covered := unionLength(spans); len(spans) > 0 && g[3]-g[1]-covered < 0.3*(g[3]-g[1]) {
				return true, true
			}
		}
		height := 0.0
		for band := 1; band <= 2; band++ {
			if len(rules[band]) > 1 {
				return true, true
			}
			for _, pe := range rules[band] {
				height += max(0, min(pe.box[3], sp.tight[3])-max(pe.box[1], sp.tight[1]))
			}
		}
		if len(rules[1]) == 1 && len(rules[2]) == 1 && overlapsInclusive(rules[1][0].box, rules[2][0].box) || height > 0.15*sp.em {
			return true, true
		}
		// Later text drawn over it, code by code: each code's advance cell (its advance to its ink
		// width, from the font box's or glyph's bottom to top, widened by a stroked span's half line
		// width) against each earlier code's. On one line,
		// text set next to text may overlap by kerning (0.15 em) along the baseline; across lines
		// nothing may overlap. A redraw of the same text within 0.05 em (simulated bold) hides
		// nothing.
		es := pc.texts[sp.op].halfWidth
		for li, later := range spans {
			// A bold group (an original and its redraws) is exempt within itself: a redraw against its
			// original or against another redraw of it (WIS-625 Codex round 37).
			group := sp.op
			if o, isRedraw := bold[sp.op]; isRedraw {
				group = o
			}
			if orig, isRedraw := bold[later.op]; later.op <= sp.op || isRedraw && orig == group {
				continue
			}
			e := pc.texts[later.op].halfWidth
			grow := func(b [4]float64, by float64) [4]float64 {
				return [4]float64{b[0] - by, b[1] - by, b[2] + by, b[3] + by}
			}
			if !overlaps(grow(later.box, e), grow(sp.box, es)) {
				continue
			}
			// Kerning is measured along the baseline in the horizontal em (with Tz), and never
			// more than half a glyph's ink width.
			sameLine := sp.upright && later.upright && math.Abs(sp.baseline-later.baseline) <= 0.01*sp.em
			kern := 0.15 * min(sp.hem, later.hem)
			checks += len(later.pens) * len(sp.pens)
			if checks > maxCoverageChecks {
				return false, false
			}
			// The next run on the line (italic "(f) Tj (i) Tj") meets the last code of this one as
			// two codes of one operator meet: this code's glyph may overhang into the next one's
			// place, so it is compared by its advance alone; every other pair by glyph box too
			// (WIS-625 Codex round 18).
			// At one size the two runs meet as codes of one operator do, so the first code of the
			// later run is compared by its advance too (Times-BoldItalic "(o) Tj (f) Tj": the f
			// overhangs left; round 19). At different sizes the later glyph keeps its box: a large
			// f set after small text reaches back over it (round 17).
			adjacent := sameLine && li == si+1
			sameSize := math.Abs(sp.em-later.em) <= 0.01*max(math.Abs(sp.em), math.Abs(later.em))
			for a, lc0 := range later.pens {
				for b, ec := range sp.pens {
					lc := lc0
					if adjacent && a == 0 && b == len(sp.pens)-1 {
						ec = sp.advs[b]
						if sameSize {
							lc = later.advs[a]
						}
					}
					lc = grow(lc, e)
					ec = grow(ec, es)
					dx := min(lc[2], ec[2]) - max(lc[0], ec[0])
					dy := min(lc[3], ec[3]) - max(lc[1], ec[1])
					tol := 0.0
					if sameLine {
						tol = min(kern, 0.5*(lc[2]-lc[0]), 0.5*(ec[2]-ec[0]))
					}
					if dx > tol-decisionMargin && dy > -decisionMargin {
						return true, true
					}
				}
			}
		}
	}
	return false, true
}

// joinRules joins a band's rule pieces that line up end to end into one rule each: the same
// bottom and top within the decision margin, touching or overlapping along x (an underline drawn
// as adjoining fills; WIS-625 Codex round 43). Pieces at another height stay separate rules. Each
// joined rule must itself be a rule of the band (ruleBand: thickness, colour, 30% of each glyph's
// ink); ok is false when one is not. members lists each rule's pieces by index into pieces.
func joinRules(band int, pieces []paintEvent, sp textSpan, ink textInk) (rules []paintEvent, members [][]int, ok bool) {
	order := make([]int, len(pieces))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return pieces[order[a]].box[0] < pieces[order[b]].box[0] })
	m := decisionMargin
	for _, i := range order {
		pe := pieces[i]
		joined := false
		for r := range rules {
			b := rules[r].box
			if math.Abs(pe.box[1]-b[1]) <= m && math.Abs(pe.box[3]-b[3]) <= m && pe.box[0] <= b[2]+m {
				rules[r].box = [4]float64{b[0], min(b[1], pe.box[1]), max(b[2], pe.box[2]), max(b[3], pe.box[3])}
				rules[r].lum, rules[r].lumG = max(rules[r].lum, pe.lum), max(rules[r].lumG, pe.lumG)
				rules[r].lumKnown = rules[r].lumKnown && pe.lumKnown
				members[r] = append(members[r], i)
				joined = true
				break
			}
		}
		if !joined {
			rules = append(rules, paintEvent{op: pe.op, box: pe.box, lum: pe.lum, lumG: pe.lumG, lumKnown: pe.lumKnown})
			members = append(members, []int{i})
		}
	}
	for _, r := range rules {
		if ruleBand(r, sp, ink) != band {
			return nil, nil, false
		}
	}
	return rules, members, true
}

// ruleBefore is the band of a rule drawn before the text that still shows over it, 0 for any other
// paint. textOnDark and textCovered both count earlier rules through it, so a rule erased before
// the text counts in neither (WIS-625 Codex rounds 41 and 42).
func ruleBefore(pc pageContent, pe paintEvent, sp textSpan) int {
	if pe.op >= sp.op || pe.whole || pe.light || !touchesGlyph(sp, pe.box, pc.texts[sp.op].halfWidth) || erasedBefore(pc, pe, sp.op) {
		return 0
	}
	return ruleBand(pe, sp, pc.texts[sp.op])
}

// erasedBefore reports a paint wholly repainted by a later opaque light solid fill under a known
// clip before op: it shows nothing and is not counted as a rule (WIS-625 Codex round 41). The fill
// must contain the paint in both readings, each exactly (round 43: an exact repaint erases); a
// paint or fill the other reading lacks is unpaired and erases nothing.
func erasedBefore(pc pageContent, pe paintEvent, op int) bool {
	contains := func(c, b [4]float64) bool { return b[0] >= c[0] && b[1] >= c[1] && b[2] <= c[2] && b[3] <= c[3] }
	for _, l := range pc.paints {
		if l.op > pe.op && l.op < op && l.light && l.solid && l.clipKnown && l.paired && pe.paired &&
			contains(clipTo(l.box, l.clip), pe.box) && contains(clipTo(l.box32, l.clip32), pe.box32) {
			return true
		}
	}
	return false
}

// unionLength is the total length of the union of intervals.
func unionLength(iv [][2]float64) float64 {
	sort.Slice(iv, func(i, j int) bool { return iv[i][0] < iv[j][0] })
	total, end := 0.0, math.Inf(-1)
	for _, v := range iv {
		if v[1] <= v[0] {
			continue
		}
		lo := max(v[0], end)
		if v[1] > lo {
			total += v[1] - lo
		}
		end = max(end, v[1])
	}
	return total
}

// touchesGlyph reports that box meets one of the span's glyph cells (each the union of the
// glyph's own box, a standard font's AFM box or else the font box, and its advance), widened by
// the stroke's half width e: the span's whole box would count the space between glyphs, where a
// TJ gap ("[(A) -8000 (B)] TJ") leaves room for a divider (WIS-625 Codex round 39).
func touchesGlyph(sp textSpan, box [4]float64, e float64) bool {
	if len(sp.cells) == 0 {
		return overlapsInclusive(sp.box, box)
	}
	for _, c := range sp.cells {
		if overlapsInclusive([4]float64{c[0] - e, c[1] - e, c[2] + e, c[3] + e}, box) {
			return true
		}
	}
	return false
}

// drawnBox is the page-space bounding box of the unit square under m.
func drawnBox(m [6]float64) [4]float64 {
	xs := []float64{m[4], m[0] + m[4], m[2] + m[4], m[0] + m[2] + m[4]}
	ys := []float64{m[5], m[1] + m[5], m[3] + m[5], m[1] + m[3] + m[5]}
	return [4]float64{min(xs[0], xs[1], xs[2], xs[3]), min(ys[0], ys[1], ys[2], ys[3]), max(xs[0], xs[1], xs[2], xs[3]), max(ys[0], ys[1], ys[2], ys[3])}
}

// overlaps reports that a and b may share area, by a margin: two readings of one page differ by up
// to 0.01 unit on each edge, so a decision within 2 x 0.01 of flipping is taken the cautious way
// (WIS-625 Codex round 28).
func overlaps(a, b [4]float64) bool {
	m := decisionMargin
	return a[0] < b[2]+m && b[0] < a[2]+m && a[1] < b[3]+m && b[1] < a[3]+m
}

// decisionMargin is how far apart two readings of an edge may be (0.01 unit each): overlap tests
// widen by it and containment tests of text (withinStrict) shrink by it.
const decisionMargin = 0.02

// withinShrunk reports that region, shrunk by decisionMargin, lies inside area: a light repaint
// covers a dark region unless more than the readings' disagreement of it stays uncovered, and an
// exact repaint covers (WIS-625 Codex round 29; shrinking the light area instead refused it).
//
// An empty region is never covered, and the two must overlap. A region under 20 margins (0.4
// unit) on an axis must lie wholly inside area as it is, so at most a tenth of a larger one is
// left to the margin, or a white strip beside a black hairline would cover it (WIS-625
// Codex round 30).
func withinShrunk(region, area [4]float64) bool {
	m := decisionMargin
	if !(region[0] < region[2] && region[1] < region[3]) || !overlapsExact(region, area) {
		return false
	}
	// Under 20 margins on an axis, shrinking by a margin could leave over a tenth of it uncovered
	// (security review of round 30: strips 0.07 high tiled under text): exact containment then.
	if region[2]-region[0] < 20*m || region[3]-region[1] < 20*m {
		return region[0] >= area[0] && region[1] >= area[1] && region[2] <= area[2] && region[3] <= area[3]
	}
	return region[0]+m >= area[0] && region[1]+m >= area[1] && region[2]-m <= area[2] && region[3]-m <= area[3]
}

// overlapsExact reports that a and b share area, with no margin.
func overlapsExact(a, b [4]float64) bool {
	return a[0] < b[2] && b[0] < a[2] && a[1] < b[3] && b[1] < a[3]
}

// withinStrict reports that box lies inside area shrunk by decisionMargin: text shown inside the
// page or a clip by both readings, not only by this one (WIS-625 Codex round 28).
func withinStrict(box, area [4]float64) bool {
	m := decisionMargin
	return box[0] >= area[0]+m && box[1] >= area[1]+m && box[2] <= area[2]-m && box[3] <= area[3]-m
}

// within reports that box lies inside area, up to a tolerance for rounded coordinates: 1e-4 of
// area's side but at most 0.01 unit, so a huge page (14400 units) cannot hide a 1 pt word in it.
func within(box, area [4]float64) bool {
	tx, ty := min(1e-4*(area[2]-area[0]), maxRounding), min(1e-4*(area[3]-area[1]), maxRounding)
	return box[0] >= area[0]-tx && box[1] >= area[1]-ty && box[2] <= area[2]+tx && box[3] <= area[3]+ty
}

// maxRounding is the most a coordinate is taken to be off by rounding, in page units.
const maxRounding = 0.01

// visibleBox is the page area a viewer shows: the MediaBox within the CropBox (inherited).
func visibleBox(p ledongthuc.Page) ([4]float64, bool) {
	visible, ok := pdfBox(inherited(p, "MediaBox"))
	if !ok {
		return visible, false
	}
	if c := inherited(p, "CropBox"); !c.IsNull() {
		crop, ok := pdfBox(c)
		if !ok {
			return visible, false
		}
		visible = [4]float64{max(visible[0], crop[0]), max(visible[1], crop[1]), min(visible[2], crop[2]), min(visible[3], crop[3])}
	}
	return visible, true
}

// pageFigures decides the images a page draws: a figure (read by the vision model) when large
// enough, else skipped. A figure must show whole and plainly, as a scan page must (plainScanPage):
// placed upright and axis-aligned inside the visible page and its clip, with nothing painted over
// it and no transparency or soft mask in the page's graphics states. Otherwise the model would
// read pixels the page hides, and the page is refused (hidden is true). A form drawn is not an
// image (the caller decides forms); any other XObject is unknown (unknown is true).
func pageFigures(p ledongthuc.Page, pc pageContent, visible [4]float64, spans []textSpan, userUnit float64) (figures []string, placed [][2]float64, skipped int, hidden, unknown bool) {
	sizes := map[string][2]float64{}
	xobjects := p.Resources().Key("XObject")
	area := (visible[2] - visible[0]) * (visible[3] - visible[1])
	qualifies := map[string]bool{}
	var order []string
	// covered: anything painted after a draw and over it, a path, a shading, another image or form
	// (by its BBox), or shown text, hides part of it (security review).
	covered := func(i int) bool {
		d := pc.draws[i]
		box := drawnBox(d.m)
		for _, pe := range pc.paints {
			if pe.op > d.op && (pe.whole || overlaps(box, pe.box)) {
				return true
			}
		}
		for _, later := range pc.draws[i+1:] {
			if b, whole, shows := drawExtent(xobjects, later); shows && (whole || overlaps(box, b)) {
				return true
			}
		}
		for _, sp := range spans {
			if sp.op > d.op && touchesGlyph(sp, box, pc.texts[sp.op].halfWidth) {
				return true
			}
		}
		return false
	}
	for i, d := range pc.draws {
		x := xobjects.Key(d.name)
		switch x.Key("Subtype").Name() {
		case "Form":
			continue
		case "Image":
		default:
			unknown = true
			continue
		}
		// A draw the clip leaves nothing of shows nothing, and one partly clipped shows only what
		// the clip leaves: it qualifies by that (WIS-625 Codex round 19). A partly clipped figure
		// is still hidden below (the model would read all of it).
		shown, _, shows := drawExtent(xobjects, d)
		if !shows {
			continue
		}
		w, h := x.Key("Width").Float64(), x.Key("Height").Float64()
		m := d.m
		cover := min(math.Abs(m[0]*m[3]-m[1]*m[2]), (shown[2]-shown[0])*(shown[3]-shown[1]))
		if _, seen := qualifies[d.name]; !seen {
			order = append(order, d.name)
			qualifies[d.name] = false
		}
		if min(w, h) < minFigureSide || area <= 0 || cover < minFigureCover*area {
			continue
		}
		qualifies[d.name] = true
		// A figure drawn more than once is sent at its largest placement (in points, 1/72 inch).
		if prior, has := sizes[d.name]; !has || m[0]*m[3] > prior[0]*prior[1]/(userUnit*userUnit) {
			sizes[d.name] = [2]float64{m[0] * userUnit, m[3] * userUnit}
		}
		box := drawnBox(m)
		// Placed in another shape than its pixels by more than 2:1 (a page of text squashed into a
		// strip), a figure shows what the vision model would read in full: refused.
		aspect := (m[0] / m[3]) / (w / h)
		if m[0] <= 0 || m[3] <= 0 || m[1] != 0 || m[2] != 0 || !within(box, visible) || !d.clipKnown || !within(box, d.clip) || covered(i) || !(aspect <= 2 && aspect >= 0.5) ||
			d.blended || d.alpha < 1 { // as drawn: blended or translucent, it shows more than its pixels
			hidden = true
		}
	}
	for _, name := range order {
		if qualifies[name] {
			figures = append(figures, name)
			placed = append(placed, sizes[name])
		} else {
			skipped++
		}
	}
	if len(figures) > 0 {
		// A Default colour space replaces the image's device space on display (as plainScanPage refuses).
		spaces := p.Resources().Key("ColorSpace")
		for _, k := range []string{"DefaultGray", "DefaultRGB", "DefaultCMYK"} {
			if !spaces.Key(k).IsNull() {
				hidden = true
			}
		}
		// Blend, soft mask and alpha are judged as in force at each draw (above), not as declared
		// in the resources: a state set and reset, or never used, changes nothing (WIS-625 Codex
		// round 17).
	}
	return figures, placed, skipped, hidden, unknown
}

// formBox is the page-space bounding box of a form's BBox under its Matrix and then m.
func formBox(x ledongthuc.Value, m [6]float64) ([4]float64, bool) {
	b, ok := pdfBox(x.Key("BBox"))
	if !ok {
		return b, false
	}
	f := [6]float64{1, 0, 0, 1, 0, 0}
	if mv := x.Key("Matrix"); !mv.IsNull() {
		if mv.Kind() != ledongthuc.Array || mv.Len() != 6 {
			return b, false
		}
		for i := range f {
			e := mv.Index(i)
			if e.Kind() != ledongthuc.Integer && e.Kind() != ledongthuc.Real || !finite(e.Float64()) {
				return b, false
			}
			f[i] = e.Float64()
		}
	}
	// The BBox's corners through f then m.
	t := [6]float64{f[0]*m[0] + f[1]*m[2], f[0]*m[1] + f[1]*m[3], f[2]*m[0] + f[3]*m[2], f[2]*m[1] + f[3]*m[3], f[4]*m[0] + f[5]*m[2] + m[4], f[4]*m[1] + f[5]*m[3] + m[5]}
	out := [4]float64{}
	for i, c := range [][2]float64{{b[0], b[1]}, {b[2], b[1]}, {b[0], b[3]}, {b[2], b[3]}} {
		x, y := t[0]*c[0]+t[2]*c[1]+t[4], t[1]*c[0]+t[3]*c[1]+t[5]
		if i == 0 {
			out = [4]float64{x, y, x, y}
			continue
		}
		out = [4]float64{min(out[0], x), min(out[1], y), max(out[2], x), max(out[3], y)}
	}
	return out, true
}
