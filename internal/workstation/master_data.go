package workstation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
)

var (
	ErrMasterDataUnchanged   = errors.New("master data revision is identical to the current revision")
	ErrLineValidationBlocked = errors.New("site profiles do not map to active lines")
	ErrMasterDataInUse       = errors.New("master data is referenced by current routing")
)

type MasterDataValidationError struct{ Path string }

func (e *MasterDataValidationError) Error() string { return "invalid master data at " + e.Path }
func invalidMasterData(path string) error          { return &MasterDataValidationError{Path: path} }

type MasterDataUnit string

const MasterDataUnitCount MasterDataUnit = "count"

type LineBodyV1 struct {
	SchemaVersion int    `json:"schema_version"`
	DisplayName   string `json:"display_name" contract:"max=128"`
	Active        bool   `json:"active"`
}

type ItemBodyV1 struct {
	SchemaVersion int            `json:"schema_version"`
	DisplayName   string         `json:"display_name" contract:"max=128"`
	Unit          MasterDataUnit `json:"unit"`
	Active        bool           `json:"active"`
}

type RoutingStepV1 struct {
	StepNumber    int    `json:"step_number"`
	WorkstationID string `json:"workstation_id" contract:"uuid"`
}

type RoutingBodyV1 struct {
	SchemaVersion int             `json:"schema_version"`
	DisplayName   string          `json:"display_name" contract:"max=128"`
	Active        bool            `json:"active"`
	LineID        string          `json:"line_id" contract:"uuid"`
	ItemID        string          `json:"item_id" contract:"uuid"`
	Steps         []RoutingStepV1 `json:"steps"`
}

type ShiftCalendarShiftV1 struct {
	DayOfWeek  int    `json:"day_of_week"`
	StartLocal string `json:"start_local"`
	EndLocal   string `json:"end_local"`
}

type ShiftCalendarBodyV1 struct {
	SchemaVersion int                    `json:"schema_version"`
	DisplayName   string                 `json:"display_name" contract:"max=128"`
	TimeZone      string                 `json:"time_zone" contract:"max=64"`
	Shifts        []ShiftCalendarShiftV1 `json:"shifts"`
}

type SiteMasterDataSettingsBodyV1 struct {
	SchemaVersion                       int  `json:"schema_version"`
	StationProfileLineValidationEnabled bool `json:"station_profile_line_validation_enabled"`
}

type MasterDataRevisionV1[T any] struct {
	Revision               int64      `json:"revision"`
	Body                   T          `json:"body"`
	CanonicalPayloadSHA256 string     `json:"canonical_payload_digest" contract:"sha256"`
	DigestSchemaVersion    int        `json:"digest_schema_version"`
	SubmittedBy            ActorRefV1 `json:"submitted_by"`
	CreatedAt              time.Time  `json:"created_at"`
}

type MasterDataEntityV1[T any] struct {
	ID               string                    `json:"id" contract:"uuid"`
	CurrentRevision  int64                     `json:"current_revision"`
	Revisions        []MasterDataRevisionV1[T] `json:"revisions"`
	Replayed         bool                      `json:"replayed"`
	ReplayedRevision int64                     `json:"replayed_revision,omitempty"`
}

type MasterDataSummaryV1[T any] struct {
	ID              string `json:"id" contract:"uuid"`
	CurrentRevision int64  `json:"current_revision"`
	Body            T      `json:"body"`
}

type LineV1 = MasterDataEntityV1[LineBodyV1]
type ItemV1 = MasterDataEntityV1[ItemBodyV1]
type RoutingV1 = MasterDataEntityV1[RoutingBodyV1]
type ShiftCalendarV1 = MasterDataEntityV1[ShiftCalendarBodyV1]

type LinePageV1 struct {
	Lines      []MasterDataSummaryV1[LineBodyV1] `json:"lines"`
	NextCursor string                            `json:"next_cursor,omitempty"`
}
type ItemPageV1 struct {
	Items      []MasterDataSummaryV1[ItemBodyV1] `json:"items"`
	NextCursor string                            `json:"next_cursor,omitempty"`
}
type RoutingPageV1 struct {
	Routings   []MasterDataSummaryV1[RoutingBodyV1] `json:"routings"`
	NextCursor string                               `json:"next_cursor,omitempty"`
}
type ShiftCalendarPageV1 struct {
	ShiftCalendars []MasterDataSummaryV1[ShiftCalendarBodyV1] `json:"shift_calendars"`
	NextCursor     string                                     `json:"next_cursor,omitempty"`
}

type SiteMasterDataSettingsV1 struct {
	CurrentRevision  int64                                                `json:"current_revision"`
	Body             SiteMasterDataSettingsBodyV1                         `json:"body"`
	Revisions        []MasterDataRevisionV1[SiteMasterDataSettingsBodyV1] `json:"revisions"`
	Replayed         bool                                                 `json:"replayed"`
	ReplayedRevision int64                                                `json:"replayed_revision,omitempty"`
}

type masterDataSpec struct{ kind, heads, revisions, schema string }

var (
	lineMasterData     = masterDataSpec{"line", "line_heads", "line_revisions", "LineBodyV1"}
	itemMasterData     = masterDataSpec{"item", "item_heads", "item_revisions", "ItemBodyV1"}
	routingMasterData  = masterDataSpec{"routing", "routing_heads", "routing_revisions", "RoutingBodyV1"}
	calendarMasterData = masterDataSpec{"shift_calendar", "shift_calendar_heads", "shift_calendar_revisions", "ShiftCalendarBodyV1"}
)

type MasterDataService struct{ plans *ShiftPlanService }

func NewMasterData(db *sql.DB) *MasterDataService {
	return &MasterDataService{plans: NewShiftPlans(db)}
}
func (s *MasterDataService) WithSessionPolicy(p session.Policy) *MasterDataService {
	c := *s
	c.plans = s.plans.WithSessionPolicy(p)
	return &c
}

// DecodeMasterDataBody applies the same strict JSON boundary as workstation mutations and also
// requires every non-optional top-level field, including explicit retirement flags.
func DecodeMasterDataBody(kind string, raw []byte) (any, error) {
	var dst any
	var required []string
	switch kind {
	case "line":
		dst = &LineBodyV1{}
		required = []string{"schema_version", "display_name", "active"}
	case "item":
		dst = &ItemBodyV1{}
		required = []string{"schema_version", "display_name", "unit", "active"}
	case "routing":
		dst = &RoutingBodyV1{}
		required = []string{"schema_version", "display_name", "active", "line_id", "item_id", "steps"}
	case "shift_calendar":
		dst = &ShiftCalendarBodyV1{}
		required = []string{"schema_version", "display_name", "time_zone", "shifts"}
	case "site_settings":
		dst = &SiteMasterDataSettingsBodyV1{}
		required = []string{"schema_version", "station_profile_line_validation_enabled"}
	default:
		return nil, invalidMasterData("")
	}
	if err := strictjson.Decode(raw, dst, limits.RequestBodyBytes); err != nil {
		return nil, invalidMasterData("")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, invalidMasterData("")
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return nil, invalidMasterData(name)
		}
	}
	return dst, nil
}

func displayNameOK(v string) bool {
	return validText(v, 512) && utf8.RuneCountInString(v) <= 128 && !Executable(v)
}
func normalizeMasterBody[T any](kind string, in T) (T, error) {
	var err error
	switch b := any(in).(type) {
	case LineBodyV1:
		if b.SchemaVersion != 1 {
			err = invalidMasterData("schema_version")
		} else if !displayNameOK(b.DisplayName) {
			err = invalidMasterData("display_name")
		}
	case ItemBodyV1:
		if b.SchemaVersion != 1 {
			err = invalidMasterData("schema_version")
		} else if !displayNameOK(b.DisplayName) {
			err = invalidMasterData("display_name")
		} else if b.Unit != MasterDataUnitCount {
			err = invalidMasterData("unit")
		}
	case RoutingBodyV1:
		b.Steps = append([]RoutingStepV1(nil), b.Steps...)
		sort.Slice(b.Steps, func(i, j int) bool { return b.Steps[i].StepNumber < b.Steps[j].StepNumber })
		if b.SchemaVersion != 1 {
			err = invalidMasterData("schema_version")
		} else if !displayNameOK(b.DisplayName) {
			err = invalidMasterData("display_name")
		} else if !canonicalID(b.LineID) {
			err = invalidMasterData("line_id")
		} else if !canonicalID(b.ItemID) {
			err = invalidMasterData("item_id")
		} else if len(b.Steps) < 1 || len(b.Steps) > 256 {
			err = invalidMasterData("steps")
		} else {
			for i, step := range b.Steps {
				if step.StepNumber != i+1 {
					err = invalidMasterData(fmt.Sprintf("steps[%d].step_number", i))
					break
				}
				if !canonicalID(step.WorkstationID) {
					err = invalidMasterData(fmt.Sprintf("steps[%d].workstation_id", i))
					break
				}
			}
		}
		in = any(b).(T)
	case ShiftCalendarBodyV1:
		b.Shifts = append([]ShiftCalendarShiftV1(nil), b.Shifts...)
		sort.Slice(b.Shifts, func(i, j int) bool {
			if b.Shifts[i].DayOfWeek != b.Shifts[j].DayOfWeek {
				return b.Shifts[i].DayOfWeek < b.Shifts[j].DayOfWeek
			}
			return b.Shifts[i].StartLocal < b.Shifts[j].StartLocal
		})
		if b.SchemaVersion != 1 {
			err = invalidMasterData("schema_version")
		} else if !displayNameOK(b.DisplayName) {
			err = invalidMasterData("display_name")
		} else if len(b.TimeZone) > 64 || !ianaZone.MatchString(b.TimeZone) {
			err = invalidMasterData("time_zone")
		} else if _, e := time.LoadLocation(b.TimeZone); e != nil {
			err = invalidMasterData("time_zone")
		} else if len(b.Shifts) == 0 || len(b.Shifts) > 256 {
			err = invalidMasterData("shifts")
		} else {
			var day int
			var previousEnd string
			for i, shift := range b.Shifts {
				start, se := time.Parse("15:04", shift.StartLocal)
				end, ee := time.Parse("15:04", shift.EndLocal)
				if shift.DayOfWeek < 1 || shift.DayOfWeek > 7 {
					err = invalidMasterData(fmt.Sprintf("shifts[%d].day_of_week", i))
					break
				}
				if se != nil || start.Format("15:04") != shift.StartLocal || ee != nil || end.Format("15:04") != shift.EndLocal || shift.StartLocal >= shift.EndLocal {
					err = invalidMasterData(fmt.Sprintf("shifts[%d]", i))
					break
				}
				if day == shift.DayOfWeek && shift.StartLocal < previousEnd {
					err = invalidMasterData(fmt.Sprintf("shifts[%d]", i))
					break
				}
				day, previousEnd = shift.DayOfWeek, shift.EndLocal
			}
		}
		in = any(b).(T)
	case SiteMasterDataSettingsBodyV1:
		if b.SchemaVersion != 1 {
			err = invalidMasterData("schema_version")
		}
	default:
		err = invalidMasterData("")
	}
	if err != nil {
		return in, err
	}
	return in, nil
}

func (s *MasterDataService) CreateLine(ctx context.Context, a Actor, sc Scope, b LineBodyV1, key string, expected int64) (LineV1, error) {
	return appendMasterData[LineBodyV1](ctx, s, a, sc, "", b, key, expected, lineMasterData)
}
func (s *MasterDataService) ReviseLine(ctx context.Context, a Actor, sc Scope, id string, b LineBodyV1, key string, expected int64) (LineV1, error) {
	return appendMasterData[LineBodyV1](ctx, s, a, sc, id, b, key, expected, lineMasterData)
}
func (s *MasterDataService) GetLine(ctx context.Context, a Actor, sc Scope, id string) (LineV1, error) {
	return getMasterData[LineBodyV1](ctx, s, a, sc, id, lineMasterData)
}
func (s *MasterDataService) ListLines(ctx context.Context, a Actor, sc Scope, limit int, cursor string) (LinePageV1, error) {
	p, e := listMasterData[LineBodyV1](ctx, s, a, sc, limit, cursor, lineMasterData)
	return LinePageV1{Lines: p.items, NextCursor: p.next}, e
}
func (s *MasterDataService) CreateItem(ctx context.Context, a Actor, sc Scope, b ItemBodyV1, key string, expected int64) (ItemV1, error) {
	return appendMasterData[ItemBodyV1](ctx, s, a, sc, "", b, key, expected, itemMasterData)
}
func (s *MasterDataService) ReviseItem(ctx context.Context, a Actor, sc Scope, id string, b ItemBodyV1, key string, expected int64) (ItemV1, error) {
	return appendMasterData[ItemBodyV1](ctx, s, a, sc, id, b, key, expected, itemMasterData)
}
func (s *MasterDataService) GetItem(ctx context.Context, a Actor, sc Scope, id string) (ItemV1, error) {
	return getMasterData[ItemBodyV1](ctx, s, a, sc, id, itemMasterData)
}
func (s *MasterDataService) ListItems(ctx context.Context, a Actor, sc Scope, limit int, cursor string) (ItemPageV1, error) {
	p, e := listMasterData[ItemBodyV1](ctx, s, a, sc, limit, cursor, itemMasterData)
	return ItemPageV1{Items: p.items, NextCursor: p.next}, e
}
func (s *MasterDataService) CreateRouting(ctx context.Context, a Actor, sc Scope, b RoutingBodyV1, key string, expected int64) (RoutingV1, error) {
	return appendMasterData[RoutingBodyV1](ctx, s, a, sc, "", b, key, expected, routingMasterData)
}
func (s *MasterDataService) ReviseRouting(ctx context.Context, a Actor, sc Scope, id string, b RoutingBodyV1, key string, expected int64) (RoutingV1, error) {
	return appendMasterData[RoutingBodyV1](ctx, s, a, sc, id, b, key, expected, routingMasterData)
}
func (s *MasterDataService) GetRouting(ctx context.Context, a Actor, sc Scope, id string) (RoutingV1, error) {
	return getMasterData[RoutingBodyV1](ctx, s, a, sc, id, routingMasterData)
}
func (s *MasterDataService) ListRoutings(ctx context.Context, a Actor, sc Scope, limit int, cursor string) (RoutingPageV1, error) {
	p, e := listMasterData[RoutingBodyV1](ctx, s, a, sc, limit, cursor, routingMasterData)
	return RoutingPageV1{Routings: p.items, NextCursor: p.next}, e
}
func (s *MasterDataService) CreateShiftCalendar(ctx context.Context, a Actor, sc Scope, b ShiftCalendarBodyV1, key string, expected int64) (ShiftCalendarV1, error) {
	return appendMasterData[ShiftCalendarBodyV1](ctx, s, a, sc, "", b, key, expected, calendarMasterData)
}
func (s *MasterDataService) ReviseShiftCalendar(ctx context.Context, a Actor, sc Scope, id string, b ShiftCalendarBodyV1, key string, expected int64) (ShiftCalendarV1, error) {
	return appendMasterData[ShiftCalendarBodyV1](ctx, s, a, sc, id, b, key, expected, calendarMasterData)
}
func (s *MasterDataService) GetShiftCalendar(ctx context.Context, a Actor, sc Scope, id string) (ShiftCalendarV1, error) {
	return getMasterData[ShiftCalendarBodyV1](ctx, s, a, sc, id, calendarMasterData)
}
func (s *MasterDataService) ListShiftCalendars(ctx context.Context, a Actor, sc Scope, limit int, cursor string) (ShiftCalendarPageV1, error) {
	p, e := listMasterData[ShiftCalendarBodyV1](ctx, s, a, sc, limit, cursor, calendarMasterData)
	return ShiftCalendarPageV1{ShiftCalendars: p.items, NextCursor: p.next}, e
}

type masterDataPage[T any] struct {
	items []MasterDataSummaryV1[T]
	next  string
}

func appendMasterData[T any](ctx context.Context, s *MasterDataService, a Actor, sc Scope, id string, body T, key string, expected int64, spec masterDataSpec) (MasterDataEntityV1[T], error) {
	out := MasterDataEntityV1[T]{Revisions: []MasterDataRevisionV1[T]{}}
	body, err := normalizeMasterBody(spec.kind, body)
	if err != nil {
		return out, err
	}
	if (id != "" && !canonicalID(id)) || !validText(key, 128) || expected < 0 {
		return out, ErrInvalidShiftPlan
	}
	_, _, err = canonical.Envelope(spec.schema, 1, sc.TenantID, sc.SiteID, body)
	if err != nil {
		return out, invalidMasterData("")
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	out, err = appendMasterDataTx(ctx, tx, a, sc, id, body, key, expected, spec)
	if err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

func appendMasterDataTx[T any](ctx context.Context, tx *sql.Tx, a Actor, sc Scope, id string, body T, key string, expected int64, spec masterDataSpec, createID ...string) (MasterDataEntityV1[T], error) {
	out := MasterDataEntityV1[T]{Revisions: []MasterDataRevisionV1[T]{}}
	body, err := normalizeMasterBody(spec.kind, body)
	if err != nil {
		return out, err
	}
	if (id != "" && !canonicalID(id)) || !validText(key, 128) || expected < 0 || len(createID) > 1 || (len(createID) == 1 && (id != "" || !canonicalID(createID[0]))) {
		return out, ErrInvalidShiftPlan
	}
	encoded, digest, err := canonical.Envelope(spec.schema, 1, sc.TenantID, sc.SiteID, body)
	if err != nil {
		return out, invalidMasterData("")
	}
	op := "create"
	if id != "" {
		op = "revise"
	}
	requestHash := requestDigest([]any{"master_data." + spec.kind + "." + op, id, expected, digest})
	var priorHash, priorKind, priorID string
	var priorRevision int64
	err = tx.QueryRowContext(ctx, `SELECT request_sha256,object_kind,object_id,revision FROM supervisor.master_data_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&priorHash, &priorKind, &priorID, &priorRevision)
	if err == nil {
		if priorHash != requestHash || priorKind != spec.kind {
			return out, ErrConflict
		}
		out, err = loadMasterData[T](ctx, tx, sc, priorID, spec)
		if err != nil {
			return out, err
		}
		if priorRevision < 1 || priorRevision > int64(len(out.Revisions)) || out.Revisions[priorRevision-1].Revision != priorRevision {
			return out, errors.New("master data receipt revision integrity failure")
		}
		out.Revisions = out.Revisions[:priorRevision]
		out.CurrentRevision = priorRevision
		out.Replayed, out.ReplayedRevision = true, priorRevision
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	var current int64
	var currentDigest string
	if id != "" {
		err = tx.QueryRowContext(ctx, `SELECT h.current_revision,r.canonical_payload_sha256 FROM supervisor.`+spec.heads+` h JOIN supervisor.`+spec.revisions+` r ON (r.tenant_id,r.site_id,r.object_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id=$3 FOR UPDATE OF h`, sc.TenantID, sc.SiteID, id).Scan(&current, &currentDigest)
		if errors.Is(err, sql.ErrNoRows) {
			return out, ErrNotFound
		}
		if err != nil {
			return out, err
		}
	} else if expected != 0 {
		return out, ErrRevision
	}
	if current != expected {
		return out, ErrRevision
	}
	if current > 0 && currentDigest == digest {
		return out, ErrMasterDataUnchanged
	}
	if id == "" {
		id = uuid.NewString()
		if len(createID) == 1 {
			id = createID[0]
		}
	}
	if err = validateMasterDataReference(ctx, tx, sc, id, body); err != nil {
		return out, err
	}
	next := current + 1
	if current == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.`+spec.heads+`(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1)`, sc.TenantID, sc.SiteID, id)
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE supervisor.`+spec.heads+` SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND id=$3 AND current_revision=$5`, sc.TenantID, sc.SiteID, id, next, current)
		if err == nil {
			var n int64
			n, err = result.RowsAffected()
			if err == nil && n != 1 {
				err = ErrRevision
			}
		}
	}
	if err != nil {
		return out, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return out, invalidMasterData("")
	}
	if spec.kind == "routing" {
		b := any(body).(RoutingBodyV1)
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.routing_revisions(tenant_id,site_id,object_id,revision,line_id,item_id,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,1,$10,$11)`, sc.TenantID, sc.SiteID, id, next, b.LineID, b.ItemID, string(payload), string(encoded), digest, a.Issuer, a.Subject)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.`+spec.revisions+`(tenant_id,site_id,object_id,revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7,1,$8,$9)`, sc.TenantID, sc.SiteID, id, next, string(payload), string(encoded), digest, a.Issuer, a.Subject)
	}
	if err != nil {
		return out, err
	}
	if spec.kind == "routing" {
		b := any(body).(RoutingBodyV1)
		for _, step := range b.Steps {
			if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.routing_steps(tenant_id,site_id,routing_id,routing_revision,step_number,workstation_id) VALUES($1,$2,$3,$4,$5,$6)`, sc.TenantID, sc.SiteID, id, next, step.StepNumber, step.WorkstationID); err != nil {
				return out, err
			}
		}
		rows, readErr := tx.QueryContext(ctx, `SELECT step_number,workstation_id FROM supervisor.routing_steps WHERE tenant_id=$1 AND site_id=$2 AND routing_id=$3 AND routing_revision=$4 ORDER BY step_number`, sc.TenantID, sc.SiteID, id, next)
		if readErr != nil {
			return out, readErr
		}
		index := 0
		for rows.Next() {
			var step RoutingStepV1
			if readErr = rows.Scan(&step.StepNumber, &step.WorkstationID); readErr != nil {
				rows.Close()
				return out, readErr
			}
			if index >= len(b.Steps) || step != b.Steps[index] {
				rows.Close()
				return out, errors.New("routing step integrity failure")
			}
			index++
		}
		readErr = rows.Err()
		rows.Close()
		if readErr != nil {
			return out, readErr
		}
		if index != len(b.Steps) {
			return out, errors.New("routing step integrity failure")
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.master_data_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,object_kind,object_id,revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, requestHash, spec.kind, id, next); err != nil {
		return out, err
	}
	out, err = loadMasterData[T](ctx, tx, sc, id, spec)
	if err != nil {
		return out, err
	}
	return out, nil
}

func validateMasterDataReference[T any](ctx context.Context, tx *sql.Tx, sc Scope, id string, body T) error {
	switch b := any(body).(type) {
	case LineBodyV1:
		if !b.Active {
			var enabled bool
			if err := lineValidationEnabled(ctx, tx, sc, &enabled); err != nil {
				return err
			}
			if enabled {
				var used bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.station_profile_heads h JOIN supervisor.station_profile_revisions r ON (r.tenant_id,r.site_id,r.workstation_id,r.revision)=(h.tenant_id,h.site_id,h.workstation_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND r.payload->>'line_id'=$3) OR EXISTS(SELECT 1 FROM supervisor.routing_heads h JOIN supervisor.routing_revisions r ON (r.tenant_id,r.site_id,r.object_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND r.line_id=$3::uuid AND r.payload->>'active'='true')`, sc.TenantID, sc.SiteID, id).Scan(&used); err != nil {
					return err
				}
				if used {
					return ErrLineValidationBlocked
				}
			}
		}
	case ItemBodyV1:
		if !b.Active {
			var used bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.routing_heads h JOIN supervisor.routing_revisions r ON (r.tenant_id,r.site_id,r.object_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND r.item_id=$3::uuid AND r.payload->>'active'='true')`, sc.TenantID, sc.SiteID, id).Scan(&used); err != nil {
				return err
			}
			if used {
				return ErrMasterDataInUse
			}
		}
	case RoutingBodyV1:
		var lineExists, lineActive, itemExists, itemActive bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.line_heads WHERE tenant_id=$1 AND site_id=$2 AND id=$3), EXISTS(SELECT 1 FROM supervisor.line_heads h JOIN supervisor.line_revisions r ON (r.tenant_id,r.site_id,r.object_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id=$3 AND r.payload->>'active'='true'), EXISTS(SELECT 1 FROM supervisor.item_heads WHERE tenant_id=$1 AND site_id=$2 AND id=$4), EXISTS(SELECT 1 FROM supervisor.item_heads h JOIN supervisor.item_revisions r ON (r.tenant_id,r.site_id,r.object_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id=$4 AND r.payload->>'active'='true')`, sc.TenantID, sc.SiteID, b.LineID, b.ItemID).Scan(&lineExists, &lineActive, &itemExists, &itemActive); err != nil {
			return err
		}
		if !lineExists || (b.Active && !lineActive) {
			return invalidMasterData("line_id")
		}
		if !itemExists || (b.Active && !itemActive) {
			return invalidMasterData("item_id")
		}
		for i, step := range b.Steps {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.station_profile_heads WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3)`, sc.TenantID, sc.SiteID, step.WorkstationID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return invalidMasterData(fmt.Sprintf("steps[%d].workstation_id", i))
			}
		}
	}
	return nil
}

func lineValidationEnabled(ctx context.Context, tx *sql.Tx, sc Scope, out *bool) error {
	return tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT r.station_profile_line_validation_enabled FROM supervisor.site_master_data_setting_heads h JOIN supervisor.site_master_data_setting_revisions r ON (r.tenant_id,r.site_id,r.revision)=(h.tenant_id,h.site_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2),false)`, sc.TenantID, sc.SiteID).Scan(out)
}

func getMasterData[T any](ctx context.Context, s *MasterDataService, a Actor, sc Scope, id string, spec masterDataSpec) (MasterDataEntityV1[T], error) {
	out := MasterDataEntityV1[T]{Revisions: []MasterDataRevisionV1[T]{}}
	if !canonicalID(id) {
		return out, ErrNotFound
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	out, err = loadMasterData[T](ctx, tx, sc, id, spec)
	if err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

func loadMasterData[T any](ctx context.Context, tx *sql.Tx, sc Scope, id string, spec masterDataSpec) (MasterDataEntityV1[T], error) {
	out := MasterDataEntityV1[T]{ID: id, Revisions: []MasterDataRevisionV1[T]{}}
	err := tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.`+spec.heads+` WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, id).Scan(&out.CurrentRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT revision,payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject,created_at FROM supervisor.`+spec.revisions+` WHERE tenant_id=$1 AND site_id=$2 AND object_id=$3 ORDER BY revision`, sc.TenantID, sc.SiteID, id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var r MasterDataRevisionV1[T]
		var payload []byte
		if err = rows.Scan(&r.Revision, &payload, &r.CanonicalPayloadSHA256, &r.DigestSchemaVersion, &r.SubmittedBy.Issuer, &r.SubmittedBy.Subject, &r.CreatedAt); err != nil {
			return out, err
		}
		r.CreatedAt = r.CreatedAt.UTC()
		if err = json.Unmarshal(payload, &r.Body); err != nil {
			return out, err
		}
		out.Revisions = append(out.Revisions, r)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Revisions) == 0 || out.Revisions[len(out.Revisions)-1].Revision != out.CurrentRevision {
		return out, errors.New("master data head integrity failure")
	}
	return out, nil
}

func listMasterData[T any](ctx context.Context, s *MasterDataService, a Actor, sc Scope, limit int, cursor string, spec masterDataSpec) (masterDataPage[T], error) {
	out := masterDataPage[T]{items: []MasterDataSummaryV1[T]{}}
	if limit < 1 || limit > 100 || (cursor != "" && !canonicalID(cursor)) {
		return out, ErrInvalidShiftPlan
	}
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT h.id,h.current_revision,r.payload FROM supervisor.`+spec.heads+` h JOIN supervisor.`+spec.revisions+` r ON (r.tenant_id,r.site_id,r.object_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id>$3 ORDER BY h.id LIMIT $4`, sc.TenantID, sc.SiteID, cursor, limit+1)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v MasterDataSummaryV1[T]
		var payload []byte
		if err = rows.Scan(&v.ID, &v.CurrentRevision, &payload); err != nil {
			rows.Close()
			return out, err
		}
		if err = json.Unmarshal(payload, &v.Body); err != nil {
			rows.Close()
			return out, err
		}
		out.items = append(out.items, v)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	if len(out.items) > limit {
		out.items = out.items[:limit]
		out.next = out.items[len(out.items)-1].ID
	}
	return out, s.plans.finish(ctx, tx, a)
}

func (s *MasterDataService) ReviseSettings(ctx context.Context, a Actor, sc Scope, b SiteMasterDataSettingsBodyV1, key string, expected int64) (SiteMasterDataSettingsV1, error) {
	out := SiteMasterDataSettingsV1{Body: SiteMasterDataSettingsBodyV1{SchemaVersion: 1}, Revisions: []MasterDataRevisionV1[SiteMasterDataSettingsBodyV1]{}}
	bAny, err := normalizeMasterBody("site_settings", b)
	if err != nil {
		return out, err
	}
	b = bAny
	if !validText(key, 128) || expected < 0 {
		return out, ErrInvalidShiftPlan
	}
	encoded, digest, err := canonical.Envelope("SiteMasterDataSettingsBodyV1", 1, sc.TenantID, sc.SiteID, b)
	if err != nil {
		return out, invalidMasterData("")
	}
	hash := requestDigest([]any{"master_data.site_settings.revise", sc.SiteID, expected, digest})
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var stored, kind, id string
	var rev int64
	err = tx.QueryRowContext(ctx, `SELECT request_sha256,object_kind,object_id,revision FROM supervisor.master_data_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &kind, &id, &rev)
	if err == nil {
		if stored != hash || kind != "site_settings" {
			return out, ErrConflict
		}
		out, err = loadSettings(ctx, tx, sc)
		if err != nil {
			return out, err
		}
		if rev < 1 || rev > int64(len(out.Revisions)) || out.Revisions[rev-1].Revision != rev {
			return out, errors.New("master data receipt revision integrity failure")
		}
		out.Revisions = out.Revisions[:rev]
		out.CurrentRevision, out.Body = rev, out.Revisions[rev-1].Body
		out.Replayed, out.ReplayedRevision = true, rev
		return out, s.plans.finish(ctx, tx, a)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	var current int64
	var currentDigest string
	err = tx.QueryRowContext(ctx, `SELECT h.current_revision,r.canonical_payload_sha256 FROM supervisor.site_master_data_setting_heads h JOIN supervisor.site_master_data_setting_revisions r ON (r.tenant_id,r.site_id,r.revision)=(h.tenant_id,h.site_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 FOR UPDATE OF h`, sc.TenantID, sc.SiteID).Scan(&current, &currentDigest)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	} else if err != nil {
		return out, err
	}
	if current != expected {
		return out, ErrRevision
	}
	if current > 0 && currentDigest == digest {
		return out, ErrMasterDataUnchanged
	}
	if b.StationProfileLineValidationEnabled {
		var unmapped bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.station_profile_heads p JOIN supervisor.station_profile_revisions pr ON (pr.tenant_id,pr.site_id,pr.workstation_id,pr.revision)=(p.tenant_id,p.site_id,p.workstation_id,p.current_revision) WHERE p.tenant_id=$1 AND p.site_id=$2 AND NOT EXISTS(SELECT 1 FROM supervisor.line_heads l JOIN supervisor.line_revisions lr ON (lr.tenant_id,lr.site_id,lr.object_id,lr.revision)=(l.tenant_id,l.site_id,l.id,l.current_revision) WHERE l.tenant_id=p.tenant_id AND l.site_id=p.site_id AND l.id=(pr.payload->>'line_id')::uuid AND lr.payload->>'active'='true'))`, sc.TenantID, sc.SiteID).Scan(&unmapped)
		if err != nil {
			return out, err
		}
		if unmapped {
			return out, ErrLineValidationBlocked
		}
	}
	next := current + 1
	if current == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.site_master_data_setting_heads(tenant_id,site_id,current_revision) VALUES($1,$2,1)`, sc.TenantID, sc.SiteID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.site_master_data_setting_heads SET current_revision=$3 WHERE tenant_id=$1 AND site_id=$2 AND current_revision=$4`, sc.TenantID, sc.SiteID, next, current)
	}
	if err != nil {
		return out, err
	}
	payload, _ := json.Marshal(b)
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.site_master_data_setting_revisions(tenant_id,site_id,revision,station_profile_line_validation_enabled,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7,1,$8,$9)`, sc.TenantID, sc.SiteID, next, b.StationProfileLineValidationEnabled, string(payload), string(encoded), digest, a.Issuer, a.Subject)
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.master_data_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,object_kind,object_id,revision) VALUES($1,$2,$3,$4,$5,$6,'site_settings',$7,$8)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, hash, sc.SiteID, next); err != nil {
		return out, err
	}
	out, err = loadSettings(ctx, tx, sc)
	if err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

func (s *MasterDataService) GetSettings(ctx context.Context, a Actor, sc Scope) (SiteMasterDataSettingsV1, error) {
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return SiteMasterDataSettingsV1{}, err
	}
	defer tx.Rollback()
	out, err := loadSettings(ctx, tx, sc)
	if err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}
func loadSettings(ctx context.Context, tx *sql.Tx, sc Scope) (SiteMasterDataSettingsV1, error) {
	out := SiteMasterDataSettingsV1{Body: SiteMasterDataSettingsBodyV1{SchemaVersion: 1}, Revisions: []MasterDataRevisionV1[SiteMasterDataSettingsBodyV1]{}}
	var currentPayload []byte
	err := tx.QueryRowContext(ctx, `SELECT h.current_revision,r.payload FROM supervisor.site_master_data_setting_heads h JOIN supervisor.site_master_data_setting_revisions r ON (r.tenant_id,r.site_id,r.revision)=(h.tenant_id,h.site_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2`, sc.TenantID, sc.SiteID).Scan(&out.CurrentRevision, &currentPayload)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if err == nil {
		if err = json.Unmarshal(currentPayload, &out.Body); err != nil {
			return out, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT revision,payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject,created_at FROM supervisor.site_master_data_setting_revisions WHERE tenant_id=$1 AND site_id=$2 ORDER BY revision`, sc.TenantID, sc.SiteID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var r MasterDataRevisionV1[SiteMasterDataSettingsBodyV1]
		var payload []byte
		if err = rows.Scan(&r.Revision, &payload, &r.CanonicalPayloadSHA256, &r.DigestSchemaVersion, &r.SubmittedBy.Issuer, &r.SubmittedBy.Subject, &r.CreatedAt); err != nil {
			return out, err
		}
		r.CreatedAt = r.CreatedAt.UTC()
		if err = json.Unmarshal(payload, &r.Body); err != nil {
			return out, err
		}
		out.Revisions = append(out.Revisions, r)
	}
	return out, rows.Err()
}

func (s *MasterDataService) ListSettingsHistory(ctx context.Context, a Actor, sc Scope) (SiteMasterDataSettingsV1, error) {
	return s.GetSettings(ctx, a, sc)
}

// SiteProfileLineAllowed is called inside the profile mutation's site-locked transaction. The
// free UUID behavior remains untouched until an owner or supervisor enables this setting.
func SiteProfileLineAllowed(ctx context.Context, tx *sql.Tx, sc Scope, lineID string) (bool, error) {
	var enabled bool
	if err := lineValidationEnabled(ctx, tx, sc, &enabled); err != nil || !enabled {
		return !enabled, err
	}
	var active bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.line_heads h JOIN supervisor.line_revisions r ON (r.tenant_id,r.site_id,r.object_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id=$3 AND r.payload->>'active'='true')`, sc.TenantID, sc.SiteID, lineID).Scan(&active)
	return active, err
}
