package evidence

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestInputBoundaryBeforeDatabase(t *testing.T) {
	s := New(nil)
	ctx := context.Background()
	id := uuid.NewString()
	for _, in := range []Input{
		{SourceID: id, Kind: "approved_document", Title: "title", Text: "text"},
		{SourceID: id, Kind: "manual", Title: "title", Text: string([]byte{0xff})},
		{SourceID: id, Kind: "manual", Title: "title", Text: "a\x00b"},
		{SourceID: id, Kind: "manual", Title: "title", Text: strings.Repeat("x", MaxTextBytes+1)},
		{SourceID: id, Kind: "manual", Title: strings.Repeat("é", 201), Text: "text"},
		{SourceID: "not-a-uuid", Kind: "manual", Title: "title", Text: "text"},
		// WIS-270: closed authority_class enum and canonical UUID group ids.
		{SourceID: id, Kind: "document", Title: "title", Text: "text", AuthorityClass: "designated"},
		{SourceID: id, Kind: "event", Title: "title", Text: "text", IndependenceGroupID: "not-a-uuid"},
		{SourceID: id, Kind: "event", Title: "title", Text: "text", IndependenceGroupID: strings.ToUpper(id)},
	} {
		if _, err := s.CreateOrRevise(ctx, Actor{}, Scope{}, in, "key", 0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("boundary error=%v", err)
		}
	}
	if _, err := s.CreateSpan(ctx, Actor{}, Scope{}, SpanInput{id, 1, 0, 4097}, "key", 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbounded span=%v", err)
	}
}

// WIS-613: a document and its spans are refused as a whole before any transaction starts. The scope
// is valid and the actor empty, so a request that passed validation would fail with ErrDenied.
func TestDocumentBoundaryBeforeDatabase(t *testing.T) {
	s := New(nil)
	sc := Scope{TenantID: uuid.NewString(), SiteID: uuid.NewString()}
	sha := strings.Repeat("ab", 32)
	valid := DocumentInput{Title: "Receiving SOP", Text: "Coil checked at Gate 2", OriginalSHA256: sha, Spans: []Range{{0, 4}}}
	tooMany := make([]Range, MaxDocumentSpans+1)
	for i := range tooMany {
		tooMany[i] = Range{0, 1}
	}
	for name, mutate := range map[string]func(*DocumentInput){
		"empty title":           func(in *DocumentInput) { in.Title = "" },
		"title over 200 runes":  func(in *DocumentInput) { in.Title = strings.Repeat("é", 201) },
		"empty text":            func(in *DocumentInput) { in.Text = "" },
		"text over source size": func(in *DocumentInput) { in.Text = strings.Repeat("x", MaxTextBytes+1) },
		"invalid utf8 text":     func(in *DocumentInput) { in.Text = string([]byte{0xff}) },
		"upper-case digest":     func(in *DocumentInput) { in.OriginalSHA256 = strings.ToUpper(sha) },
		"short digest":          func(in *DocumentInput) { in.OriginalSHA256 = sha[:62] },
		"no spans":              func(in *DocumentInput) { in.Spans = nil },
		"too many spans":        func(in *DocumentInput) { in.Spans = tooMany },
		"empty span":            func(in *DocumentInput) { in.Spans = []Range{{3, 3}} },
		"negative start":        func(in *DocumentInput) { in.Spans = []Range{{-1, 2}} },
		"span past the text":    func(in *DocumentInput) { in.Spans = []Range{{0, 23}} },
		"span over 4096 runes":  func(in *DocumentInput) { in.Text = strings.Repeat("x", 5000); in.Spans = []Range{{0, 4097}} },
	} {
		in := valid
		mutate(&in)
		if _, err := s.CreateDocument(context.Background(), Actor{}, sc, in, "key"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := s.CreateDocument(context.Background(), Actor{}, sc, valid, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("missing key: %v, want ErrInvalid", err)
	}
	if _, err := s.CreateDocument(context.Background(), Actor{}, sc, valid, "key"); !errors.Is(err, ErrDenied) {
		t.Errorf("valid input: %v, want ErrDenied from the actor check", err)
	}
}

// WIS-613: the idempotency key binds the original file, not only the text extracted from it.
func TestDocumentFingerprintBindsTheOriginal(t *testing.T) {
	in := DocumentInput{Title: "t", Text: "x", OriginalSHA256: strings.Repeat("ab", 32), Spans: []Range{{0, 1}}}
	h := fingerprint("document", in, 0)
	for name, mutate := range map[string]func(*DocumentInput){
		"original": func(c *DocumentInput) { c.OriginalSHA256 = strings.Repeat("cd", 32) },
		"title":    func(c *DocumentInput) { c.Title = "u" },
		"text":     func(c *DocumentInput) { c.Text = "y" },
		"spans":    func(c *DocumentInput) { c.Spans = []Range{{0, 1}, {0, 1}} },
	} {
		c := in
		mutate(&c)
		if fingerprint("document", c, 0) == h {
			t.Errorf("%s is not bound into the document fingerprint", name)
		}
	}
}

func TestFingerprintBindsBodyOperationAndPrecondition(t *testing.T) {
	in := Input{SourceID: uuid.NewString(), Kind: "manual", Title: "t", Text: "é😀"}
	h := fingerprint("source", in, 0)
	// WIS-270 added optional fields; receipts stored before 039 must still replay.
	legacy := struct {
		SourceID string `json:"source_id"`
		Kind     string `json:"kind"`
		Title    string `json:"title"`
		Text     string `json:"text"`
	}{in.SourceID, in.Kind, in.Title, in.Text}
	if h != fingerprint("source", legacy, 0) {
		t.Fatal("pre-WIS-270 source receipts would no longer replay")
	}
	grouped := in
	grouped.IndependenceGroupID = uuid.NewString()
	if h == fingerprint("source", grouped, 0) {
		t.Fatal("independence group not bound into the idempotency fingerprint")
	}
	if h != fingerprint("source", in, 0) || h == fingerprint("source", in, 1) || h == fingerprint("span", in, 0) {
		t.Fatal("unbound fingerprint")
	}
	in.Text = "e😀"
	if h == fingerprint("source", in, 0) {
		t.Fatal("content not bound")
	}
}
