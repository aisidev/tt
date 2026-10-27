package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// List and Map preserve serde's empty collection representation even for Go's
// zero value. Null is never a collection; optional collections use pointers.
type List[T any] []T

func (v List[T]) MarshalJSON() ([]byte, error) {
	if v == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]T(v))
}
func (v *List[T]) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("expected an array, found null")
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	result := make(List[T], len(raw))
	for i := range raw {
		if err := decodeValue(raw[i], &result[i]); err != nil {
			return err
		}
	}
	*v = result
	return nil
}

type Map[K comparable, V any] map[K]V

func (v Map[K, V]) MarshalJSON() ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	kind := reflect.TypeFor[K]().Kind()
	if kind >= reflect.Uint && kind <= reflect.Uint64 {
		keys := make([]K, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return reflect.ValueOf(keys[i]).Uint() < reflect.ValueOf(keys[j]).Uint() })
		out := []byte{'{'}
		for i, key := range keys {
			if i > 0 {
				out = append(out, ',')
			}
			name := strconv.FormatUint(reflect.ValueOf(key).Uint(), 10)
			out = append(out, '"')
			out = append(out, name...)
			out = append(out, '"', ':')
			value, err := json.Marshal(v[key])
			if err != nil {
				return nil, err
			}
			out = append(out, value...)
		}
		return append(out, '}'), nil
	}
	return json.Marshal(map[K]V(v))
}
func (v *Map[K, V]) UnmarshalJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return fmt.Errorf("expected an object")
	}
	result := make(Map[K, V])
	for d.More() {
		start := d.InputOffset()
		if _, err := d.Token(); err != nil {
			return err
		}
		keyData := bytes.TrimLeft(data[start:d.InputOffset()], " \t\n\r,")
		if err := validStrings(keyData); err != nil {
			return err
		}
		kind := reflect.TypeFor[K]().Kind()
		if kind >= reflect.Uint && kind <= reflect.Uint64 {
			// serde_json parses numeric map keys from the raw JSON digits;
			// leading zeroes, signs and escaped digits are not accepted.
			digits := keyData[1 : len(keyData)-1]
			if len(digits) == 0 || (len(digits) > 1 && digits[0] == '0') {
				return fmt.Errorf("invalid integer map key")
			}
			for _, c := range digits {
				if c < '0' || c > '9' {
					return fmt.Errorf("invalid integer map key")
				}
			}
		}
		// Preserve numeric-key validation instead of accepting arbitrary port strings.
		one := append([]byte{'{'}, keyData...)
		one = append(one, ':', '0', '}')
		var keys map[K]json.RawMessage
		if err := json.Unmarshal(one, &keys); err != nil {
			return err
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return err
		}
		var value V
		if err := decodeValue(raw, &value); err != nil {
			return err
		}
		for key := range keys {
			result[key] = value
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("trailing JSON value")
	}
	*v = result
	return nil
}

// DecodeRecord implements serde field presence and spelling rules. Fields with
// defaults are initialized by the caller before decoding; all other non-optional
// fields appear in required. It also handles nested custom codecs consistently.
func DecodeRecord(data []byte, dst any, required []string, strict bool, sequenceRequired ...string) error {
	if !json.Valid(data) {
		return fmt.Errorf("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') && token != json.Delim('[') {
		return fmt.Errorf("expected an object or sequence")
	}
	value := reflect.ValueOf(dst).Elem()
	typ := value.Type()
	fields := make(map[string]int, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			fields[name] = i
		}
	}
	seen := map[string]bool{}
	if token == json.Delim('[') {
		index := 0
		for d.More() {
			if index >= typ.NumField() {
				return fmt.Errorf("too many sequence fields")
			}
			var raw json.RawMessage
			if err := d.Decode(&raw); err != nil {
				return err
			}
			if err := decodeValue(raw, value.Field(index).Addr().Interface()); err != nil {
				return err
			}
			name := strings.Split(typ.Field(index).Tag.Get("json"), ",")[0]
			seen[name] = true
			index++
		}
		for _, name := range sequenceRequired {
			if !seen[name] {
				return fmt.Errorf("missing sequence field %q", name)
			}
		}
	}
	for token == json.Delim('{') && d.More() {
		start := d.InputOffset()
		keyToken, err := d.Token()
		if err != nil {
			return err
		}
		keyData := bytes.TrimLeft(data[start:d.InputOffset()], " \t\n\r,")
		if err := validStrings(keyData); err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("expected field name")
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return err
		}
		i, known := fields[key]
		if !known {
			if strict {
				return fmt.Errorf("unknown field %q", key)
			}
			continue
		}
		if seen[key] {
			return fmt.Errorf("duplicate field %q", key)
		}
		seen[key] = true
		if err := decodeValue(raw, value.Field(i).Addr().Interface()); err != nil {
			return fmt.Errorf("field %s: %w", key, err)
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("trailing JSON value")
	}
	for _, name := range required {
		if !seen[name] {
			return fmt.Errorf("missing field %q", name)
		}
	}
	return nil
}

func decodeValue(data []byte, dst any) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) && reflect.ValueOf(dst).Elem().Kind() != reflect.Pointer {
		return fmt.Errorf("unexpected null")
	}
	typ := reflect.TypeOf(dst).Elem()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() == reflect.String {
		if err := validStrings(data); err != nil {
			return err
		}
	}
	return json.Unmarshal(data, dst)
}

// encoding/json replaces malformed Unicode with U+FFFD; serde rejects it.
func validStrings(data []byte) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("invalid UTF-8")
	}
	if !json.Valid(data) {
		return fmt.Errorf("invalid JSON")
	}
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if data[i] != 'u' {
			continue
		}
		n, _ := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return fmt.Errorf("unpaired Unicode surrogate")
		}
		if n < 0xd800 || n > 0xdbff {
			continue
		}
		if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
			return fmt.Errorf("unpaired Unicode surrogate")
		}
		low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return fmt.Errorf("unpaired Unicode surrogate")
		}
		i += 6
	}
	return nil
}

// Serde's externally tagged unit enums accept a canonical string or exactly one
// canonical variant key with a null value. Serialization always emits a string.
func decodeUnitVariant(data []byte) (string, error) {
	if err := validStrings(data); err != nil {
		return "", err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil {
		return "", err
	}
	if name, ok := token.(string); ok {
		return name, nil
	}
	if token != json.Delim('{') || !d.More() {
		return "", fmt.Errorf("expected enum variant")
	}
	token, err = d.Token()
	if err != nil {
		return "", err
	}
	name, ok := token.(string)
	if !ok {
		return "", fmt.Errorf("expected enum variant")
	}
	var value json.RawMessage
	if err := d.Decode(&value); err != nil {
		return "", err
	}
	if !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return "", fmt.Errorf("expected unit variant")
	}
	if d.More() {
		return "", fmt.Errorf("expected a single enum variant")
	}
	if _, err := d.Token(); err != nil {
		return "", err
	}
	return name, nil
}
