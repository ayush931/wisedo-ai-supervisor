package onboarding

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A strict reader for PDF object bodies (WIS-625 review). pdfcpu and ledongthuc are lenient in
// ways a viewer such as PDF.js is not: they split names on any Unicode space, drop null-valued
// entries and keep the first of two equal keys, resolve object-stream members by index alone. Each
// such difference lets the checks see one document and the reader see another, so every body the
// screen trusts is first read here by PDF's own rules, and anything outside them refuses the file
// (PDF_NONSTANDARD_SYNTAX). Stream data is never read here.

type pdfToken struct {
	kind  byte   // 'n' name (decoded, without '/'), 's' string, 'd' << >> [ ], 'w' word
	text  string // the decoded name or the word
	start int    // its offset in the lexed bytes
}

var pdfInteger = regexp.MustCompile(`^[0-9]+$`)

// lexPDF tokenises b up to a "stream" or "endobj" word (stop reports which, "" at the end). It
// fails on a separator other than PDF whitespace (a control byte, or a UTF-8 Unicode space such as
// U+0085), a bad name escape, a bad hex string, an unterminated string or a stray delimiter. Each token
// records where it starts.
func lexPDF(b []byte) (tokens []pdfToken, stop string, ok bool) {
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
			from, depth := i, 0
			for ; ; i++ {
				if i >= len(b) {
					return nil, "", false
				}
				if b[i] == '\\' {
					i++
				} else if b[i] == '(' {
					depth++
				} else if b[i] == ')' {
					if depth--; depth == 0 {
						break
					}
				}
			}
			i++
			tokens = append(tokens, pdfToken{kind: 's', start: from})
		case (c == '<' || c == '>') && i+1 < len(b) && b[i+1] == c:
			tokens = append(tokens, pdfToken{kind: 'd', text: string(b[i : i+2]), start: i})
			i += 2
		case c == '<':
			from := i
			for i++; i < len(b) && b[i] != '>'; i++ {
				if !pdfWhitespace(b[i]) && !strings.ContainsRune("0123456789abcdefABCDEF", rune(b[i])) {
					return nil, "", false
				}
			}
			if i >= len(b) {
				return nil, "", false
			}
			i++
			tokens = append(tokens, pdfToken{kind: 's', start: from})
		case c == '[' || c == ']':
			tokens = append(tokens, pdfToken{kind: 'd', text: string(c), start: i})
			i++
		case c == '/' || !pdfDelimiter(c):
			from := i
			if c == '/' {
				i++
			}
			for i < len(b) && !pdfWhitespace(b[i]) && !pdfDelimiter(b[i]) {
				if b[i] < 0x80 {
					if b[i] < 0x20 || b[i] == 0x7F {
						return nil, "", false
					}
					i++
					continue
				}
				r, n := utf8.DecodeRune(b[i:])
				if unicode.IsSpace(r) {
					return nil, "", false
				}
				i += n
			}
			if c != '/' {
				word := string(b[from:i])
				if word == "stream" || word == "endobj" {
					return tokens, word, true
				}
				tokens = append(tokens, pdfToken{kind: 'w', text: word, start: from})
				continue
			}
			name, ok := pdfNameDecode(b[from+1 : i])
			if !ok {
				return nil, "", false
			}
			tokens = append(tokens, pdfToken{kind: 'n', text: name, start: from})
		default: // ) > { } outside their place
			return nil, "", false
		}
	}
	return tokens, "", true
}

// pdfNameDecode resolves #xx escapes, so /Sub#74ype is the key Subtype.
func pdfNameDecode(b []byte) (string, bool) {
	if bytes.IndexByte(b, '#') < 0 {
		return string(b), true
	}
	var out []byte
	for i := 0; i < len(b); i++ {
		if b[i] != '#' {
			out = append(out, b[i])
			continue
		}
		if i+2 >= len(b) {
			return "", false
		}
		v, err := strconv.ParseUint(string(b[i+1:i+3]), 16, 8)
		if err != nil || v == 0 {
			return "", false
		}
		out = append(out, byte(v))
		i += 2
	}
	return string(out), true
}

// pdfValue reads one value at t[i] and returns the index after it, or -1. A dictionary may not
// repeat a key (pdfcpu keeps the first, a viewer the last) or give a key the value null (pdfcpu
// drops the entry, a viewer keeps it).
func pdfValue(t []pdfToken, i, depth int) int {
	if i >= len(t) || depth > 64 {
		return -1
	}
	switch tok := t[i]; {
	case tok.kind == 'n' || tok.kind == 's':
		return i + 1
	case tok.kind == 'd' && tok.text == "<<":
		keys := map[string]bool{}
		for i++; i < len(t); {
			if t[i].kind == 'd' && t[i].text == ">>" {
				return i + 1
			}
			if t[i].kind != 'n' || keys[t[i].text] || i+1 >= len(t) || t[i+1].kind == 'w' && t[i+1].text == "null" {
				return -1
			}
			keys[t[i].text] = true
			if i = pdfValue(t, i+1, depth+1); i < 0 {
				return -1
			}
		}
		return -1
	case tok.kind == 'd' && tok.text == "[":
		for i++; i < len(t); {
			if t[i].kind == 'd' && t[i].text == "]" {
				return i + 1
			}
			if i = pdfValue(t, i, depth+1); i < 0 {
				return -1
			}
		}
		return -1
	case tok.kind == 'w':
		if pdfInteger.MatchString(tok.text) && i+2 < len(t) && t[i+1].kind == 'w' && pdfInteger.MatchString(t[i+1].text) && t[i+2].kind == 'w' && t[i+2].text == "R" {
			return i + 3
		}
		if pdfNumber.MatchString(tok.text) || tok.text == "true" || tok.text == "false" || tok.text == "null" {
			return i + 1
		}
	}
	return -1
}

// plainObjectSyntax reads a plain object from its offset: "n g obj", one value, then "endobj" or
// "stream" (whose data is not read).
func plainObjectSyntax(b []byte) bool {
	t, stop, ok := lexPDF(b)
	if !ok || stop == "" || len(t) < 4 || t[0].kind != 'w' || !pdfInteger.MatchString(t[0].text) || t[1].kind != 'w' || !pdfInteger.MatchString(t[1].text) ||
		t[2].kind != 'w' || t[2].text != "obj" {
		return false
	}
	return pdfValue(t, 3, 0) == len(t)
}

// objectStreamSyntax reads an object stream's decoded content: a prologue of n pairs "number
// offset" before first, with distinct numbers and strictly increasing offsets, each landing on a
// token start of the stream read as a whole (not inside a comment, string or token), and each member,
// from its offset to the next, exactly one value. It returns the member numbers in order.
func objectStreamSyntax(content []byte, first, n int) ([]int, bool) {
	if n <= 0 || first <= 0 || first > len(content) {
		return nil, false
	}
	head, stop, ok := lexPDF(content[:first])
	if !ok || stop != "" || len(head) != 2*n {
		return nil, false
	}
	nums, offsets := make([]int, n), make([]int, n)
	seen := map[int]bool{}
	for i := range n {
		num, err1 := strconv.Atoi(head[2*i].text)
		off, err2 := strconv.Atoi(head[2*i+1].text)
		if head[2*i].kind != 'w' || head[2*i+1].kind != 'w' || err1 != nil || err2 != nil || num <= 0 || off < 0 || seen[num] ||
			(i > 0 && off <= offsets[i-1]) || first+off >= len(content) {
			return nil, false
		}
		seen[num], nums[i], offsets[i] = true, num, off
	}
	body := content[first:]
	// Each member must start exactly where a token of the stream read whole starts: not inside a
	// comment, a string or another token (an offset inside "/XIm1" would read "Im1").
	whole, stop, ok := lexPDF(body)
	if !ok || stop != "" {
		return nil, false
	}
	starts := map[int]bool{}
	for _, t := range whole {
		starts[t.start] = true
	}
	for i, off := range offsets {
		if !starts[off] {
			return nil, false
		}
		end := len(body)
		if i+1 < n {
			end = offsets[i+1]
		}
		t, stop, ok := lexPDF(body[off:end])
		if !ok || stop != "" || pdfValue(t, 0, 0) != len(t) {
			return nil, false
		}
	}
	return nums, true
}
