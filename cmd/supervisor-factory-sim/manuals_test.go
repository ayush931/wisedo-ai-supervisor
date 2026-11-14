package main

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
)

func TestMain(m *testing.M) { onboarding.RunPDFChildIfRequested(); os.Exit(m.Run()) }

func TestWIS709ManualsDeterministicAndExtractEveryNumericLine(t *testing.T) {
	a, key, err := generateManuals(278, "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := generateManuals(278, "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(key.Documents) != 27 || !bytes.Equal(a.files["manifest.json"], b.files["manifest.json"]) {
		t.Fatal("manual count or determinism")
	}
	digits := regexp.MustCompile(`[0-9]`)
	for _, doc := range key.Documents {
		t.Run(doc.Name, func(t *testing.T) {
			pdf := a.files["manuals/"+doc.Name+".pdf"]
			kind, err := onboarding.Inspect(pdf, "application/pdf", onboarding.DefaultLimits)
			if err != nil {
				t.Fatal(err)
			}
			extracted, err := onboarding.Extract(context.Background(), pdf, kind, onboarding.DefaultLimits)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(doc.Text, "\n") {
				if digits.MatchString(line) && !strings.Contains(strings.Join(strings.Fields(extracted), " "), strings.Join(strings.Fields(line), " ")) {
					t.Fatalf("numeric statement missing after extraction: %q", line)
				}
			}
		})
	}
}
func TestWIS709PDFNormalizationRejectsUnknownWriterShape(t *testing.T) {
	if _, err := normalizePDF([]byte("%PDF-unknown"), time.Now(), strings.Repeat("a", 64)); err == nil {
		t.Fatal("unknown PDF metadata accepted")
	}
	if _, err := typedPDF("non-ASCII \u00e9", time.Now()); err == nil {
		t.Fatal("unsupported core font text accepted")
	}
}
