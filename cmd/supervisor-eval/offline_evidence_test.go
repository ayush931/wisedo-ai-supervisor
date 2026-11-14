//go:build offline

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
)

func TestMain(m *testing.M) {
	onboarding.RunPDFChildIfRequested()
	os.Exit(m.Run())
}

func writeOfflineJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func generateOfflineScans(t *testing.T) (string, []offlinePage, [][]byte) {
	t.Helper()
	out := os.Getenv("SUPERVISOR_OFFLINE_OUTPUT")
	if out == "" {
		t.Fatal("BLOCKED: SUPERVISOR_OFFLINE_OUTPUT is required")
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	pages := offlinePages()
	var images [][]byte
	for _, page := range pages {
		b, err := offlineImage(page)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(out, page.CaseID+".png"), b, 0600); err != nil {
			t.Fatal(err)
		}
		images = append(images, b)
	}
	writeOfflineJSON(t, filepath.Join(out, "manifest.json"), pages)
	return out, pages, images
}

func TestWIS708GenerateOfflineScans(t *testing.T) {
	generateOfflineScans(t)
}

// Opt-in measured evidence, using the existing evaluator's real API, database and worker path.
// Accuracy is reported, not a qualification threshold; failed/quarantined runs count as wrong fields.
func TestWIS708OfflineScans(t *testing.T) {
	model.ConfigPath = "disable"
	out, pages, images := generateOfflineScans(t)
	a, err := agentConfig(os.Getenv)
	if err != nil || a.Hosted.Provider != "" {
		t.Fatalf("BLOCKED: local configuration required: %v", err)
	}
	vm, err := agent.NewVisionModel(a, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err = vm.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	admin, db, done, err := disposable(ctx, os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	h := harness{ctx: ctx, control: admin, db: db, webDir: t.TempDir(), mode: ModeActual,
		actual: vm, model: a.Model, revision: a.ModelRevision, agentCfg: a, vision: vm}
	type result struct {
		offlinePage
		ImageSHA256 string      `json:"image_sha256"`
		Route       string      `json:"route"`
		Observation OCRObserved `json:"observation"`
		Run         RunResult   `json:"run"`
	}
	manifest, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	report := struct {
		SchemaVersion int               `json:"schema_version"`
		Issue         string            `json:"issue"`
		Mode          string            `json:"mode"`
		Qualified     bool              `json:"qualified"`
		GitSHA        string            `json:"git_sha"`
		GitDirty      bool              `json:"git_dirty"`
		Argv          []string          `json:"argv"`
		StartedAt     time.Time         `json:"started_at"`
		CorpusSHA256  string            `json:"corpus_sha256"`
		Environment   map[string]string `json:"environment"`
		Notes         []string          `json:"notes"`
		Cases         []result          `json:"cases"`
	}{SchemaVersion: 1, Issue: "WIS-708", Mode: ModeActual, GitSHA: gitOutput("rev-parse", "HEAD"), GitDirty: gitDirty(""), Argv: os.Args, StartedAt: time.Now().UTC(),
		CorpusSHA256: fmt.Sprintf("%x", sha256.Sum256(manifest)),
		Environment: map[string]string{"model": a.Model, "model_revision": a.ModelRevision, "vision_model": a.VisionModel, "vision_model_revision": a.VisionModelRevision,
			"prompt_version": agent.OCRPromptVersion, "prompt_sha256": agent.OCRPromptSHA256, "tool_version": agent.ToolVersion, "endpoint": a.BaseURL, "hardware": os.Getenv("SUPERVISOR_OFFLINE_HARDWARE")},
		Notes: []string{"Synthetic engineering-authored labels; zero independent reviews; measured evidence, not customer acceptance or WIS-280 qualification.",
			"One run per case; no retries or cherry-picking. Exact field values after label/whitespace normalization; duplicate labels fail.",
			"Only the six labelled fields are scored; OCRObserved.exact_text is not evaluated and retains its zero value.",
			"Handwritten-style pages use deterministic italic glyphs and baseline jitter, not human handwriting.",
			"In-process authenticated HTTP API and real Runtime.ExtractScan on disposable PostgreSQL; no browser, OIDC round trip or Temporal transport."}}
	for i, page := range pages {
		w, err := h.newAPIWorld(nil)
		if err != nil {
			t.Fatal(err)
		}
		uploader, err := w.member(w.tenant, w.site, "operator")
		if err != nil {
			t.Fatal(err)
		}
		route, media, data := "/source-scans", "image/png", images[i]
		if i == 0 {
			var pdf bytes.Buffer
			if err = api.ImportImages(nil, &pdf, []io.Reader{bytes.NewReader(data)}, nil, nil); err != nil {
				t.Fatal(err)
			}
			route, media, data = "/source-scan-batches", "application/pdf", pdf.Bytes()
			if err = os.WriteFile(filepath.Join(out, page.CaseID+".pdf"), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		code, body := w.raw(uploader, "POST", "/v1/sites/"+w.site+route, media, data,
			map[string]string{"X-Source-Title": url.PathEscape(page.CaseID), "Idempotency-Key": uuid.NewString()})
		if code != 202 {
			t.Fatalf("%s queue: HTTP %d %s", page.CaseID, code, body)
		}
		var job evidence.ScanJob
		if i == 0 {
			var batch evidence.ScanBatch
			if err = json.Unmarshal(body, &batch); err != nil || len(batch.Jobs) != 1 {
				t.Fatalf("PDF batch: %s %v", body, err)
			}
			job = batch.Jobs[0]
		} else if err = json.Unmarshal(body, &job); err != nil {
			t.Fatal(err)
		}
		p := &responseRecorder{Provider: vm}
		started := time.Now()
		extracted, err := h.visionRuntime(p).ExtractScan(ctx, agent.ScanRef{TenantID: w.tenant, SiteID: w.site, JobID: job.JobID, RunID: job.RunID})
		if err != nil {
			t.Fatal(err)
		}
		observed, err := w.observeScan(job, extracted)
		if err != nil {
			t.Fatal(err)
		}
		observed.LatencyMs, observed.FinishReason = time.Since(started).Milliseconds(), p.reason()
		page.Fields = scoreOfflineFields(page.Fields, observed.StoredText)
		run := RunResult{Run: 1, Knowledge: &observed.KnowledgeObserved, OCR: &observed, Breaches: ocrBreaches(ModeActual, observed)}
		report.Cases = append(report.Cases, result{page, fmt.Sprintf("%x", sha256.Sum256(images[i])), route, observed, run})
		writeOfflineJSON(t, filepath.Join(out, "accuracy.json"), report)
		correct := 0
		for _, f := range page.Fields {
			if f.Correct {
				correct++
			}
		}
		t.Logf("%s: %s fields=%d/%d latency_ms=%d", page.CaseID, observed.JobState, correct, len(page.Fields), observed.LatencyMs)
		if len(run.Breaches) > 0 || extracted.Replayed || !observed.ProviderActual {
			t.Errorf("%s: invalid measurement: breaches=%v replayed=%v actual=%v", page.CaseID, run.Breaches, extracted.Replayed, observed.ProviderActual)
		}
	}
}
