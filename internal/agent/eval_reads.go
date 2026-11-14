package agent

// The eval (cmd/supervisor-eval) judges answers through these, so its reading of a number or a
// statement label can't drift from the runtime's (WIS-640).

// NumberTokens returns the numbers of s that may source or be sourced, as chat and interview
// grounding read them (chatNumbers): ".45" is "0.45", and every token in or glued to exponent
// notation is left out. Read each with GroundingNumber.
func NumberTokens(s string) []string {
	var out []string
	for _, t := range chatNumbers(s) {
		out = append(out, t.n)
	}
	return out
}

// NumeralsUnsafe reports text whose numbers a reader would see differently from the tokens
// NumberTokens reads (exponent forms, non-ASCII digits, invisible or bidi runes touching a
// number): the runtime refuses such model text (chatNumeralsUnsafe), so the eval flags it too.
func NumeralsUnsafe(s string) bool { return chatNumeralsUnsafe(s) }

// ChatKindLabel is the label a chat reply shows a statement kind under.
func ChatKindLabel(kind string) string { return chatKindLabel[kind] }
