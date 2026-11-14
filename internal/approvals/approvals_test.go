package approvals

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

const (
	tenant = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
	site   = "1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a"
)

// Every state x event pair is checked against the specification table; anything not listed
// is illegal and leaves the state unchanged.
func TestTransitionMatrixIsExhaustive(t *testing.T) {
	want := map[State]map[Event]State{
		Pending:  {Approve: Approved, Reject: Rejected, Expire: Expired, Supersede: Superseded},
		Approved: {Consume: Consumed, Revoke: Revoked, Expire: Expired, MarkStale: Stale},
	}
	checked := 0
	for _, from := range States() {
		for _, event := range Events() {
			checked++
			got, err := Apply(from, event)
			if to, ok := want[from][event]; ok {
				if err != nil || got != to {
					t.Errorf("%s --%s--> want %s, got %s err=%v", from, event, to, got, err)
				}
				continue
			}
			if !errors.Is(err, ErrIllegalTransition) || got != from {
				t.Errorf("%s --%s-- must be illegal, got %s err=%v", from, event, got, err)
			}
		}
	}
	if checked != 56 {
		t.Fatalf("checked %d pairs, want 8 states x 7 events", checked)
	}
}

var (
	alice   = Actor{Issuer: "https://idp.example/realms/plant", Subject: "alice"}
	bob     = Actor{Issuer: "https://idp.example/realms/plant", Subject: "bob"}
	carol   = Actor{Issuer: "https://idp.example/realms/plant", Subject: "carol"}
	human   = func(a Actor) Reviewer { return Reviewer{Actor: a, Kind: KindHuman, HasProposalApprove: true} }
	byAlice = ProposalAuthority{SubmittedBy: &alice}
)

func TestReviewerEligibility(t *testing.T) {
	sameSubjectOtherIssuer := Actor{Issuer: "https://other.example", Subject: "alice"}
	upper := Actor{Issuer: alice.Issuer, Subject: "ALICE"}
	cases := map[string]struct {
		r    Reviewer
		a    ProposalAuthority
		want error
	}{
		"different human with the capability": {human(bob), byAlice, nil},
		"generated proposal, any human":       {human(alice), ProposalAuthority{}, nil},
		"owner without the grant":             {Reviewer{Actor: bob, Kind: KindHuman}, byAlice, ErrNotEligible},
		"service principal with the grant":    {Reviewer{Actor: bob, Kind: KindService, HasProposalApprove: true}, byAlice, ErrNotEligible},
		"model principal with the grant":      {Reviewer{Actor: bob, Kind: KindModel, HasProposalApprove: true}, byAlice, ErrNotEligible},
		"unknown principal kind":              {Reviewer{Actor: bob, HasProposalApprove: true}, byAlice, ErrNotEligible},
		"submitter approving own":             {human(alice), byAlice, ErrNotEligible},
		"editor approving own edit":           {human(bob), ProposalAuthority{SubmittedBy: &alice, EditedBy: []Actor{carol, bob}}, ErrNotEligible},
		"same reviewer counted twice":         {human(bob), ProposalAuthority{SubmittedBy: &alice, PriorReviewers: []Actor{bob}}, ErrNotEligible},
		"second distinct reviewer":            {human(carol), ProposalAuthority{SubmittedBy: &alice, PriorReviewers: []Actor{bob}}, nil},
		"anonymous":                           {Reviewer{Kind: KindHuman, HasProposalApprove: true}, byAlice, ErrNotEligible},
		"padded subject":                      {human(Actor{Issuer: alice.Issuer, Subject: " bob"}), byAlice, ErrNotEligible},
		"invalid UTF-8 subject":               {human(Actor{Issuer: alice.Issuer, Subject: "b\xffb"}), byAlice, ErrNotEligible},
		// OIDC subjects are case-sensitive and scoped by issuer: these are different people.
		"same subject at another issuer": {human(sameSubjectOtherIssuer), byAlice, nil},
		"subject differing only in case": {human(upper), byAlice, nil},
	}
	for name, c := range cases {
		if err := CheckReviewer(c.r, c.a); !errors.Is(err, c.want) && err != c.want {
			t.Errorf("%s: got %v want %v", name, err, c.want)
		}
	}
}

func TestWindowProfile(t *testing.T) {
	expires, err := ExpiresAt(t0, WindowProfile{})
	if err != nil || expires.Sub(t0) != ApprovalTTL {
		t.Fatalf("default window = %s err=%v", expires.Sub(t0), err)
	}
	cases := map[string]struct {
		p  WindowProfile
		ok bool
	}{
		"customer tightens to 10m":        {WindowProfile{TTL: 10 * time.Minute}, true},
		"exactly 30m uncommissioned":      {WindowProfile{TTL: 30 * time.Minute}, true},
		"31m without a commissioned flag": {WindowProfile{TTL: 31 * time.Minute}, false},
		"2h commissioned":                 {WindowProfile{TTL: 2 * time.Hour, Commissioned: true}, true},
		"24h commissioned":                {WindowProfile{TTL: 24 * time.Hour, Commissioned: true}, true},
		"24h and a second commissioned":   {WindowProfile{TTL: 24*time.Hour + time.Second, Commissioned: true}, false},
		"negative":                        {WindowProfile{TTL: -time.Minute}, false},
		"sub-second":                      {WindowProfile{TTL: 1500 * time.Millisecond}, false},
	}
	for name, c := range cases {
		_, err := ExpiresAt(t0, c.p)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v want ok=%v", name, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrInvalidWindow) {
			t.Errorf("%s: err=%v is not ErrInvalidWindow", name, err)
		}
	}
	// Expiry is bound to creation: neither a shorter nor a longer window than the profile passes.
	for _, end := range []time.Time{t0.Add(29 * time.Minute), t0.Add(31 * time.Minute), t0} {
		if err := ValidateWindow(t0, end, WindowProfile{}); !errors.Is(err, ErrInvalidWindow) {
			t.Errorf("window ending %s accepted: %v", end, err)
		}
	}
	if err := ValidateWindow(t0, t0.Add(30*time.Minute), WindowProfile{}); err != nil {
		t.Fatal(err)
	}
	// The window is half-open: the exact expiry instant is already closed.
	if err := CheckWindow(expires.Add(-time.Microsecond), expires); err != nil {
		t.Fatalf("last instant refused: %v", err)
	}
	if err := CheckWindow(expires, expires); !errors.Is(err, ErrExpired) {
		t.Fatalf("expiry instant accepted: %v", err)
	}
}

func basePayload() Payload {
	expires, _ := ExpiresAt(t0, WindowProfile{})
	return Payload{
		ActionKind: "maintenance_investigation.create", Template: "inspect_report_only",
		ProposalID: "12121212-1212-4212-8212-121212121212", ProposalRevision: 1,
		AssetID: "22222222-2222-4222-8222-222222222222", LineID: "33333333-3333-4333-8333-333333333333",
		OrderID: "abcdefab-cdef-4abc-8def-abcdefabcdef", AssigneeID: "44444444-4444-4444-8444-444444444444",
		Instructions: "Inspect the press and report findings. Do not change the machine.",
		DueAt:        t0.Add(2 * time.Hour),
		CaseID:       "55555555-5555-4555-8555-555555555555", CaseRevision: 3,
		ContextSnapshotID: "57575757-5757-4757-8757-575757575757", ContextRevision: 1, ContextDigest: strings.Repeat("a", 64),
		ConfigRevision: 1, GraphRevision: 2, PolicyRevision: 3,
		Prerequisites: []Ref{{Kind: "membership", ID: "58585858-5858-4858-8858-585858585858", Revision: 4}},
		Evidence: []Ref{
			{Kind: "source", ID: "59595959-5959-4959-8959-595959595959", Revision: 2},
			{Kind: "claim", ID: "60606060-6060-4060-8060-606060606060", Revision: 1},
		},
		IssuedAt: t0, ExpiresAt: expires, GeneratedByRun: "66666666-6666-4666-8666-666666666666",
	}
}

func digest(t *testing.T, p Payload) string {
	t.Helper()
	d, err := Digest(tenant, site, p, WindowProfile{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The committed canonical fixture is the contract shared by the API, runtime and executor.
// Regenerate deliberately with UPDATE_APPROVAL_FIXTURE=1 and review the diff.
func TestCanonicalFixture(t *testing.T) {
	encoded, d, err := Canonical(tenant, site, basePayload(), WindowProfile{})
	if err != nil {
		t.Fatal(err)
	}
	const path = "testdata/approved_action_v1.json"
	if os.Getenv("UPDATE_APPROVAL_FIXTURE") == "1" {
		if err = os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(encoded)+"\n" {
		t.Fatalf("canonical bytes drifted from %s:\n%s", path, encoded)
	}
	const wantDigest = "8259c1ba221703483cf9a8583186a940afff43276caa1173035a519145acf324"
	if d != wantDigest {
		t.Fatalf("digest %s, want %s", d, wantDigest)
	}
}

// Same instants in another zone, refs in another order: identical digest.
func TestDigestIsStableAcrossSpellings(t *testing.T) {
	base := digest(t, basePayload())
	ist := time.FixedZone("IST", 5*3600+1800)
	p := basePayload()
	p.IssuedAt, p.ExpiresAt, p.DueAt = p.IssuedAt.In(ist), p.ExpiresAt.In(ist), p.DueAt.In(ist)
	p.Evidence = []Ref{p.Evidence[1], p.Evidence[0]}
	if got := digest(t, p); got != base {
		t.Fatalf("same payload, different digest: %s vs %s", got, base)
	}
	// A microsecond-precision time survives a PostgreSQL round trip unchanged.
	p = basePayload()
	p.IssuedAt = t0.Add(123456 * time.Microsecond)
	p.ExpiresAt, _ = ExpiresAt(p.IssuedAt, WindowProfile{})
	roundTripped := p
	roundTripped.IssuedAt, roundTripped.ExpiresAt = p.IssuedAt.Truncate(time.Microsecond), p.ExpiresAt.Truncate(time.Microsecond)
	if digest(t, p) != digest(t, roundTripped) {
		t.Fatal("microsecond time changed after truncation")
	}
}

// Every Payload field is bound: changing any one of them changes the digest or is rejected.
// The test walks the struct by reflection, so a new field without coverage fails here.
func TestEveryPayloadFieldIsBound(t *testing.T) {
	base := digest(t, basePayload())
	typ := reflect.TypeOf(Payload{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		p := basePayload()
		v := reflect.ValueOf(&p).Elem().Field(i)
		switch field.Name {
		case "ActionKind", "Template":
			v.SetString(v.String() + "_other")
			if _, err := Digest(tenant, site, p, WindowProfile{}); !errors.Is(err, ErrInvalidPayload) {
				t.Errorf("%s change was not rejected: %v", field.Name, err)
			}
			continue
		case "IssuedAt":
			// Moving creation must move expiry with it, or the window check rejects it.
			p.IssuedAt = p.IssuedAt.Add(time.Second)
			p.ExpiresAt = p.ExpiresAt.Add(time.Second)
		case "ExpiresAt":
			p.ExpiresAt = p.ExpiresAt.Add(time.Second)
			if _, err := Digest(tenant, site, p, WindowProfile{}); !errors.Is(err, ErrInvalidWindow) {
				t.Errorf("an expiry moved without issued_at was not rejected: %v", err)
			}
			continue
		default:
			switch v.Kind() {
			case reflect.String:
				if canonicalUUID(v.String()) {
					v.SetString("77777777-7777-4777-8777-777777777777")
				} else if field.Name == "ContextDigest" {
					v.SetString(strings.Repeat("b", 64))
				} else {
					v.SetString(v.String() + " Also photograph the guard.")
				}
			case reflect.Int64:
				v.SetInt(v.Int() + 1)
			case reflect.Slice:
				refs := v.Interface().([]Ref)
				refs[0].Revision++
			case reflect.Struct: // time.Time
				v.Set(reflect.ValueOf(v.Interface().(time.Time).Add(time.Second)))
			default:
				t.Fatalf("field %s has an unhandled kind %s", field.Name, v.Kind())
			}
		}
		d, err := Digest(tenant, site, p, WindowProfile{})
		if err != nil || d == base {
			t.Errorf("%s did not change the digest (err=%v)", field.Name, err)
		}
	}
	if d, err := Digest("2a2a2a2a-2a2a-4a2a-8a2a-2a2a2a2a2a2a", site, basePayload(), WindowProfile{}); err != nil || d == base {
		t.Errorf("digest does not bind the tenant (err=%v)", err)
	}
	if d, err := Digest(tenant, "2a2a2a2a-2a2a-4a2a-8a2a-2a2a2a2a2a2a", basePayload(), WindowProfile{}); err != nil || d == base {
		t.Errorf("digest does not bind the site (err=%v)", err)
	}
}

func TestPayloadValidation(t *testing.T) {
	devanagari := strings.Repeat("क", MaxInstructionRunes)
	cases := map[string]struct {
		change func(*Payload)
		want   error // nil means the payload is valid
	}{
		"4000 multi-byte characters":       {func(p *Payload) { p.Instructions = devanagari }, nil},
		"4001 characters":                  {func(p *Payload) { p.Instructions = devanagari + "क" }, ErrInvalidPayload},
		"empty instructions":               {func(p *Payload) { p.Instructions = "" }, ErrInvalidPayload},
		"blank instructions":               {func(p *Payload) { p.Instructions = " \n\t" }, ErrInvalidPayload},
		"invalid UTF-8":                    {func(p *Payload) { p.Instructions = "abc\xff" }, ErrInvalidPayload},
		"control character":                {func(p *Payload) { p.Instructions = "abc\x00def" }, ErrInvalidPayload},
		"newline and tab allowed":          {func(p *Payload) { p.Instructions = "Step 1\n\tStep 2" }, nil},
		"uppercase UUID":                   {func(p *Payload) { p.OrderID = strings.ToUpper(p.OrderID) }, ErrInvalidPayload},
		"empty assignee":                   {func(p *Payload) { p.AssigneeID = "" }, ErrInvalidPayload},
		"nil case":                         {func(p *Payload) { p.CaseID = "00000000-0000-0000-0000-000000000000" }, ErrInvalidPayload},
		"zero proposal revision":           {func(p *Payload) { p.ProposalRevision = 0 }, ErrInvalidPayload},
		"uppercase context digest":         {func(p *Payload) { p.ContextDigest = strings.Repeat("A", 64) }, ErrInvalidPayload},
		"zero due date":                    {func(p *Payload) { p.DueAt = time.Time{} }, ErrInvalidPayload},
		"due at issue time":                {func(p *Payload) { p.DueAt = p.IssuedAt }, ErrInvalidPayload},
		"nanosecond issue time":            {func(p *Payload) { p.IssuedAt = p.IssuedAt.Add(1); p.ExpiresAt = p.ExpiresAt.Add(1) }, ErrInvalidPayload},
		"no evidence":                      {func(p *Payload) { p.Evidence = nil }, ErrInvalidPayload},
		"no prerequisites":                 {func(p *Payload) { p.Prerequisites = nil }, nil},
		"duplicate evidence":               {func(p *Payload) { p.Evidence = append(p.Evidence, p.Evidence[0]) }, ErrInvalidPayload},
		"evidence revision zero":           {func(p *Payload) { p.Evidence[0].Revision = 0 }, ErrInvalidPayload},
		"evidence kind with capitals":      {func(p *Payload) { p.Evidence[0].Kind = "Source" }, ErrInvalidPayload},
		"template outside v1":              {func(p *Payload) { p.Template = "stop_machine" }, ErrInvalidPayload},
		"expiry beyond the profile window": {func(p *Payload) { p.ExpiresAt = p.IssuedAt.Add(2 * time.Hour) }, ErrInvalidWindow},
	}
	for name, c := range cases {
		p := basePayload()
		c.change(&p)
		_, err := Digest(tenant, site, p, WindowProfile{})
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err=%v want %v", name, err, c.want)
		}
	}
	// A commissioned 2h profile accepts the 2h window that the default profile rejects.
	p := basePayload()
	p.ExpiresAt = p.IssuedAt.Add(2 * time.Hour)
	if _, err := Digest(tenant, site, p, WindowProfile{TTL: 2 * time.Hour, Commissioned: true}); err != nil {
		t.Fatalf("commissioned window: %v", err)
	}
	// Normalizing does not mutate the caller's refs.
	p = basePayload()
	before := append([]Ref(nil), p.Evidence...)
	digest(t, p)
	if !reflect.DeepEqual(before, p.Evidence) {
		t.Fatal("Digest reordered the caller's evidence slice")
	}
}

func pending() Proposal {
	expires, _ := ExpiresAt(t0, WindowProfile{})
	return Proposal{State: Pending, Revision: 2, ExpiresAt: expires, Authority: byAlice}
}

func TestDecide(t *testing.T) {
	approve := Decision{Kind: DecideApprove, ExpectedRevision: 2, Reason: "Evidence supports an inspection."}
	now := t0.Add(10 * time.Minute)
	if got, err := Decide(now, pending(), human(bob), approve); err != nil || got != Approved {
		t.Fatalf("approve: %s %v", got, err)
	}
	reject := approve
	reject.Kind = DecideReject
	if got, err := Decide(now, pending(), human(bob), reject); err != nil || got != Rejected {
		t.Fatalf("reject: %s %v", got, err)
	}
	long := approve
	long.Reason = strings.Repeat("r", MaxReasonRunes)
	if _, err := Decide(now, pending(), human(bob), long); err != nil {
		t.Fatalf("2000-character reason: %v", err)
	}

	approved := pending()
	approved.State = Approved
	expires := pending().ExpiresAt
	cases := map[string]struct {
		now  time.Time
		p    Proposal
		r    Reviewer
		d    Decision
		want error
	}{
		"unknown decision":   {now, pending(), human(bob), Decision{Kind: "maybe", ExpectedRevision: 2, Reason: "x"}, ErrInvalidDecision},
		"missing reason":     {now, pending(), human(bob), Decision{Kind: DecideApprove, ExpectedRevision: 2}, ErrInvalidDecision},
		"reason too long":    {now, pending(), human(bob), Decision{Kind: DecideApprove, ExpectedRevision: 2, Reason: strings.Repeat("r", MaxReasonRunes+1)}, ErrInvalidDecision},
		"revision zero":      {now, pending(), human(bob), Decision{Kind: DecideApprove, Reason: "x"}, ErrInvalidDecision},
		"stale revision":     {now, pending(), human(bob), Decision{Kind: DecideApprove, ExpectedRevision: 1, Reason: "x"}, ErrRevisionConflict},
		"already approved":   {now, approved, human(bob), approve, ErrIllegalTransition},
		"at expiry":          {expires, pending(), human(bob), approve, ErrExpired},
		"after expiry":       {expires.Add(time.Hour), pending(), human(bob), reject, ErrExpired},
		"submitter approves": {now, pending(), human(alice), approve, ErrNotEligible},
		"submitter rejects":  {now, pending(), human(alice), reject, ErrNotEligible},
		"service approves":   {now, pending(), Reviewer{Actor: bob, Kind: KindService, HasProposalApprove: true}, approve, ErrNotEligible},
		// Precedence: a malformed request is 422 even when it is also stale and expired.
		"malformed beats stale and expired": {expires, approved, human(alice), Decision{Kind: "x", ExpectedRevision: 1}, ErrInvalidDecision},
		"stale beats expired":               {expires, approved, human(alice), Decision{Kind: DecideApprove, ExpectedRevision: 1, Reason: "x"}, ErrRevisionConflict},
	}
	for name, c := range cases {
		got, err := Decide(c.now, c.p, c.r, c.d)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err=%v want %v", name, err, c.want)
		}
		if got != c.p.State {
			t.Errorf("%s: state changed to %s on refusal", name, got)
		}
	}
}

func TestCheckDispatch(t *testing.T) {
	p := pending()
	p.State = Approved
	now := t0.Add(20 * time.Minute)
	d := digest(t, basePayload())
	if got, err := CheckDispatch(now, p, []Reviewer{human(bob)}, d, d); err != nil || got != Consumed {
		t.Fatalf("dispatch: %s %v", got, err)
	}
	revokedGrant := human(bob)
	revokedGrant.HasProposalApprove = false
	edited := p
	edited.Authority = ProposalAuthority{SubmittedBy: &alice, EditedBy: []Actor{bob}}
	cases := map[string]struct {
		now       time.Time
		p         Proposal
		approvers []Reviewer
		stored    string
		want      error
	}{
		"not approved":                    {now, pending(), []Reviewer{human(bob)}, d, ErrIllegalTransition},
		"expired before dispatch":         {p.ExpiresAt, p, []Reviewer{human(bob)}, d, ErrExpired},
		"payload changed":                 {now, p, []Reviewer{human(bob)}, strings.Repeat("0", 64), ErrDigestMismatch},
		"no stored digest":                {now, p, []Reviewer{human(bob)}, "", ErrDigestMismatch},
		"capability revoked since":        {now, p, []Reviewer{revokedGrant}, d, ErrNotEligible},
		"approver later edited":           {now, edited, []Reviewer{human(bob)}, d, ErrNotEligible},
		"no approvers":                    {now, p, nil, d, ErrNotEligible},
		"one of two approvers ineligible": {now, p, []Reviewer{human(carol), revokedGrant}, d, ErrNotEligible},
	}
	for name, c := range cases {
		got, err := CheckDispatch(c.now, c.p, c.approvers, c.stored, d)
		if !errors.Is(err, c.want) || got != c.p.State {
			t.Errorf("%s: got %s err=%v want %v", name, got, err, c.want)
		}
	}
}
