package onboarding

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"path"
	"strings"
)

// The Office Open XML namespaces whose elements carry text.
const (
	wordNS  = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	sheetNS = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	relNS   = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	mcNS    = "http://schemas.openxmlformats.org/markup-compatibility/2006"
)

// maxSheetColumns is the spreadsheet column limit (XFD); a cell reference past it is unreadable.
const maxSheetColumns = 16384

var cellBreaks = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "\t", " ")

// partBudget is the decompression budget shared by every part read from one package, so reading
// many parts (or one part many times) cannot inflate more than MaxDecompressedBytes in total.
type partBudget struct{ left int64 }

// readPart returns one package part, inflated within the shared decompression budget. A missing
// part is nil; the caller decides whether it is required.
func readPart(zr *zip.Reader, name string, b *partBudget) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		if f.UncompressedSize64 > uint64(b.left) {
			return nil, ErrTooLarge
		}
		rc, err := f.Open()
		if err != nil {
			return nil, ErrUnreadableSource
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, b.left+1))
		if err != nil {
			return nil, ErrUnreadableSource
		}
		if int64(len(data)) > b.left {
			return nil, ErrTooLarge
		}
		b.left -= int64(len(data))
		return data, nil
	}
	return nil, nil
}

func openPackage(data []byte, l Limits) (*zip.Reader, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, ErrUnreadableSource
	}
	if len(zr.File) > l.MaxZipEntries {
		return nil, ErrTooLarge
	}
	return zr, nil
}

// limitedText accumulates extracted text and stops at the extracted-text bound.
type limitedText struct {
	strings.Builder
	max int64
}

func (t *limitedText) line(s string) error {
	t.WriteString(s)
	t.WriteByte('\n')
	if int64(t.Len()) > t.max {
		return ErrTooLarge
	}
	return nil
}

// maxParagraphDepth bounds paragraphs nested in text boxes; real documents nest one or two deep.
const maxParagraphDepth = 32

// extractDOCX returns the main document's paragraphs, one per line; a table row is its cells'
// text separated by tabs. Headers, footers, comments and embedded objects are not read.
func extractDOCX(data []byte, l Limits) (string, error) {
	zr, err := openPackage(data, l)
	if err != nil {
		return "", err
	}
	body, err := readPart(zr, "word/document.xml", &partBudget{left: l.MaxDecompressedBytes})
	if err != nil {
		return "", err
	}
	if body == nil {
		return "", ErrUnsupportedMedia
	}
	out := limitedText{max: l.MaxExtractedText}
	// A paragraph inside a text box sits inside its outer paragraph. Each open paragraph has its
	// own buffer on a stack, so opening or closing one never copies the outer text (WIS-613 audit:
	// copying it out and back made many empty nested paragraphs cost a copy of the open text each).
	stack := []*strings.Builder{{}}
	para := stack[0]
	var cell []string
	var row []string
	tables := 0
	dec := newXMLTokens(body)
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", ErrUnreadableSource
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space == mcNS && t.Name.Local == "Fallback" {
				// The fallback repeats the content of its mc:Choice for older readers.
				if dec.Skip() != nil {
					return "", ErrUnreadableSource
				}
				continue
			}
			if t.Name.Space != wordNS {
				continue
			}
			switch t.Name.Local {
			case "p":
				if len(stack) > maxParagraphDepth {
					return "", ErrUnreadableSource
				}
				para = &strings.Builder{}
				stack = append(stack, para)
			case "tbl":
				tables++
			case "t":
				inText = true
			case "tab":
				para.WriteByte('\t')
			case "br", "cr":
				para.WriteByte(' ')
			}
		case xml.EndElement:
			if t.Name.Space != wordNS {
				continue
			}
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				text := strings.TrimSpace(para.String())
				if len(stack) > 1 {
					stack = stack[:len(stack)-1]
					para = stack[len(stack)-1]
				} else {
					para.Reset()
				}
				if tables > 0 {
					// A tab or line break inside a cell must not split its row or shift its columns.
					if text = strings.TrimSpace(cellBreaks.Replace(text)); text != "" {
						cell = append(cell, text)
					}
					continue
				}
				if err := out.line(text); err != nil {
					return "", err
				}
			case "tc":
				row = append(row, strings.Join(cell, " "))
				cell = nil
			case "tr":
				if err := out.line(strings.Join(row, "\t")); err != nil {
					return "", err
				}
				row = nil
			case "tbl":
				tables--
				if tables == 0 {
					if err := out.line(""); err != nil {
						return "", err
					}
				}
			}
		case xml.CharData:
			if inText {
				para.Write(t)
				if int64(para.Len()) > l.MaxExtractedText {
					return "", ErrTooLarge
				}
			}
		}
	}
	return out.String(), nil
}

// extractXLSX returns each worksheet as "# <name>" followed by one line per row, cells separated
// by tabs in document order. Shared and inline strings are resolved; other cells are their stored
// value, so a formula gives its cached result and a date its serial number (no conversion).
func extractXLSX(data []byte, l Limits) (string, error) {
	zr, err := openPackage(data, l)
	if err != nil {
		return "", err
	}
	budget := &partBudget{left: l.MaxDecompressedBytes}
	workbook, err := readPart(zr, "xl/workbook.xml", budget)
	if err != nil {
		return "", err
	}
	rels, err := readPart(zr, "xl/_rels/workbook.xml.rels", budget)
	if err != nil {
		return "", err
	}
	if workbook == nil || rels == nil {
		return "", ErrUnsupportedMedia
	}
	var wb struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			ID   string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	var rel struct {
		Relationships []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if xml.Unmarshal(workbook, &wb) != nil || xml.Unmarshal(rels, &rel) != nil {
		return "", ErrUnreadableSource
	}
	targets := map[string]string{}
	for _, r := range rel.Relationships {
		targets[r.ID] = r.Target
	}
	shared, err := sharedStrings(zr, budget, l)
	if err != nil {
		return "", err
	}
	out := limitedText{max: l.MaxExtractedText}
	read := map[string]bool{}
	for i, sheet := range wb.Sheets {
		raw, ok := targets[sheet.ID]
		if !ok {
			return "", ErrUnreadableSource
		}
		target, ok := sheetTarget(raw)
		if !ok || read[target] {
			// Each worksheet part is read once: a sheet listed twice would repeat its work and text.
			return "", ErrUnreadableSource
		}
		read[target] = true
		part, err := readPart(zr, target, budget)
		if err != nil {
			return "", err
		}
		if part == nil {
			return "", ErrUnreadableSource
		}
		if i > 0 {
			if err = out.line(""); err != nil {
				return "", err
			}
		}
		if err = out.line("# " + strings.TrimSpace(sheet.Name)); err != nil {
			return "", err
		}
		if err = sheetRows(part, shared, &out); err != nil {
			return "", err
		}
	}
	return out.String(), nil
}

// sheetTarget resolves a workbook relationship target to its package part name. A target is
// relative to xl/ unless it starts with "/"; anything that resolves outside xl/ is refused.
func sheetTarget(raw string) (string, bool) {
	var target string
	switch {
	case strings.HasPrefix(raw, "/"):
		target = path.Clean(strings.TrimPrefix(raw, "/"))
	case strings.HasPrefix(raw, "xl/"):
		target = path.Clean(raw)
	default:
		target = path.Join("xl", raw)
	}
	return target, strings.HasPrefix(target, "xl/")
}

// maxSharedStrings bounds a workbook's shared string table (WIS-694, security audit run 1).
const maxSharedStrings = 1 << 20

// sharedStrings reads the workbook's shared string table; each entry is its text runs joined.
func sharedStrings(zr *zip.Reader, b *partBudget, l Limits) ([]string, error) {
	part, err := readPart(zr, "xl/sharedStrings.xml", b)
	if err != nil || part == nil {
		return nil, err
	}
	var out []string
	var item strings.Builder
	inText, inItem := false, false
	dec := newXMLTokens(part)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, ErrUnreadableSource
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space == sheetNS && t.Name.Local == "rPh" {
				// A phonetic guide repeats the reading of the text before it; it is not cell text.
				if dec.Skip() != nil {
					return nil, ErrUnreadableSource
				}
				continue
			}
			if t.Name.Space == sheetNS && t.Name.Local == "si" {
				inItem = true
				item.Reset()
			}
			inText = inText || (inItem && t.Name.Space == sheetNS && t.Name.Local == "t")
		case xml.EndElement:
			switch {
			case t.Name.Space == sheetNS && t.Name.Local == "t":
				inText = false
			case t.Name.Space == sheetNS && t.Name.Local == "si":
				inItem = false
				// Each entry is kept whatever its length, so the count is bounded too: 16 MiB of
				// empty "<si/>" held 200 MiB (WIS-694).
				if len(out) >= maxSharedStrings {
					return nil, ErrTooLarge
				}
				out = append(out, item.String())
			}
		case xml.CharData:
			if inText {
				item.Write(t)
				if int64(item.Len()) > l.MaxExtractedText {
					return nil, ErrTooLarge
				}
			}
		}
	}
}

// sheetRows writes one line per non-empty row of a worksheet part. Cells are placed by their
// reference, so an empty cell keeps its column; a reference that goes backwards is unreadable. The
// row is measured as it is built, so a row of many large shared strings is refused before it is
// joined rather than after.
//
// A row is written as it is read: each cell with text goes after one tab per column since the
// previous cell with text. Empty cells are written only as the tabs before a later cell with
// text, so trailing ones cost nothing, and padding to a reference such as "XFD1" is the tab bytes
// themselves, charged to the output budget, not 16383 appends no budget counted (WIS-694).
func sheetRows(part []byte, shared []string, out *limitedText) error {
	var row strings.Builder
	width, last := 0, -1 // width: the row's columns so far, empty ones included; last: the column of the last cell written
	var value strings.Builder
	cellType, cellRef, inValue := "", "", false
	add := func(col int, text string) error {
		tabs := col - last - 1
		if last >= 0 {
			tabs++
		}
		if int64(out.Len())+int64(row.Len()+tabs+len(text)+1) > out.max {
			return ErrTooLarge
		}
		row.WriteString(strings.Repeat("\t", tabs))
		row.WriteString(text)
		last = col
		return nil
	}
	dec := newXMLTokens(part)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return ErrUnreadableSource
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space != sheetNS {
				continue
			}
			if t.Name.Local == "rPh" {
				if dec.Skip() != nil {
					return ErrUnreadableSource
				}
				continue
			}
			switch t.Name.Local {
			case "c":
				cellType, cellRef = "", ""
				value.Reset()
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "t":
						cellType = a.Value
					case "r":
						cellRef = a.Value
					}
				}
			case "v", "t":
				inValue = true
			}
		case xml.EndElement:
			if t.Name.Space != sheetNS {
				continue
			}
			switch t.Name.Local {
			case "v", "t":
				inValue = false
			case "c":
				col := width
				if cellRef != "" {
					c, ok := cellColumn(cellRef)
					if !ok || c < width {
						return ErrUnreadableSource
					}
					col = c
				}
				text := strings.TrimSpace(value.String())
				if cellType == "s" {
					i, ok := cellIndex(text)
					if !ok || i >= len(shared) {
						return ErrUnreadableSource
					}
					text = strings.TrimSpace(shared[i])
				}
				// A line break or tab inside a cell must not split its row or shift its columns.
				text = cellBreaks.Replace(text)
				width = col + 1
				if text == "" {
					continue
				}
				if err := add(col, text); err != nil {
					return err
				}
			case "row":
				if row.Len() > 0 {
					if err := out.line(row.String()); err != nil {
						return err
					}
				}
				row.Reset()
				width, last = 0, -1
			}
		case xml.CharData:
			if inValue {
				value.Write(t)
				if int64(value.Len()) > out.max {
					return ErrTooLarge
				}
			}
		}
	}
}

// cellColumn returns the zero-based column of a cell reference such as "C12".
func cellColumn(ref string) (int, bool) {
	col, i := 0, 0
	for ; i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z'; i++ {
		col = col*26 + int(ref[i]-'A') + 1
		if col > maxSheetColumns {
			return 0, false
		}
	}
	if i == 0 {
		return 0, false
	}
	if _, ok := cellIndex(ref[i:]); !ok {
		return 0, false
	}
	return col - 1, true
}

func cellIndex(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
