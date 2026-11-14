// Package contracts generates the published JSON Schemas and OpenAPI document
// for the OEE component contract (WIS-225) from the Go types, deterministically.
// Output is committed under contracts/; `make supervisor-contracts` regenerates
// it and fails on any uncommitted drift.
package contracts

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	uuidType = reflect.TypeOf(uuid.UUID{})
	timeType = reflect.TypeOf(time.Time{})
	rawType  = reflect.TypeOf(json.RawMessage{})
)

const (
	sha256Pattern   = `^[0-9a-f]{64}$`
	decimal6Pattern = `^-?[0-9]+(\.[0-9]{1,6})?$`
)

// Schema builds a closed (additionalProperties=false) JSON Schema for a Go
// type. Enums register named string types; an unsupported kind or an unknown
// `contract` tag panics so a contract can never be published half-described.
func Schema(t reflect.Type, enums map[reflect.Type][]string) map[string]any {
	out := schema(t, enums)
	out["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	return out
}

func schema(t reflect.Type, enums map[reflect.Type][]string) map[string]any {
	switch {
	case t == uuidType:
		return map[string]any{"type": "string", "format": "uuid"}
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == rawType:
		// Typed by a sibling field (for example a knowledge literal's type); open here.
		return map[string]any{"description": "Any JSON value"}
	}
	if values, ok := enums[t]; ok {
		sorted := append([]string(nil), values...)
		return map[string]any{"type": "string", "enum": toAny(sorted)}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schema(t.Elem(), enums)}
	case reflect.Ptr:
		return schema(t.Elem(), enums)
	case reflect.Struct:
		properties := map[string]any{}
		required := []string{}
		promoted := []map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous && f.Tag.Get("json") == "" && f.Type.Kind() == reflect.Struct {
				// encoding/json promotes an untagged embedded struct's fields; outer fields win.
				promoted = append(promoted, schema(f.Type, enums))
				continue
			}
			name, omit, skip := jsonName(f)
			if skip {
				continue
			}
			prop := schema(f.Type, enums)
			applyContractTag(prop, f)
			if f.Type.Kind() == reflect.Ptr && !omit {
				// A pointer that is always serialized may be null (for example avoided_loss).
				prop = map[string]any{"anyOf": []any{prop, map[string]any{"type": "null"}}}
			}
			properties[name] = prop
			if !omit {
				required = append(required, name)
			}
		}
		for _, inner := range promoted {
			for name, prop := range inner["properties"].(map[string]any) {
				if _, shadowed := properties[name]; shadowed {
					continue
				}
				properties[name] = prop
				for _, r := range inner["required"].([]any) {
					if r == name {
						required = append(required, name)
					}
				}
			}
		}
		sort.Strings(required)
		return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": toAny(required)}
	}
	panic(fmt.Sprintf("contracts: unsupported kind %s for %s", t.Kind(), t))
}

func jsonName(f reflect.StructField) (name string, omitempty, skip bool) {
	if !f.IsExported() {
		return "", false, true
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		panic(fmt.Sprintf("contracts: field %s has no json name", f.Name))
	}
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty, false
}

func applyContractTag(prop map[string]any, f reflect.StructField) {
	tag := f.Tag.Get("contract")
	if tag == "" {
		return
	}
	for _, rule := range strings.Split(tag, ",") {
		switch {
		case rule == "uuid":
			prop["format"] = "uuid"
		case rule == "sha256":
			prop["pattern"] = sha256Pattern
		case rule == "decimal6":
			prop["pattern"] = decimal6Pattern
		case rule == "datetime":
			prop["format"] = "date-time"
		case strings.HasPrefix(rule, "max="):
			n, err := strconv.Atoi(strings.TrimPrefix(rule, "max="))
			if err != nil || n < 1 {
				panic(fmt.Sprintf("contracts: bad max on %s", f.Name))
			}
			prop["maxLength"] = n
		case strings.HasPrefix(rule, "description="):
			description := strings.TrimPrefix(rule, "description=")
			if description == "" {
				panic(fmt.Sprintf("contracts: empty description on %s", f.Name))
			}
			prop["description"] = description
		default:
			panic(fmt.Sprintf("contracts: unknown contract rule %q on %s", rule, f.Name))
		}
	}
}

func toAny[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}
