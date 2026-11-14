package onboarding

import (
	"bytes"
	"image"
	"image/png"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"golang.org/x/image/ccitt"
)

// maxScanSide is the largest width or height of a page image, as the scan jobs table allows it.
const maxScanSide = 20000

// scanPixelsAllowed reports that a page image of w × h pixels is within the photo limits. It is
// checked from the image dictionary, before anything decodes the page (WIS-645): a small stream
// can declare a huge canvas.
func scanPixelsAllowed(w, h int) bool {
	return w >= 1 && h >= 1 && w <= maxScanSide && h <= maxScanSide && int64(w)*int64(h) <= agent.MaxImagePixels
}

// ccittParams are the CCITTFaxDecode parameters a plain fax page may carry. The only ones accepted
// are those whose decoded picture is the picture a viewer shows: BlackIs1 true, a Decode array or an
// image mask would show the decoded bits inverted or as a stencil, and EndOfLine markers or K > 0
// (two-dimensional Group 3) are not supported by the decoder. Anything else refuses the page as
// unsupported; nothing is guessed.
type ccittParams struct {
	k, columns, rows int
	align            bool
}

var ccittParamKeys = map[string]bool{"K": true, "Columns": true, "Rows": true, "BlackIs1": true, "EncodedByteAlign": true, "EndOfBlock": true}

func faxParams(ctx *model.Context, sd types.StreamDict, width, height int) (ccittParams, bool) {
	p := ccittParams{columns: 1728} // the PDF default
	raw, found := sd.Dict["DecodeParms"]
	if !found {
		return p, p.columns == width
	}
	d, err := ctx.DereferenceDict(raw)
	if err != nil || d == nil {
		return p, false
	}
	for k := range d {
		if !ccittParamKeys[k] {
			return p, false
		}
	}
	if v := d.IntEntry("K"); v != nil {
		p.k = *v
	}
	if v := d.IntEntry("Columns"); v != nil {
		p.columns = *v
	}
	if v := d.IntEntry("Rows"); v != nil {
		p.rows = *v
	}
	if v := d.BooleanEntry("BlackIs1"); v != nil && *v {
		return p, false
	}
	if v := d.BooleanEntry("EndOfBlock"); v != nil && !*v {
		return p, false
	}
	if v := d.BooleanEntry("EncodedByteAlign"); v != nil {
		p.align = *v
	}
	return p, p.k <= 0 && p.columns == width && (p.rows == 0 || p.rows == height)
}

// faxPage decodes the fax-encoded (CCITT Group 3 one-dimensional or Group 4) page image object
// objNr of w × h pixels into a PNG (WIS-645). The caller has checked the pixel limits from the
// dictionary, so the decoded picture is exactly w × h bytes and cannot grow; the encoded stream is
// read as stored, never through a decoder without a bound. ok is false for any parameter outside
// ccittParams, a decode error or a PNG over agent.MaxImageBytes (large reports which).
func faxPage(ctx *model.Context, objNr, w, h int) (data []byte, ok, large bool) {
	obj, err := ctx.FindObject(objNr)
	if err != nil {
		return nil, false, false
	}
	sd, isStream := obj.(types.StreamDict)
	if !isStream || len(sd.Raw) == 0 {
		return nil, false, false
	}
	if f, _ := sd.Dict["Filter"].(types.Name); f != "CCITTFaxDecode" {
		return nil, false, false // a filter chain or array is not a plain fax page
	}
	p, valid := faxParams(ctx, sd, w, h)
	if !valid {
		return nil, false, false
	}
	sf := ccitt.Group4
	if p.k == 0 {
		sf = ccitt.Group3
	}
	gray := image.NewGray(image.Rect(0, 0, w, h))
	if ccitt.DecodeIntoGray(gray, bytes.NewReader(sd.Raw), ccitt.MSB, sf, &ccitt.Options{Align: p.align}) != nil {
		return nil, false, false
	}
	var out bytes.Buffer
	if png.Encode(&out, gray) != nil {
		return nil, false, false
	}
	if out.Len() > agent.MaxImageBytes {
		return nil, false, true
	}
	return out.Bytes(), true, false
}
