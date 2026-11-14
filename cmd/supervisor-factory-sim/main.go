// supervisor-factory-sim generates synthetic data and loads only a local fixture API.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func readJSON(b []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		return e
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func validate(d dataset) error {
	for _, f := range d.manifest.Files {
		b, ok := d.files[f.Path]
		if !ok || sha(b) != f.SHA256 {
			return fmt.Errorf("missing or changed file %s", f.Path)
		}
		switch filepath.Ext(f.Path) {
		case ".md":
			var source evidence.Input
			if e := json.Unmarshal(d.files[strings.TrimSuffix(f.Path, ".md")+".json"], &source); e != nil || source.Text != string(b) {
				return fmt.Errorf("text differs from source contract: %s", f.Path)
			}
			b = wire(source)
		case ".csv":
			rows, e := workstation.ParseConfigurationCSV(b)
			if e != nil {
				return e
			}
			b = wire(factorysim.ConfigurationRows{Rows: rows})
			if !bytes.Equal(b, d.files[strings.TrimSuffix(f.Path, ".csv")+".json"]) {
				return fmt.Errorf("CSV differs from JSON projection: %s", f.Path)
			}
		}
		// Keep the runtime semantic checks; load also verifies every byte against the seed.
		switch f.Schema {
		case "FactoryAnswerKeyV1":
			var key factorysim.AnswerKey
			if e := readJSON(b, &key); e != nil {
				return e
			}
		case "StationProfileBodyV1":
			var p workstation.StationProfileBodyV1
			if e := json.Unmarshal(b, &p); e != nil {
				return e
			}
			if e := p.Validate(); e != nil {
				return fmt.Errorf("%s: %w", f.Path, e)
			}
		case "JobOrderBodyV1":
			var p workstation.JobOrderBodyV1
			if e := json.Unmarshal(b, &p); e != nil {
				return e
			}
			if e := p.Validate(); e != nil {
				return fmt.Errorf("%s: %w", f.Path, e)
			}
		case "ShiftPlanBodyV1":
			if _, e := workstation.DecodeShiftPlan(b); e != nil {
				return fmt.Errorf("%s: %w", f.Path, e)
			}
		case "OeeCalculationRequestV1":
			var p oee.OeeCalculationRequestV1
			if e := json.Unmarshal(b, &p); e != nil {
				return e
			}
			if e := oee.VerifyDataset(p.Dataset); e != nil {
				return fmt.Errorf("%s: %w", f.Path, e)
			}
		}
		if f.Schema == "OperationalBatchV1" {
			var batch ingestion.Batch
			if e := json.Unmarshal(b, &batch); e != nil {
				return e
			}
			if e := batch.Validate(); e != nil {
				return e
			}
			for _, event := range batch.Events {
				var e error
				if event.Kind == "production_delta" {
					_, e = production.DecodeDelta(event.Payload)
				} else {
					var observation factorysim.Observation
					e = readJSON(event.Payload, &observation)
				}
				if e != nil {
					return fmt.Errorf("%s %s: %w", f.Path, event.ExternalKey, e)
				}
			}
		}
	}
	return nil
}
func writeDataset(d dataset, out string) error {
	if out == "" {
		return errors.New("--out required")
	}
	if _, e := os.Lstat(out); !errors.Is(e, os.ErrNotExist) {
		return errors.New("output must not already exist")
	}
	parent := filepath.Dir(out)
	if e := os.MkdirAll(parent, 0700); e != nil {
		return e
	}
	tmp, e := os.MkdirTemp(parent, ".factory-sim-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(tmp)
	for name, b := range d.files {
		path := filepath.Join(tmp, name)
		if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			return e
		}
		if e = os.WriteFile(path, b, 0600); e != nil {
			return e
		}
	}
	return os.Rename(tmp, out)
}
func readDataset(dir string) (dataset, error) {
	b, e := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if e != nil {
		return dataset{}, e
	}
	if len(b) > 4<<20 {
		return dataset{}, errors.New("oversized manifest")
	}
	var m factorysim.Manifest
	if e = readJSON(b, &m); e != nil {
		return dataset{}, e
	}
	// Only this version's deterministic fixture is loadable: edited payloads are not a seed.
	d, e := generate(m.Seed, m.Days, m.Start)
	if e != nil {
		return d, e
	}
	if !bytes.Equal(b, d.files["manifest.json"]) {
		return d, errors.New("manifest is not this version's synthetic dataset")
	}
	for name, expected := range d.files {
		path := filepath.Join(dir, name)
		info, e := os.Lstat(path)
		if e != nil || !info.Mode().IsRegular() || info.Size() != int64(len(expected)) {
			return d, fmt.Errorf("missing, linked or changed file %s", name)
		}
		actual, e := os.ReadFile(path)
		if e != nil {
			return d, e
		}
		if !bytes.Equal(actual, expected) {
			return d, fmt.Errorf("changed file %s", name)
		}
	}
	return d, nil
}
func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: supervisor-factory-sim generate|load|run|manuals|scenarios --out DIR [--days 14 --seed 278 --start 2026-09-01]")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	dir := flags.String("out", "", "dataset directory")
	days := flags.Int("days", 14, "simulated operating days (1..31)")
	seed := flags.Int64("seed", 278, "deterministic seed")
	start := flags.String("start", "2026-09-01", "first operating date, YYYY-MM-DD")
	base := flags.String("url", "", "literal loopback fixture URL")
	wallStart := flags.String("wall-start", "", "frozen RFC3339 wall start (run, defaults to now + 2 minutes)")
	speed := flags.Int64("speed", 60, "model seconds per wall second (run, 1..300)")
	scenario := flags.String("scenario", "all", "edge catalogue switch (scenarios)")
	exclude := flags.String("exclude", "", "comma-separated case IDs to leave out of --scenario all (scenarios)")
	sessions := flags.String("sessions", "", "renewable browser cookie JSON file (run)")
	if e := flags.Parse(args[1:]); e != nil {
		return e
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	began := time.Now()
	switch args[0] {
	case "persona-step":
		return personaStep(ctx, os.Stdin, out)
	case "generate":
		d, e := generate(*seed, *days, *start)
		if e != nil {
			return e
		}
		if e = validate(d); e != nil {
			return e
		}
		if e = writeDataset(d, *dir); e != nil {
			return e
		}
		fmt.Fprintf(out, "SYNTHETIC: days=%d stations=48 events=%d files=%d revenue_INR=%d annualized_INR=%d elapsed=%s\n", *days, d.manifest.Events, len(d.files), d.manifest.RevenueINR, d.manifest.RevenueINR*300/int64(*days), time.Since(began).Round(time.Millisecond))
		return nil
	case "run":
		if e := facilityURL(*base); e != nil {
			return e
		}
		wall := time.Now().UTC().Truncate(time.Second).Add(2 * time.Minute)
		if *wallStart != "" {
			var e error
			wall, e = time.Parse(time.RFC3339, *wallStart)
			if e != nil {
				return e
			}
		}
		d, schedule, e := generateLive(*seed, *start, wall, *speed)
		if e != nil {
			return e
		}
		if e = writeDataset(d, *dir); e != nil {
			return e
		}
		l, e := sessionLoader(*base, *sessions)
		if e != nil {
			return e
		}
		defer l.client.CloseIdleConnections()
		_, e = runLive(ctx, l, d, schedule, factorysim.WallClock{}, *dir, *sessions, out)
		return e
	case "manuals":
		if e := facilityURL(*base); e != nil {
			return e
		}
		d, key, e := generateManuals(*seed, *start)
		if e != nil {
			return e
		}
		if e = writeDataset(d, *dir); e != nil {
			return e
		}
		l, e := sessionLoader(*base, *sessions)
		if e != nil {
			return e
		}
		defer l.client.CloseIdleConnections()
		r, e := uploadManuals(ctx, l, d, key, *dir)
		if e != nil {
			return e
		}
		fmt.Fprintf(out, "SYNTHETIC manuals: documents=%d cycle_claims=%d citation_check=%t\n", len(r.Documents), len(r.CycleSpans), r.Passed)
		return nil
	case "scenarios":
		if e := facilityURL(*base); e != nil {
			return e
		}
		d, e := generate(*seed, 1, *start)
		if e != nil {
			return e
		}
		l, e := sessionLoader(*base, *sessions)
		if e != nil {
			return e
		}
		defer l.client.CloseIdleConnections()
		return runScenarios(ctx, l, d, *scenario, *exclude, *dir, out)
	case "load":
		d, e := readDataset(*dir)
		if e != nil {
			return e
		}
		if e = validate(d); e != nil {
			return e
		}
		client, e := newLoader(*base, os.Getenv)
		if e != nil {
			return e
		}
		defer client.client.CloseIdleConnections()
		report, e := client.load(ctx, d)
		if e != nil {
			return e
		}
		fmt.Fprintf(out, "SYNTHETIC (approvals and reviews use scripted synthetic fixture identities): %s elapsed=%s\n", wire(report), time.Since(began).Round(time.Millisecond))
		return nil
	default:
		return errors.New("expected generate, load, run, manuals or scenarios")
	}
}
func main() {
	onboarding.RunPDFChildIfRequested()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if e := run(ctx, os.Args[1:], os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

func sessionLoader(base, sessions string) (*loader, error) {
	getenv := os.Getenv
	if sessions != "" {
		cookies, err := sessionCookies(sessions)
		if err != nil {
			return nil, err
		}
		getenv = func(k string) string {
			if strings.HasPrefix(k, "SUPERVISOR_SIM_") {
				return cookies[strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(k, "SUPERVISOR_SIM_"), "_COOKIE"))]
			}
			return os.Getenv(k)
		}
	}
	return newLoader(base, getenv)
}
