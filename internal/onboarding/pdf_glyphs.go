package onboarding

import (
	"math"
	"strings"
	"unicode/utf8"

	ledongthuc "github.com/ledongthuc/pdf"
)

// opGlyph is one glyph the extractor reads, with the index of the content operator that showed
// it (WIS-625 post-merge review). The index is the same in walkContent, whose operator sequence is
// checked equal to the extractor's, so a glyph and a later paint over it can be ordered.
type opGlyph struct {
	ledongthuc.Text
	op        int
	separator bool // the line break the extractor adds after a TJ: no code was shown
}

type textMatrix [3][3]float64

var identity = textMatrix{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}

func (x textMatrix) mul(y textMatrix) textMatrix {
	var z textMatrix
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			for k := 0; k < 3; k++ {
				t := x[i][k] * y[k][j]
				if math.Abs(t) > maxCoordinate {
					panic(operandMismatch{}) // a reader in float32 would place it elsewhere (round 25)
				}
				z[i][j] += t
			}
			if math.Abs(z[i][j]) > maxCoordinate {
				panic(operandMismatch{})
			}
		}
	}
	return z
}

// operandMismatch is raised when the extractor's operands are not of the operator's types.
type operandMismatch struct{}

// pageGlyphs reads a page's content with the extractor's own interpreter (ledongthuc.Interpret,
// which joins a content array without a separator) and places each glyph exactly as
// ledongthuc's Page.Content does (the same operators and the same arithmetic), so the text stored
// is the text Content would give. It also returns the operator sequence the interpreter saw.
// Malformed operands panic, as in Content; parsePDF turns that into PDF_UNREADABLE.
//
// For overpainting it also returns, per text-show operator, the page-space box its text covers
// (textSpan), placed with the PDF rules ledongthuc's arithmetic leaves out: each character code's
// width from the font (two-byte codes for an Identity-H Type0 font, a simple font's /Widths, or a
// full em for a font without them), word spacing on single-byte code 32, and the whole rendering
// matrix, so rotated, mirrored or sheared text gets its true box. ok is false when a font's widths
// cannot be read that way (a Type3 font, a Type0 font with another CMap, a missing font): its text
// cannot be placed, so the page cannot be checked.
//
// drawn holds the operators that showed at least one character code, whatever the codes decode
// to (WIS-625 Codex round 16): a code a ToUnicode CMap maps to nothing still paints its glyph.
func pageGlyphs(p ledongthuc.Page, userUnit float64) (glyphs []opGlyph, spans []textSpan, ops []string, ok bool, drawn map[int]bool) {
	return pageGlyphsAt(p, userUnit, false)
}

// pageGlyphsAt is pageGlyphs; with f32 every operand, matrix product and advance is rounded to
// float32, as a reader computing in float32 (MuPDF) holds it. parsePDF compares the two readings
// (WIS-625 Codex rounds 26 and 27).
func pageGlyphsAt(p ledongthuc.Page, userUnit float64, f32 bool) (glyphs []opGlyph, spans []textSpan, ops []string, ok bool, drawn map[int]bool) {
	rnd := func(v float64) float64 { return v }
	if f32 {
		rnd = func(v float64) float64 { return float64(float32(v)) }
	}
	// In float32 each multiply and each add is rounded, as a float32 reader computes (WIS-625
	// Codex round 28); the explicit conversions keep Go from fusing them.
	mm := func(a, b textMatrix) textMatrix {
		z := a.mul(b)
		if !f32 {
			return z
		}
		for i := range 3 {
			for j := range 3 {
				acc := float32(0)
				for k := range 3 {
					acc = float32(acc + float32(float32(a[i][k])*float32(b[k][j])))
				}
				z[i][j] = float64(acc)
			}
		}
		return z
	}
	cb := func(x0, x1, y0, y1 float64, m textMatrix) ([4]float64, bool) {
		if !f32 {
			return cellBox(x0, x1, y0, y1, m)
		}
		return cellBox32(x0, x1, y0, y1, m)
	}
	num := func(v ledongthuc.Value) float64 { return rnd(v.Float64()) } // an operand as the reader parses it
	// Advances in float32 arithmetic, each operation rounded, in the float32 reading (round 29).
	mulf := func(a, b float64) float64 {
		if f32 {
			return float64(float32(float32(a) * float32(b)))
		}
		return a * b
	}
	addf := func(a, b float64) float64 {
		if f32 {
			return float64(float32(float32(a) + float32(b)))
		}
		return a + b
	}
	divf := func(a, b float64) float64 {
		if f32 {
			return float64(float32(float32(a) / float32(b)))
		}
		return a / b
	}
	drawn = map[int]bool{}
	strm := p.V.Key("Contents")
	if p.V.IsNull() || strm.Kind() == ledongthuc.Null {
		return nil, nil, nil, true, drawn
	}
	type state struct {
		Tc, Tw, Th, Tl, Tfs, Trise float64
		Tf                         ledongthuc.Font
		fontName                   string
		enc                        ledongthuc.TextEncoding // the decoder, saved and restored with the font
		Tm, Tlm, CTM               textMatrix
		real                       textMatrix // Tm advanced by the PDF rules, for spans
		lineWidth                  float64    // for stroked text (render modes 1 and 2)
		mode                       int64
	}
	g := state{Th: 1, CTM: identity, Tm: identity, Tlm: identity, real: identity, lineWidth: 1}
	// travel is the pen's distance in this text object: beyond maxCoordinate it can drift apart
	// in readers that round differently, and no real page needs it (WIS-625 Codex round 29).
	// Measured on the page: the advance through the text matrix and the CTM (round 30), so small
	// text drawn large under a small CTM is not counted large.
	travel := 0.0
	move := func(tx float64, tm textMatrix) {
		at := tm.mul(g.CTM)
		if travel += math.Abs(tx) * math.Hypot(at[0][0], at[0][1]); travel > maxCoordinate || !finite(travel) {
			panic(operandMismatch{})
		}
	}
	states := p.Resources().Key("ExtGState")
	var stack []state
	op := -1
	ok = true
	metrics := map[string]fontMetrics{}
	var span textSpan
	spanned := false
	// pen and penEnd run along the operator's baseline, in text space, from where it started: the
	// next code's origin, and the furthest end of a code's advance so far (overprint).
	pen, penEnd := 0.0, 0.0
	// place adds string s, shown from g.real, to the operator's span and advances g.real.
	place := func(s string) {
		m, seen := metrics[g.fontName]
		if !seen {
			m = metricsOf(g.Tf)
			metrics[g.fontName] = m
		}
		if !m.ok || m.twoByte && len(s)%2 != 0 {
			ok = false
			return
		}
		// Each code covers where its glyph may be drawn: from its origin over its advance or the
		// font box, whichever is wider (a glyph is not confined to its advance), from the font's
		// descent to its ascent. Each code's text is checked too (codeText).
		inkLo, inkHi := 0.0, 0.0 // the advance the ink may cover: the pen's, or past it to a code's ink width
		// A stroked glyph (modes 1 and 2) reaches half the line width past its outline: in text
		// space along the baseline, that is the page width over the baseline's scale.
		strokeHalf := 0.0
		stroked := g.mode == 1 || g.mode == 2
		at0 := mm(g.real, g.CTM)
		_, _, strokeHalfPage := strokeHalfWidths(g.lineWidth, [6]float64{g.CTM[0][0], g.CTM[0][1], g.CTM[1][0], g.CTM[1][1], 0, 0}, userUnit)
		strokePage := 2 * strokeHalfPage // the outline's full width on the page, at least a pixel
		if stroked && !finite(strokePage) {
			panic(operandMismatch{}) // a singular CTM: no measure, so refused (security review of round 21)
		}
		if hs := math.Hypot(at0[0][0], at0[0][1]); stroked && hs > 0 {
			strokeHalf = 0.5 * strokePage / hs
		}
		k := g.Tfs / 1000 * g.Th
		at := mm(g.real, g.CTM)
		var cells, pens, advs, inks [][4]float64 // each code's page-space cell, glyph-and-advance cell and advance cell, in order
		var decoded strings.Builder
		adv := 0.0
		for i := 0; i < len(s); {
			code, n := int(s[i]), 1
			if m.twoByte {
				code, n = int(s[i])<<8|int(s[i+1]), 2
			}
			w, known := m.width(code)
			text, honest := m.codeText(g.enc, s[i:i+n], code)
			if !known || !honest {
				ok = false
				return
			}
			decoded.WriteString(text)
			// The pen moves by the declared width w, as a viewer moves it; the ink may reach Adobe's
			// width when the font is a standard one that declares less.
			inkW := max(w, m.ink[code])
			// The code's cell: where its glyph may be drawn. For a standard font, Adobe's box of
			// that glyph with the font's ascent and descent; for any other, the font box.
			gx0, gx1, gy0, gy1 := min(0, m.llx), max(inkW, m.urx), min(0, m.descent), max(0, m.ascent)
			if gb, has := m.glyphs[code]; has {
				gx0, gx1, gy0, gy1 = min(0, gb[0]), max(inkW, gb[2]), min(0, m.descent, gb[1]), max(0, m.ascent, gb[3])
			}
			cell, fine := cb(adv+gx0*k, adv+gx1*k, g.Trise+gy0/1000*g.Tfs, g.Trise+gy1/1000*g.Tfs, at)
			if !fine {
				ok = false
				return
			}
			cells = append(cells, cell)
			// Against other text, its advance (to its ink width) from descent to ascent: the font box
			// across would make every word set after another overlap it.
			// For a standard font, the glyph's own box from bottom to top on both sides of the
			// comparison: the font-wide descent would make an accent on the line below overlap it.
			py0, py1 := gy0, gy1
			if gb, has := m.glyphs[code]; has {
				py0, py1 = min(0, gb[1]), max(0, gb[3])
			}
			// Across, the advance united with the glyph's own box where it is known (a standard
			// font's AFM box): an italic f reaches past its advance, and a large one drawn later
			// could cover earlier text whose advances it never meets (WIS-625 Codex round 17).
			// ponytail: another font's box is the union of all its glyphs (Arial's reaches 0.67 em
			// left), which would make every run set after another on a line overlap it; there the
			// advance stays, as part of the embedded-font limit above codeText.
			px0, px1 := min(adv, adv+inkW*k), max(adv, adv+inkW*k)
			if gb, has := m.glyphs[code]; has {
				px0, px1 = min(px0, adv+gb[0]*k, adv+gb[2]*k), max(px1, adv+gb[0]*k, adv+gb[2]*k)
			}
			penCell, fine := cb(px0, px1, g.Trise+py0/1000*g.Tfs, g.Trise+py1/1000*g.Tfs, at)
			if !fine {
				ok = false
				return
			}
			pens = append(pens, penCell)
			advCell, fine := cb(min(adv, adv+inkW*k), max(adv, adv+inkW*k), g.Trise+py0/1000*g.Tfs, g.Trise+py1/1000*g.Tfs, at)
			if !fine {
				ok = false
				return
			}
			advs = append(advs, advCell)
			// A standard font's glyph ink box (AFM), for rules that may cross it (round 38).
			if gb, has := m.glyphs[code]; has && gb[3] > gb[1] && gb[2] > gb[0] {
				inkCell, fine := cb(adv+gb[0]*k, adv+gb[2]*k, g.Trise+gb[1]/1000*g.Tfs, g.Trise+gb[3]/1000*g.Tfs, at)
				if !fine {
					ok = false
					return
				}
				inks = append(inks, inkCell)
			}
			inkLo, inkHi = min(inkLo, adv, adv+inkW*k), max(inkHi, adv, adv+inkW*k)
			// Overprint within the operator: a code that starts back over an earlier code's advance,
			// by more than kerning does (0.15 em), may paint over it (negative Tc or Tw, or a TJ
			// number moving the pen back).
			dir := math.Copysign(1, k)
			start, end := (pen+adv)*dir, (pen+adv+inkW*k)*dir
			if start < penEnd-min(150, 0.5*inkW)*math.Abs(k)+2*strokeHalf {
				span.overprint = true
			}
			penEnd = max(penEnd, end)
			tx := addf(mulf(divf(w, 1000), g.Tfs), g.Tc)
			if !m.twoByte && code == 32 {
				tx = addf(tx, g.Tw)
			}
			move(mulf(tx, g.Th), g.real)
			adv = addf(adv, mulf(tx, g.Th))
			i += n
		}
		// Decoded code by code, the text must be what the extractor decodes from the whole string.
		if whole := s; g.enc != nil {
			whole = g.enc.Decode(s)
			if whole != decoded.String() {
				ok = false
				return
			}
		}
		union := func(a, b [4]float64) [4]float64 {
			return [4]float64{min(a[0], b[0]), min(a[1], b[1]), max(a[2], b[2]), max(a[3], b[3])}
		}
		if len(cells) == 0 {
			return
		}
		box := cells[0]
		for _, c := range cells[1:] {
			box = union(box, c)
		}
		inkLo, inkHi = min(inkLo, adv), max(inkHi, adv)
		tight, fine2 := cb(inkLo, inkHi, g.Trise, g.Trise+0.7*g.Tfs, at)
		if !fine2 {
			ok = false
			return
		}
		under := box
		// The size a reader sees: the smaller singular value of the glyph's rendering matrix, so a
		// singular or nearly singular matrix (text squashed to a line) is too small to read.
		a, b, c, d := g.Tfs*g.Th*at[0][0], g.Tfs*g.Th*at[0][1], g.Tfs*at[1][0], g.Tfs*at[1][1]
		sq := a*a + b*b + c*c + d*d
		det := a*d - b*c
		size := math.Sqrt(max(0, (sq-math.Sqrt(max(0, sq*sq-4*det*det)))/2))
		largest := math.Sqrt((sq + math.Sqrt(max(0, sq*sq-4*det*det))) / 2)
		if !finite(size) || !finite(largest) {
			ok = false
			return
		}
		// Glyphs squashed to under half their size in one direction, however the scaling is split
		// across Tz, Tm and cm, stack or flatten; an outline stroked wider than a tenth of the
		// glyph's size fills it in.
		if size < 0.5*largest || stroked && strokePage > maxStrokeOfSize*size {
			span.squeezed = true
		}
		span.text += decoded.String()
		span.codes += s
		if span.font == "" || span.font == g.fontName {
			span.font = g.fontName
		} else {
			span.font = "\x00mixed" // not one font: no redraw matches it
		}
		span.cells = append(span.cells, cells...)
		span.pens = append(span.pens, pens...)
		span.advs = append(span.advs, advs...)
		span.inks = append(span.inks, inks...)
		if !spanned {
			span.op, span.box, span.tight, span.under, span.size, span.largest, spanned = op, box, tight, under, size, largest, true
			span.matrix = [4]float64{a, b, c, d}
			// Upright text (no rotation, mirror or shear) has a baseline and an em on the page, which
			// underline and strike-through bands are measured from.
			span.upright = at[0][1] == 0 && at[1][0] == 0 && at[0][0] > 0 && at[1][1] > 0 && g.Tfs > 0 && g.Th > 0
			span.baseline, span.em = g.Trise*at[1][1]+at[2][1], g.Tfs*at[1][1]
			// The em along the baseline, with horizontal scaling (Tz): kerning is measured in it.
			span.hem = math.Abs(g.Tfs*g.Th) * math.Hypot(at[0][0], at[0][1])
			// (The em along the baseline already includes Tz, Tm and cm.)
		} else {
			span.box, span.tight, span.under = union(span.box, box), union(span.tight, tight), union(span.under, under)
			span.size = min(span.size, size)
			span.largest = max(span.largest, largest)
		}
		g.real = mm(textMatrix{{1, 0, 0}, {0, 1, 0}, {adv, 0, 1}}, g.real)
		pen += adv
	}
	showText := func(s string) {
		if len(s) > 0 {
			drawn[op] = true
		}
		n := 0
		decoded := s
		if g.enc != nil {
			decoded = g.enc.Decode(s)
		}
		names := metrics[g.fontName].names
		for _, ch := range decoded {
			var w0 float64
			if n < len(s) {
				w0 = g.Tf.Width(int(s[n]))
				// The extractor's raw C1 code for a WinAnsi glyph becomes the glyph's character.
				if code := int(s[n]); ch == rune(code) && code >= 128 && code <= 159 && names != nil && names[code] == winAnsiNames[code] {
					if r, ok := glyphRunes[names[code]]; ok {
						ch = r
					}
				}
			}
			n++
			f := g.Tf.BaseFont()
			if i := strings.Index(f, "+"); i >= 0 {
				f = f[i+1:]
			}
			trm := mm(mm(textMatrix{{g.Tfs * g.Th, 0, 0}, {0, g.Tfs, 0}, {0, g.Trise, 1}}, g.Tm), g.CTM)
			glyphs = append(glyphs, opGlyph{Text: ledongthuc.Text{Font: f, FontSize: trm[0][0], X: trm[2][0], Y: trm[2][1], W: w0 / 1000 * trm[0][0], S: string(ch)}, op: op})
			tx := mulf(addf(mulf(divf(w0, 1000), g.Tfs), g.Tc), g.Th)
			g.Tm = mm(textMatrix{{1, 0, 0}, {0, 1, 0}, {tx, 0, 1}}, g.Tm)
		}
	}
	matrixArgs := func(args []ledongthuc.Value) textMatrix {
		if len(args) != 6 {
			panic("bad matrix")
		}
		var m textMatrix
		for i := 0; i < 6; i++ {
			m[i/2][i%2] = num(args[i])
		}
		m[2][2] = 1
		return m
	}
	compat := 0
	ledongthuc.Interpret(strm, func(stk *ledongthuc.Stack, name string) {
		op++
		ops = append(ops, name)
		args := make([]ledongthuc.Value, stk.Len())
		for i := len(args) - 1; i >= 0; i-- {
			args[i] = stk.Pop()
		}
		// The operands must be of the operator's types, as walkContent requires of its tokens
		// (operandsMatch; WIS-625 Codex round 12): Float64 reads a string as 0.
		// A BX ... EX section may hold operators a reader does not know, which it ignores with their
		// operands; it must be balanced (WIS-625 Codex round 13).
		switch {
		case name == "BX":
			compat++
		case name == "EX":
			if compat--; compat < 0 {
				panic(operandMismatch{})
			}
		case !pdfOperators[name] && compat > 0:
			return
		}
		if !operandsMatch(name, valueKinds(args)) {
			panic(operandMismatch{})
		}
		one := func() {
			if len(args) != 1 {
				panic("bad operands")
			}
		}
		switch name {
		case "cm":
			g.CTM = mm(matrixArgs(args), g.CTM)
		case "q":
			stack = append(stack, g)
		case "Q":
			if len(stack) == 0 {
				panic("unbalanced Q")
			}
			g, stack = stack[len(stack)-1], stack[:len(stack)-1]
		case "BT":
			g.Tm, g.Tlm, g.real = identity, identity, identity
			travel = 0
		case "T*":
			g.Tlm = mm(textMatrix{{1, 0, 0}, {0, 1, 0}, {0, -g.Tl, 1}}, g.Tlm)
			g.Tm, g.real = g.Tlm, g.Tlm
		case "Tc":
			one()
			g.Tc = num(args[0])
		case "TD", "Td":
			if len(args) != 2 {
				panic("bad Td")
			}
			if name == "TD" {
				g.Tl = -num(args[1])
			}
			g.Tlm = mm(textMatrix{{1, 0, 0}, {0, 1, 0}, {num(args[0]), num(args[1]), 1}}, g.Tlm)
			g.Tm, g.real = g.Tlm, g.Tlm
		case "Tf":
			if len(args) != 2 {
				panic("bad Tf")
			}
			g.Tf, g.fontName = p.Font(args[0].Name()), args[0].Name()
			g.enc = g.Tf.Encoder()
			g.Tfs = num(args[1])
		case "\"", "'", "Tj":
			if name == "\"" {
				if len(args) != 3 {
					panic("bad \" operator")
				}
				g.Tw, g.Tc = num(args[0]), num(args[1])
				args = args[2:]
			}
			if name != "Tj" {
				one()
				g.Tlm = mm(textMatrix{{1, 0, 0}, {0, 1, 0}, {0, -g.Tl, 1}}, g.Tlm)
				g.Tm, g.real = g.Tlm, g.Tlm
			}
			one()
			spanned, span, pen, penEnd = false, textSpan{}, 0, 0
			place(args[0].RawString())
			showText(args[0].RawString())
			if spanned {
				spans = append(spans, span)
			}
		case "TJ":
			one()
			v := args[0]
			if v.Len() > maxTJElements {
				panic(operandMismatch{}) // round 29: no real array is this long
			}
			spanned, span, pen, penEnd = false, textSpan{}, 0, 0
			for i := 0; i < v.Len(); i++ {
				if x := v.Index(i); x.Kind() == ledongthuc.String {
					place(x.RawString())
					showText(x.RawString())
				} else {
					tx := mulf(mulf(divf(-num(x), 1000), g.Tfs), g.Th)
					move(tx, g.Tm)
					g.Tm = mm(textMatrix{{1, 0, 0}, {0, 1, 0}, {tx, 0, 1}}, g.Tm)
					g.real = mm(textMatrix{{1, 0, 0}, {0, 1, 0}, {tx, 0, 1}}, g.real)
					pen = addf(pen, tx)
				}
			}
			// The extractor ends a TJ with a line break. It is inserted as itself, never decoded
			// through the font (a ToUnicode entry for code 0x0A would store text never drawn).
			trm := mm(mm(textMatrix{{g.Tfs * g.Th, 0, 0}, {0, g.Tfs, 0}, {0, g.Trise, 1}}, g.Tm), g.CTM)
			glyphs = append(glyphs, opGlyph{Text: ledongthuc.Text{FontSize: trm[0][0], X: trm[2][0], Y: trm[2][1], S: "\n"}, op: op, separator: true})
			if spanned {
				spans = append(spans, span)
			}
		case "TL":
			one()
			g.Tl = num(args[0])
		case "Tm":
			g.Tm = matrixArgs(args)
			g.Tlm, g.real = g.Tm, g.Tm
		case "Ts":
			one()
			g.Trise = num(args[0])
		case "Tw":
			one()
			g.Tw = num(args[0])
		case "Tz":
			one()
			g.Th = num(args[0]) / 100
		case "w":
			one()
			g.lineWidth = num(args[0])
		case "Tr":
			one()
			// Read as a number (Int64 is 0 for a real such as 3.0); the walk refuses a mode that is
			// not an integer from 0 to 7, and so does this pass.
			mode := num(args[0])
			if mode != math.Trunc(mode) || mode < 0 || mode > 7 {
				panic(operandMismatch{})
			}
			g.mode = int64(mode)
		case "gs":
			one()
			if lw := states.Key(args[0].Name()).Key("LW"); !lw.IsNull() {
				g.lineWidth = num(lw)
			}
		}
	})
	if compat != 0 {
		panic(operandMismatch{})
	}
	return glyphs, spans, ops, ok, drawn
}

// maxTJElements is the most elements a TJ array may hold (WIS-625 Codex round 29).
const maxTJElements = 10000

// maxStrokeOfSize is the widest stroke outline text (render modes 1 and 2) may have, as a share of
// the glyph's smaller size on the page: wider, the stroke fills the letters in.
const maxStrokeOfSize = 0.1

// textSpan is where the text of one text-show operator lies on the page. box is where its glyphs
// may be drawn (each code's advance or font box, descent to ascent): what paint over it would hide.
// tight is its advance from the baseline to 0.7 em: what it may itself cover of other text. size
// is the smallest glyph height or width a reader sees, in points.
// under is its advance from descent to ascent: what must not lie on a dark or unknown
// background. overprint is a code starting back over an earlier code of the same operator.
// text is what it decodes to (simulated bold redraws the same text).
type textSpan struct {
	op                int
	box, tight, under [4]float64
	size              float64
	overprint         bool
	text              string
	codes, font       string       // the codes shown and the font resource they were shown in
	largest           float64      // the larger singular value of its rendering matrix (size is the smaller)
	matrix            [4]float64   // the linear part of its first code\'s rendering matrix
	cells             [][4]float64 // each code's cell: where its glyph may be drawn
	pens              [][4]float64 // each code's advance (to its ink width) and glyph box, descent to ascent
	advs              [][4]float64 // each code's advance alone (to its ink width), descent to ascent
	inks              [][4]float64 // a standard font's glyphs' own ink boxes (AFM), where known
	upright           bool
	baseline, em, hem float64
	squeezed          bool
}

// cellBox32 is cellBox in float32 arithmetic, each multiply and add rounded (WIS-625 Codex round 28).
func cellBox32(x0, x1, y0, y1 float64, m textMatrix) (box [4]float64, fine bool) {
	f := func(v float64) float32 { return float32(v) }
	for i, c := range [][2]float64{{x0, y0}, {x1, y0}, {x0, y1}, {x1, y1}} {
		cx, cy := f(c[0]), f(c[1])
		x := float64(float32(float32(float32(cx*f(m[0][0]))+float32(cy*f(m[1][0]))) + f(m[2][0])))
		y := float64(float32(float32(float32(cx*f(m[0][1]))+float32(cy*f(m[1][1]))) + f(m[2][1])))
		if !finite(x) || !finite(y) {
			return box, false
		}
		if i == 0 {
			box = [4]float64{x, y, x, y}
			continue
		}
		box = [4]float64{min(box[0], x), min(box[1], y), max(box[2], x), max(box[3], y)}
	}
	return box, true
}

// cellBox maps the text-space cell [x0, x1] x [y0, y1] through m (row vectors, as PDF) and
// returns its page-space bounding box; fine is false for a non-finite result.
func cellBox(x0, x1, y0, y1 float64, m textMatrix) (box [4]float64, fine bool) {
	for i, c := range [][2]float64{{x0, y0}, {x1, y0}, {x0, y1}, {x1, y1}} {
		x, y := c[0]*m[0][0]+c[1]*m[1][0]+m[2][0], c[0]*m[0][1]+c[1]*m[1][1]+m[2][1]
		if !finite(x) || !finite(y) {
			return box, false
		}
		if i == 0 {
			box = [4]float64{x, y, x, y}
			continue
		}
		box = [4]float64{min(box[0], x), min(box[1], y), max(box[2], x), max(box[3], y)}
	}
	return box, true
}

// fontMetrics are the widths of a font's character codes and the horizontal extent of its font
// box, in thousandths of an em.
type fontMetrics struct {
	ok              bool
	ascent, descent float64            // the font's ascent and (negative) descent
	names           map[int]string     // a standard, non-embedded font's glyph names by code
	minWidth        float64            // the smallest code width it may use
	offBaseline     bool               // a declared font box that does not straddle the baseline
	ink             map[int]float64    // a standard font\'s Adobe width where larger than the declared one
	glyphs          map[int][4]float64 // a standard font\'s Adobe glyph box by code
	twoByte         bool               // an Identity-H Type0 font: two-byte codes
	widths          map[int]float64    // a simple font's width by code
	cid             map[int]float64    // a Type0 font's /W
	dw              float64            // a Type0 font's /DW
	llx, urx        float64            // the font box's left and right edges
}

// minFontBox and minCodeWidth (thousandths of an em) are the smallest font box side and code
// width taken from a font Adobe's metrics do not vouch for: a declared box or width smaller than
// that would shrink where the text is taken to lie below the ink it may draw, so its text cannot
// be placed. Ordinary fonts' boxes are about an em; their narrowest glyphs (i, l, a period) are
// a fifth of one or more, and a space is allowed any positive width.
const (
	minFontBox   = 500
	minCodeWidth = 100
)

// plausible marks a font with an implausibly small declared font box as not ok, and sets the
// smallest code width it may use.
func (m fontMetrics) plausible() fontMetrics {
	if m.urx-m.llx < minFontBox || m.ascent-m.descent < minFontBox || m.offBaseline {
		return fontMetrics{}
	}
	m.minWidth = minCodeWidth
	return m
}

// width is code's advance; known is false for a code the font gives no positive width (a code
// outside /Widths would take MissingWidth, which says nothing about the glyph drawn).
func (m fontMetrics) width(code int) (w float64, known bool) {
	if m.twoByte {
		if w, ok := m.cid[code]; ok {
			return w, w >= max(m.minWidth, 1e-9)
		}
		return m.dw, m.dw >= max(m.minWidth, 1e-9)
	}
	w, ok := m.widths[code]
	if code == 32 {
		return w, ok && w > 0
	}
	return w, ok && w >= max(m.minWidth, 1e-9)
}

// metricsOf reads a font's widths. A simple font gives them in /Widths, or, when it is one of the
// 14 standard fonts by name and not embedded, they come from Adobe's metrics (core14_widths.go)
// through its encoding (built-in, Standard or WinAnsi, with /Differences). A Type0 font must use
// Identity-H. Anything else (a Type3 font, another CMap, an embedded or unknown font with no
// /Widths, MacRoman, a missing font) is not ok.
func metricsOf(f ledongthuc.Font) fontMetrics {
	v := f.V
	num := func(x ledongthuc.Value) (float64, bool) {
		if x.Kind() != ledongthuc.Integer && x.Kind() != ledongthuc.Real {
			return 0, false
		}
		return x.Float64(), finite(x.Float64())
	}
	// box reads the font box and the ascent and descent (FontDescriptor /Ascent and /Descent, else
	// the font box's top and bottom, else an em above and a quarter below).
	box := func(fd ledongthuc.Value, m *fontMetrics) {
		m.llx, m.urx, m.descent, m.ascent = 0, 1000, -250, 1000
		if a, ok := num(fd.Key("Ascent")); ok && a > 0 {
			m.ascent = a
		}
		if d, ok := num(fd.Key("Descent")); ok && d < 0 {
			m.descent = d
		}
		// The font box bounds the glyphs (Ascent and Descent do not: accents and descenders pass
		// them); the cell takes the wider of the two, and a box that does not straddle the baseline
		// cannot bound text drawn on it (plausible).
		if b, ok := pdfBox(fd.Key("FontBBox")); ok {
			m.llx, m.urx = b[0], b[2]
			m.descent, m.ascent = min(m.descent, b[1]), max(m.ascent, b[3])
			m.offBaseline = b[1] >= 0 || b[3] <= 0
		}
	}
	switch v.Key("Subtype").Name() {
	case "Type1", "TrueType", "MMType1":
		fd := v.Key("FontDescriptor")
		embedded := !fd.Key("FontFile").IsNull() || !fd.Key("FontFile2").IsNull() || !fd.Key("FontFile3").IsNull()
		base := v.Key("BaseFont").Name()
		if i := strings.IndexByte(base, '+'); i == 6 {
			base = base[i+1:] // a subset prefix
		}
		core, standard := core14Fonts[base]
		m := fontMetrics{ok: true, widths: map[int]float64{}}
		if w := v.Key("Widths"); !w.IsNull() {
			first, fine := num(v.Key("FirstChar"))
			if w.Kind() != ledongthuc.Array || w.Len() > 256 || !fine {
				return fontMetrics{}
			}
			for i := 0; i < w.Len(); i++ {
				n, fine := num(w.Index(i))
				if !fine {
					return fontMetrics{}
				}
				m.widths[int(first)+i] = n
			}
			box(fd, &m)
		} else if !standard || embedded {
			return fontMetrics{}
		}
		if !standard || embedded {
			return m.plausible()
		}
		// A standard font that is not embedded draws Adobe's glyphs: its codes are decoded through
		// its encoding's glyph names, which the stored text must agree with (codeText).
		names, ok := encodingNames(v.Key("Encoding"), core, base)
		if !ok {
			return fontMetrics{}
		}
		m.names = names
		m.ink = map[int]float64{}
		m.glyphs = map[int][4]float64{}
		for code, name := range names {
			if gb, ok := core.boxes[name]; ok {
				m.glyphs[code] = gb
			}
		}
		// Adobe's glyphs are drawn whatever the file declares: each width and the font box are the
		// larger of the two, so a declared tiny width or box cannot shrink the text's place.
		declared := !v.Key("Widths").IsNull()
		for code, name := range names {
			w, ok := core.widths[name]
			if !ok {
				continue
			}
			if d, has := m.widths[code]; declared && has {
				m.ink[code] = max(d, w) // the pen still moves by d, as a viewer moves it
			} else if !declared {
				m.widths[code] = w
			}
		}
		var adobe fontMetrics
		core.metrics(&adobe)
		if !declared || fd.IsNull() {
			m.llx, m.urx, m.descent, m.ascent = adobe.llx, adobe.urx, adobe.descent, adobe.ascent
		} else {
			m.llx, m.urx, m.descent, m.ascent = min(m.llx, adobe.llx), max(m.urx, adobe.urx), min(m.descent, adobe.descent), max(m.ascent, adobe.ascent)
		}
		return m
	case "Type0":
		if v.Key("Encoding").Name() != "Identity-H" {
			return fontMetrics{}
		}
		d := v.Key("DescendantFonts").Index(0)
		m := fontMetrics{ok: true, twoByte: true, dw: 1000, cid: map[int]float64{}}
		box(d.Key("FontDescriptor"), &m)
		if dw := d.Key("DW"); !dw.IsNull() {
			n, fine := num(dw)
			if !fine {
				return fontMetrics{}
			}
			m.dw = n
		}
		w := d.Key("W")
		if !w.IsNull() && w.Kind() != ledongthuc.Array {
			return fontMetrics{}
		}
		for i := 0; i < w.Len(); {
			c, fine := num(w.Index(i))
			if !fine || i+1 >= w.Len() {
				return fontMetrics{}
			}
			if list := w.Index(i + 1); list.Kind() == ledongthuc.Array { // c [w1 w2 ...]
				for k := 0; k < list.Len(); k++ {
					n, fine := num(list.Index(k))
					if !fine || len(m.cid) > 1<<16 {
						return fontMetrics{}
					}
					m.cid[int(c)+k] = n
				}
				i += 2
				continue
			}
			last, fine1 := num(w.Index(i + 1)) // c last w
			if i+2 >= w.Len() || !fine1 || last < c || last-c > 1<<16 {
				return fontMetrics{}
			}
			n, fine := num(w.Index(i + 2))
			if !fine || len(m.cid)+int(last-c) > 1<<16 {
				return fontMetrics{}
			}
			for k := int(c); k <= int(last); k++ {
				m.cid[k] = n
			}
			i += 3
		}
		return m.plausible()
	}
	return fontMetrics{}
}

// core14Font is one standard font's Adobe metrics.
type core14Font struct {
	bbox            [4]float64
	ascent, descent float64
	builtin         map[int]string
	widths          map[string]float64
	boxes           map[string][4]float64 // each glyph's bounding box
}

// metrics sets m's font box, ascent and descent from Adobe's (the font box when the AFM gives no
// ascender, as for Symbol and ZapfDingbats).
func (c core14Font) metrics(m *fontMetrics) {
	m.llx, m.urx, m.descent, m.ascent = c.bbox[0], c.bbox[2], c.bbox[1], c.bbox[3]
	if c.ascent > 0 && c.descent < 0 {
		m.descent, m.ascent = c.descent, c.ascent
	}
}

// ligatures are the only codes a ToUnicode CMap may map to more than one character.
var ligatures = map[string]bool{"fi": true, "fl": true, "ff": true, "ffi": true, "ffl": true}

// Known limit (product decision, WIS-625 post-merge): the text of an embedded font is trusted as
// its ToUnicode CMap or encoding gives it. Whether the glyph drawn is that character can only be
// known by rendering the font's outlines, which this extractor does not do; such text, like all
// uploaded text, is a candidate until the owner reviews it against the original.
//
// codeText is the text the extractor stores for one code, and whether it is honest as far as can
// be checked without drawing the glyph (WIS-625 post-merge review): exactly one character, or a
// standard ligature; and for a standard, non-embedded font, the character its glyph name stands
// for (a ToUnicode CMap, or the extractor's own decoding, that says otherwise is refused).
func (m fontMetrics) codeText(enc ledongthuc.TextEncoding, raw string, code int) (string, bool) {
	text := raw
	if enc != nil {
		text = enc.Decode(raw)
	}
	n := utf8.RuneCountInString(text)
	if n == 0 || n > 1 && !ligatures[text] || !utf8.ValidString(text) {
		return text, false
	}
	if m.names != nil {
		r, known := glyphRunes[m.names[code]]
		got, _ := utf8.DecodeRuneInString(text)
		switch {
		case known && text == string(r):
		case ligatures[text] && m.names[code] == text:
			// A ligature glyph (named fi, fl, ff, ffi or ffl) read as its exact letters, as a
			// ToUnicode CMap commonly maps it (WIS-625 Codex round 38).
		case known && n == 1 && quoteFamily[r] != 0 && quoteFamily[r] == quoteFamily[got]:
			// The extractor's ' for a curly apostrophe (and ` for an opening quote) is the same mark.
		case known && n == 1 && code >= 128 && code <= 159 && got == rune(code) && m.names[code] == winAnsiNames[code]:
			// A /Differences encoding the extractor reads code by code leaves WinAnsi's 128-159
			// (curly quotes, dashes, the euro) as their code; where the glyph is WinAnsi's own, the
			// stored text gets its real character (showText).
		default:
			return text, false
		}
	}
	return text, true
}

// quoteFamily groups the marks a glyph name and the extractor may give for one quote: the
// apostrophe (quoteright, quotesingle) and the opening quote (quoteleft, grave).
var quoteFamily = map[rune]int{'\'': 1, '\u2019': 1, '`': 2, '\u2018': 2}

// encodingNames is the glyph name of each code of a standard font: its built-in encoding,
// StandardEncoding (the built-in one of the Latin fonts) or WinAnsiEncoding, then any
// /Differences. ok is false for any other encoding.
func encodingNames(enc ledongthuc.Value, core core14Font, base string) (map[int]string, bool) {
	symbolic := base == "Symbol" || base == "ZapfDingbats"
	named := func(name string) (map[int]string, bool) {
		switch {
		case name == "" || name == "StandardEncoding" && !symbolic:
			return core.builtin, true
		case name == "WinAnsiEncoding" && !symbolic:
			return winAnsiNames, true
		}
		return nil, false
	}
	switch enc.Kind() {
	case ledongthuc.Null:
		return core.builtin, true
	case ledongthuc.Name:
		return named(enc.Name())
	case ledongthuc.Dict:
	default:
		return nil, false
	}
	baseNames, ok := named(enc.Key("BaseEncoding").Name())
	if !ok || !enc.Key("BaseEncoding").IsNull() && enc.Key("BaseEncoding").Kind() != ledongthuc.Name {
		return nil, false
	}
	names := make(map[int]string, len(baseNames))
	for c, n := range baseNames {
		names[c] = n
	}
	diffs := enc.Key("Differences")
	if !diffs.IsNull() && diffs.Kind() != ledongthuc.Array {
		return nil, false
	}
	code := -1
	for i := 0; i < diffs.Len(); i++ {
		switch x := diffs.Index(i); x.Kind() {
		case ledongthuc.Integer:
			code = int(x.Int64())
		case ledongthuc.Name:
			if code < 0 || code > 255 {
				return nil, false
			}
			names[code] = x.Name()
			code++
		default:
			return nil, false
		}
	}
	return names, true
}

// overlapsInclusive reports two boxes that overlap or touch: a paint that only touches text still
// counts.
func overlapsInclusive(a, b [4]float64) bool {
	m := decisionMargin
	return a[0] <= b[2]+m && b[0] <= a[2]+m && a[1] <= b[3]+m && b[1] <= a[3]+m
}
