package workstation

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func recordBody(kind RecordKind) StationRecordBodyV1 {
	return StationRecordBodyV1{SchemaVersion: 1, Kind: kind, WorkstationID: "6f1c7a52-58a3-4c1e-9d4e-0d6f3c1b2a90", JobOrderID: "0b7e5b2c-3f6a-4f0e-8a51-2c9d7e4f1a33", JobOrderRevision: 1, EventAt: "2026-10-19T09:00:00+02:00"}
}

func ptr[T any](v T) *T { return &v }

// TestStationRecordValidate pins the kind-specific contract: each kind takes exactly its
// members, values are bounded, and a release claim is refused with its own code.
func TestStationRecordValidate(t *testing.T) {
	with := func(kind RecordKind, edit func(*StationRecordBodyV1)) StationRecordBodyV1 {
		b := recordBody(kind)
		edit(&b)
		return b
	}
	valid := []StationRecordBodyV1{
		recordBody(KindAcknowledged), recordBody(KindStarted), recordBody(KindResumed),
		with(KindBlocked, func(b *StationRecordBodyV1) { b.Comment = "Waiting for coil" }),
		with(KindQuantity, func(b *StationRecordBodyV1) { b.GoodQuantity, b.RejectQuantity = ptr(int64(40)), ptr(int64(0)) }),
		with(KindCycleTime, func(b *StationRecordBodyV1) { b.CycleTimeSeconds = "2.75" }),
		with(KindQualityMeasurement, func(b *StationRecordBodyV1) { b.Values = []EntryValueV1{{Field: "bore", Value: "10.01", Unit: "mm"}} }),
		with(KindQualityDisposition, func(b *StationRecordBodyV1) { b.Disposition, b.AuthorizesRelease = DispositionHold, ptr(false) }),
		with(KindMaterialException, func(b *StationRecordBodyV1) { b.MaterialCode, b.MaterialIssue = "COIL-7", MaterialShortage }),
		with(KindCycleTime, func(b *StationRecordBodyV1) { b.CycleTimeSeconds, b.CorrectsRecordID = "2.5", uuid.NewString() }),
	}
	for i, b := range valid {
		if e := b.Validate(); e != nil {
			t.Errorf("valid[%d] %s: %v", i, b.Kind, e)
		}
	}
	for name, tc := range map[string]struct {
		body       StationRecordBodyV1
		code, path string
	}{
		"release claimed":          {with(KindQualityDisposition, func(b *StationRecordBodyV1) { b.Disposition, b.AuthorizesRelease = DispositionAccept, ptr(true) }), CodeReleaseForbidden, "authorizes_release"},
		"release on quantity":      {with(KindQuantity, func(b *StationRecordBodyV1) { b.AuthorizesRelease = ptr(true) }), CodeReleaseForbidden, "authorizes_release"},
		"disposition no release":   {with(KindQualityDisposition, func(b *StationRecordBodyV1) { b.Disposition = DispositionHold }), CodeInvalidStationRecord, "authorizes_release"},
		"release member elsewhere": {with(KindNote, func(b *StationRecordBodyV1) { b.Comment, b.AuthorizesRelease = "ok", ptr(false) }), CodeInvalidStationRecord, "authorizes_release"},
		"disposition is release":   {with(KindQualityDisposition, func(b *StationRecordBodyV1) { b.Disposition, b.AuthorizesRelease = "release", ptr(false) }), CodeInvalidStationRecord, "disposition"},
		"missing reject":           {with(KindQuantity, func(b *StationRecordBodyV1) { b.GoodQuantity = ptr(int64(1)) }), CodeInvalidStationRecord, "reject_quantity"},
		"negative good":            {with(KindQuantity, func(b *StationRecordBodyV1) { b.GoodQuantity, b.RejectQuantity = ptr(int64(-1)), ptr(int64(0)) }), CodeInvalidStationRecord, "good_quantity"},
		"quantity on started":      {with(KindStarted, func(b *StationRecordBodyV1) { b.GoodQuantity = ptr(int64(1)) }), CodeInvalidStationRecord, "good_quantity"},
		"zero cycle time":          {with(KindCycleTime, func(b *StationRecordBodyV1) { b.CycleTimeSeconds = "0" }), CodeInvalidStationRecord, "cycle_time_seconds"},
		"seven fraction digits":    {with(KindCycleTime, func(b *StationRecordBodyV1) { b.CycleTimeSeconds = "2.1234567" }), CodeInvalidStationRecord, "cycle_time_seconds"},
		"float cycle time":         {with(KindCycleTime, func(b *StationRecordBodyV1) { b.CycleTimeSeconds = "2.50" }), CodeInvalidStationRecord, "cycle_time_seconds"},
		"pause without reason":     {recordBody(KindPaused), CodeInvalidStationRecord, "comment"},
		"formula comment":          {with(KindNote, func(b *StationRecordBodyV1) { b.Comment = `=HYPERLINK("x")` }), CodeInvalidStationRecord, "comment"},
		"unknown material issue":   {with(KindMaterialException, func(b *StationRecordBodyV1) { b.MaterialCode, b.MaterialIssue = "COIL-7", "lost" }), CodeInvalidStationRecord, "material_issue"},
		"empty completion":         {recordBody(KindCompletionEvidence), CodeInvalidStationRecord, "values"},
		"unknown kind":             {recordBody("released"), CodeInvalidStationRecord, "kind"},
		"no offset":                {with(KindStarted, func(b *StationRecordBodyV1) { b.EventAt = "2026-10-19T09:00:00" }), CodeInvalidStationRecord, "event_at"},
		"nanosecond event":         {with(KindStarted, func(b *StationRecordBodyV1) { b.EventAt = "2026-10-19T09:00:00.000000001Z" }), CodeInvalidStationRecord, "event_at"},
		"correct a lifecycle":      {with(KindStarted, func(b *StationRecordBodyV1) { b.CorrectsRecordID = uuid.NewString() }), CodeInvalidCorrection, "corrects_record_id"},
		"zero job revision":        {with(KindStarted, func(b *StationRecordBodyV1) { b.JobOrderRevision = 0 }), CodeInvalidStationRecord, "job_order_revision"},
	} {
		e := tc.body.Validate()
		if e == nil || e.Code != tc.code || e.Path != tc.path {
			t.Errorf("%s: got %v, want %s at %s", name, e, tc.code, tc.path)
		}
	}
}

// TestDecodeStationRecordRefusesProvenanceAndScope: source, tenant and actor never come
// from the body (rules 03 and 06); strict decoding refuses them.
func TestDecodeStationRecordRefusesProvenanceAndScope(t *testing.T) {
	ok := `{"schema_version":1,"kind":"started","workstation_id":"6f1c7a52-58a3-4c1e-9d4e-0d6f3c1b2a90","job_order_id":"0b7e5b2c-3f6a-4f0e-8a51-2c9d7e4f1a33","job_order_revision":1,"event_at":"2026-10-19T09:00:00Z"`
	if _, err := DecodeStationRecord([]byte(ok + `}`)); err != nil {
		t.Fatalf("valid body: %v", err)
	}
	for _, extra := range []string{`,"source":"mes_adapter"`, `,"tenant_id":"6f1c7a52-58a3-4c1e-9d4e-0d6f3c1b2a90"`, `,"recorded_by":"x"`, `,"received_at":"2026-10-19T09:00:00Z"`, `,"job_order_revision":1`} {
		var config *ConfigError
		if _, err := DecodeStationRecord([]byte(ok + extra + `}`)); !errors.As(err, &config) || config.Code != CodeInvalidStationRecord {
			t.Errorf("%s: got %v", extra, err)
		}
	}
}

// TestStationRecordDigest (rule 04): the same instant in another offset hashes identically;
// any changed observed value changes the digest.
func TestStationRecordDigest(t *testing.T) {
	sc := Scope{TenantID: uuid.NewString(), SiteID: uuid.NewString()}
	a := recordBody(KindCycleTime)
	a.CycleTimeSeconds = "2.5"
	b := a
	b.EventAt = "2026-10-19T07:00:00Z"
	c := a
	c.CycleTimeSeconds = "2.6"
	_, _, da, _ := recordEnvelope(sc, a)
	_, _, db, _ := recordEnvelope(sc, b)
	_, _, dc, _ := recordEnvelope(sc, c)
	if da != db || da == dc || len(da) != 64 {
		t.Fatalf("digests a=%s b=%s c=%s", da, db, dc)
	}
}

// TestLifecycleAllows pins the station-job state machine.
func TestLifecycleAllows(t *testing.T) {
	for _, tc := range []struct {
		last, kind RecordKind
		want       error
	}{
		{"", KindAcknowledged, nil},
		{"", KindStarted, ErrLifecycle},
		{"", KindQuantity, ErrLifecycle},
		{"", KindNote, nil},
		{KindAcknowledged, KindAcknowledged, ErrLifecycle},
		{KindAcknowledged, KindStarted, nil},
		{KindAcknowledged, KindCycleTime, ErrLifecycle},
		{KindStarted, KindQuantity, nil},
		{KindStarted, KindPaused, nil},
		{KindStarted, KindResumed, ErrLifecycle},
		{KindPaused, KindQualityDisposition, nil},
		{KindBlocked, KindMaterialException, nil},
		{KindBlocked, KindCompletionEvidence, ErrLifecycle},
		{KindBlocked, KindPaused, ErrLifecycle},
		{KindBlocked, KindResumed, nil},
		{KindResumed, KindCompletionEvidence, nil},
		{KindCompletionEvidence, KindNote, ErrStationJobCompleted},
		{KindCompletionEvidence, KindQuantity, ErrStationJobCompleted},
	} {
		if got := lifecycleAllows(tc.last, tc.kind); !errors.Is(got, tc.want) || (tc.want == nil && got != nil) {
			t.Errorf("%q then %q: got %v want %v", tc.last, tc.kind, got, tc.want)
		}
	}
}

// TestInstructionRevokedIsUnavailable: the revoked reason stays an unavailable instruction for
// every caller that matches the general error, and is distinguishable from it.
func TestInstructionRevokedIsUnavailable(t *testing.T) {
	if !errors.Is(ErrInstructionRevoked, ErrInstructionUnavailable) || errors.Is(ErrInstructionUnavailable, ErrInstructionRevoked) {
		t.Fatal("ErrInstructionRevoked must wrap ErrInstructionUnavailable and not the reverse")
	}
}
