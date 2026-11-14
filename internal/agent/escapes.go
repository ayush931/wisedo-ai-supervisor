package agent

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Escapes spelled with characters outside ASCII are found, never decoded (WIS-623 Codex round 35).
// Every earlier way of decoding them under the readings of their characters (each reading, the
// longest, the shortest, the one they agree on) left combinations of readings, or of readings and
// the decoders' own fallbacks, that it did not read ("Al&#х69;ce&#х58;", "&nοtiп;"). Real model
// output does not spell escapes with look-alike characters, so such text is refused: an escape
// whose characters include one outside ASCII makes the decoding step lossy, which every check
// refuses. Escapes in ASCII are decoded by the decoders of their syntax as they always were.
//
// Finding them uses reading sets, the way the trie matches names. Each character of the text as
// written (ignorables dropped, as the matcher drops them) stands for every ASCII string any of its
// readings is: as written, its NFKC form, its UTS #39 prototype with its case kept, the matcher's
// visual and folded views, each with its marks stripped, each upper- and lower-cased, for the
// character and for its upper- and lower-case forms. An ASCII character stands for itself only. A
// reading of several characters is read across as many places ("⑩" is "10"), and a mark may be
// passed over ("ú" is "u"). A place of an escape's grammar matches a character that can be what it
// requires; named references walk a trie of the HTML entity names. The search charges the audit
// budget for every character it visits and every way it tries, and for each reading set it
// computes; it is linear in the text (a run is matched one escape at a time).

// Escape syntaxes with an ASCII decoder of their own, in decodeSteps order.
const (
	escJSON = iota
	escBrace
	escHex
	escPercent
	escHTML
	escKinds
)

// More escape syntaxes of the catalogue (see decodeSteps), which discovery reads too.
const (
	discOctal    = escKinds + iota // \101 (JavaScript and Python octal)
	discCSS                        // \41 and \000041 (CSS)
	discPyU                        // \U00000041 (Python)
	discPyN                        // \N{...} (Python)
	discPercentU                   // %u0041 (JavaScript unescape)
	discKinds
)

// asciiSet is a set of ASCII characters.
type asciiSet [2]uint64

func (a *asciiSet) add(b byte)     { a[b>>6] |= 1 << (b & 63) }
func (a asciiSet) has(b byte) bool { return b < utf8.RuneSelf && a[b>>6]&(1<<(b&63)) != 0 }

// hasAny reports that the set holds one of the characters of chars.
func (a asciiSet) hasAny(chars string) bool {
	for i := 0; i < len(chars); i++ {
		if a.has(chars[i]) {
			return true
		}
	}
	return false
}

// charReadings are what one character can read as: single ASCII characters, and ASCII strings of
// several characters (at most maxReading).
type charReadings struct {
	set   asciiSet
	multi []string
}

const maxReading = 8

// readingCache holds the readings of characters already read, process-wide, up to
// readingCacheMax of them (they depend on the character alone).
var (
	readingCache     sync.Map
	readingCacheSize atomic.Int64
)

const readingCacheMax = 1 << 16

// readingsOf returns the readings of the character x; miss reports that they were computed.
func readingsOf(x string, fold cases.Caser) (rd charReadings, miss bool) {
	if len(x) == 1 && x[0] < utf8.RuneSelf {
		rd.set.add(x[0])
		return rd, false
	}
	if v, ok := readingCache.Load(x); ok {
		return v.(charReadings), false
	}
	// The character, and its upper- and lower-case forms, each read every way: case is taken
	// before mapping (Greek "ν" upper-cased is "Ν", whose prototype is "N": Codex round 33).
	for _, o := range []string{x, strings.ToUpper(x), strings.ToLower(x)} {
		nfkc := norm.NFKC.String(o)
		for _, v := range []string{o, nfkc, perRune(o, confusableKeepCase), perRune(nfkc, confusableKeepCase),
			canonicalCluster(o, visualView, fold), canonicalCluster(o, foldedView, fold)} {
			for _, w := range []string{v, stripMarks(v)} {
				for _, z := range []string{w, strings.ToUpper(w), strings.ToLower(w)} {
					switch {
					case z == "" || !isASCII(z) || len(z) > maxReading:
					case len(z) == 1:
						rd.set.add(z[0])
					case !slices.Contains(rd.multi, z):
						rd.multi = append(rd.multi, z)
					}
				}
			}
		}
	}
	if readingCacheSize.Load() < readingCacheMax {
		if _, loaded := readingCache.LoadOrStore(x, rd); !loaded {
			readingCacheSize.Add(1)
		}
	}
	return rd, true
}

//go:generate go run entities_gen.go

// entityNode is a node of the trie of HTML character reference names (entities_table.go).
type entityNode struct {
	kids map[byte]*entityNode
	term bool
}

var entityTrie = sync.OnceValue(func() *entityNode {
	root := &entityNode{}
	for _, name := range htmlEntityNames {
		n := root
		for i := 0; i < len(name); i++ {
			if n.kids == nil {
				n.kids = map[byte]*entityNode{}
			}
			k := n.kids[name[i]]
			if k == nil {
				k = &entityNode{}
				n.kids[name[i]] = k
			}
			n = k
		}
		n.term = true
	}
	return root
})

// unit is one character of the text: what it can read as, whether it may be passed over (a
// mark), and whether it is ASCII.
type unit struct {
	rd     charReadings
	skip   bool
	ascii  bool
	strong bool // outside ASCII, but ASCII once normalised or without its marks (full-width, "⑩", "ú", a mark)
	gap    bool // an ignorable character was dropped right before it
}

// escReader searches escape grammars over the units of a text.
type escReader struct {
	u      []unit
	charge func(int) bool // the audit budget (nil: none)
	spent  bool           // the budget or the context is spent
}

const (
	hexChars = "0123456789abcdefABCDEF"
	decChars = "0123456789"
)

// spend takes one step of the search, charged to the audit budget: false when the budget or the
// context is spent.
func (r *escReader) spend() bool {
	if r.spent {
		return false // spent stays spent: nothing more is charged
	}
	if r.charge != nil && !r.charge(costSearch) {
		r.spent = true
		return false
	}
	return true
}

// cur is a place in the units: unit k, after pend, the rest of a reading of several characters
// of the unit before it that is still being read.
type cur struct {
	k    int
	pend string
}

// next calls f for each way to read one grammar character in chars from at: the next character
// of a reading being read, or a unit k2 >= at.k reading as it (alone or as the first of several
// characters), every unit before k2 passed over (marks). With exact, the character is unit at.k.
// It reports false when f does or the search is spent.
func (r *escReader) next(at cur, chars string, exact bool, f func(to cur) bool) bool {
	if at.pend != "" {
		if strings.IndexByte(chars, at.pend[0]) < 0 {
			return true
		}
		return r.spend() && f(cur{at.k, at.pend[1:]})
	}
	for k := at.k; k < len(r.u); k++ {
		if !r.spend() {
			return false
		}
		u := &r.u[k]
		for i := 0; i < len(chars); i++ {
			if u.rd.set.has(chars[i]) {
				if !f(cur{k + 1, ""}) {
					return false
				}
				break // one way per unit: which character it is does not matter here
			}
		}
		for _, m := range u.rd.multi {
			if strings.IndexByte(chars, m[0]) >= 0 && (!r.spend() || !f(cur{k + 1, m[1:]})) {
				return false
			}
		}
		if exact || !u.skip {
			return true
		}
	}
	return true
}

// spelledOutsideASCII reports that an escape of kind can be spelled beginning exactly at unit i
// with a character outside ASCII in it: one of its characters, an ignorable between two of them
// (dropped by a renderer, "%4" U+200B "1": security review of d63081e7), or a reading of several
// characters. It is exempt only where it holds no escape syntax beyond an ASCII lead: letters of
// another script read as its digits or name letters ("100%бесплатно": "%бе" looks like "%6e";
// "C:\восток": "\в" looks like "\b") spell no escape any decoder reads and are not refused
// (Codex round 35). Escape syntax is an ASCII character besides the lead, a lead outside ASCII, a
// character that is ASCII once normalised or without its marks (full-width, "⑩", a mark), an
// ignorable, or a character one of whose readings is a fixed syntax character (\ x u # & % ; { }:
// an escape spelled wholly in look-alikes is refused). It stops at the first; a run is matched one
// escape at a time, so the work is linear in the text.
func (r *escReader) spelledOutsideASCII(i, kind int) bool {
	found := false
	// The flags of the units [i, k) are a property of the units alone, whatever path reached k:
	// they are scanned once, as far as any path has gone (Codex round 36: rescanning the prefix at
	// every digit of "&#000...0" was quadratic).
	var outsideTo, syntaxTo []bool // [k-i]: the flags of [i, k)
	outsideTo, syntaxTo = append(outsideTo, false), append(syntaxTo, !r.u[i].ascii)
	done := func(to cur) bool {
		for k := i + len(outsideTo) - 1; k < to.k; k++ {
			if !r.spend() {
				return false
			}
			u := &r.u[k]
			gap := k > i && u.gap
			n := len(outsideTo) - 1
			outsideTo = append(outsideTo, outsideTo[n] || !u.ascii || gap)
			syntaxTo = append(syntaxTo, syntaxTo[n] || gap || u.strong || k > i && u.ascii || !u.ascii && u.rd.set.hasAny(`\xuUN#&%;{}`))
		}
		found = to.pend != "" || outsideTo[to.k-i] && syntaxTo[to.k-i]
		return !found // stop at the first
	}
	var seq func(at cur, elems []string, exact bool, done func(cur) bool) bool
	seq = func(at cur, elems []string, exact bool, done func(cur) bool) bool {
		if len(elems) == 0 {
			return done(at)
		}
		return r.next(at, elems[0], exact, func(to cur) bool { return seq(to, elems[1:], false, done) })
	}
	// digits reads one or more digits of chars in one work list, each place once, and ends the
	// escape after any of them (bare, when bare) or after a following character of term.
	digits := func(at cur, chars, term string, bare bool) bool {
		stack, seen := []cur{at}, map[cur]bool{}
		for len(stack) > 0 {
			at, stack = stack[len(stack)-1], stack[:len(stack)-1]
			var nexts []cur
			if !r.next(at, chars, false, func(to cur) bool { nexts = append(nexts, to); return true }) {
				return false
			}
			for _, to := range nexts {
				if seen[to] {
					continue
				}
				seen[to] = true
				if bare && !done(to) || term != "" && !seq(to, []string{term}, false, done) {
					return false
				}
				stack = append(stack, to)
			}
		}
		return true
	}
	start := cur{k: i}
	switch kind {
	case escJSON:
		seq(start, []string{`\`, "u", hexChars, hexChars, hexChars, hexChars}, true, done)
		seq(start, []string{`\`, `"\/bfnrt`}, true, done)
	case escBrace:
		seq(start, []string{`\`, "u", "{"}, true, func(at cur) bool { return digits(at, hexChars, "}", false) })
	case escHex:
		seq(start, []string{`\`, "x", hexChars, hexChars}, true, done)
	case escPercent:
		seq(start, []string{"%", hexChars, hexChars}, true, done)
	case escHTML:
		// A numeric reference: every extent of its digits, then a ";" (the character outside
		// ASCII may come at any digit, or be the ";"). A named one: a whole name of the table (at
		// most the longest name deep).
		seq(start, []string{"&", "#"}, true, func(at cur) bool {
			return digits(at, decChars, ";", true) && seq(at, []string{"xX"}, false, func(to cur) bool { return digits(to, hexChars, ";", true) })
		})
		var walk func(at cur, n *entityNode) bool
		walk = func(at cur, n *entityNode) bool {
			for c, kid := range n.kids {
				if !r.next(at, string(c), false, func(to cur) bool { return (!kid.term || done(to)) && walk(to, kid) }) {
					return false
				}
			}
			return true
		}
		r.next(start, "&", true, func(to cur) bool { return walk(to, entityTrie()) })
	case discOctal:
		seq(start, []string{`\`, "01234567"}, true, done)
	case discCSS:
		// At most six hex digits, as CSS reads them (Codex round 38: a seventh character is
		// no part of the escape).
		var d func(at cur, n int) bool
		d = func(at cur, n int) bool {
			return r.next(at, hexChars, false, func(to cur) bool {
				return done(to) && seq(to, []string{" \t\n\r\f"}, false, done) && (n == 6 || d(to, n+1))
			})
		}
		seq(start, []string{`\`}, true, func(at cur) bool { return d(at, 1) })
	case discPyU:
		seq(start, []string{`\`, "U", hexChars, hexChars, hexChars, hexChars, hexChars, hexChars, hexChars, hexChars}, true, done)
	case discPyN:
		seq(start, []string{`\`, "N", "{"}, true, done)
	case discPercentU:
		seq(start, []string{"%", "u", hexChars, hexChars, hexChars, hexChars}, true, done)
	}
	return found
}

// lookalikeEscape reports that s holds an escape, of any syntax, spelled with a character outside
// ASCII, or that the budget was spent finding out (charge, nil: none). The decoding steps then
// refuse s: it is never decoded.
func lookalikeEscape(charge func(int) bool, s string) bool {
	if isASCII(s) {
		return false
	}
	if charge != nil && !charge(costStepRead*len(s)) {
		return true
	}
	fold := cases.Fold()
	r := escReader{charge: charge}
	gap := false
	for _, c := range s {
		if ignorable(c) {
			gap = true // dropped, as the matcher drops it; an escape across it is refused
			continue
		}
		rd, miss := readingsOf(string(c), fold)
		if miss && charge != nil && !charge(costReadings) {
			return true
		}
		strong := false
		if c >= utf8.RuneSelf {
			n := norm.NFKC.String(string(c))
			strong = attaches(c) || n != "" && (isASCII(n) || isASCII(stripMarks(n)))
		}
		r.u = append(r.u, unit{rd: rd, skip: attaches(c), ascii: c < utf8.RuneSelf, strong: strong, gap: gap})
		gap = false
	}
	leads := [discKinds]byte{escJSON: '\\', escBrace: '\\', escHex: '\\', escPercent: '%', escHTML: '&',
		discOctal: '\\', discCSS: '\\', discPyU: '\\', discPyN: '\\', discPercentU: '%'}
	for i := range r.u {
		for kind, lead := range leads {
			can := r.u[i].rd.set.has(lead)
			for _, m := range r.u[i].rd.multi {
				can = can || m[0] == lead
			}
			if can && (r.spelledOutsideASCII(i, kind) || r.spent) {
				return true
			}
		}
	}
	return r.spelledOutsideASCIIFamily() || r.spent
}
