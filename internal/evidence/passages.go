package evidence

import "unicode"

// MaxDocumentSpanRunes bounds one automatic span: a passage short enough to cite and review,
// well inside the evidence span limit.
const MaxDocumentSpanRunes = 2000

// DocumentSpans splits extracted text into citable passages (WIS-613): consecutive lines are packed
// into one passage until the next line would take it over MaxDocumentSpanRunes. A line is split
// only when it alone is longer than the bound. Passages carry no surrounding whitespace, offsets
// are runes, and the same text always gives the same spans. Packing keeps the count within
// MaxDocumentSpans for any text that fits an evidence source.
func DocumentSpans(text string) []Range {
	runes := []rune(text)
	var out []Range
	var passage Range
	open := false
	flush := func() {
		if open {
			out = append(out, passage)
		}
		open = false
	}
	for start := 0; start < len(runes); {
		end := start
		for end < len(runes) && runes[end] != '\n' {
			end++
		}
		line, ok := trimmed(runes, Range{Start: start, End: end})
		start = end + 1
		for ok && line.End-line.Start > MaxDocumentSpanRunes {
			flush()
			if cut, kept := trimmed(runes, Range{Start: line.Start, End: line.Start + MaxDocumentSpanRunes}); kept {
				out = append(out, cut)
			}
			line, ok = trimmed(runes, Range{Start: line.Start + MaxDocumentSpanRunes, End: line.End})
		}
		if !ok {
			continue
		}
		if open && line.End-passage.Start > MaxDocumentSpanRunes {
			flush()
		}
		if !open {
			passage, open = line, true
		}
		passage.End = line.End
	}
	flush()
	return out
}

// trimmed narrows r to exclude leading and trailing whitespace; it reports false when nothing is left.
func trimmed(runes []rune, r Range) (Range, bool) {
	for r.Start < r.End && unicode.IsSpace(runes[r.Start]) {
		r.Start++
	}
	for r.End > r.Start && unicode.IsSpace(runes[r.End-1]) {
		r.End--
	}
	return r, r.Start < r.End
}
