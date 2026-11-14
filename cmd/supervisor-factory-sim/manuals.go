package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

type manualDocument struct {
	Name      string `json:"name"`
	Title     string `json:"title"`
	Text      string `json:"text"`
	PDFSHA256 string `json:"pdf_sha256"`
	Class     string `json:"machine_class,omitempty"`
}
type manualAnswerKey struct {
	SchemaVersion int                       `json:"schema_version"`
	Synthetic     bool                      `json:"synthetic"`
	ModelStart    time.Time                 `json:"model_start"`
	Sheets        []factorysim.MachineSheet `json:"machine_sheets"`
	Tooling       factorysim.ToolRegister   `json:"tooling"`
	Documents     []manualDocument          `json:"documents"`
}

func typedPDF(text string, at time.Time) ([]byte, error) {
	if strings.IndexFunc(text, func(r rune) bool { return r > 126 || r < 32 && r != '\n' }) >= 0 {
		return nil, errors.New("typed fictional manuals require ASCII text")
	}
	model.ConfigPath = "disable"
	config := model.NewDefaultConfiguration()
	config.WriteObjectStream = false
	config.WriteXRefStream = false
	pages := map[string]any{}
	var lines []string
	for _, paragraph := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		line := ""
		for _, word := range strings.Fields(paragraph) {
			if len(word) > 80 {
				return nil, errors.New("manual word exceeds printable width")
			}
			if len(line)+len(word)+1 > 80 {
				lines = append(lines, line)
				line = ""
			}
			if line != "" {
				line += " "
			}
			line += word
		}
		lines = append(lines, line)
	}
	for offset := 0; offset < len(lines); offset += 45 {
		pages[strconv.Itoa(offset/45+1)] = map[string]any{"content": map[string]any{"text": []any{map[string]any{"value": strings.Join(lines[offset:min(offset+45, len(lines))], "\n"), "pos": []int{36, 790}, "font": map[string]any{"name": "Helvetica", "size": 10}}}}}
	}
	var out bytes.Buffer
	if err := api.Create(nil, bytes.NewReader(wire(map[string]any{"paper": "A4", "pages": pages})), &out, config); err != nil {
		return nil, err
	}
	return normalizePDF(out.Bytes(), at, sha([]byte(text)))
}

// Replace only fixed-width metadata, preserving the writer's xref offsets.
func normalizePDF(b []byte, at time.Time, digest string) ([]byte, error) {
	dates := regexp.MustCompile(`/((?:CreationDate|ModDate))\((D:[0-9]{14}[+-][0-9]{2}'[0-9]{2}')\)`)
	ids := regexp.MustCompile(`/ID\[<([0-9A-Fa-f]{32})> <([0-9A-Fa-f]{32})>\]`)
	dm, im := dates.FindAllSubmatchIndex(b, -1), ids.FindAllSubmatchIndex(b, -1)
	if len(dm) != 2 || len(im) != 1 || len(digest) != 64 {
		return nil, errors.New("pdfcpu metadata shape changed; cannot normalize deterministically")
	}
	out := bytes.Clone(b)
	date := []byte("D:" + at.UTC().Format("20060102150405") + "+00'00'")
	for _, m := range dm {
		if m[5]-m[4] != len(date) {
			return nil, errors.New("PDF date width changed")
		}
		copy(out[m[4]:m[5]], date)
	}
	for _, span := range [][2]int{{im[0][2], im[0][3]}, {im[0][4], im[0][5]}} {
		copy(out[span[0]:span[1]], []byte(digest[:32]))
	}
	return out, nil
}

func generateManuals(seed int64, start string) (dataset, manualAnswerKey, error) {
	plant, err := generate(seed, 1, start)
	if err != nil {
		return dataset{}, manualAnswerKey{}, err
	}
	zone, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return dataset{}, manualAnswerKey{}, err
	}
	at, err := time.ParseInLocation("2006-01-02", start, zone)
	if err != nil {
		return dataset{}, manualAnswerKey{}, err
	}
	at = at.Add(6 * time.Hour)
	if err = addMachineModel(&plant, at); err != nil {
		return dataset{}, manualAnswerKey{}, err
	}
	key := manualAnswerKey{SchemaVersion: 1, Synthetic: true, ModelStart: at, Sheets: factorysim.MachineSheets(plant.manifest), Documents: []manualDocument{}}
	if err = json.Unmarshal(plant.files["tool-register.json"], &key.Tooling); err != nil {
		return dataset{}, key, err
	}
	d := dataset{manifest: plant.manifest, files: map[string][]byte{}}
	d.manifest.Files = nil
	add := func(name, title, text, class string) error {
		pdf, err := typedPDF(text, at)
		if err != nil {
			return err
		}
		key.Documents = append(key.Documents, manualDocument{Name: name, Title: title, Text: text, PDFSHA256: sha(pdf), Class: class})
		d.raw("manuals/"+name+".md", "FactoryManualTextV1", []byte(text))
		d.raw("manuals/"+name+".pdf", "FactoryTypedPDFV1", pdf)
		return nil
	}
	for _, sheet := range key.Sheets {
		var text strings.Builder
		fmt.Fprintf(&text, "FICTIONAL %s operating and maintenance manual\nSahyadri Precision Components: synthetic test data, not OEM instructions.\nDurations use the simulated clock, not compressed wall time.\nInspect guards and material before work; only authorized people approve restart.\n", sheet.Class)
		for _, s := range sheet.Stations {
			fmt.Fprintf(&text, "\nStation: %s\nRated cycle time: %d seconds.\nChangeover time: %d minutes.\nMaintenance inspection interval: %d hours.\nLubrication duration: %d minutes.\nMean time between failures: %d hours.\nRepair duration: %d minutes.\n", s.Name, s.RatedCycleSeconds, s.ChangeoverMinutes, s.MaintenanceIntervalHours, s.LubricationMinutes, s.MTBFHours, s.RepairMinutes)
			for _, v := range s.SKUCycles {
				fmt.Fprintf(&text, "SKU %s cycle time: %d seconds.\n", v.SKU, v.Seconds)
			}
		}
		text.WriteString("\nAlarm reference\n")
		for _, a := range sheet.Alarms {
			fmt.Fprintf(&text, "%s: %s.\n", a.Code, a.Meaning)
		}
		if err = add("machine-"+strings.ToLower(strings.ReplaceAll(sheet.Class, " ", "-")), "Fictional "+sheet.Class+" machine manual", text.String(), sheet.Class); err != nil {
			return d, key, err
		}
	}
	for li, line := range plant.manifest.Lines {
		var source evidence.Input
		if err = json.Unmarshal(plant.files[fmt.Sprintf("sources/line-%d.json", li)], &source); err != nil {
			return d, key, err
		}
		if err = add(fmt.Sprintf("line-%d-sop", li), "Fictional "+line.Name+" SOP", "FICTIONAL line SOP - synthetic test data.\n"+source.Text, ""); err != nil {
			return d, key, err
		}
	}
	var tools strings.Builder
	tools.WriteString("FICTIONAL tool, die and fixture register\nAll counters describe the simulated day; replacement history is append-only.\n")
	for _, t := range key.Tooling.Tools {
		fmt.Fprintf(&tools, "\nTool: %s\nStation: %s; SKU: %s; kind: %s\nLife: %d processed parts; opening age: %d; closing age: %d.\n", t.ID, t.StationID, t.SKU, t.Kind, t.LifeParts, t.InitialParts, t.UsedParts)
	}
	for _, event := range key.Tooling.Events {
		fmt.Fprintf(&tools, "%s %s: %s, counter %d to %d.\n", event.At.Format(time.RFC3339), event.ToolID, event.Kind, event.Before, event.After)
	}
	if err = add("tool-register", "Fictional tool, die and fixture register", tools.String(), ""); err != nil {
		return d, key, err
	}
	var gauges strings.Builder
	gauges.WriteString("FICTIONAL gauge calibration register\nA reading at or after its due instant is held as CALIBRATION_OVERDUE.\n")
	for _, g := range key.Tooling.Gauges {
		fmt.Fprintf(&gauges, "\nGauge: %s\nStation: %s\nCalibration interval: %d days.\nCalibrated: %s\nDue: %s\n", g.ID, g.StationID, g.CalibrationIntervalDays, g.CalibratedAt.Format(time.RFC3339), g.DueAt.Format(time.RFC3339))
	}
	if err = add("gauge-register", "Fictional gauge calibration register", gauges.String(), ""); err != nil {
		return d, key, err
	}
	d.put("manual-answer-key.json", "FactoryManualAnswerKeyV1", key)
	d.files["manifest.json"] = wire(d.manifest)
	return d, key, nil
}
