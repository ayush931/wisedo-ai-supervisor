package onboarding

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestWIS271AnswerTextRuneBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"empty", "", false},
		{"one rune", "é", true},
		{"8000 multibyte runes", strings.Repeat("é", 8000), true},
		{"8001 runes", strings.Repeat("é", 8001), false},
		{"nul", "a\x00b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidateAnswerText(tc.text); got != tc.want {
				t.Fatalf("ValidateAnswerText() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWIS271InterviewLifecycle(t *testing.T) {
	for _, tc := range []struct {
		from, to InterviewStatus
		want     bool
	}{
		{StatusActive, StatusPaused, true},
		{StatusActive, StatusSubmitted, true},
		{StatusPaused, StatusActive, true},
		{StatusSubmitted, StatusClosed, true},
		{StatusClosed, StatusActive, false},
		{StatusActive, StatusActive, false},
		{StatusSubmitted, StatusPaused, false},
	} {
		if got := canMove(tc.from, tc.to); got != tc.want {
			t.Errorf("canMove(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// TestWIS614AgentTurnPassageSelection: a turn may name at most eight distinct passages; a repeated,
// nil or ninth passage is refused before anything is read, and a turn with none is unchanged.
func TestWIS614AgentTurnPassageSelection(t *testing.T) {
	ids := func(n int) []uuid.UUID {
		out := make([]uuid.UUID, n)
		for i := range out {
			out[i] = uuid.New()
		}
		return out
	}
	one := uuid.New()
	for name, c := range map[string]struct {
		in StartAgentTurnInput
		ok bool
	}{
		"no passages":      {StartAgentTurnInput{RequestID: uuid.New()}, true},
		"eight passages":   {StartAgentTurnInput{RequestID: uuid.New(), SourceSpanIDs: ids(8)}, true},
		"nine passages":    {StartAgentTurnInput{RequestID: uuid.New(), SourceSpanIDs: ids(9)}, false},
		"repeated passage": {StartAgentTurnInput{RequestID: uuid.New(), SourceSpanIDs: []uuid.UUID{one, one}}, false},
		"nil passage":      {StartAgentTurnInput{RequestID: uuid.New(), SourceSpanIDs: []uuid.UUID{uuid.Nil}}, false},
		"missing request":  {StartAgentTurnInput{SourceSpanIDs: ids(1)}, false},
	} {
		if err := c.in.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: %v, want ok=%v", name, err, c.ok)
		}
	}
}
