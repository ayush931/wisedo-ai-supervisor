// Package strictjson rejects ambiguous or permissive JSON at trust boundaries.
package strictjson

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"reflect"
	"strconv"
	"unicode/utf8"
)

var ErrInvalid = errors.New("invalid strict JSON")

// Decode enforces a byte bound, UTF-8, unique keys, known struct fields and one
// complete JSON value. The destination's UnmarshalJSON method may add rules.
func Decode(data []byte, dst any, maxBytes int) error {
	if maxBytes < 1 || len(data) == 0 || len(data) > maxBytes || !utf8.Valid(data) || !validUnicodeEscapes(data) || duplicateKey(data) != nil {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return ErrInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrInvalid
	}
	return exactNames(data, dst)
}

// exactNames rejects member names that encoding/json (v1) accepted only because it matches
// struct fields case-insensitively: {"TENANT_ID": ...} is not the field "tenant_id", and
// {"token":"a","TOKEN":"b"} is not one key. encoding/json/v2 matches names exactly, so
// the same document must also decode under v2 with unknown members rejected.
func exactNames(data []byte, dst any) error {
	t := reflect.TypeOf(dst)
	if t == nil || t.Kind() != reflect.Pointer {
		return ErrInvalid
	}
	if jsonv2.Unmarshal(data, reflect.New(t.Elem()).Interface(), jsonv2.RejectUnknownMembers(true)) != nil {
		return ErrInvalid
	}
	return nil
}

func validUnicodeEscapes(data []byte) bool {
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		value, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil || value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value >= 0xd800 && value <= 0xdbff {
			if i+10 >= len(data) || data[i+5] != '\\' || data[i+6] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+7:i+11]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 10
		} else {
			i += 4
		}
	}
	return true
}

func duplicateKey(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value func() error
	value = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return ErrInvalid
				}
				if _, exists := seen[key]; exists {
					return ErrInvalid
				}
				seen[key] = struct{}{}
				if err := value(); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := value(); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		default:
			return ErrInvalid
		}
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrInvalid
	}
	return nil
}
