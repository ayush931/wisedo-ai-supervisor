package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strings"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goitalic"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

type offlineField struct {
	Name     string `json:"name"`
	Expected string `json:"expected"`
	Observed string `json:"observed"`
	Correct  bool   `json:"correct"`
}

type offlinePage struct {
	CaseID string         `json:"case_id"`
	Style  string         `json:"style"`
	Fields []offlineField `json:"fields"`
}

func offlinePages() []offlinePage {
	var pages []offlinePage
	for _, style := range []string{"clean", "skewed", "low_contrast", "stamped", "handwritten_style"} {
		for sample := 1; sample <= 4; sample++ {
			n := len(pages) + 1
			pages = append(pages, offlinePage{CaseID: fmt.Sprintf("offline-%s-%02d", style, sample), Style: style, Fields: []offlineField{
				{Name: "Record", Expected: fmt.Sprintf("INSP-%03d", n)},
				{Name: "Machine", Expected: fmt.Sprintf("PRESS-%02d", sample)},
				{Name: "Lot", Expected: fmt.Sprintf("BATCH-%04d", 2300+n)},
				{Name: "Accepted", Expected: fmt.Sprint(100 + 7*n)},
				{Name: "Rejected", Expected: fmt.Sprint(n % 5)},
				{Name: "Date", Expected: fmt.Sprintf("2026-11-%02d", n)},
			}})
		}
	}
	return pages
}

func offlineImage(p offlinePage) ([]byte, error) {
	page := image.NewRGBA(image.Rect(0, 0, 720, 400))
	draw.Draw(page, page.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	ttf := goregular.TTF
	if p.Style == "handwritten_style" {
		// shortcut: synthetic italic glyphs with baseline jitter, not human handwriting; use human-labelled pages for acceptance.
		ttf = goitalic.TTF
	}
	f, err := opentype.Parse(ttf)
	if err != nil {
		return nil, err
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 24, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	defer face.Close()
	ink := color.RGBA{20, 20, 20, 255}
	if p.Style == "low_contrast" {
		ink = color.RGBA{180, 180, 180, 255}
	}
	d := font.Drawer{Dst: page, Src: image.NewUniform(ink), Face: face}
	lines := []string{"SYNTHETIC INSPECTION LOG"}
	for _, field := range p.Fields {
		lines = append(lines, field.Name+": "+field.Expected)
	}
	for row, line := range lines {
		d.Dot = fixed.P(45, 48+row*43)
		if p.Style != "handwritten_style" || row == 0 {
			d.DrawString(line)
			continue
		}
		for i, r := range line {
			d.Dot.Y = fixed.I(48 + row*43 + (i+row)%5 - 2)
			d.DrawString(string(r))
		}
	}
	if p.Style == "stamped" {
		d.Src = image.NewUniform(color.RGBA{160, 35, 35, 255})
		d.Dot = fixed.P(470, 255)
		d.DrawString("CHECKED")
		for _, edge := range []image.Rectangle{image.Rect(455, 220, 615, 223), image.Rect(455, 267, 615, 270), image.Rect(455, 220, 458, 270), image.Rect(612, 220, 615, 270)} {
			draw.Draw(page, edge, d.Src, image.Point{}, draw.Src)
		}
	}
	if p.Style == "skewed" {
		skew := image.NewRGBA(page.Bounds())
		draw.Draw(skew, skew.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		a := 3.0 * math.Pi / 180
		for y := range 400 {
			for x := range 720 {
				sx := int(math.Round(float64(x-360)*math.Cos(a) + float64(y-200)*math.Sin(a) + 360))
				sy := int(math.Round(-float64(x-360)*math.Sin(a) + float64(y-200)*math.Cos(a) + 200))
				if image.Pt(sx, sy).In(page.Bounds()) {
					skew.Set(x, y, page.At(sx, sy))
				}
			}
		}
		page = skew
	}
	var out bytes.Buffer
	err = png.Encode(&out, page)
	return out.Bytes(), err
}

// Score exact values by labelled line; a substring or duplicate/conflicting label is not a match.
func scoreOfflineFields(fields []offlineField, text string) []offlineField {
	out := append([]offlineField(nil), fields...)
	for i := range out {
		var values []string
		for _, line := range strings.Split(text, "\n") {
			key, val, found := strings.Cut(line, ":")
			if !found {
				key, val, found = strings.Cut(line, "\t")
			}
			if found && strings.EqualFold(strings.TrimSpace(key), out[i].Name) {
				values = append(values, strings.TrimSpace(val))
			}
		}
		out[i].Observed = strings.Join(values, " | ")
		out[i].Correct = len(values) == 1 && values[0] == out[i].Expected
	}
	return out
}

func TestWIS708OfflineFieldScoring(t *testing.T) {
	fields := []offlineField{{Name: "Accepted", Expected: "107"}}
	for _, tc := range []struct {
		text string
		want bool
	}{{"Accepted: 107", true}, {"accepted\t107", true}, {"Accepted: 1070", false}, {"Accepted: 108", false}, {"", false}, {"Accepted: 107\nAccepted: 108", false}} {
		if got := scoreOfflineFields(fields, tc.text)[0].Correct; got != tc.want {
			t.Errorf("%q: correct=%v, want %v", tc.text, got, tc.want)
		}
	}
}
