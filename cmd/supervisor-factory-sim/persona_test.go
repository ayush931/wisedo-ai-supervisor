package main

import (
	"strings"
	"testing"
)

func TestWIS709PersonaBoundary(t *testing.T) {
	in := personaInput{Role: "supervisor", Goal: "Read handover", Screen: "Untrusted screen", Choices: []personaChoice{{"observe", "observe", "Observe"}, {"finish", "finish", "Finish"}}}
	good := personaDecision{ActionID: "observe", Observation: "Unknown", ExpectedBehavior: "unknown", Confidence: "low"}
	if err := validatePersona(in, &good); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"javascript:alert(1)", "approve", "https://example.com", "shell"} {
		bad := good
		bad.ActionID = id
		if validatePersona(in, &bad) == nil {
			t.Fatal("accepted invented action", id)
		}
	}
	bad := good
	bad.Observation = strings.Repeat("x", 601)
	if validatePersona(in, &bad) == nil {
		t.Fatal("accepted oversized memory")
	}
	in.Choices[0].Kind = "execute"
	if validatePersona(in, nil) == nil {
		t.Fatal("accepted executable action kind")
	}
}
