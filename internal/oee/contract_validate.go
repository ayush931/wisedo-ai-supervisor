package oee

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
)

// ErrInvalidContract means a value breaks its published v1 wire contract.
var ErrInvalidContract = errors.New("invalid OEE contract value")

// MaxCalculationRequestBytes bounds a CalculationRequestV1 body.
const MaxCalculationRequestBytes = 16 << 10

// ValidateContract enforces at runtime the same rules the generated JSON Schemas publish,
// so a value the schema rejects is never accepted by the server:
//   - contract:"max=N": a non-empty string of at most N characters (omitempty fields may be empty);
//   - contract:"sha256": 64 lowercase hex characters;
//   - contract:"decimal6": a decimal string with at most six fractional digits;
//   - contract:"datetime": RFC 3339 with an explicit offset;
//   - uuid.UUID fields are not the nil UUID; Status and ReasonCode values are published.
func ValidateContract(v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return fmt.Errorf("%w: not a contract struct", ErrInvalidContract)
	}
	statuses := map[Status]bool{}
	for _, s := range Statuses() {
		statuses[s] = true
	}
	reasons := map[ReasonCode]bool{}
	for _, c := range ReasonCodes() {
		reasons[c] = true
	}
	t := rv.Type()
	for i := 0; i < t.NumField(); i++ {
		f, fv := t.Field(i), rv.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		bad := func(why string) error { return fmt.Errorf("%w: %s %s", ErrInvalidContract, name, why) }
		omitEmpty := strings.Contains(opts, "omitempty")
		switch value := fv.Interface().(type) {
		case uuid.UUID:
			if value == uuid.Nil {
				return bad("must be a non-nil UUID")
			}
		case *uuid.UUID:
			if value != nil && *value == uuid.Nil {
				return bad("must be a non-nil UUID")
			}
		case Status:
			if !statuses[value] {
				return bad("is not a published status")
			}
		case []ReasonCode:
			for _, c := range value {
				if !reasons[c] {
					return bad("contains an unpublished reason code")
				}
			}
		}
		tag := f.Tag.Get("contract")
		if tag == "" {
			continue
		}
		s, ok := fv.Interface().(string)
		if !ok {
			return bad("has a contract tag but is not a string")
		}
		if s == "" && omitEmpty {
			continue
		}
		for _, rule := range strings.Split(tag, ",") {
			switch {
			case rule == "sha256":
				if !evidenceSHA256.MatchString(s) {
					return bad("must be a sha256 hex digest")
				}
			case rule == "decimal6":
				if !evidenceDecimal6.MatchString(s) {
					return bad("must be a decimal with at most six fractional digits")
				}
			case rule == "datetime":
				if !explicitOffsetTime(s) {
					return bad("must be RFC 3339 with an explicit offset")
				}
			case strings.HasPrefix(rule, "max="):
				n, err := strconv.Atoi(strings.TrimPrefix(rule, "max="))
				if err != nil {
					return bad("has a malformed max rule")
				}
				if !utf8.ValidString(s) || s == "" || utf8.RuneCountInString(s) > n || strings.TrimSpace(s) != s {
					return bad(fmt.Sprintf("must be 1 to %d characters without surrounding space", n))
				}
			default:
				return bad("has an unknown contract rule " + rule)
			}
		}
	}
	return nil
}

func explicitOffsetTime(s string) bool {
	t, err := time.Parse(time.RFC3339Nano, s)
	return err == nil && t.Year() >= 1 && t.Year() <= 9999
}

// DecodeCalculationRequest strictly decodes and validates a CalculationRequestV1 body:
// bounded size, exact field names, no unknown or duplicate fields, string timestamps only
// (numbers and booleans are refused), and a window that ends after it starts.
func DecodeCalculationRequest(data []byte) (CalculationRequestV1, error) {
	var r CalculationRequestV1
	if err := strictjson.Decode(data, &r, MaxCalculationRequestBytes); err != nil {
		return CalculationRequestV1{}, fmt.Errorf("%w: %v", ErrInvalidContract, err)
	}
	if err := ValidateContract(r); err != nil {
		return CalculationRequestV1{}, err
	}
	start, _ := time.Parse(time.RFC3339Nano, r.WindowStart)
	end, _ := time.Parse(time.RFC3339Nano, r.WindowEnd)
	if !start.Before(end) {
		return CalculationRequestV1{}, fmt.Errorf("%w: window_end must be after window_start", ErrInvalidContract)
	}
	return r, nil
}
