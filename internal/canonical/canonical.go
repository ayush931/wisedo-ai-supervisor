// Package canonical is the shared schema-ordered serializer for digests. A body is a
// Go struct (field order is the schema order); values are strings, integers, booleans,
// arrays, objects or null. Floats are rejected: decimals travel as canonical strings and
// times as UTC RFC3339 strings, so equal inputs always produce equal bytes.
package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
)

var ErrNotCanonical = errors.New("value is not canonically serializable")

// Envelope serializes {schema, schema_version, tenant_id, site_id, body} and returns the
// bytes and their lowercase SHA-256 hex digest. Scope is part of the digest, so a digest
// is never portable across tenants or sites.
func Envelope(schema string, version int, tenantID, siteID string, body any) ([]byte, string, error) {
	if schema == "" || strings.TrimSpace(schema) != schema || version < 1 || !scopeID(tenantID) || !scopeID(siteID) {
		return nil, "", ErrNotCanonical
	}
	return digest(struct {
		Schema        string `json:"schema"`
		SchemaVersion int    `json:"schema_version"`
		TenantID      string `json:"tenant_id"`
		SiteID        string `json:"site_id"`
		Body          any    `json:"body"`
	}{schema, version, tenantID, siteID, body})
}

// Digest serializes an unscoped request with the same schema-ordered canonical rules as
// Envelope. It is for privileged setup requests that create their own tenant/site scope.
func Digest(schema string, version int, body any) ([]byte, string, error) {
	if schema == "" || strings.TrimSpace(schema) != schema || version < 1 {
		return nil, "", ErrNotCanonical
	}
	return digest(struct {
		Schema        string `json:"schema"`
		SchemaVersion int    `json:"schema_version"`
		Body          any    `json:"body"`
	}{schema, version, body})
}

func digest(value any) ([]byte, string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, "", ErrNotCanonical
	}
	if err = rejectFloats(encoded); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(sum[:]), nil
}

func scopeID(v string) bool {
	id, err := uuid.Parse(v)
	return err == nil && id != uuid.Nil && id.String() == v
}

func rejectFloats(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var walk func(any) error
	walk = func(v any) error {
		switch value := v.(type) {
		case json.Number:
			if strings.ContainsAny(string(value), ".eE") {
				return ErrNotCanonical
			}
		case []any:
			for _, item := range value {
				if err := walk(item); err != nil {
					return err
				}
			}
		case map[string]any:
			for _, item := range value {
				if err := walk(item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	var root any
	if err := decoder.Decode(&root); err != nil {
		return ErrNotCanonical
	}
	return walk(root)
}
