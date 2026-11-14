package onboarding

import (
	"bytes"
	"encoding/xml"
	"errors"
)

// maxXMLDepth is the deepest element nesting an Office part may have. encoding/xml's Decoder keeps
// one stack entry for every open element, so without a bound a few kilobytes of compressed
// "<a><a><a>…" made the API process allocate about 35 times the inflated part before the file was
// refused (WIS-694, security audit run 1). Word and Excel parts nest well under this.
const maxXMLDepth = 64

// errXMLTooDeep is returned once a part nests deeper than maxXMLDepth; callers refuse the file.
var errXMLTooDeep = errors.New("onboarding: XML nested too deeply")

// xmlTokens reads an Office part's tokens with the nesting depth bounded. Every token loop over
// an uploaded part uses it instead of a bare xml.Decoder, Skip included: Decoder.Skip walks the
// skipped element with the decoder's own Token, which would grow the stack unbounded.
type xmlTokens struct {
	dec   *xml.Decoder
	depth int
}

func newXMLTokens(data []byte) *xmlTokens {
	return &xmlTokens{dec: xml.NewDecoder(bytes.NewReader(data))}
}

// Token returns the next token, or errXMLTooDeep when an element would open past maxXMLDepth.
func (x *xmlTokens) Token() (xml.Token, error) {
	tok, err := x.dec.Token()
	if err != nil {
		return tok, err
	}
	switch tok.(type) {
	case xml.StartElement:
		x.depth++
		if x.depth > maxXMLDepth {
			return nil, errXMLTooDeep
		}
	case xml.EndElement:
		x.depth--
	}
	return tok, nil
}

// Skip consumes the element whose start element was just returned, through Token, so the depth
// bound holds inside it too.
func (x *xmlTokens) Skip() error {
	for target := x.depth - 1; x.depth > target; {
		if _, err := x.Token(); err != nil {
			return err
		}
	}
	return nil
}
