package agent

import (
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// The language families of the escape catalogue (WIS-623 Codex round 38; the table on
// decodeSteps lists them all). Each family is one literal decoder over ASCII escapes: a regular
// expression, and a decoder of each match from its groups. Each is a leaf stage of the walk: it
// reads every stage the core decoders reach, and what it decodes is read again by the core
// decoders, but not by another family (one text is never read as two languages at once; that
// keeps backslash-heavy text from multiplying the stages). The families marked core (Latin-1
// \xHH, \UXXXXXXXX, \N{}, CSS, %uXXXX) chain with the core decoders in any order instead. Each family also names the grammar
// discovery reads it by (lookalikeEscape), so the family spelled with characters outside ASCII
// is refused, not decoded.
type family struct {
	// core: the family chains with the core decoders in any order (the families the audit read
	// before round 38); the others read the text as written (see walkStages).
	core bool
	name string
	re   *regexp.Regexp
	// pre prepares the text the way the language's input stage does (CSS newlines), or is nil.
	pre func(string) string
	// dec decodes one match (g[0]) from its groups; lossy for a value that is no character.
	dec func(g []string) (string, bool)
	// disc is the grammar, over ASCII, that discovery reads the family by; "" when the family
	// adds no grammar of its own (it decodes no letter, or another grammar covers it).
	disc string
}

// decode applies the family to s.
func (f *family) decode(s string) (string, bool) {
	if f.pre != nil {
		s = f.pre(s)
	}
	idx := f.re.FindAllStringSubmatchIndex(s, -1)
	if len(idx) == 0 {
		return s, false
	}
	var b strings.Builder
	last, lossy := 0, false
	for _, m := range idx {
		g := make([]string, len(m)/2)
		for i := range g {
			if m[2*i] >= 0 {
				g[i] = s[m[2*i]:m[2*i+1]]
			}
		}
		v, l := f.dec(g)
		b.WriteString(s[last:m[0]])
		b.WriteString(v)
		last, lossy = m[1], lossy || l
	}
	b.WriteString(s[last:])
	return b.String(), lossy
}

// codePoint is the character of the hexadecimal (base 16) or other-base value v; lossy, and v
// itself, when it is no character.
func codePoint(v string, base int, orig string) (string, bool) {
	v = strings.ReplaceAll(strings.TrimSpace(v), "_", "")
	n, err := strconv.ParseUint("0"+strings.TrimLeft(v, "0"), base, 32)
	if err != nil || len(strings.TrimLeft(v, "0")) > 8 || !utf8.ValidRune(rune(n)) {
		return orig, true
	}
	return string(rune(n)), false
}

// bytesUTF8 reads byte values as UTF-8; lossy when one is past 255 or they are not UTF-8.
func bytesUTF8(vals []uint64, orig string) (string, bool) {
	b := make([]byte, 0, len(vals))
	for _, v := range vals {
		if v > 255 {
			return orig, true
		}
		b = append(b, byte(v))
	}
	if !utf8.Valid(b) {
		return orig, true
	}
	return string(b), false
}

var families = sync.OnceValue(func() []*family {
	octalRun := regexp.MustCompile(`\\([0-7]{1,3})`)
	decRun := regexp.MustCompile(`\\([0-9]{1,3})`)
	runBytes := func(re *regexp.Regexp, base int) func(g []string) (string, bool) {
		return func(g []string) (string, bool) {
			var vals []uint64
			for _, m := range re.FindAllStringSubmatch(g[0], -1) {
				v, _ := strconv.ParseUint(m[1], base, 16)
				vals = append(vals, v)
			}
			return bytesUTF8(vals, g[0])
		}
	}
	hex := func(g []string) (string, bool) { return codePoint(g[1], 16, g[0]) }
	ctrl := func(c byte) string { return string(rune(c & 0x1f)) }
	return []*family{
		{core: true, name: "Latin-1 \\xHH (JavaScript, Python str)", re: regexp.MustCompile(`\\x([0-9a-fA-F]{2})`),
			dec: func(g []string) (string, bool) { v, _ := strconv.ParseUint(g[1], 16, 8); return string(rune(v)), false }},
		{name: "C/C++ variable-length \\x", re: regexp.MustCompile(`\\x([0-9a-fA-F]+)`), dec: hex, disc: `\\x[0-9a-fA-F]{1,8}`},
		{name: "C# \\x (1-4 digits)", re: regexp.MustCompile(`\\x([0-9a-fA-F]{1,4})`), dec: hex},
		{core: true, name: "Python/C# \\UXXXXXXXX", re: regexp.MustCompile(`\\U([0-9a-fA-F]{8})`), dec: hex},
		{name: "Bash $'' \\u (1-4 digits)", re: regexp.MustCompile(`\\u([0-9a-fA-F]{1,4})`), dec: hex, disc: `\\u[0-9a-fA-F]{1,4}`},
		{name: "Bash $'' \\U (1-8 digits)", re: regexp.MustCompile(`\\U([0-9a-fA-F]{1,8})`), dec: hex, disc: `\\U[0-9a-fA-F]{1,8}`},
		{name: "Java \\uuuuXXXX", re: regexp.MustCompile(`\\u+([0-9a-fA-F]{4})`),
			dec: func(g []string) (string, bool) {
				if v, _ := strconv.ParseUint(g[1], 16, 16); v >= 0xD800 && v < 0xE000 {
					return g[0], true // a surrogate alone
				}
				return hex(g)
			}, disc: `\\u+[0-9a-fA-F]{4}`},
		{name: "R \\U{...}", re: regexp.MustCompile(`\\U\{([0-9a-fA-F]{1,8})\}`), dec: hex, disc: `\\U\{[0-9a-fA-F]{1,8}\}`},
		{name: "Rust/Ruby \\u{...} (underscores, several code points)",
			re: regexp.MustCompile(`\\u\{[ \t]*([0-9a-fA-F_]+(?:[ \t]+[0-9a-fA-F_]+)*)[ \t]*\}`),
			dec: func(g []string) (string, bool) {
				var b strings.Builder
				for _, f := range strings.Fields(g[1]) {
					v, l := codePoint(f, 16, g[0])
					if l {
						return g[0], true
					}
					b.WriteString(v)
				}
				return b.String(), false
			}, disc: `\\u\{[ \t]*[0-9a-fA-F_]+(?:[ \t]+[0-9a-fA-F_]+)*[ \t]*\}`},
		{core: true, name: "Python \\N{NAME}: refused (no name table)", re: regexp.MustCompile(`\\N\{([^}]*)\}?`),
			dec: func(g []string) (string, bool) {
				if strings.HasPrefix(g[1], "U+") && strings.HasSuffix(g[0], "}") {
					return codePoint(g[1][2:], 16, g[0]) // Perl \N{U+...}
				}
				return g[0], true
			}, disc: `\\N\{`},
		{name: "Perl \\x{...}", re: regexp.MustCompile(`\\x\{([0-9a-fA-F_ ]*)\}`), dec: hex, disc: `\\x\{[0-9a-fA-F_ ]*\}`},
		{name: "Perl \\o{...}", re: regexp.MustCompile(`\\o\{([0-7_]+)\}`),
			dec: func(g []string) (string, bool) { return codePoint(g[1], 8, g[0]) }, disc: `\\o\{[0-7_]+\}`},
		{name: "Perl case escapes \\U \\L \\u \\l \\Q \\E", re: regexp.MustCompile(`\\[ULQE]|\\[ul].`),
			dec: func(g []string) (string, bool) {
				switch {
				case len(g[0]) == 2: // \U \L \Q \E: the case of the region does not change what is read
					return "", false
				case g[0][1] == 'u':
					return strings.ToUpper(g[0][2:]), false
				default:
					return strings.ToLower(g[0][2:]), false
				}
			}, disc: `\\[ULQElu]`},
		{name: "Python octal \\ooo (to \\777)", re: octalRun,
			dec: func(g []string) (string, bool) { return codePoint(g[1], 8, g[0]) }},
		{name: "octal byte runs as UTF-8 (C, Python bytes)", re: regexp.MustCompile(`(?:\\[0-7]{1,3})+`), dec: runBytes(octalRun, 8)},
		{name: "Lua decimal \\ddd byte runs as UTF-8", re: regexp.MustCompile(`(?:\\[0-9]{1,3})+`), dec: runBytes(decRun, 10), disc: `\\[0-9]{1,3}`},
		{name: "Lua \\z (skips whitespace)", re: regexp.MustCompile(`\\z[ \t\r\n\f\v]*`),
			dec: func([]string) (string, bool) { return "", false }, disc: `\\z`},
		{name: "Latin-1 %HH (JavaScript unescape)", re: regexp.MustCompile(`%([0-9a-fA-F]{2})`),
			dec: func(g []string) (string, bool) { v, _ := strconv.ParseUint(g[1], 16, 8); return string(rune(v)), false }},
		{core: true, name: "%uXXXX runs (JavaScript unescape)", re: percentURun,
			dec: func(g []string) (string, bool) {
				if unpairedSurrogate(g[0]) {
					return g[0], true
				}
				units := make([]uint16, 0, len(g[0])/6)
				for i := 0; i+6 <= len(g[0]); i += 6 {
					v, _ := strconv.ParseUint(g[0][i+2:i+6], 16, 16)
					units = append(units, uint16(v))
				}
				return string(utf16.Decode(units)), false
			}},
		{core: true, name: "CSS (\\ + 1-6 hex + one space; \\ + any other character)",
			pre: func(s string) string { // CSS input preprocessing: CRLF, CR, FF are LF
				if !strings.ContainsAny(s, "\r\f") {
					return s
				}
				return strings.NewReplacer("\r\n", "\n", "\r", "\n", "\f", "\n").Replace(s)
			},
			re: regexp.MustCompile(`\\(?:([0-9a-fA-F]{1,6})[ \t\n]?|([^0-9a-fA-F\n]))`),
			dec: func(g []string) (string, bool) {
				if g[2] != "" {
					return g[2], false
				}
				v, _ := strconv.ParseUint(g[1], 16, 32)
				if v == 0 || v >= 0xD800 && v < 0xE000 || v > 0x10FFFF {
					return "\uFFFD", false
				}
				return string(rune(v)), false
			}},
		{name: "Ruby \\s \\cx \\C-x \\M-x", re: regexp.MustCompile(`\\s|\\c(.)|\\C-(.)|\\M-(?:\\C-(.)|(.))`),
			dec: func(g []string) (string, bool) {
				switch {
				case g[0] == `\s`:
					return " ", false
				case g[1] != "":
					return ctrl(g[1][0]), false
				case g[2] != "":
					return ctrl(g[2][0]), false
				case g[3] != "":
					return string(rune(g[3][0]&0x1f | 0x80)), false
				default:
					return string(rune(g[4][0] | 0x80)), false
				}
			}, disc: `\\(?:s|c.|C-.|M-.)`},
		{name: "YAML named escapes and escaped line breaks",
			re: regexp.MustCompile(`\\(?:([0abtnvfre "/\\N_LP\t])|\r?\n[ \t]*)`),
			dec: func(g []string) (string, bool) {
				if g[1] == "" {
					return "", false
				}
				return map[string]string{"0": "\x00", "a": "\a", "b": "\b", "t": "\t", "\t": "\t", "n": "\n", "v": "\v", "f": "\f",
					"r": "\r", "e": "\x1b", " ": " ", `"`: `"`, "/": "/", `\`: `\`, "N": "\u0085", "_": "\u00a0", "L": "\u2028", "P": "\u2029"}[g[1]], false
			}, disc: `\\[0abtnvfre "/\\N_LP]`},
		{name: "TOML line-ending backslash", re: regexp.MustCompile(`\\[ \t]*\r?\n[ \t\r\n]*`),
			dec: func([]string) (string, bool) { return "", false }, disc: `\\[ \t]*\r?\n`},
		{name: "form-urlencoded +", re: regexp.MustCompile(`\+`),
			dec: func([]string) (string, bool) { return " ", false }},
		{name: "doubled quotes (SQL, CSV)", re: regexp.MustCompile(`''|""`),
			dec: func(g []string) (string, bool) { return g[0][:1], false }},
		{name: "SQL U&'...' (\\XXXX, \\+XXXXXX, UESCAPE)",
			re: regexp.MustCompile(`(?i:U&)(['"])((?:[^'"]|''|"")*)['"](?:\s*(?i:UESCAPE)\s*'([^'])')?`),
			dec: func(g []string) (string, bool) {
				esc := `\`
				if g[3] != "" {
					esc = g[3]
				}
				e := regexp.QuoteMeta(esc)
				re := regexp.MustCompile(e + `(?:\+([0-9a-fA-F]{6})|([0-9a-fA-F]{4})|` + e + `)`)
				lossy := false
				body := re.ReplaceAllStringFunc(g[2], func(m string) string {
					sub := re.FindStringSubmatch(m)
					switch {
					case sub[1] != "":
						v, l := codePoint(sub[1], 16, m)
						lossy = lossy || l
						return v
					case sub[2] != "":
						v, l := codePoint(sub[2], 16, m)
						lossy = lossy || l
						return v
					}
					return esc
				})
				return g[1] + body + g[1], lossy
			}, disc: `(?i:U&)['"]`},
		{name: "PowerShell backtick escapes and line continuations", re: regexp.MustCompile("`(?:u\\{([0-9a-fA-F]+)\\}|(\r?\n)|(.))"),
			dec: func(g []string) (string, bool) {
				switch {
				case g[1] != "":
					return codePoint(g[1], 16, g[0])
				case g[2] != "":
					return "", false // a backtick before a line break joins the lines
				}
				if v, ok := map[string]string{"0": "\x00", "a": "\a", "b": "\b", "e": "\x1b", "f": "\f", "n": "\n", "r": "\r", "t": "\t", "v": "\v"}[g[3]]; ok {
					return v, false
				}
				return g[3], false
			}, disc: "`(?:u\\{[0-9a-fA-F]+\\}|\r?\n)"},
		{name: "Swift \\#u{...}, \\#n and string interpolation",
			re: regexp.MustCompile(`\\#*(?:u\{([0-9a-fA-F]+)\}|\(\s*"((?:[^"\\]|\\.)*)"\s*\))|\\#+([nrt0"'\\])`),
			dec: func(g []string) (string, bool) {
				switch {
				case g[1] != "":
					return codePoint(g[1], 16, g[0])
				case g[3] != "":
					return map[string]string{"n": "\n", "r": "\r", "t": "\t", "0": "\x00", `"`: `"`, "'": "'", `\`: `\`}[g[3]], false
				}
				return g[2], false // an interpolated string literal
			}, disc: `\\#+(?:u\{|[nrt0"'\\(])|\\\(\s*"`},
		{name: "Power Query #(...)",
			re: regexp.MustCompile(`#\((?:([0-9a-fA-F]{8}|[0-9a-fA-F]{4})|((?:cr|lf|tab)(?:,(?:cr|lf|tab))*)|(#))\)`),
			dec: func(g []string) (string, bool) {
				switch {
				case g[1] != "":
					return codePoint(g[1], 16, g[0])
				case g[3] != "":
					return "#", false
				}
				return strings.NewReplacer("cr", "\r", "lf", "\n", "tab", "\t", ",", "").Replace(g[2]), false
			}, disc: `#\((?:[0-9a-fA-F]{4}|[0-9a-fA-F]{8}|cr|lf|tab|#)`},
		{name: "RTF \\'HH (cp1252) and \\uN with its fallback",
			re: regexp.MustCompile(`\\'([0-9a-fA-F]{2})|\\u(-?[0-9]{1,5}) ?(?:\\'[0-9a-fA-F]{2}|[^\\{}])?`),
			dec: func(g []string) (string, bool) {
				if g[1] != "" {
					v, _ := strconv.ParseUint(g[1], 16, 8)
					return string(charmap.Windows1252.DecodeByte(byte(v))), false
				}
				n, _ := strconv.Atoi(g[2])
				if n < 0 {
					n += 65536
				}
				if n > 0xFFFF || n >= 0xD800 && n < 0xE000 {
					return g[0], true
				}
				return string(rune(n)), false
			}, disc: `\\'[0-9a-fA-F]{2}|\\u-?[0-9]{1,5}`},
		{name: "TeX ^^hh and ^^X", re: regexp.MustCompile(`\^\^(?:([0-9a-f]{2})|([\x00-\x7f]))`),
			dec: func(g []string) (string, bool) {
				if g[1] != "" {
					v, _ := strconv.ParseUint(g[1], 16, 8)
					return string(rune(v)), false
				}
				c := g[2][0]
				if c < 64 {
					return string(rune(c + 64)), false
				}
				return string(rune(c - 64)), false
			}, disc: `\^\^(?:[0-9a-f]{2}|[\x21-\x7e])`},
		// Read by jsEscapes (a core step); here only their grammar, for discovery.
		{name: "JavaScript \\' \\v and line continuations", disc: `\\(?:'|v|\r\n|[\r\n\x{2028}\x{2029}])`},
	}
})

// familySteps are the decoding steps of the language families that are core (core) or not, in
// families order (a family with no decoder of its own, only a grammar, has none).
func familySteps(core bool) []func(string) (string, bool) {
	var steps []func(string) (string, bool)
	for _, f := range families() {
		if f.re != nil && f.core == core {
			steps = append(steps, f.decode)
		}
	}
	return steps
}

// catalogueNFA is the grammar of every family, as one program discovery runs over reading sets.
var catalogueNFA = sync.OnceValue(func() *syntax.Prog {
	var alts []string
	for _, f := range families() {
		if f.disc != "" {
			alts = append(alts, "(?:"+f.disc+")")
		}
	}
	re, err := syntax.Parse(strings.Join(alts, "|"), syntax.Perl)
	if err != nil {
		panic(err)
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		panic(err)
	}
	return prog
})

// familyLeads are the characters a family's grammar can begin with.
const familyLeads = "\\%#`^Uu"

// spelledOutsideASCIIFamily reports that a family's grammar can be spelled over the units, with
// a character outside ASCII and escape syntax in it, by the rules of spelledOutsideASCII. The
// program runs over the reading sets of the units as a set of threads, each with its flags, so
// the work is linear in the text and the size of the program.
func (r *escReader) spelledOutsideASCIIFamily() bool {
	prog := catalogueNFA()
	type thread struct {
		pc uint32
		fl uint8
	}
	const (
		fOut      = 1 // a character outside ASCII, or an ignorable, is in it
		fSyn      = 2 // it holds escape syntax beyond an ASCII lead
		fConsumed = 4 // it has read its lead
	)
	found := false
	var closure func(pc uint32, fl uint8, out map[thread]bool)
	closure = func(pc uint32, fl uint8, out map[thread]bool) {
		t := thread{pc, fl}
		if out[t] || found {
			return
		}
		out[t] = true
		switch in := &prog.Inst[pc]; in.Op {
		case syntax.InstAlt, syntax.InstAltMatch:
			closure(in.Out, fl, out)
			closure(in.Arg, fl, out)
		case syntax.InstCapture, syntax.InstNop, syntax.InstEmptyWidth:
			closure(in.Out, fl, out)
		case syntax.InstMatch:
			found = fl&fOut != 0 && fl&fSyn != 0
		}
	}
	// step reads one character c from the threads.
	step := func(cur map[thread]bool, c rune, fl uint8) map[thread]bool {
		next := map[thread]bool{}
		for t := range cur {
			if !r.spend() {
				found = true // spent: refused
				return next
			}
			in := &prog.Inst[t.pc]
			switch in.Op {
			case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
				if in.MatchRune(c) {
					closure(in.Out, t.fl|fl, next)
				}
			}
		}
		return next
	}
	leads := asciiSet{}
	for i := 0; i < len(familyLeads); i++ {
		leads.add(familyLeads[i])
	}
	cur := map[thread]bool{}
	for k := range r.u {
		u := &r.u[k]
		can := u.rd.set.hasAny(familyLeads)
		for _, m := range u.rd.multi {
			can = can || leads.has(m[0])
		}
		if can {
			closure(uint32(prog.Start), 0, cur)
		}
		if len(cur) == 0 {
			continue
		}
		next := map[thread]bool{}
		// The flags the unit gives each thread that reads it: as lead (nothing read yet) or after.
		flags := func(t thread) uint8 {
			fl := uint8(fConsumed)
			lead := t.fl&fConsumed == 0
			if !u.ascii || !lead && u.gap {
				fl |= fOut
			}
			if lead && !u.ascii || !lead && (u.ascii || u.gap) || u.strong || !u.ascii && u.rd.set.hasAny("\\xuUN#&%;{}`^$'\"") {
				fl |= fSyn
			}
			return fl
		}
		for t := range cur {
			fl := flags(t)
			one := map[thread]bool{t: true}
			for c := 0; c < utf8.RuneSelf; c++ {
				if u.rd.set.has(byte(c)) {
					for n := range step(one, rune(c), fl) {
						next[n] = true
					}
				}
			}
			for _, m := range u.rd.multi {
				part := one
				for i := 0; i < len(m) && len(part) > 0; i++ {
					part = step(part, rune(m[i]), fl|fOut)
				}
				for n := range part {
					next[n] = true
				}
			}
			if u.skip && t.fl&fConsumed != 0 {
				next[thread{t.pc, t.fl | fOut | fSyn}] = true // a mark passed over inside it
			}
			if found {
				return true
			}
		}
		cur = next
	}
	return found
}
