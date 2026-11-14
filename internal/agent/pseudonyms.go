package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Pseudonyms replaces the names and identifiers the system knows about with request-local tokens
// before a hosted call, and restores them from the same map afterwards (rule 08). It is
// dictionary pseudonymisation: member display names, the tenant and site names and customer names
// from the knowledge graph, plus e-mail addresses and international or Indian mobile numbers. A
// name the system does not know (for example one written in free SOP text) is not caught.
type Pseudonyms struct {
	names  *trieNode // every known term: what hideText replaces and the audit refuses on
	count  int       // distinct terms in names, exact and refuseOn
	exact  []string  // letterless names without digits ("★★"): hidden byte for byte as whole tokens
	refuse []string  // letterless names with digits ("42"): a request holding one as a whole token is refused
	// refuseNum holds the decimal keys (see decimalKey) of the refuse names that are decimal
	// numbers, computed once at load.
	refuseNum map[string]bool
	jsonish   *Pseudonyms // the stored names that contain JSON syntax (see jsonSyntax)
	// reach is the most characters any stored name has in a canonical view: no match spans more
	// text clusters (see windows).
	reach int
	// ctx and auditBudget bound one HideRequestContext or RestoreResponse call (see charge).
	ctx         context.Context
	auditBudget int
	// replacers caches the audit's token replacers for the tokens issued so far.
	replIssued int
	replAll    *strings.Replacer
	// restoreLimit bounds the bytes any restored layer, and all restored output together, may
	// take (see RestoreResponse).
	restoreLimit int
	// SkippedSeparatorNames counts stored names made only of separators (" ()-+._%@"), which
	// identify no one and are not used (a diagnostic count; the names are never logged).
	SkippedSeparatorNames int
	// terms are the stored names this map was built from: a continuation protects them as well
	// as the names read again (WIS-623 Codex round 17).
	terms []string
	// auditCarried is set while a carried model message is audited: only there are this map's
	// tokens in an encoded form accepted as placeholders (Codex round 17).
	auditCarried bool
	token        map[string]string // exact replaced text -> token issued in this request
	term         map[string]string // token -> the exact text it replaced
	issued       int
	// refuseHit is set when a walk found a number name (see refused).
	refuseHit bool
	// overBudget is set when matching a text took more than maxMatchWorkPerSlot steps per slot;
	// HideRequest then refuses the request (PSEUDONYMS_INCOMPLETE).
	overBudget bool
}

var (
	// pseudonymToken is the shape of an issued token; any such text in a response that this
	// request did not issue refuses the whole output.
	pseudonymToken = regexp.MustCompile(`⟦P[0-9]+⟧`)
	tokenBrackets  = strings.NewReplacer("⟦", "[", "⟧", "]")
	emailPattern   = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	// Only unmistakable phone numbers: an international number written with "+", or a 10-digit
	// Indian mobile number. Measurements and counts (which grounding compares exactly) are left alone.
	phonePattern = regexp.MustCompile(`\+[0-9][0-9 ()-]{7,18}[0-9]|[6-9][0-9]{9}`)
)

// Bounds on the terms one request is pseudonymised with (WIS-623 security review). They keep the
// work of reading the terms, canonicalising them and building the matcher bounded whatever the
// knowledge graph holds.
const (
	// MaxTermRunes is the knowledge contract's limit for a display name or alias. A longer term
	// refuses the hosted call: it cannot be dropped without sending it in clear.
	MaxTermRunes = 200
)

// maxPseudonymTerms bounds the distinct canonical terms (match keys) of one tenant, and
// maxPseudonymTerms x MaxTermRunes bounds both the runes canonicalised and the runes of all keys.
// More refuses the hosted call, never truncates. Tests lower it to reach the bound cheaply.
var maxPseudonymTerms = 5000

// ErrPseudonymTerms is a term set NewPseudonymsFor cannot use: too many terms or runes, a term
// longer than MaxTermRunes, or a matcher that does not compile. The hosted call is refused.
var ErrPseudonymTerms = errors.New("pseudonym terms out of bounds")

// NewPseudonymsFor prepares the map for one request from the known terms. Every term with a
// letter is kept, however short ("Li", "GE"), and matches as a whole word in any case and in any
// look-alike spelling the readings recognise (see readings). A standalone ordinary word equal to
// a short known name (a member called "Al" or "Do") is pseudonymised too: over-redaction is the
// safe direction for data leaving the host.
//
// A stored name is never dropped silently, and one odd name cannot stop a site's hosted calls
// (WIS-623 security reviews of the round 7 work). A name with no letter in its stored form or
// reading is kept byte for byte: one without digits ("★★") is hidden wherever it stands as a
// whole token, like any name; one with digits ("42") is not hidden, because hiding it would turn
// counts and measurements in the text into tokens, so a request that holds it as a whole token is
// refused instead (only that request). Token delimiters in a stored name are read as the plain
// brackets the request text is given (hideText), so the name still matches. Empty terms are
// ignored.
//
// Each term is stored once, as its sequence of reading sets; matching intersects them with the
// request's reading sets (see find).
func NewPseudonymsFor(terms []string) (*Pseudonyms, error) {
	p, err := newPseudonyms(terms)
	if err != nil {
		return nil, err
	}
	p.terms = slices.Clone(terms)
	// Names that contain JSON syntax may span JSON structure, which hiding (string by string)
	// cannot rewrite: they are also kept apart and read on each JSON layer's raw text, literals
	// included, so one left in the output refuses the request (see leaksAt).
	var syntax []string
	fold, memo := cases.Fold(), map[string][]string{}
	for _, t := range terms {
		if hasJSONSyntax(t, fold, memo) {
			syntax = append(syntax, t)
		}
	}
	if len(syntax) > 0 {
		if p.jsonish, err = newPseudonyms(syntax); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// jsonSyntax are the characters of JSON structure and string escapes. A stored name holding any
// of them (as written, or in a reading: "［true］") may span several JSON strings; inside one
// string it is hidden as usual (WIS-623 Codex rounds 11 and 12). Realistic names are unaffected
// ("O'Brien" with an apostrophe is not JSON syntax).
const jsonSyntax = `"\\{}[]:,`

// hasJSONSyntax reports JSON syntax in a name as written, in its canonical views or in any of its
// readings, so a full-width or look-alike bracket or quote ("＂", "［", "｛", "：") counts like the
// ASCII one (security review of 89e4bb50).
func hasJSONSyntax(t string, fold cases.Caser, memo map[string][]string) bool {
	// Classified on exactly what is indexed (token brackets read as plain brackets, and the number
	// form of a name with digits), its canonical views and every slot reading: one source of
	// truth with the matcher (WIS-623 Codex round 13: "⟦true⟧" is indexed as "[true]").
	idx := tokenBrackets.Replace(strings.TrimSpace(t))
	for _, f := range []string{idx, refuseForm(idx)} {
		if strings.ContainsAny(f, jsonSyntax) || strings.ContainsAny(canonicalString(f, visualView), jsonSyntax) ||
			strings.ContainsAny(canonicalString(f, foldedView), jsonSyntax) || strings.ContainsAny(strings.Join(analyze(f, fold, memo).slots, ""), jsonSyntax) {
			return true
		}
	}
	return false
}

func newPseudonyms(terms []string) (*Pseudonyms, error) {
	runes := 0
	for _, t := range terms {
		n := utf8.RuneCountInString(strings.TrimSpace(t))
		if n > MaxTermRunes {
			return nil, ErrPseudonymTerms
		}
		runes += n
	}
	if runes > maxPseudonymTerms*MaxTermRunes {
		return nil, ErrPseudonymTerms
	}
	p := &Pseudonyms{names: &trieNode{}, token: map[string]string{}, term: map[string]string{}}
	seen := map[string]bool{}
	slotsTotal := 0
	memo := map[string][]string{}
	fold := cases.Fold()
	for _, t := range terms {
		t = tokenBrackets.Replace(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		for _, v := range []string{t, canonicalString(t, visualView), canonicalString(t, foldedView)} {
			p.reach = max(p.reach, utf8.RuneCountInString(v))
		}
		a := analyze(t, fold, memo)
		if a.overflow {
			return nil, ErrPseudonymTerms
		}
		if !strings.ContainsFunc(t+a.folded, unicode.IsLetter) {
			if seen["\x01"+t] {
				continue
			}
			seen["\x01"+t] = true
			if p.count++; p.count > maxPseudonymTerms {
				return nil, ErrPseudonymTerms
			}
			switch {
			case strings.ContainsFunc(t, unicode.IsDigit):
				// Compared on the normalised text, without marks (see refuseText).
				f := refuseForm(t)
				p.refuse = append(p.refuse, f)
				// Found by the same budgeted walk as every name, as a whole token (no letter,
				// digit, mark or symbol beside it), on its readings: "４２", "4" U+200B "2" and
				// "42" U+0301 read as "42" (Codex round 12).
				fa := analyze(f, fold, memo)
				if slotsTotal += len(fa.slots); slotsTotal > maxPseudonymTerms*MaxTermRunes {
					return nil, ErrPseudonymTerms
				}
				p.names.insert(fa.slots).refuse = true
				if k, ok := decimalKey(f); ok {
					if p.refuseNum == nil {
						p.refuseNum = map[string]bool{}
					}
					p.refuseNum[k] = true
				}
			case strings.Trim(t, detectorSeparators) == "":
				// A name made only of the characters the e-mail and phone patterns read between
				// digits and letters ("-", "+", ".", "( )") identifies no one and, hidden as a
				// token, could split a phone number or an e-mail address. It is not used and is
				// counted (SkippedSeparatorNames); refusing would let one such name stop every
				// hosted call of the site (lead decision on 3227f17a).
				p.SkippedSeparatorNames++
				p.count--
			default:
				// A letterless name without digits ("★★") walks the same trie as the names,
				// on its own readings (for a symbol, the symbol itself), and matches only as a
				// whole token (see symbolBoundary).
				p.exact = append(p.exact, t)
				if slotsTotal += len(a.slots); slotsTotal > maxPseudonymTerms*MaxTermRunes {
					return nil, ErrPseudonymTerms
				}
				p.names.insert(a.slots).symbol = true
			}
			continue
		}
		key := strings.Join(a.slots, "\x00")
		if seen[key] {
			continue
		}
		seen[key] = true
		p.count++
		slotsTotal += len(a.slots)
		if p.count > maxPseudonymTerms || slotsTotal > maxPseudonymTerms*MaxTermRunes {
			return nil, ErrPseudonymTerms
		}
		p.names.insert(a.slots).term = true
	}
	return p, nil
}

// detectorSeparators are the characters the e-mail and phone patterns accept between the digits
// and letters they find.
const detectorSeparators = " ()-+._%@"

// refuseForm is the form letterless names with digits are compared in: NFKC (full-width "４２"
// is "42"), default-ignorable characters and marks removed ("4" U+200B "2", "42" U+FE0F).
func refuseForm(s string) string {
	return stripMarks(strings.Map(func(r rune) rune {
		if ignorable(r) {
			return -1
		}
		return r
	}, norm.NFKC.String(s)))
}

// refused reports that v holds a letterless name with digits as a whole token, read as written,
// in both canonical views, and in refuseForm.
func (p *Pseudonyms) refused(v string) bool {
	if len(p.refuse) == 0 {
		return false
	}
	q := p.fresh()
	q.find(analyze(v, cases.Fold(), map[string][]string{}))
	return q.refuseHit || q.overBudget
}

// refusedNumber reports that the exact value of a JSON number literal equals a letterless name
// made only of a decimal number ("42" against 42, 42.0, 420e-1, 0.42e2 or 4.2e1). Both are
// compared by decimalKey, a linear scan that never expands an exponent or parses a big number
// (security reviews of 3227f17a and 2a73a787).
func (p *Pseudonyms) refusedNumber(lit string) bool {
	if len(p.refuseNum) == 0 {
		return false
	}
	if !p.charge(len(lit)) {
		return true
	}
	k, ok := decimalKey(lit)
	return ok && p.refuseNum[k]
}

// decimalKey is the exact value of a decimal number written as -?digits(.digits)?(e[+-]?digits)?,
// as text: "0" for zero, otherwise the sign, the significant digits without leading or trailing
// zeros, and the decimal magnitude (the exponent plus the position of the leading significant
// digit), so 42, 42.0, 420e-1, 0.42e2 and 4200000000e-8 share one key. An exponent whose
// magnitude no realistic literal can offset (beyond 10^15) gives a key no name can have, unless
// the digits are all zero. ok is false for text that is not such a number.
func decimalKey(lit string) (string, bool) {
	neg := strings.HasPrefix(lit, "-")
	if neg || strings.HasPrefix(lit, "+") {
		lit = lit[1:]
	}
	mant, exp, hasExp := lit, "0", false
	if k := strings.IndexAny(lit, "eE"); k >= 0 {
		mant, exp, hasExp = lit[:k], lit[k+1:], true
	}
	intPart, frac, _ := strings.Cut(mant, ".")
	digits := func(x string) bool { return strings.Trim(x, "0123456789") == "" }
	if intPart+frac == "" || !digits(intPart) || !digits(frac) || hasExp && (exp == "" || !digits(strings.TrimLeft(exp, "+-")) || strings.TrimLeft(exp, "+-") == "") {
		return "", false
	}
	d := strings.TrimLeft(intPart+frac, "0")
	point := len(intPart) - (len(intPart+frac) - len(d)) // position of the leading significant digit
	d = strings.TrimRight(d, "0")
	if d == "" {
		return "0", true
	}
	e, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || e > 1e15 || e < -1e15 {
		return "out-of-range", true // no name is this large or this small
	}
	sign := "+"
	if neg {
		sign = "-"
	}
	return sign + d + "e" + strconv.FormatInt(int64(point)+e, 10), true
}

// continueFrom makes p continue the tokens c issued: the same token for the same text, and new
// tokens numbered after them. p keeps its own (current) known names and its own copies of the
// token maps, so c is never changed by p (security review of 233ae21e).
func (p *Pseudonyms) continueFrom(c *Pseudonyms) {
	p.token, p.term, p.issued = maps.Clone(c.token), maps.Clone(c.term), max(c.issued, len(c.term))
}

// fresh returns a map with the same known terms and no tokens issued.
func (p *Pseudonyms) fresh() *Pseudonyms {
	return &Pseudonyms{names: p.names, count: p.count, exact: p.exact, jsonish: p.jsonish, refuse: p.refuse, refuseNum: p.refuseNum, ctx: p.ctx, reach: p.reach,
		token: map[string]string{}, term: map[string]string{}}
}

// symbolBoundary reports that s[i:j] has no letter, digit, mark or symbol right before or after
// it: "★★" is not a whole token of "★★★", nor "42" of "420".
func symbolBoundary(s string, i, j int) bool {
	joins := func(r rune) bool { return unicode.In(r, unicode.L, unicode.N, unicode.M, unicode.S) }
	before, _ := utf8.DecodeLastRuneInString(s[:i])
	after, _ := utf8.DecodeRuneInString(s[j:])
	return !(i > 0 && joins(before)) && !(j < len(s) && joins(after))
}

// trieNode is one node of the term trie. The edge into a node is labelled with a reading set
// (the runes a stored slot can be read as); children are found by any rune of their set.
type trieNode struct {
	set    string // the reading set of the edge into this node
	kids   []*trieNode
	idx    map[rune][]*trieNode // built once a node has more than 8 children
	term   bool                 // a stored name ends here
	symbol bool                 // a stored letterless name ends here (matched as a whole token)
	refuse bool                 // a stored number name ends here: a request holding it is refused
	seen   uint64               // step stamp: the node is already active in this step (O(1) dedup)
	clock  uint64               // on the root only: the last step stamp issued
}

func (n *trieNode) insert(slots []string) *trieNode {
	for _, set := range slots {
		var next *trieNode
		for _, k := range n.kids {
			if k.set == set {
				next = k
				break
			}
		}
		if next == nil {
			next = &trieNode{set: set}
			n.kids = append(n.kids, next)
			switch {
			case n.idx != nil:
				n.index(next)
			case len(n.kids) > 8:
				n.idx = map[rune][]*trieNode{}
				for _, k := range n.kids {
					n.index(k)
				}
			}
		}
		n = next
	}
	return n
}

func (n *trieNode) index(k *trieNode) {
	for _, r := range k.set {
		n.idx[r] = append(n.idx[r], k)
	}
}

func (n *trieNode) next(r rune, visit func(*trieNode)) {
	if n.idx != nil {
		for _, k := range n.idx[r] {
			visit(k)
		}
		return
	}
	for _, k := range n.kids {
		if strings.ContainsRune(k.set, r) {
			visit(k)
		}
	}
}

// maxMatchWorkPerSlot bounds the trie steps matching may take per request slot. A request that
// needs more is refused, never sent partly matched. Each slot has at most six readings (see
// readings, plus those of a merged cluster's letter), so this allows, per slot, about ten trie
// nodes active at once. The total work for a
// text of n slots is at most maxMatchWorkPerSlot x (n + 1) trie steps, and no walk holds more
// than maxActivePerSlot nodes at a slot.
const maxMatchWorkPerSlot = 64

// maxActivePerSlot bounds the trie nodes active at one slot of one walk; more refuses the text.
const maxActivePerSlot = 16

// analysis is a text read slot by slot: slots[i] is the reading set of slot i (its distinct
// runes); a slot belongs to a cluster of the text, whose original byte span is [from[i], to[i]);
// start[i] and end[i] report that slot i begins or ends its cluster. folded is the folded
// reading of the whole text and at[i] the byte offset of slot i in it (at[len] = len(folded)).
type analysis struct {
	slots      []string
	from, to   []int
	start, end []bool
	folded     string
	at         []int
	overflow   bool   // more merged marks than maxMergedMarks: the text is refused
	canEnd     []bool // a match may end after this slot: it ends its cluster, or only optional mark slots follow
}

// readings are the readings of one cluster: the folded and visual views of the cluster as
// written, upper-cased and lower-cased, deduplicated, the folded view of the cluster as written
// first (at most six; a Latin, Greek or Cyrillic letter is read without its marks). ASCII reads
// as its lower case only: that is every view of it.
func readings(raw string, fold cases.Caser, memo map[string][]string) []string {
	key := raw
	if rs, ok := memo[key]; ok {
		return rs
	}
	var rs []string
	if len(raw) == 1 && raw[0] < utf8.RuneSelf {
		rs = []string{strings.ToLower(raw)}
	} else {
		// A Latin, Greek or Cyrillic letter reads without its marks, before and after the views,
		// so "Niná" or "Nina" with any appended mark (even U+0345, which case folding turns into
		// a letter) still reads "nina". Other scripts keep their marks: Devanagari vowel signs and
		// virama are marks that tell words apart.
		lgc := false
		if r, _ := utf8.DecodeRuneInString(norm.NFD.String(raw)); readsWithoutMarks(r) {
			lgc = true
			if bare := stripMarks(raw); bare != "" {
				raw = bare
			}
		}
		for _, v := range [6]string{canonicalCluster(raw, foldedView, fold), canonicalCluster(raw, visualView, fold),
			canonicalCluster(strings.ToUpper(raw), foldedView, fold), canonicalCluster(strings.ToUpper(raw), visualView, fold),
			canonicalCluster(strings.ToLower(raw), foldedView, fold), canonicalCluster(strings.ToLower(raw), visualView, fold)} {
			if lgc {
				if bare := stripMarks(v); bare != "" {
					v = bare
				}
			}
			if !slices.Contains(rs, v) {
				rs = append(rs, v)
			}
		}
	}
	memo[key] = rs
	return rs
}

// readsWithoutMarks reports a base whose marks are ignored on both sides: Latin, Greek and
// Cyrillic letters, and digits ("4" U+0301 "2" reads "42": WIS-623 Codex round 13).
func readsWithoutMarks(r rune) bool {
	return unicode.In(r, unicode.Latin, unicode.Greek, unicode.Cyrillic, unicode.Nd)
}

// stripMarks removes the nonspacing and enclosing marks of s (decomposed, then composed again).
func stripMarks(s string) string {
	return norm.NFC.String(strings.Map(func(r rune) rune {
		if attaches(r) {
			return -1
		}
		return r
	}, norm.NFD.String(s)))
}

// analyze reads s slot by slot. A cluster has as many slots as its folded reading has runes; a
// reading of the same length adds its i-th rune to slot i (one of another length cannot align and
// adds nothing: "ß" is two slots, "s" and "s"). The same function reads stored terms and request
// text, and slots match when their reading sets intersect, so matching is symmetric.
func analyze(s string, fold cases.Caser, memo map[string][]string) analysis {
	var a analysis
	var b strings.Builder
	add := func(c cluster) {
		// A cluster's slots are its core, the letters before any trailing marks, then those
		// marks as optional slots: a match may end before them ("Nina" U+0310, or a Devanagari
		// name followed by U+0345, which case folding turns into a letter: security review of
		// 010223a6). Trailing marks are taken from the cluster as written and from its folded
		// reading. Latin, Greek and Cyrillic letters get no mark slots: they read without their
		// marks on both sides (see readings).
		lgc := false
		if r, _ := utf8.DecodeRuneInString(norm.NFD.String(c.raw)); readsWithoutMarks(r) {
			lgc = true
		}
		coreRunes, tail := splitTrailingMarks(c.raw)
		coreRaw := string(coreRunes)
		if coreRaw == "" {
			coreRaw, tail = c.raw, nil
		}
		rs := readings(coreRaw, fold, memo)
		if c.base != c.raw && c.base != coreRaw {
			// A letter followed by mapped marks also reads as the letter alone.
			for _, r := range readings(c.base, fold, memo) {
				if !slices.Contains(rs, r) {
					rs = append(slices.Clone(rs), r)
				}
			}
		}
		f, marks := splitTrailingMarks(rs[0])
		if len(f) == 0 {
			f, marks = []rune(rs[0]), nil
		}
		var opt [][]rune
		if !lgc {
			for _, m := range marks {
				opt = append(opt, []rune{m})
			}
			for _, m := range tail {
				set := []rune{m}
				for _, r := range readings(string(m), fold, memo) {
					if rr := []rune(r); len(rr) == 1 && !slices.Contains(set, rr[0]) {
						set = append(set, rr[0])
					}
				}
				opt = append(opt, set)
			}
		}
		sets := make([][]rune, len(f))
		for _, r := range rs {
			rr, _ := splitTrailingMarks(r)
			if len(rr) != len(f) {
				continue
			}
			for i, x := range rr {
				if !slices.Contains(sets[i], x) {
					sets[i] = append(sets[i], x)
				}
			}
		}
		n := len(sets) + len(opt)
		for i := range n {
			var set []rune
			var r rune
			if i < len(sets) {
				set, r = sets[i], f[i]
			} else {
				set = opt[i-len(sets)]
				r = set[0]
			}
			slices.Sort(set)
			a.slots = append(a.slots, string(set))
			a.from, a.to = append(a.from, c.from), append(a.to, c.to)
			a.start, a.end = append(a.start, i == 0), append(a.end, i == n-1)
			a.canEnd = append(a.canEnd, i >= len(sets)-1)
			a.at = append(a.at, b.Len())
			b.WriteRune(r)
		}
	}
	if isASCII(s) {
		for i := range len(s) {
			add(cluster{raw: s[i : i+1], base: s[i : i+1], from: i, to: i + 1})
		}
	} else {
		us, uFrom, uTo := prepared(s)
		clusters, ok := mergedClusters(us, uFrom, uTo, fold)
		a.overflow = !ok
		for _, c := range clusters {
			add(c)
		}
	}
	a.folded = b.String()
	a.at = append(a.at, b.Len())
	return a
}

// splitTrailingMarks returns the runes of r before its trailing combining marks, and those marks.
func splitTrailingMarks(r string) (core, marks []rune) {
	rr := []rune(r)
	k := len(rr)
	for k > 0 && attaches(rr[k-1]) {
		k--
	}
	return rr[:k], rr[k:]
}

// attaches reports a character that normalisation or rendering attaches to the letter before it:
// a mark of any kind (Mn, Mc, Me) or any character with a non-zero canonical combining class
// (WIS-623 Codex round 9: the spacing mark U+302E after "Nina").
func attaches(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Mc, unicode.Me) || norm.NFD.PropertiesString(string(r)).CCC() != 0
}

// match is a stored term found at request slots [start, end).
type match struct{ start, end int }

// find returns every place a stored term matches s: a run of whole clusters whose slots each
// share a reading with the stored term's slots, on word boundaries. All candidates from every
// start are collected and then chosen longest first, then leftmost, without overlap, so a
// shorter name never hides a longer one around it ("Νινα Smith" with "Smith" also known: WIS-623
// Codex round 7); overlapping candidates are joined (see below). Matching takes at most maxMatchWorkPerSlot trie steps per slot; past that
// overBudget is set and nothing is returned.
func (p *Pseudonyms) find(a analysis) []match {
	if a.overflow {
		p.overBudget = true
		return nil
	}
	if p.names == nil || len(p.names.kids) == 0 {
		return nil
	}
	// The budget is per input position: a short text costs little however many names are stored.
	budget, work := maxMatchWorkPerSlot*(len(a.slots)+1), 0
	var found []match
	for i := range a.slots {
		if !a.start[i] {
			continue
		}
		if p.ctx != nil && p.ctx.Err() != nil {
			p.overBudget = true
			return nil
		}
		active := []*trieNode{p.names}
		for q := i; q < len(a.slots) && len(active) > 0; q++ {
			p.names.clock++
			stamp := p.names.clock
			var next []*trieNode
			for _, n := range active {
				for _, r := range a.slots[q] {
					n.next(r, func(k *trieNode) {
						work++
						if k.seen != stamp {
							k.seen = stamp
							next = append(next, k)
						}
					})
				}
			}
			if work > budget || len(next) > maxActivePerSlot {
				p.overBudget = true
				return nil
			}
			active = next
			if a.canEnd[q] {
				end := q + 1
				for end < len(a.slots) && !a.start[end] {
					end++ // the optional mark slots left in the cluster go with the match
				}
				if slices.ContainsFunc(active, func(n *trieNode) bool {
					return n.term && boundary(a.folded, a.at[i], a.at[end]) || n.symbol && symbolBoundary(a.folded, a.at[i], a.at[end])
				}) {
					found = append(found, match{i, end})
				}
				if slices.ContainsFunc(active, func(n *trieNode) bool { return n.refuse }) && symbolBoundary(a.folded, a.at[i], a.at[end]) {
					p.refuseHit = true
				}
			}
		}
	}
	// Overlapping or touching candidates are replaced as one span, so no part of a known name is
	// left in clear: "Νινα Smith" with "Smith" also known is one span, and so is "Ram Kumar Singh"
	// with "Ram Kumar" and "Kumar Singh" (security review of 010223a6). The span is one token that
	// restores exactly the text it replaced.
	sort.Slice(found, func(x, y int) bool { return found[x].start < found[y].start })
	var chosen []match
	for _, m := range found {
		if n := len(chosen); n > 0 && m.start <= chosen[n-1].end {
			chosen[n-1].end = max(chosen[n-1].end, m.end)
			continue
		}
		chosen = append(chosen, m)
	}
	return chosen
}

// tokenFor returns the token for one exact replaced text. A token restores to exactly the text it
// replaced, never to a recorded spelling, so the restored answer carries nothing the request did
// not (WIS-623 security review: "ORBIT CASTINGS" must not come back as a sibling site's
// "Orbit Castings"). Each distinct spelling therefore gets its own token.
func (p *Pseudonyms) tokenFor(text string) string {
	if tok, ok := p.token[text]; ok {
		return tok
	}
	// A token never stands for two texts: one already issued is skipped (security review of
	// 233ae21e).
	tok := ""
	for {
		p.issued++
		if tok = fmt.Sprintf("⟦P%d⟧", p.issued); p.term[tok] == "" {
			break
		}
	}
	p.token[text], p.term[tok] = tok, text
	return tok
}

// hide pseudonymises one text. A JSON object or array (every user message, tool result and
// tool-call argument the runtime builds) is pseudonymised in its decoded strings, keys included,
// so a name written JSON-escaped ("A\u0026B Motors", "O\"Brien") or used as a map key is still
// found; a number whose exact value is a phone number becomes a quoted token. Everything else is
// kept byte for byte. Any other text, or JSON that cannot be rewritten, is pseudonymised as written.
func (p *Pseudonyms) hide(s string) string { return p.hideAt(s, 1) }

// maxJSONDepth is how many JSON layers a request string may nest: a JSON document (layer 1)
// whose strings may themselves be JSON documents (layer 2). Deeper nesting refuses the request
// (WIS-623 Codex round 10): it bounds the work and the re-escaping of every layer.
const maxJSONDepth = 2

// hideAt hides s at JSON layer depth. A JSON document is walked: its strings are hidden as text,
// or walked again when they are JSON documents themselves. A stored name that spans JSON syntax
// ("Alice\",\"role", "[42]") is therefore not hidden here; the audit reads every layer as text and
// refuses the request rather than guess how to rewrite it.
func (p *Pseudonyms) hideAt(s string, depth int) string {
	if isJSONDocument(s) {
		if depth > maxJSONDepth {
			p.overBudget = true
			return s
		}
		if out, err := walkJSON(s, func(v string) (string, error) {
			if isJSONDocument(v) {
				return p.hideAt(v, depth+1), nil
			}
			return p.hideTextKeepShape(v), nil
		}, p.hideNumber); err == nil {
			return out
		}
	}
	return p.hideTextKeepShape(s)
}

// hideTextKeepShape hides text that is not a JSON document. Hiding must not change whether a
// string parses as JSON: a malformed snippet that a token would turn into valid JSON
// ({"supplier":"O"Brien Tools"}) would come back restored as JSON, in another shape. Such a text
// refuses the request (WIS-623 Codex round 13). With that rule a string is restored in the mode
// it was hidden in: text stays text, and JSON is walked layer by layer on both sides.
func (p *Pseudonyms) hideTextKeepShape(s string) string {
	out := p.hideText(s)
	if isJSONDocument(out) != isJSONDocument(s) {
		p.overBudget = true
	}
	return out
}

// issuedTokenForm is a token exactly as hideText writes it.
var issuedTokenForm = regexp.MustCompile(`⟦P[0-9]+⟧`)

// hideCarried hides a message the model wrote in this exchange (see HideRequestContext): JSON is
// walked as in hide, and each text is hidden around this map's issued tokens, which are kept
// exactly as written (#278 review).
func (p *Pseudonyms) hideCarried(s string) string { return p.hideCarriedAt(s, 1) }

func (p *Pseudonyms) hideCarriedAt(s string, depth int) string {
	if isJSONDocument(s) {
		if depth > maxJSONDepth {
			p.overBudget = true
			return s
		}
		if out, err := walkJSON(s, func(v string) (string, error) {
			if isJSONDocument(v) {
				return p.hideCarriedAt(v, depth+1), nil
			}
			return p.hideCarriedKeepShape(v), nil
		}, p.hideNumber); err == nil {
			return out
		}
	}
	return p.hideCarriedKeepShape(s)
}

// hideCarriedKeepShape hides the text between this map's issued tokens and keeps the tokens.
// Like hideTextKeepShape, hiding must not change whether the text parses as JSON.
func (p *Pseudonyms) hideCarriedKeepShape(s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range issuedTokenForm.FindAllStringIndex(s, -1) {
		if _, issued := p.term[s[loc[0]:loc[1]]]; !issued {
			continue
		}
		b.WriteString(p.hideText(s[last:loc[0]]))
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(p.hideText(s[last:]))
	out := b.String()
	if isJSONDocument(out) != isJSONDocument(s) {
		p.overBudget = true
	}
	return out
}

func isJSONDocument(s string) bool {
	t := strings.TrimSpace(s)
	return t != "" && (t[0] == '{' || t[0] == '[') && json.Valid([]byte(t))
}

// walkJSON rewrites valid JSON s: every string (key or value) is replaced by str of its decoded
// text, re-encoded, and every number literal by num of it (nil keeps numbers). Structure is
// copied unchanged.
func walkJSON(s string, str func(string) (string, error), num func(string) string) (string, error) {
	return walkJSONLimit(s, str, num, 0)
}

// withoutJSONLiterals replaces the bare literal tokens true, false and null of a JSON document
// with spaces, found by tokenising it: strings are kept whole (with their escapes), and a literal
// counts only as a whole token. A string that is itself a JSON document has its own literals left
// out the same way (re-encoded in place), so a nested true is not read as a word either.
// Everything else is kept as written.
func withoutJSONLiterals(s string) string {
	var out strings.Builder
	word := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
	}
	for i := 0; i < len(s); {
		if s[i] == '"' {
			j := i + 1
			for ; j < len(s) && s[j] != '"'; j++ {
				if s[j] == '\\' {
					j++
				}
			}
			j = min(j+1, len(s))
			var v string
			if json.Unmarshal([]byte(s[i:j]), &v) == nil && isJSONDocument(v) {
				q := jsonString(withoutJSONLiterals(v))
				out.WriteString(q)
			} else {
				out.WriteString(s[i:j])
			}
			i = j
			continue
		}
		matched := false
		for _, lit := range [3]string{"true", "false", "null"} {
			if j := i + len(lit); j <= len(s) && s[i:j] == lit && (i == 0 || !word(s[i-1])) && (j == len(s) || !word(s[j])) {
				out.WriteString(strings.Repeat(" ", len(lit)))
				i, matched = j, true
				break
			}
		}
		if !matched {
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String()
}

// jsonString encodes s as a JSON string without HTML escaping, so "A&B" stays "A&B" rather
// than "A\u0026B" (WIS-623 Codex round 18).
func jsonString(s string) string {
	var b strings.Builder
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// unionTerms is a then b without repeats (by exact NFKC form): a continuation counts each name
// once against the bounds (WIS-623 Codex round 18).
func unionTerms(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range append(slices.Clone(a), b...) {
		if k := norm.NFKC.String(strings.TrimSpace(t)); !seen[k] {
			seen[k] = true
			out = append(out, t)
		}
	}
	return out
}

// errRestoreLimit is a restoration that would pass its output bound.
var errRestoreLimit = &ProviderError{Code: CodeProviderResponseLarge}

// walkJSONLimit is walkJSON with the output bounded to limit bytes (0: unbounded), checked as it
// is written.
func walkJSONLimit(s string, str func(string) (string, error), num func(string) string, limit int) (string, error) {
	var b strings.Builder
	over := func(n int) bool { return limit > 0 && b.Len()+n > limit }
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return "", errors.New("unterminated JSON string")
			}
			var v string
			if err := json.Unmarshal([]byte(s[i:j+1]), &v); err != nil {
				return "", err
			}
			out, err := str(v)
			if err != nil {
				return "", err
			}
			q := jsonString(out)
			if over(len(q)) {
				return "", errRestoreLimit
			}
			b.WriteString(q)
			i = j + 1
		case c == '-' || c >= '0' && c <= '9':
			j := i
			for j < len(s) && strings.IndexByte("+-.eE0123456789", s[j]) >= 0 {
				j++
			}
			if num != nil {
				b.WriteString(num(s[i:j]))
			} else {
				b.WriteString(s[i:j])
			}
			i = j
		default:
			if over(1) {
				return "", errRestoreLimit
			}
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

// hideNumber returns a JSON number literal unchanged unless its exact value is an integer whose
// digits are a phone number ("9876543210", "9876543210.0", "9.876543210e9" alike); that becomes a
// quoted token, the same one the digits get in text. The value is computed exactly (math/big).
// An exponent far beyond the literal's own length cannot give a ten-digit integer and is never
// expanded, so a hostile literal cannot make the walk allocate.
func (p *Pseudonyms) hideNumber(lit string) string {
	if digits, ok := p.phoneValue(lit); ok {
		q := jsonString(p.tokenFor(digits))
		return q
	}
	// A number literal whose text is a known name ("1e3" stored) is hidden like one (Codex
	// round 12): every name detector reads it as text.
	if sp := p.nameSpans(lit); len(sp) == 1 && sp[0] == [2]int{0, len(lit)} {
		q := jsonString(p.tokenFor(lit))
		return q
	}
	return lit
}

// phoneValue reports whether number literal lit is exactly an integer whose digits are a phone
// number, and returns those digits. An exponent far beyond the literal's own length cannot give
// a ten-digit integer and is never expanded ("1e999999999" costs nothing).
func (p *Pseudonyms) phoneValue(lit string) (string, bool) {
	// Classified by the linear decimal key, never by parsing a big number: a ten-digit integer
	// has magnitude 10 and at most ten significant digits (WIS-623 Codex round 21: 900,000
	// digits cost superlinear work).
	if !p.charge(len(lit)) {
		p.overBudget = true
		return "", false
	}
	k, ok := decimalKey(lit)
	if !ok || len(k) < 2 || k[0] != '+' && k[0] != '-' {
		return "", false
	}
	d, mag, _ := strings.Cut(k[1:], "e")
	if mag != "10" || len(d) > 10 {
		return "", false
	}
	digits := d + strings.Repeat("0", 10-len(d))
	if digits[0] >= '6' {
		return digits, true
	}
	return "", false
}

// numberToken is a standalone decimal number in text, with an optional fraction and exponent.
var numberToken = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?(?:[eE][-+]?[0-9]+)?`)

// numberSpans applies the JSON number rule to numbers written in prose ("9.876543210e9",
// "9876543210.0"): one whose exact value is a phone number becomes that number's token.
func (p *Pseudonyms) numberSpans(s string) [][2]int {
	var out [][2]int
	for _, loc := range numberToken.FindAllStringIndex(s, -1) {
		i, j := loc[0], loc[1]
		if i > 0 {
			if r, _ := utf8.DecodeLastRuneInString(s[:i]); unicode.IsLetter(r) || r == '.' {
				continue
			}
		}
		if r, _ := utf8.DecodeRuneInString(s[j:]); j < len(s) && unicode.IsLetter(r) {
			continue
		}
		if _, ok := p.phoneValue(s[i:j]); ok {
			out = append(out, [2]int{i, j})
		}
	}
	return out
}

// phoneSpans finds unmistakable phone numbers: an international number written with "+", or a
// 10-digit Indian mobile number. The ten digits are matched alone and their neighbours checked
// separately, so adjacent numbers in a contact list ("9876543210 8765432109", ".../...", one per
// line) and a number ending a sentence ("Phone: 9876543210.") are all caught, while a digit run
// that is part of a longer number or a decimal ("1.9876543210", "9876543210.5") is left alone.
func phoneSpans(s string) [][2]int {
	var out [][2]int
	for _, loc := range phonePattern.FindAllStringIndex(s, -1) {
		if i, j := loc[0], loc[1]; s[i] == '+' || phoneEdges(s, i, j) {
			out = append(out, [2]int{i, j})
		}
	}
	return out
}

func phoneEdges(s string, i, j int) bool {
	digit := func(k int) bool { return k >= 0 && k < len(s) && s[k] >= '0' && s[k] <= '9' }
	if digit(i-1) || digit(j) {
		return false
	}
	if i >= 2 && s[i-1] == '.' && digit(i-2) { // the fraction of a decimal
		return false
	}
	return !(s[j:] != "" && s[j] == '.' && digit(j+1)) // the integer part of a decimal
}

// hideText pseudonymises plain text. Token-shaped text already in the source is neutralised
// first, so a document cannot plant a token that restores to a real name.
func (p *Pseudonyms) hideText(s string) string {
	s = tokenBrackets.Replace(s)
	// Every detector reads the same text: e-mail addresses, phone numbers, numbers whose value is
	// a phone number, letterless names and known names. Overlapping or touching places are joined
	// and each join is replaced once, by one token that restores exactly the text it replaced, so
	// no detector can hide part of what another would find ("Alice alice@example.com" stored as
	// one name: WIS-623 Codex round 8).
	spans := p.textSpans(s)
	if len(spans) == 0 {
		return s
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	var b strings.Builder
	last, cur := 0, spans[0]
	flush := func() {
		if cur[1]-cur[0] > maxTokenSpan {
			p.overBudget = true // one token may stand for at most maxTokenSpan bytes
		}
		b.WriteString(s[last:cur[0]])
		b.WriteString(p.tokenFor(s[cur[0]:cur[1]]))
		last = cur[1]
	}
	for _, sp := range spans[1:] {
		if sp[0] <= cur[1] {
			cur[1] = max(cur[1], sp[1])
			continue
		}
		flush()
		cur = sp
	}
	flush()
	b.WriteString(s[last:])
	return b.String()
}

// textSpans are the places every text detector finds in s.
func (p *Pseudonyms) textSpans(s string) [][2]int {
	var spans [][2]int
	for _, loc := range emailPattern.FindAllStringIndex(s, -1) {
		spans = append(spans, [2]int{loc[0], loc[1]})
	}
	spans = append(spans, phoneSpans(s)...)
	spans = append(spans, p.numberSpans(s)...)
	spans = append(spans, p.nameSpans(s)...)
	return spans
}

// nameSpans are the original byte spans of the known names find locates in s, so "Ａｌｉｃｅ",
// "A\u200blice" and a Cyrillic "Аlice" are each replaced whole.
func (p *Pseudonyms) nameSpans(s string) [][2]int {
	if p.names == nil || len(p.names.kids) == 0 {
		return nil
	}
	a := analyze(s, cases.Fold(), map[string][]string{})
	var out [][2]int
	for _, m := range p.find(a) {
		out = append(out, [2]int{a.from[m.start], a.to[m.end-1]})
	}
	return out
}

//go:generate go run confusables_gen.go

// lookalikes is the project's own folding of lower-case Cyrillic and Greek letters to the Latin
// letter they look like, applied before the UTS #39 data (confusables_table.go). It keeps the
// readings the earlier views established where the official prototype is a different letter
// (β is "ß" there, м is "ʍ", н is small capital "ʜ", κ is "ĸ", ε is "ꞓ") and so would not meet
// the Latin letter of a name after case folding.
var lookalikes = map[rune]rune{
	// Cyrillic
	'а': 'a', 'в': 'b', 'е': 'e', 'ё': 'e', 'з': '3', 'к': 'k', 'м': 'm', 'н': 'h', 'о': 'o', 'п': 'n', 'р': 'p', 'с': 'c', 'т': 't',
	'у': 'y', 'х': 'x', 'ь': 'b', 'і': 'i', 'ї': 'i', 'ј': 'j', 'ѕ': 's', 'һ': 'h', 'ԁ': 'd', 'ӏ': 'l', 'ԛ': 'q', 'ԝ': 'w', 'г': 'r',
	// Greek
	'α': 'a', 'β': 'b', 'γ': 'y', 'ε': 'e', 'ζ': 'z', 'η': 'n', 'ι': 'i', 'κ': 'k', 'μ': 'u', 'ν': 'v', 'ο': 'o', 'ρ': 'p',
	'σ': 'o', 'τ': 't', 'υ': 'u', 'χ': 'x', 'ω': 'w',
}

// viewKind selects one of the two canonical views. A single view cannot serve both readings of a
// few Greek capitals: capital nu looks like Latin "N" while its small letter looks like "v". The
// visual view maps look-alikes before case folding, so "ΝΙΝΑ" reads "nina" (a disguised Latin
// "Nina"); the folded view folds case first, so "ΝΙΚΟΣ" and "νικος" both read "vikoo" (one Greek
// name in two cases). Text is matched on both; terms carry the keys of both.
type viewKind int

const (
	visualView viewKind = iota
	foldedView
)

// canonicalView is the view names are matched on. The same function serves stored terms and
// request text (WIS-623 Codex rounds 3 to 6):
//  1. NFKC (full-width and other compatibility forms become plain letters and digits);
//  2. default-ignorable characters removed: format characters (Unicode Cf: zero-width spaces and
//     joiners, U+2060, U+FEFF, soft hyphen, bidirectional marks), variation selectors, the
//     combining grapheme joiner, Hangul fillers and the other Default_Ignorable_Code_Points;
//  3. NFKC again, so marks that a removed character kept apart compose ("A", U+200B, U+0301 is
//     "Á" like the composed letter);
//  4. per cluster (a starter and its marks): full Unicode case folding (golang.org/x/text/cases,
//     so final sigma, U+1C80 and the like fold to their letter; made case-consistent and with
//     the dot of a folded İ dropped by caseFold) and the look-alike mapping:
//     lookalikes, then the UTS #39 prototype of every non-ASCII character (confusables_table.go,
//     Unicode 16.0.0). The visual view maps once before folding as well. ASCII is never mapped:
//     "m" is not "rn" and digits stay digits for the phone and number rules. A non-ASCII
//     capital whose prototype is "l" (Greek and Cyrillic capital I) maps to "I" in the visual
//     view, since after folding the Latin capital I it imitates reads "i". The cluster is
//     repeated through this step until it settles, so the view of a view is itself.
//
// from[i] and to[i] are the original byte span that canonical byte i came from.
func canonicalView(s string, k viewKind) (string, []int, []int) {
	// ASCII text has no compatibility forms, ignorables or mapped characters: both views are its
	// lower case, byte for byte (the common case, kept cheap).
	if isASCII(s) {
		from, to := make([]int, len(s)), make([]int, len(s))
		for i := range s {
			from[i], to[i] = i, i+1
		}
		return strings.ToLower(s), from, to
	}
	// Steps 3 and 4: NFKC clusters of the result, each folded and mapped as a whole.
	us, uFrom, uTo := prepared(s)
	fold := cases.Fold()
	var b strings.Builder
	var from, to []int
	clusters, _ := mergedClusters(us, uFrom, uTo, fold) // past the merge bound, analyze refuses the text
	for _, c := range clusters {
		out := canonicalCluster(c.raw, k, fold)
		n := b.Len()
		b.WriteString(out)
		for range b.Len() - n {
			from, to = append(from, c.from), append(to, c.to)
		}
	}
	return b.String(), from, to
}

// prepared does steps 1 and 2: NFKC segments of s, ignorables dropped, each byte of the result
// with the original span of the segment it came from.
func prepared(s string) (string, []int, []int) {
	var u strings.Builder
	var uFrom, uTo []int
	var it norm.Iter
	it.InitString(norm.NFKC, s)
	for !it.Done() {
		start := it.Pos()
		seg := it.Next()
		end := it.Pos()
		for _, r := range string(seg) {
			if ignorable(r) {
				continue
			}
			n := u.Len()
			u.WriteRune(r)
			for range u.Len() - n {
				uFrom, uTo = append(uFrom, start), append(uTo, end)
			}
		}
	}
	return u.String(), uFrom, uTo
}

// cluster is a run of normalised text read as one unit, with its original byte span.
type cluster struct {
	raw      string
	base     string // raw without the clusters merged into it (see mergedClusters)
	from, to int
}

// maxTokenSpan bounds the original text one token may stand for: a longer join of detected
// places refuses the request rather than becoming one token (WIS-623 Codex round 9).
const maxTokenSpan = 4 << 10

// maxMergedMarks bounds the mark clusters merged into one letter, as stream-safe normalisation
// bounds non-starters (UAX #15: 30). Text past it is refused, not read in part.
const maxMergedMarks = 30

// mergedClusters splits us (steps 1 and 2 done) into NFKC clusters. A cluster whose folded
// reading begins with a combining mark (the UTS #39 prototype of U+0E4D is U+030A) belongs to the
// letter before it: the two are read again as one cluster, so the mark composes with that letter
// as it would in text that wrote the composed letter ("A" U+0E4D "ke" reads like "Åke", WIS-623
// Codex round 7). Merging repeats while the next cluster starts with a mark, so the result is a
// whole-string fixpoint, at most maxMergedMarks marks per letter: past that ok is false. uFrom
// and uTo give each byte of us its original span.
func mergedClusters(us string, uFrom, uTo []int, fold cases.Caser) (out []cluster, ok bool) {
	ok = true
	var raw strings.Builder
	merged := 0
	flush := func() {
		if len(out) > 0 {
			out[len(out)-1].raw = raw.String()
		}
	}
	var it norm.Iter
	it.InitString(norm.NFKC, us)
	for !it.Done() {
		start := it.Pos()
		seg := it.Next()
		end := it.Pos()
		if start == end {
			continue
		}
		raw0, _ := utf8.DecodeRune(seg)
		if r, _ := utf8.DecodeRuneInString(canonicalCluster(string(seg), foldedView, fold)); len(out) > 0 && (attaches(r) || attaches(raw0)) {
			if merged++; merged > maxMergedMarks {
				ok = false
				break
			}
			raw.Write(seg)
			out[len(out)-1].to = max(out[len(out)-1].to, uTo[end-1])
			continue
		}
		flush()
		raw.Reset()
		raw.Write(seg)
		merged = 0
		out = append(out, cluster{base: string(seg), from: uFrom[start], to: uTo[end-1]})
	}
	flush()
	return out, ok
}

// canonicalCluster folds and maps one cluster until it settles (at most four rounds).
func canonicalCluster(x string, k viewKind, fold cases.Caser) string {
	for range 4 {
		y := x
		if k == visualView {
			y = lookalikeMap(y, true)
		}
		y = lookalikeMap(caseFold(y, fold), false)
		y = norm.NFKC.String(strings.Map(func(r rune) rune {
			if ignorable(r) {
				return -1
			}
			return r
		}, y))
		if y == x {
			break
		}
		x = y
	}
	return x
}

// caseFold is full Unicode case folding made case-consistent. x/text's Fold maps Cherokee small
// letters to capitals and capitals to small letters, so each rune is then lower-cased; and full
// folding writes the Turkish dotted capital İ as "i" plus U+0307, so a combining dot above that
// follows a soft-dotted letter (i, j and the like, which carry the dot already) is dropped. The
// result is composed again (NFC), so "j" plus a caron is "ǰ" like the precomposed letter
// (WIS-623 security review of 8aafb153).
func caseFold(s string, fold cases.Caser) string {
	var b strings.Builder
	var prev rune
	for _, r := range norm.NFD.String(fold.String(s)) {
		r = unicode.ToLower(r)
		if r == 0x0307 && unicode.Is(unicode.Soft_Dotted, prev) {
			continue
		}
		if unicode.Is(unicode.Mn, r) && r != 0x0307 {
			b.WriteRune(r) // other marks do not reset the soft-dotted base
			continue
		}
		b.WriteRune(r)
		prev = r
	}
	return norm.NFC.String(b.String())
}

// lookalikeMap maps each non-ASCII character of the decomposed s to the Latin letter it looks
// like: lookalikes first, then the UTS #39 prototype. Token delimiters are never mapped.
func lookalikeMap(s string, visual bool) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		switch t, ok := confusables[r]; {
		case r < utf8.RuneSelf || r == '⟦' || r == '⟧':
			b.WriteRune(r)
		case lookalikes[r] != 0:
			b.WriteRune(lookalikes[r])
		case ok && visual && t == "l" && unicode.IsUpper(r):
			b.WriteByte('I')
		case ok:
			b.WriteString(t)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// ignorable reports a Default_Ignorable_Code_Point (removed from the view).
func ignorable(r rune) bool {
	return unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Variation_Selector, r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) ||
		r == 0x034F || r == 0x115F || r == 0x1160 || r == 0x3164 || r == 0xFFA0
}

// canonicalString is the canonical view k of s.
func canonicalString(s string, k viewKind) string {
	c, _, _ := canonicalView(s, k)
	return c
}

// boundary reports that s[i:j] is not inside a longer word (Unicode letters and digits, so it
// works for Devanagari names as well). Scripts written without spaces between words (Han, kana,
// Hangul with its attached particles, Thai and its neighbours) have no word boundary to find: an
// edge of the match in such a script, or next to one ("Li说"), is always a boundary. A match never starts inside an
// issued token ("P1" in "⟦P1⟧").
func boundary(s string, i, j int) bool {
	if i > 0 {
		r, _ := utf8.DecodeLastRuneInString(s[:i])
		first, _ := utf8.DecodeRuneInString(s[i:])
		if r == '⟦' || (unicode.IsLetter(r) || unicode.IsDigit(r)) && !unspaced(first) && !unspaced(r) {
			return false
		}
	}
	if j < len(s) {
		r, _ := utf8.DecodeRuneInString(s[j:])
		last, _ := utf8.DecodeLastRuneInString(s[:j])
		if (unicode.IsLetter(r) || unicode.IsDigit(r)) && !unspaced(last) && !unspaced(r) {
			return false
		}
	}
	return true
}

func unspaced(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Thai, unicode.Lao, unicode.Khmer, unicode.Myanmar)
}

// HideRequest returns a copy of r with every message, and the arguments of earlier tool calls,
// pseudonymised. Tool definitions and the response schema are static and carry no names.
//
// It then fails closed (leaks): every outbound string is decoded layer by layer as a reader of it
// could decode it (JSON and JavaScript escapes, \xXX, percent-encoding, HTML entities), and if
// any layer's canonical view still contains a known name, an e-mail address or a phone
// number, or a decoding step is lossy, or the stages exceed the audit budget, the
// request is refused with PSEUDONYMS_INCOMPLETE instead of sent. Refusing is chosen over hiding
// inside the decoded view and re-escaping because the escaped text can be anything (a JSON
// fragment in prose, a fenced block, an HTML page): re-escaping it faithfully is not possible in
// general, and a refused call fails visibly while a wrong rewrite would leak silently.
//
// Stated limit: pseudonymisation is best-effort. Names inside base64 or other binary encodings
// are not detected (speculative base64 decoding both missed short or split names and refused
// ordinary words that happen to decode to a known name). The control for data leaving the host
// is the site owner's opt-in with a data processing agreement (rule 08).
func (p *Pseudonyms) HideRequest(r ModelRequest) (ModelRequest, error) {
	return p.HideRequestContext(context.Background(), r)
}

// HideRequestContext is HideRequest under ctx: a canceled context refuses the request. Before
// anything is replaced, the original text is checked for number names (WIS-623 Codex round 9: a
// phone token must not consume part of "42 9876543210"); after, the audit reads the output. The
// audit's work is charged to a budget of auditWorkPerByte per request byte (plus auditWorkBase);
// past it the request is refused.
func (p *Pseudonyms) HideRequestContext(ctx context.Context, r ModelRequest) (ModelRequest, error) {
	refuse := func() (ModelRequest, error) { return ModelRequest{}, &ProviderError{Code: CodePseudonymsIncomplete} }
	size := 0
	for _, m := range r.Messages {
		size += len(m.Content)
		for _, c := range m.ToolCalls {
			size += len(c.Arguments)
		}
	}
	p.ctx, p.auditBudget = ctx, auditWorkBase+auditWorkPerByte*size
	out := r
	out.Messages = make([]Message, len(r.Messages))
	for i, m := range r.Messages {
		// A JSON document is read through json.Unmarshal, which writes an unpaired surrogate as
		// U+FFFD before any audit sees it: the text as written is checked first (Codex round 36).
		if nestedUnpairedSurrogate(m.Content, 0) || slices.ContainsFunc(m.ToolCalls, func(c ToolCall) bool { return nestedUnpairedSurrogate(string(c.Arguments), 0) }) {
			return refuse()
		}
		if m.hidden {
			// A message the model wrote, carried with this map's tokens. Its tokens are kept as
			// written and are never restored and hidden again. The clear text around them is
			// hidden like any other text, so an ordinary word the model wrote that equals a short
			// known name ("will" for a member called Will) is tokenised, not refused (#278
			// review). A token-shaped text this map did not issue still refuses, in any written
			// form. Then the message is audited as it goes out, content and tool arguments.
			if _, unknown := p.withoutIssuedTokens(m.Content); unknown || slices.ContainsFunc(m.ToolCalls, func(c ToolCall) bool {
				_, u := p.withoutIssuedTokens(string(c.Arguments))
				return u
			}) {
				return refuse()
			}
			m.Content = p.hideCarried(m.Content)
			if len(m.ToolCalls) > 0 {
				calls := make([]ToolCall, len(m.ToolCalls))
				for k, c := range m.ToolCalls {
					c.Arguments = json.RawMessage(p.hideCarried(string(c.Arguments)))
					calls[k] = c
				}
				m.ToolCalls = calls
			}
			p.auditCarried = true
			leak := p.overBudget || p.leaks(m.Content) || slices.ContainsFunc(m.ToolCalls, func(c ToolCall) bool { return p.leaks(string(c.Arguments)) })
			p.auditCarried = false
			if leak {
				return refuse()
			}
			out.Messages[i] = m
			continue
		}
		if p.refusedLayers(m.Content) {
			return refuse()
		}
		m.Content = p.hide(m.Content)
		if p.overBudget || p.leaks(m.Content) {
			return refuse()
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]ToolCall, len(m.ToolCalls))
			for k, c := range m.ToolCalls {
				if p.refusedLayers(string(c.Arguments)) {
					return refuse()
				}
				c.Arguments = json.RawMessage(p.hide(string(c.Arguments)))
				if p.overBudget || p.leaks(string(c.Arguments)) {
					return refuse()
				}
				calls[k] = c
			}
			m.ToolCalls = calls
		}
		out.Messages[i] = m
	}
	return out, nil
}

// refusedLayers reports a number name in s or in any decoding stage of it (see decodeSteps).
func (p *Pseudonyms) refusedLayers(s string) bool {
	if len(p.refuse) == 0 {
		return false
	}
	return !p.stages(s, nil, func(v string) bool { return p.charge(matchCost(v)) && !p.refused(v) })
}

// Audit budget, in units of about a nanosecond of work on the reference machine, every kind of
// work charged at its measured cost (decoding steps, the escape search, reading sets, matcher
// passes on ASCII and on other text), so units stay within 1.5 times of nanoseconds across
// realistic, densely escaped, non-Latin and long inputs. A request may take auditWorkPerByte per
// byte plus auditWorkBase (about 0.25 s); past it the request is refused. The bound is on work, never on wall-clock time, so it
// refuses the same input the same way under -race (gate of 3df43c8d).
const (
	costStep         = 8
	costStage        = 32768 // the bookkeeping of one stage (its key, positions, queue)
	costStepRead     = 160   // a decoding step over a byte of text that is not all ASCII (read over reading sets)
	costReadings     = 8192  // computing the readings of one character not read before
	costSearch       = 16    // one step of the escape search over reading sets
	costMatch        = 400   // ASCII text
	costMatchRead    = 900   // text that is not all ASCII (the canonical views take their slow path)
	auditWorkPerByte = 49152
	auditWorkBase    = 256 << 20
)

// matchCost is the cost of one matcher pass over x.
func matchCost(x string) int {
	if isASCII(x) {
		return costMatch * len(x)
	}
	return costMatchRead * len(x)
}

// charge takes n units of audit work; false means the budget or the context is spent (refuse).
func (p *Pseudonyms) charge(n int) bool {
	if p.ctx != nil && p.ctx.Err() != nil {
		return false
	}
	if p.auditBudget == 0 {
		return true // no budget set: a map used outside HideRequest (tests of single steps)
	}
	if p.auditBudget -= n; p.auditBudget <= 0 {
		p.auditBudget = -1 // spent stays spent: 0 would read as "no budget"
		return false
	}
	return true
}

// The residual audit decodes what a reader of the outbound text could decode, one layer per
// pass, and looks for anything hideText would still replace in every layer's canonical view.
var (
	jsonEscapeRun = regexp.MustCompile(`(?:\\u[0-9a-fA-F]{4})+|\\["\\/bfnrt]`) // \uXXXX runs decode together (surrogate pairs)
	braceEscape   = regexp.MustCompile(`\\u\{[0-9a-fA-F]+\}`)
	percentURun   = regexp.MustCompile(`(?:%u[0-9a-fA-F]{4})+`)
	hexEscapeRun  = regexp.MustCompile(`(?:\\x[0-9a-fA-F]{2})+`)
	percentRun    = regexp.MustCompile(`(?:%[0-9a-fA-F]{2})+`)
)

// leaks reports whether pseudonymised text s may still name someone. A complete JSON document is
// audited by type: its decoded strings (keys and values) as text, its numbers by the phone-value
// rule, and never its literals true, false and null.
func (p *Pseudonyms) leaks(s string) bool { return p.leaksAt(s, 1) }

// leaksAt audits s at JSON layer depth: first as text (every detector and the number names), then,
// when it is a JSON document, each of its strings and numbers at the next layer (WIS-623 Codex
// round 10: a stored name shaped like JSON is seen whole at the layer that holds it).
func (p *Pseudonyms) leaksAt(s string, depth int) bool {
	if !isJSONDocument(s) {
		return p.leaksText(s)
	}
	// A JSON document is audited as a whole text with only its bare literal tokens true, false and
	// null left out (they are syntax, never compared with names: Codex rounds 3 and 11), and by
	// structure: its strings (each as text, then, when it is JSON, again at the next layer) and
	// its numbers. Names with JSON syntax are also read on the layer's raw text, literals
	// included ("[true]"): such a name left anywhere in what goes on the wire, however the
	// re-encoding produced it, refuses (Codex round 12). This audits the hidden output, so it
	// replaces the check made before hiding.
	if p.leaksText(withoutJSONLiterals(s)) {
		return true
	}
	if p.jsonish != nil {
		// Read on the layer's raw text and on each of its decoded layers (JSON escapes and the
		// other encodings decodeSteps undo), structure and literals kept: "A\u0026B\":true"
		// is "A&B\":true" once decoded (Codex round 13).
		q := p.jsonish.fresh()
		q.ctx = p.ctx
		if !p.stages(s, nil, func(v string) bool {
			return p.charge(2*matchCost(v)) && len(q.nameSpans(v)) == 0 && !q.refused(v) && !q.overBudget
		}) {
			return true
		}
	}
	if depth > maxJSONDepth {
		return true
	}
	found := false
	_, err := walkJSON(s, func(v string) (string, error) {
		found = found || p.leaksAt(v, depth+1)
		return v, nil
	}, func(lit string) string {
		_, phone := p.phoneValue(lit)
		found = found || phone || p.refusedNumber(lit)
		return lit
	})
	return found || err != nil
}

// leaksText audits every decoding layer of s. Only the tokens this request issued, found exactly
// in the outbound text itself, are set aside; any other token delimiter, raw or recovered by
// decoding, is refused, so decoded text cannot pass itself off as a placeholder. It is a leak when
// any layer's canonical view holds a known name, e-mail or phone; when a layer decodes lossily;
// or when the stages exceed the audit budget.
func (p *Pseudonyms) leaksText(s string) bool {
	if p.replAll == nil || p.replIssued != p.issued {
		issued := make([]string, 0, 2*len(p.term))
		for tok := range p.term {
			issued = append(issued, tok, " ")
		}
		p.replAll, p.replIssued = strings.NewReplacer(issued...), p.issued
	}
	// With every placeholder taken out, the phone and e-mail detectors must find nothing: no token,
	// however its name was matched (a symbol name through a reading, "＊＊" for "**"), can hide
	// that it split one (security reviews of e59e7736 and 89e4bb50).
	if w := pseudonymToken.ReplaceAllString(s, ""); emailPattern.MatchString(w) || len(phoneSpans(w)) > 0 {
		return true
	}
	scratch := p.fresh()
	// In a carried model message, this map's tokens in any written form are placeholders (Codex
	// round 16): every stage has them taken out before it is read or decoded further. Anywhere
	// else only the literal tokens hiding inserted are (taken out above): token-shaped text a user
	// wrote, encoded, would otherwise pass as a placeholder and be restored to a name (Codex round
	// 17), so it refuses.
	var rewrite func(string) (string, bool)
	if p.auditCarried {
		rewrite = func(v string) (string, bool) {
			v, unknown := p.withoutIssuedTokens(v)
			return v, !unknown
		}
	}
	return !p.stages(p.replAll.Replace(s), rewrite, func(v string) bool {
		passes := 3
		if len(p.refuse) > 0 {
			passes++
		}
		if !p.charge(passes*matchCost(v)) || anyToken.MatchString(v) || strings.ContainsAny(v, "⟦⟧") || scratch.hideText(v) != v || p.refused(v) {
			return false
		}
		for _, k := range [2]viewKind{visualView, foldedView} {
			if c := canonicalString(v, k); scratch.hideText(c) != c {
				return false
			}
		}
		return !scratch.overBudget
	})
}

// decodeSteps are the decodings a reader of the outbound text could apply, each exactly one
// transformation: JSON and JavaScript escapes, \u{...}, \xXX, percent-encoding, HTML entities.
// The audit and the round-trip check read every stage they reach, in any order (WIS-623 Codex
// round 27: "ACME\\x41Tools" is the stored "ACME\x41Tools" after JSON unescaping alone, and
// "ACMEATools" only after hex decoding too). A step reports lossy for a decode that is not exact:
// a \u run that is not valid UTF-16, or escaped bytes that decode to a character that is neither
// printable nor space, or %/\x escaped bytes that are not valid UTF-8. Such bytes have no reading
// the audit can trust (as Latin-1, "Al%FFice" is the word "Alÿice"), so the call is refused.
// Accepted false refusal: a name written in legacy Latin-1 percent-encoding ("Jos%E9") is refused
// rather than hidden; UTF-8 percent-encoding ("Jos%C3%A9") is decoded and audited.
//
// The escape catalogue (WIS-623 Codex rounds 37 and 38). The audit reads exactly these textual
// escape syntaxes, each by a literal decoder of its own on ASCII escapes, each a stage of its own.
// Core decoders (marked core below) chain in any order. Each other language family is a leaf: it
// reads the text as written, as its language's parser does, and its output is read once more by
// the core decoders, but not by another family; a transport before such a family (URL-encoded
// TeX) is not read. Canonical steps change no escape.
//
//	syntax                                  reader                         step
//	\uXXXX \" \\ \/ \b \f \n \r \t          JSON, JavaScript               core: JSON unescape
//	\u{X...} (leading zeros)                JavaScript                     core: brace escape (<= 10FFFF)
//	\xHH runs                               C, Python bytes                core: bytes as UTF-8
//	%HH runs                                URL percent-encoding           core: bytes as UTF-8
//	&#D; &#xH; &name;                       HTML, XML                      core: html.UnescapeString
//	\' \v \0 \101 \ + line break, \ + other  JavaScript (sloppy)            core: jsEscapes
//	escapes spelled outside ASCII           any                            core: refused (lookalikeEscape)
//	\xHH                                    JavaScript, Python str         core: Latin-1 code unit
//	\xH... / \xH{1,4}                       C, C++ / C#                    family: code point
//	\UXXXXXXXX                              Python, C#                     core: code point
//	\uH{1,4} \UH{1,8}                       Bash $'...'                    family: code point
//	\uuuuXXXX                               Java                           family: code point
//	\U{H...}                                R                              family: code point
//	\u{6_9} \u{69 6c}                       Rust, Ruby                     family: code points
//	\N{NAME} / \N{U+H}                      Python / Perl                  core: refused / code point
//	\x{H} \o{O} \U \L \u \l \Q \E           Perl                           family: code point, case
//	\ooo (to \777) / octal byte runs        Python / C, Python bytes       family: code point / UTF-8
//	\ddd byte runs, \z                      Lua                            family: UTF-8, skip whitespace
//	%HH                                     JavaScript unescape            family: Latin-1 code unit
//	%uXXXX runs                             JavaScript unescape            core: UTF-16
//	\H{1,6} + space, \ + other character    CSS (CRLF, CR, FF read as LF)  core: code point
//	\s \cx \C-x \M-x                        Ruby                           family: control, meta
//	\0 \a ... \_ \N \L \P, \ + line break    YAML                           family: named, folding
//	\ + spaces + line break                 TOML                           family: trims whitespace
//	+                                       form-urlencoded                family: space
//	'' ""                                   SQL, CSV                       family: one quote
//	U&'...' \XXXX \+XXXXXX UESCAPE          SQL                            family: code points
//	`n `u{H} ...                            PowerShell                     family: named, code point
//	\#u{H} \#n \("...")                     Swift                          family: code point, literal
//	#(0000H) #(cr,lf) #(#)                  Power Query                    family: code point, named
//	\'HH \uN?                               RTF                            family: cp1252, code unit
//	^^hh ^^X                                TeX                            family: byte, X +/- 64
//	default-ignorables                      renderers                      canonical: removed
//	Unicode decimal digits                  Java Character.digit, Python   canonical: ASCII digits
//
// Every syntax spelled with a character outside ASCII is refused, not decoded (lookalikeEscape),
// and so are default-ignorables inside an escape. Not read: identity escapes after a look-alike
// backslash (any character would complete one: "A ∖ B" would refuse), and look-alike "+" and
// quotes (decoding them joins no letters).
//
// Known limits (accepted; pseudonymisation is best-effort, the site owner's opt-in with a data
// processing agreement is the control for data leaving the host, rule 08):
//   - Escape styles outside this table are not read: base64, quoted-printable, punycode and any
//     other encoding or language escape not listed.
//   - The families that are not core read the text as written only: a core decoder that produces
//     one of their escapes ("&#94;^69", HTML giving TeX "^^69") is not read by them.
//   - A transport applied before such a family is not decoded through it (URL-encoded TeX,
//     "%5E%5E69").
//   - Python's \N{NAME} is refused rather than read (no Unicode name table in the standard
//     library); Perl's \N{U+H} is read.
//   - Very escape-dense input can spend the audit budget, or pass maxDecodeDepth; the request is
//     then refused (PSEUDONYMS_INCOMPLETE), never sent unread.
func (p *Pseudonyms) decodeSteps() []func(string) (string, bool) {
	steps := make([]func(string) (string, bool), 0, escapeSteps+2)
	steps = append(steps, asciiDecoders[:]...)
	steps = append(steps, familySteps(true)...)
	steps = append(steps,
		func(s string) (string, bool) { return jsEscapes(s), false },
		func(s string) (string, bool) { return s, lookalikeEscape(p.charge, s) })
	steps = append(steps, familySteps(false)...)
	return append(steps,
		func(s string) (string, bool) {
			if isASCII(s) {
				return s, false
			}
			return strings.Map(func(r rune) rune {
				if ignorable(r) {
					return -1
				}
				return r
			}, s), false
		},
		func(s string) (string, bool) { return asciiDigits(s), false })
}

// jsEscapes decodes the JavaScript string escapes no other step decodes (security review of
// f7ba2518): "\'" and "\v"; "\0" and the legacy octal escapes ("\151" is "i": up to three digits
// when the first is 0 to 3, else up to two); a line continuation (a backslash before a line
// terminator: "\n", "\r\n", "\r", U+2028, U+2029) removes both; and an identity escape drops the
// backslash before any other character ("\в" is "в", Codex round 36). A pair that another step
// decodes is kept for it: \\ \" \/ \b \f \n \r \t (the JSON escapes), \u and \x.
func jsEscapes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		c := s[i+1]
		switch {
		case strings.IndexByte(`\"/bfnrtux`, c) >= 0:
			b.WriteString(s[i : i+2]) // decoded by the JSON, \u{...} or \x step
			i += 2
		case c == '\'':
			b.WriteByte('\'')
			i += 2
		case c == 'v':
			b.WriteByte('\v')
			i += 2
		case c >= '0' && c <= '7':
			n, v := 2, 0 // at most three digits when the first is 0 to 3, else two
			if c <= '3' {
				n = 3
			}
			j := i + 1
			for ; j < len(s) && j < i+1+n && s[j] >= '0' && s[j] <= '7'; j++ {
				v = v*8 + int(s[j]-'0')
			}
			b.WriteRune(rune(v))
			i = j
		case c == '\n':
			i += 2
		case c == '\r':
			if i += 2; i < len(s) && s[i] == '\n' {
				i++
			}
		default:
			r, size := utf8.DecodeRuneInString(s[i+1:])
			if r != ' ' && r != ' ' { // a line continuation removes both
				b.WriteString(s[i+1 : i+1+size])
			}
			i += 1 + size
		}
	}
	return b.String()
}

// asciiDigits writes every Unicode decimal digit outside ASCII as its ASCII digit.
func asciiDigits(s string) string {
	if isASCII(s) || !strings.ContainsFunc(s, func(r rune) bool { return r >= utf8.RuneSelf && unicode.IsDigit(r) }) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r < utf8.RuneSelf || !unicode.IsDigit(r) {
			return r
		}
		// Decimal digits are encoded in runs of ten from zero (some runs adjoin): the value is
		// the distance from the start of the run, modulo ten.
		start := r
		for start > 0 && unicode.IsDigit(start-1) && r-start < 100 {
			start--
		}
		return '0' + (r-start)%10
	}, s)
}

// asciiDecoders decode one escape syntax in ASCII text, in decodeSteps order.
var asciiDecoders = [escKinds]func(string) (string, bool){
	func(s string) (string, bool) {
		lossy := false
		return jsonEscapeRun.ReplaceAllStringFunc(s, func(e string) string {
			var d string
			if json.Unmarshal([]byte(`"`+e+`"`), &d) != nil {
				lossy = true
				return e
			}
			// A \u escape the decoder could not decode alone (an unpaired surrogate) comes back as
			// U+FFFD; that is lossy whatever else the run holds, a written "\uFFFD" included
			// (Codex round 35).
			lossy = lossy || unpairedSurrogate(e)
			return d
		}), lossy
	},
	func(s string) (string, bool) {
		lossy := false
		return braceEscape.ReplaceAllStringFunc(s, func(e string) string {
			// Any number of leading zeros (Codex round 37: "\u{0000069}").
			digits := strings.TrimLeft(e[3:len(e)-1], "0")
			n, err := strconv.ParseUint("0"+digits, 16, 32)
			if err != nil || len(digits) > 6 || !utf8.ValidRune(rune(n)) {
				lossy = true
				return e
			}
			return string(rune(n))
		}), lossy
	},
	func(s string) (string, bool) { return escapedBytes(s, hexEscapeRun, 4) },
	func(s string) (string, bool) { return escapedBytes(s, percentRun, 3) },
	func(s string) (string, bool) { return html.UnescapeString(s), false },
}

// confusableKeepCase is r's UTS #39 prototype as the table gives it (the prototype keeps the
// case of the letter it looks like: Greek capital zeta is "Z"), or r itself.
func confusableKeepCase(r rune) string {
	if t, ok := confusables[r]; ok {
		return t
	}
	return string(r)
}

// perRune rewrites every non-ASCII character of s with read, ASCII kept as written.
func perRune(s string, read func(rune) string) string {
	i := strings.IndexFunc(s, func(r rune) bool { return r >= utf8.RuneSelf })
	if i < 0 {
		return s
	}
	memo := map[rune]string{}
	var b strings.Builder
	b.WriteString(s[:i])
	for _, r := range s[i:] {
		if r < utf8.RuneSelf {
			b.WriteRune(r)
			continue
		}
		v, ok := memo[r]
		if !ok {
			v = read(r)
			memo[r] = v
		}
		b.WriteString(v)
	}
	return b.String()
}

// coreSteps is the number of core decodeSteps: the core decoders, the core families, the
// JavaScript escapes and the check for escapes spelled outside ASCII. The other language families
// follow, up to escapeSteps; then the canonical steps (default-ignorables removed, ASCII digits).
var coreSteps = escKinds + len(familySteps(true)) + 2

// escapeSteps is the number of decodeSteps that are escape steps (core and families).
var escapeSteps = coreSteps + len(familySteps(false))

// nestedUnpairedSurrogate reports an unpaired surrogate in s as written or, when s is a JSON
// document, in any of its strings once that layer is unescaped and before it is read as JSON in
// turn, at every depth (Codex round 37: a doubled backslash hides it from the outer layer).
// Past the depth any reader goes, it reports true.
func nestedUnpairedSurrogate(s string, depth int) bool {
	if rawUnpairedSurrogate(s) {
		return true
	}
	if !isJSONDocument(s) {
		return false
	}
	if depth > maxJSONDepth+1 {
		return true
	}
	found := false
	_, err := walkJSON(s, func(v string) (string, error) {
		found = found || nestedUnpairedSurrogate(v, depth+1)
		return v, nil
	}, nil)
	return found || err != nil
}

// rawUnpairedSurrogate reports a run of \uXXXX escapes in s, as written, holding an unpaired
// surrogate.
func rawUnpairedSurrogate(s string) bool {
	for _, run := range jsonEscapeRun.FindAllString(s, -1) {
		if strings.HasPrefix(run, `\u`) && unpairedSurrogate(run) {
			return true
		}
	}
	return false
}

// unpairedSurrogate reports a run of \uXXXX escapes holding a surrogate that is not one half of a
// pair, which a JSON decoder replaces with U+FFFD.
func unpairedSurrogate(run string) bool {
	var units []uint16
	for i := 0; i+6 <= len(run); i += 6 {
		v, _ := strconv.ParseUint(run[i+2:i+6], 16, 16)
		units = append(units, uint16(v))
	}
	for i := 0; i < len(units); i++ {
		switch u := units[i]; {
		case u >= 0xD800 && u < 0xDC00:
			if i+1 == len(units) || units[i+1] < 0xDC00 || units[i+1] >= 0xE000 {
				return true
			}
			i++
		case u >= 0xDC00 && u < 0xE000:
			return true
		}
	}
	return false
}

// escapedBytes decodes each run of byte escapes (step bytes per escape, the hex digits last) as UTF-8.
func escapedBytes(s string, run *regexp.Regexp, step int) (string, bool) {
	lossy := false
	return run.ReplaceAllStringFunc(s, func(e string) string {
		b := make([]byte, 0, len(e)/step)
		for i := 0; i+step <= len(e); i += step {
			v, _ := strconv.ParseUint(e[i+step-2:i+step], 16, 8)
			b = append(b, byte(v))
		}
		var out strings.Builder
		for len(b) > 0 {
			r, size := utf8.DecodeRune(b)
			if r == utf8.RuneError && size <= 1 {
				lossy = true // not UTF-8: there is no reading we can audit with certainty
			}
			out.WriteRune(r)
			b = b[size:]
		}
		text := out.String()
		if strings.ContainsFunc(text, func(r rune) bool { return !unicode.IsPrint(r) && !unicode.IsSpace(r) }) {
			lossy = true
		}
		return text
	}), lossy
}

// A stage is one decoding of a text, with the restored spans in it (roundTrips only): pos[i] is
// span i in x, and esc[i] reports that an escape step (not a canonicalisation) changed its text
// on the way. parent and via record a stage made by a canonicalisation (see walkStages).
type stage struct {
	x      string
	pos    [][2]int
	esc    []bool
	parent *stage
	via    int
	depth  int // single decodings from the text as written
}

// maxDecodeDepth bounds the single decodings on any path from the text as written: text that
// has not settled after it is refused, as the audit always refused text that did not settle. It
// is not a bound on the stages, which only the budget bounds (Codex round 28): realistic outputs
// settle within 4, the densest mixed escaping measured within 8, and "&amp;amp;..." nested a
// thousand times is refused after 16 instead of after spending the budget.
const maxDecodeDepth = 16

// stepStage applies one step to st, carrying the span positions through it: the pieces between
// span edges are decoded one by one and must join to exactly the decoding of the whole text, so
// an edge inside an escape the decoder reads as one unit ("Alic&#101" then ";") fails (Codex
// round 26). rewrite, when set, runs on the result (a carried message's tokens taken out). ok
// false means refuse.
func stepStage(st *stage, step func(string) (string, bool), escape bool, rewrite func(string) (string, bool)) (next stage, lossy, ok bool) {
	n, lossy := step(st.x)
	next.x = n
	if len(st.pos) > 0 && n != st.x {
		next.pos, next.esc = make([][2]int, len(st.pos)), slices.Clone(st.esc)
		var j strings.Builder
		last := 0
		piece := func(end int) string {
			d, _ := step(st.x[last:end])
			j.WriteString(d)
			last = end
			return d
		}
		for i, im := range st.pos {
			piece(im[0])
			start := j.Len()
			if d := piece(im[1]); escape && d != st.x[im[0]:im[1]] {
				next.esc[i] = true
			}
			next.pos[i] = [2]int{start, j.Len()}
		}
		piece(len(st.x))
		if j.String() != n {
			return next, lossy, false
		}
	} else {
		next.pos, next.esc = st.pos, st.esc
	}
	if rewrite != nil {
		if next.x, ok = rewrite(next.x); !ok {
			return next, lossy, false
		}
	}
	return next, lossy, true
}

func stageKey(st *stage) string { return fmt.Sprint(st.x, st.pos, st.esc) }

// walkStages visits first and every distinct stage reachable from it by steps, in any order,
// breadth first. steps[canon:] are canonicalisations. A stage c(x) made by one of them is visited
// but not decoded further when every step s commutes with c at x (s(c(x)) is c(s(x))): each stage
// below it is then a canonicalisation of a stage the walk reaches anyway (by induction on the
// path), so it would only repeat them; one that does not commute (a view that turns "з" into "3"
// inside "%6з") is decoded like any other stage. It reports false, refuse, when visit does, when
// a step is lossy, when positions do not survive a step, when a path is longer than
// maxDecodeDepth, or when the audit budget is spent. A lossy step always refuses, for every
// caller: an escape the readings do not agree on is left as written, so a caller that went on
// would read past it (security review of eb57aa1b). Every caller is a security check: the request
// audit, the number-name and JSON-syntax reads, the leftover-token check and the round-trip check.
func (p *Pseudonyms) walkStages(first stage, steps []func(string) (string, bool), canon int, rewrite func(string) (string, bool), visit func(*stage) bool) bool {
	if rewrite != nil {
		var ok bool
		if first.x, ok = rewrite(first.x); !ok {
			return false
		}
	}
	// Each step costs costStep per byte here; a step that reads over reading sets or normalises
	// charges its own work on top (lookalikeEscape). Positions double the decoding.
	full := canon == escapeSteps && len(steps) > escapeSteps // the families are steps here
	cost := func(st *stage) bool {
		applied := len(steps)
		if full && st.depth > 0 {
			applied -= escapeSteps - coreSteps // the other families read the first stage only
		}
		n := costStage + costStep*applied*len(st.x)
		if len(st.pos) > 0 {
			n *= 2
		}
		return p.charge(n)
	}
	// prunable reports that every step commutes with the canonicalisation that made st.
	prunable := func(st *stage) bool {
		if st.parent == nil || !cost(st) {
			return false
		}
		c := steps[st.via]
		for j, s := range steps {
			a, lossy, okA := stepStage(st, s, j < canon, rewrite)
			y, _, okY := stepStage(st.parent, s, j < canon, rewrite)
			if !okA || !okY || lossy {
				return false
			}
			b, _, okB := stepStage(&y, c, false, rewrite)
			if !okB || stageKey(&a) != stageKey(&b) {
				return false
			}
		}
		return true
	}
	seen := map[string]bool{stageKey(&first): true}
	for queue := []*stage{&first}; len(queue) > 0; queue = queue[1:] {
		st := queue[0]
		if !visit(st) || !cost(st) {
			return false
		}
		if prunable(st) {
			continue
		}
		for i, step := range steps {
			if full && i >= coreSteps && i < escapeSteps {
				// A language family that is not core reads the text as written, as its language's
				// parser does (the first stage); its output is visited, and so is that output after
				// one pass of every core decoder in turn ("\\5C u0069" is "\\u0069" to CSS, "i" to
				// JSON). Neither is queued, so no family reads another's output. (Reading these
				// families on every stage multiplied the work past the budget on ordinary code;
				// the families the audit read before are core and chain everywhere, security
				// review of 3f0de2dd.)
				if st.depth > 0 {
					continue
				}
				fam, lossy, ok := stepStage(st, step, true, rewrite)
				if !ok || lossy {
					return false
				}
				if fam.x == st.x {
					continue
				}
				// Charged for itself and the one core pass over it, not for every step.
				once := func(x string) bool { return p.charge(costStage + costStep*coreSteps*len(x)) }
				if !visit(&fam) || !once(fam.x) {
					return false
				}
				cur := fam
				for j := 0; j < coreSteps; j++ {
					c, lossy, ok := stepStage(&cur, steps[j], true, rewrite)
					if !ok || lossy {
						return false
					}
					cur = c
				}
				if cur.x != fam.x && (!visit(&cur) || !once(cur.x)) {
					return false
				}
				continue
			}
			next, lossy, ok := stepStage(st, step, i < canon, rewrite)
			if !ok || lossy {
				return false
			}
			if next.x == st.x {
				continue
			}
			if key := stageKey(&next); !seen[key] {
				if next.depth = st.depth + 1; next.depth > maxDecodeDepth {
					return false
				}
				seen[key] = true
				if i >= canon {
					next.parent, next.via = st, i
				}
				queue = append(queue, &next)
			}
		}
	}
	return true
}

// stages calls visit on every stage a reader could reach from s: every distinct text reachable
// by decodeSteps, in any order (Codex rounds 27 and 28). The walk is not repeated over the whole
// text for each stage (Codex round 29: one escaped snippet in 6 KB of prose spent the budget).
// The whole text is walked with the canonicalisations only; each escape-bearing region is walked
// with every step, inside a window of the text around it (see windows). rewrite, when set, runs
// on every stage before it is visited or decoded further. It reports false, refuse, as
// walkStages does.
func (p *Pseudonyms) stages(s string, rewrite func(string) (string, bool), visit func(string) bool) bool {
	wins, ok := p.windows(s, nil)
	if !ok {
		return false
	}
	v := func(st *stage) bool { return visit(st.x) }
	steps := p.decodeSteps()
	if !p.walkStages(stage{x: s}, steps[escapeSteps:], 0, rewrite, v) {
		return false
	}
	for _, w := range wins {
		if !p.walkStages(stage{x: s[w.start:w.end]}, steps, escapeSteps, rewrite, v) {
			return false
		}
	}
	return true
}

// A window is a region of s the full stage walk reads (see windows): spans[span[0]:span[1]] lie
// in it, and active reports an escape-bearing piece in it.
type window struct {
	start, end int
	span       [2]int
	active     bool
}

// windows returns the regions of s, in order and disjoint, that the full stage walk reads: one
// around each escape-bearing piece and, for roundTrips, around each restored span (spans in text
// order, disjoint).
//
// The guarantee. s is cut into pieces at runs of ASCII whitespace followed by an ASCII character
// and not following a backslash (a line continuation). No step changes such a run or reads across it (no escape holds a raw space, NFKC never composes
// onto an ASCII character, and the canonical views keep ASCII whitespace), so every stage of s is
// the stages of its pieces joined by the same runs, and a window's stages are exactly the
// corresponding parts of the stages of s. A piece is plain when no escape step changes it, or
// reports lossy, in any of its canonical forms: in every stage of s it is one of those forms,
// which the whole-text canonical walk already reads. Every other piece is escape-bearing. A name,
// e-mail, phone or number the detectors could find in a stage of s either lies wholly in plain
// pieces and runs, so it is in a canonical stage of s, or touches an escape-bearing piece. A match
// holds at most reach characters of a canonical view (reach: the most characters of any stored
// name in its canonical views, its own whitespace included; an e-mail or a number holds no
// whitespace, a phone at most 21 characters, so at most 11 pieces), and every piece it holds whole gives it at least one
// unless one of the piece's forms holds no base character (such pieces are not counted). So the
// pieces past reach+1 counted pieces on each side, two more for the characters the boundary rules
// read, and the whitespace at the ends of s when the window reaches them (a name may begin or end
// with whitespace: Codex round 30), hold every match that touches the piece, with its context. An
// inner window edge is a piece edge, beside whitespace in s, which the detectors read as they read
// the end of the text, so a window finds nothing there that s does not (Codex round 31). Overlapping windows are merged;
// a restored span's pieces are all seeds, so no window edge cuts a span.
//
// It is one left-to-right pass over the pieces and the spans (Codex round 30), charged to the
// audit budget.
func (p *Pseudonyms) windows(s string, spans [][2]int) ([]window, bool) {
	if !p.charge(costStep * (len(s) + len(spans))) {
		return nil, false
	}
	type piece struct {
		start, end      int
		active, counted bool
	}
	space := func(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '\v' }
	var ps []piece
	start := 0
	for i := 0; i < len(s); {
		if !space(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && space(s[j]) {
			j++
		}
		// A run right after a backslash is no cut: a JavaScript line continuation reads across
		// it ("Al\" + newline + "ice" is "Alice", security review of f7ba2518).
		// Nor after a backslash and hex digits: a CSS escape takes one whitespace character
		// after them ("\\41 b" is "Ab").
		h := i
		for h > 0 && i-h < 6 && strings.IndexByte(hexChars, s[h-1]) >= 0 {
			h--
		}
		// Nor after a lead that takes a raw whitespace character next: TeX "^^" ("^^" TAB is
		// "I"), PowerShell's backtick (a line continuation), Lua "\z" (security review of
		// 3f0de2dd); nor after a character that reads as a backslash (a look-alike line
		// continuation, Codex round 38).
		if i > 0 && (s[i-1] == '^' || s[i-1] == '`' || i > 1 && s[i-1] == 'z' && s[i-2] == '\\') {
			i = j
			continue
		}
		lookBackslash := false
		if i > 0 && s[i-1] >= utf8.RuneSelf {
			r, _ := utf8.DecodeLastRuneInString(s[:i])
			rd, _ := readingsOf(string(r), cases.Fold())
			lookBackslash = rd.set.has('\\')
		}
		if (j == len(s) || s[j] < utf8.RuneSelf) && (i == 0 || s[i-1] != '\\') && (h == i || h == 0 || s[h-1] != '\\') && !lookBackslash {
			if i > start {
				ps = append(ps, piece{start: start, end: i})
			}
			start = j
		}
		i = j
	}
	if start < len(s) {
		ps = append(ps, piece{start: start, end: len(s)})
	}
	base := func(r rune) bool { return !ignorable(r) && !unicode.Is(unicode.M, r) && !unicode.IsSpace(r) }
	steps := p.decodeSteps()
	cnt := make([]int, len(ps)+1) // cnt[i]: counted pieces in ps[:i]
	for k := range ps {
		t := s[ps[k].start:ps[k].end]
		ps[k].counted = true
		// A piece of ASCII without a character any catalogue syntax begins with is plain.
		if strings.ContainsFunc(t, func(r rune) bool { return r >= utf8.RuneSelf || strings.ContainsRune(`%&\+'"`+"`#^", r) }) {
			ok := p.walkStages(stage{x: t}, steps[escapeSteps:], 0, nil, func(st *stage) bool {
				if !strings.ContainsFunc(st.x, base) {
					ps[k].counted = false
				}
				for _, step := range steps[:escapeSteps] {
					if n, lossy := step(st.x); n != st.x || lossy {
						ps[k].active = true
					}
				}
				return p.charge(costStep * escapeSteps * len(st.x))
			})
			if !ok {
				return nil, false
			}
			if ps[k].active {
				ps[k].counted = false // its forms are not walked here: counted as holding nothing
			}
		} // else ASCII without an escape introducer: every form is itself
		cnt[k+1] = cnt[k]
		if ps[k].counted {
			cnt[k+1]++
		}
	}
	// Seeds: escape-bearing pieces, and every piece a restored span touches (one sweep).
	seed := make([]bool, len(ps))
	for k := range ps {
		seed[k] = ps[k].active
	}
	for k, i := 0, 0; i < len(spans); i++ {
		for k < len(ps) && ps[k].end <= spans[i][0] {
			k++
		}
		for m := k; m < len(ps) && ps[m].start < spans[i][1]; m++ {
			seed[m] = true
		}
	}
	// A phone match holds at most 21 characters, its pieces separated by whitespace: at most 11
	// pieces. A name holds at most p.reach characters, so at most that many pieces.
	reach := max(p.reach, 11) + 1
	var out []window
	lo, hi := 0, 0
	for k := range ps {
		if !seed[k] {
			continue
		}
		for lo+1 <= k && cnt[k]-cnt[lo+1] >= reach+1 {
			lo++
		}
		hi = max(hi, k)
		for hi < len(ps)-1 && cnt[hi+1]-cnt[k+1] < reach+1 {
			hi++
		}
		a, b := max(lo-2, 0), min(hi+2, len(ps)-1)
		// A window starts and ends at piece edges. Inside s such an edge has whitespace beside
		// it, which every detector reads as it reads the start or end of the text; at the ends of
		// s the whitespace there is the real context and is kept (a name may begin or end with
		// whitespace: Codex round 30). A whitespace run kept at an inner edge would end the window
		// on a boundary that s does not have ("Alice " before "X": Codex round 31).
		w := window{start: ps[a].start, end: ps[b].end, active: ps[k].active}
		if a == 0 {
			w.start = 0
		}
		if b == len(ps)-1 {
			w.end = len(s)
		}
		if n := len(out); n > 0 && w.start <= out[n-1].end {
			out[n-1].end = max(out[n-1].end, w.end)
			out[n-1].active = out[n-1].active || w.active
			continue
		}
		out = append(out, w)
	}
	// Each span lies whole in one window (one sweep); anything else refuses.
	i := 0
	for k := range out {
		out[k].span[0] = i
		for i < len(spans) && spans[i][1] <= out[k].end {
			if spans[i][0] < out[k].start {
				return nil, false
			}
			i++
		}
		out[k].span[1] = i
	}
	return out, i == len(spans)
}

// RestoreResponse puts the terms back into the model's content and tool-call arguments from this
// request's own map. JSON (the runtime's contracts) is restored in its decoded strings, keys
// included, so a token the model wrote JSON-escaped ("\u27e6P1\u27e7") is restored or refused
// exactly like a literal one; other text is restored by literal substitution. Any
// token-shaped text this request did not issue refuses the whole response.
func (p *Pseudonyms) RestoreResponse(r ModelResponse) (ModelResponse, error) {
	in := len(r.Content)
	for _, c := range r.ToolCalls {
		in += len(c.Arguments)
	}
	// The restored output, every layer of it while it is written and all of it together, may take
	// at most max(4 x input, input + 1 MiB) bytes: tokens repeated by the model, and re-escaping
	// at each JSON layer, cannot expand into gigabytes (WIS-623 Codex rounds 9 and 10). Past that
	// the call fails (PROVIDER_RESPONSE_TOO_LARGE).
	p.restoreLimit = max(4*in, in+1<<20)
	// The round-trip and leftover checks walk decoding stages under the same per-byte audit
	// budget as a request; past it the response is refused.
	p.auditBudget = auditWorkBase + auditWorkPerByte*in
	defer func() { p.restoreLimit, p.auditBudget = 0, 0 }()
	// An unpaired surrogate is replaced by U+FFFD when JSON is decoded: refused as written (Codex
	// round 36).
	if nestedUnpairedSurrogate(r.Content, 0) || slices.ContainsFunc(r.ToolCalls, func(c ToolCall) bool { return nestedUnpairedSurrogate(string(c.Arguments), 0) }) {
		return ModelResponse{}, &ProviderError{Code: CodeSchemaInvalid}
	}
	total := 0
	add := func(n int) error {
		if total += n; total > p.restoreLimit {
			return errRestoreLimit
		}
		return nil
	}
	content, err := p.restore(r.Content)
	if err == nil {
		err = add(len(content))
	}
	if err != nil {
		return ModelResponse{}, err
	}
	r.Content = content
	if len(r.ToolCalls) > 0 {
		calls := make([]ToolCall, len(r.ToolCalls))
		for i, c := range r.ToolCalls {
			args, err := p.restore(string(c.Arguments))
			if err == nil {
				err = add(len(args))
			}
			if err != nil {
				return ModelResponse{}, err
			}
			c.Arguments = json.RawMessage(args)
			calls[i] = c
		}
		r.ToolCalls = calls
	}
	return r, nil
}

func (p *Pseudonyms) restore(s string) (string, error) { return p.restoreAt(s, 1) }

// restoreAt restores s at JSON layer depth, mirroring hideAt: a JSON document is walked and its
// strings re-encoded layer by layer (WIS-623 Codex round 8). Every layer's output is written
// against p.restoreLimit, counted as it is written (Codex round 10), and nesting deeper than
// maxJSONDepth is refused.
func (p *Pseudonyms) restoreAt(s string, depth int) (string, error) {
	if isJSONDocument(s) {
		if depth > maxJSONDepth {
			return "", &ProviderError{Code: CodePseudonymsIncomplete}
		}
		return walkJSONLimit(s, func(v string) (string, error) {
			if isJSONDocument(v) {
				return p.restoreAt(v, depth+1)
			}
			return p.restoreText(v, true)
		}, nil, p.restoreLimit)
	}
	// Text that is not a JSON document is restored by literal substitution: JSON escaping is
	// applied only when the text is an actual JSON value, by walkJSON re-encoding the decoded
	// strings (WIS-623 Codex round 14: a prose draft "⟦P1⟧" for "A&B Motors" must not come back
	// as "A\u0026B Motors").
	return p.restoreText(s, false)
}

// restoreText replaces every issued token in s with its term, literally.
// restoreText restores the tokens of s. decoded is set for a decoded JSON string value, which
// walkJSON re-encodes: there terms are written as they are; in other text a token inside a
// double-quoted region is JSON-escaped.
func (p *Pseudonyms) restoreText(s string, decoded bool) (string, error) {
	locs := anyToken.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s, p.leftoverToken(s)
	}
	// One streaming builder: the size is checked before every append (WIS-623 Codex round 10).
	// check is the same text with every term written as it is, for the round-trip check.
	var b, check strings.Builder
	var spans [][2]int
	last := 0
	write := func(x, raw string) error {
		if p.restoreLimit > 0 && (b.Len()+len(x) > p.restoreLimit || check.Len()+len(raw) > p.restoreLimit) {
			return errRestoreLimit
		}
		b.WriteString(x)
		check.WriteString(raw)
		return nil
	}
	quoted := quotedRegions(s)
	for _, loc := range locs {
		raw := s[loc[0]:loc[1]]
		key := tokenKey(raw)
		term, known := p.term[key]
		if !known {
			return "", &ProviderError{Code: CodePseudonymUnknown} // in every written form
		}
		// In a double-quoted region (embedded JSON, also inside a decoded JSON string: Codex
		// rounds 16 and 17), and for a token written encoded in text that is not a decoded JSON
		// value, a term holding a quote, backslash or control character is JSON-escaped so it
		// cannot break that string; any other term is written as it is (Codex round 18). In a
		// decoded JSON value walkJSON escapes it once.
		form := term
		if (quoted[loc[0]] || !decoded && raw != key) && strings.ContainsFunc(term, func(r rune) bool { return r == '"' || r == '\\' || r < 0x20 }) {
			q := jsonString(term)
			form = q[1 : len(q)-1]
		}
		if err := write(s[last:loc[0]], s[last:loc[0]]); err != nil {
			return "", err
		}
		start := check.Len()
		if err := write(form, term); err != nil {
			return "", err
		}
		spans = append(spans, [2]int{start, check.Len()})
		last = loc[1]
	}
	if err := write(s[last:], s[last:]); err != nil {
		return "", err
	}
	if !p.roundTrips(check.String(), spans) {
		return "", &ProviderError{Code: CodeSchemaInvalid}
	}
	return b.String(), p.leftoverToken(s)
}

// safeEdge reports that the character at an edge of a decoded restored name is an explicit
// separator: the start or end of the text (r < 0), ASCII whitespace, the ASCII punctuation
// . , ; : ! ? ( ) [ ] { } " ' / - (WIS-623 Codex round 25), or Unicode whitespace or punctuation
// whose NFKC form is still whitespace or punctuation and holds no escape introducer % & \ # ;
// (curly quotes, the em dash, CJK punctuation: Codex round 29).
func safeEdge(r rune) bool {
	if r < 0 || r < utf8.RuneSelf && strings.ContainsRune(" \t\n\r\f\v.,;:!?()[]{}\"'/-", r) {
		return true
	}
	if r < utf8.RuneSelf {
		return false
	}
	k := norm.NFKC.String(string(r))
	return !strings.ContainsAny(k, `%&\#;`) && !strings.ContainsFunc(k, func(c rune) bool { return !unicode.IsSpace(c) && !unicode.In(c, unicode.Z, unicode.P) })
}

// roundTrips reports that hiding the restored text c again finds each restored name where it
// was restored: every restored span lies wholly inside a span the detectors find in context, on
// the text as written and on each stage a reader could reach (see stages). A name glued to a
// neighbour ("⟦P1⟧⟦P2⟧" is "AliceBob", "X" U+200B "⟦P1⟧" reads "XAlice", a "%58" that decodes to
// "X") would be left where no later hiding finds it (persisted claims, later hosted runs): such
// output is invalid (WIS-623 Codex rounds 17 to 19). A longer stored name around it is fine
// ("Acme Ltd" inside "Acme Ltd."). Each span is checked in the window around it, with every
// step, positions carried by the decoder (Codex round 26).
func (p *Pseudonyms) roundTrips(c string, spans [][2]int) bool {
	wins, ok := p.windows(c, spans)
	if !ok {
		return false
	}
	for _, w := range wins {
		in := spans[w.span[0]:w.span[1]]
		if len(in) == 0 {
			continue
		}
		first := stage{x: c[w.start:w.end], pos: make([][2]int, len(in))}
		names := make([]string, len(in))
		for i, sp := range in {
			first.pos[i] = [2]int{sp[0] - w.start, sp[1] - w.start}
			names[i] = c[sp[0]:sp[1]]
		}
		if !w.active {
			// No escape in reach: the text as written is what every reader reads, and the
			// matcher reads its canonical views itself.
			if !p.stageRoundTrips(names, &first) {
				return false
			}
			continue
		}
		first.esc = make([]bool, len(names))
		// A decoding that is not exact, or readings that disagree, leave no restored text that
		// later hiding reads one way: invalid (Codex round 33: "X\u{202Б}⟦P1⟧").
		if !p.walkStages(first, p.decodeSteps(), escapeSteps, nil, func(st *stage) bool { return p.stageRoundTrips(names, st) }) {
			return false
		}
	}
	return true
}

// stageRoundTrips checks one stage: each restored span must be found again in context. A span no
// escape step changed is read as the name it is, between its context as this stage reads it: a
// canonical reading of the name itself tells nothing about the name (Greek capital iota's UTS #39
// prototype is "l": "ΝΙΝΑ" reads "NlNA", which no view of the name is: Codex rounds 30 and 31),
// while a reading of its context can glue to it ("%5з" before it is "S"). A span an escape step
// changed must have separators beside it (round 25).
func (p *Pseudonyms) stageRoundTrips(names []string, st *stage) bool {
	if p.ctx != nil && p.ctx.Err() != nil || !p.charge(matchCost(st.x)) {
		return false
	}
	// x is the stage with every span no escape step changed put back as its name.
	var b strings.Builder
	pos := make([][2]int, len(st.pos))
	last := 0
	for i, im := range st.pos {
		b.WriteString(st.x[last:im[0]])
		start := b.Len()
		if len(st.esc) > i && st.esc[i] {
			b.WriteString(st.x[im[0]:im[1]])
		} else {
			b.WriteString(names[i])
		}
		pos[i] = [2]int{start, b.Len()}
		last = im[1]
	}
	b.WriteString(st.x[last:])
	x := b.String()
	q := p.fresh()
	found := q.textSpans(x)
	if q.overBudget {
		return false
	}
	// Coverage is joined as hiding joins it (overlapping or touching places become one
	// token: "Alice Smith@example.com"), then one ordered sweep checks containment.
	sort.Slice(found, func(i, j int) bool { return found[i][0] < found[j][0] })
	var joined [][2]int
	for _, f := range found {
		if n := len(joined); n > 0 && f[0] <= joined[n-1][1] {
			joined[n-1][1] = max(joined[n-1][1], f[1])
			continue
		}
		joined = append(joined, f)
	}
	k := 0
	for i, im := range pos { // spans are in text order
		if len(st.esc) > i && st.esc[i] {
			// No matcher is imitated for a name an escape changed (a backslash, "%" or "&"
			// inside it): the characters right beside it must be separators.
			before, after := rune(-1), rune(-1)
			if im[0] > 0 {
				before, _ = utf8.DecodeLastRuneInString(x[:im[0]])
			}
			if im[1] < len(x) {
				after, _ = utf8.DecodeRuneInString(x[im[1]:])
			}
			if !safeEdge(before) || !safeEdge(after) {
				return false
			}
			continue
		}
		for k < len(joined) && joined[k][1] < im[1] {
			k++
		}
		if k == len(joined) || joined[k][0] > im[0] {
			return false
		}
	}
	return true
}

// leftoverToken refuses text that still holds a token once its other encodings are decoded
// (percent-encoding, HTML entities and the like): an unknown or oddly encoded token is never
// passed through (Codex round 16).
func (p *Pseudonyms) leftoverToken(s string) error {
	v := anyToken.ReplaceAllString(s, " ") // the tokens restoreText handled
	if !p.stages(v, nil, func(v string) bool { return !strings.ContainsAny(v, "⟦⟧") }) {
		return &ProviderError{Code: CodePseudonymUnknown}
	}
	return nil
}

// quotedRegions marks, per byte of s, whether it lies inside a double-quoted string, scanning
// each line and honouring backslash escapes.
func quotedRegions(s string) []bool {
	in := make([]bool, len(s))
	inside := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\n':
			inside = false
		case inside && c == '\\':
			in[i] = true
			if i+1 < len(s) {
				i++
				in[i] = true
			}
			continue
		case c == '"':
			inside = !inside
			in[i] = true
			continue
		}
		in[i] = inside
	}
	return in
}

// anyToken is a token in any written form: every character of it (the brackets, "P" and each
// digit) literal or JSON-escaped (\u27e6, \u0050, \u0031, either case), so an encoded token is
// restored, or refused when unissued, like a literal one (WIS-623 Codex rounds 15 and 16).
var anyToken = regexp.MustCompile(`(?:⟦|\\u27[eE]6)(?:P|\\u0050)((?:[0-9]|\\u003[0-9])+)(?:⟧|\\u27[eE]7)`)

// tokenKey is the issued form of a token written in any form anyToken accepts.
func tokenKey(raw string) string {
	digits := anyToken.FindStringSubmatch(raw)[1]
	return "⟦P" + strings.ReplaceAll(digits, `\u003`, "") + "⟧"
}

// withoutIssuedTokens replaces every token of this map in s, in any written form, with a space;
// unknown reports a token-shaped text in any form that this map did not issue.
func (p *Pseudonyms) withoutIssuedTokens(s string) (out string, unknown bool) {
	out = anyToken.ReplaceAllStringFunc(s, func(raw string) string {
		if _, ok := p.term[tokenKey(raw)]; !ok {
			unknown = true
			return raw
		}
		return " "
	})
	return out, unknown
}

type pseudonymsKey struct{}

// WithPseudonyms puts the request's map on the context for the hosted adapter.
func WithPseudonyms(ctx context.Context, p *Pseudonyms) context.Context {
	return context.WithValue(ctx, pseudonymsKey{}, p)
}

func pseudonymsFrom(ctx context.Context) *Pseudonyms {
	p, _ := ctx.Value(pseudonymsKey{}).(*Pseudonyms)
	return p
}

// PseudonymTermsTx reads, inside the run's tenant transaction, the names this site's requests are
// pseudonymised with: the tenant and site names, the display names of the site's members, and
// every display name and alias any revision of any customer entity of the tenant has carried (a
// renamed customer's earlier name is still in chat and retained evidence).
//
// Customers of every site of the tenant are treated exactly like this site's own names: hidden,
// audited (an encoded form refuses the call) and restored to the exact text sent. Sites belong to
// one tenant (one company) and rule 03 isolates tenants, so that a same-tenant site's behaviour
// depends on the company's customers elsewhere is accepted; protecting known names (rule 08) wins
// (WIS-623 security review, lead decision).
//
// The terms are distinct stored spellings, at most maxPseudonymTerms+1 rows, sorted (rule 04).
// They keep their casing: case is folded only by canonicalView, which needs it (a Greek capital
// "ΝΙΝΑ" reads as "nina", but lower-cased first, "νινα", it would read as "viva"; WIS-623 Codex
// round 3). A term longer than MaxTermRunes comes back as one leading NULL row. Either bound
// exceeded is ErrPseudonymTerms, and the hosted call is refused.
func PseudonymTermsTx(ctx context.Context, tx *sql.Tx, sc RunScope) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `WITH t AS (SELECT DISTINCT btrim(n) AS n FROM (
 SELECT name FROM supervisor.tenants WHERE id=$1
 UNION ALL SELECT name FROM supervisor.sites WHERE tenant_id=$1 AND id=$2
 UNION ALL SELECT p.display_name FROM supervisor.member_profiles p JOIN supervisor.memberships m ON (m.tenant_id,m.issuer,m.subject)=(p.tenant_id,p.issuer,p.subject)
  WHERE m.tenant_id=$1 AND m.site_id=$2
 UNION ALL SELECT n FROM supervisor.knowledge_entity_revisions r
  CROSS JOIN LATERAL (SELECT r.payload->>'display_name' UNION ALL SELECT jsonb_array_elements_text(CASE WHEN jsonb_typeof(r.payload->'aliases')='array' THEN r.payload->'aliases' ELSE '[]' END)) a(n)
  WHERE r.tenant_id=$1 AND r.entity_type='customer') a(n) WHERE n IS NOT NULL)
SELECT n FROM (SELECT n FROM t WHERE char_length(n)<=$3 UNION ALL SELECT NULL WHERE EXISTS(SELECT 1 FROM t WHERE char_length(n)>$3)) b
 ORDER BY n COLLATE "C" NULLS FIRST LIMIT $4`, sc.TenantID, sc.SiteID, MaxTermRunes, maxPseudonymTerms+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var terms []string
	for rows.Next() {
		var t sql.NullString
		if err = rows.Scan(&t); err != nil {
			return nil, err
		}
		if !t.Valid {
			return nil, ErrPseudonymTerms
		}
		terms = append(terms, t.String)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(terms) > maxPseudonymTerms {
		return nil, ErrPseudonymTerms
	}
	sort.Strings(terms)
	return terms, nil
}
