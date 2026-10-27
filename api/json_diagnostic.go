package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aisidev/tt/model"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

type jsonMember struct {
	name               string
	raw                []byte
	start, end, keyEnd int
}

func jsonMembers(data []byte) []jsonMember {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil
	}
	members := []jsonMember{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return members
		}
		name, ok := token.(string)
		if !ok {
			return members
		}
		keyEnd := int(decoder.InputOffset())
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return members
		}
		end := int(decoder.InputOffset())
		start := keyEnd + bytes.Index(data[keyEnd:end], raw)
		members = append(members, jsonMember{name, raw, start, end, keyEnd})
	}
	return members
}
func indirectType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}
func fieldType(t reflect.Type, name string) reflect.Type {
	t = indirectType(t)
	if t.Kind() != reflect.Struct {
		return t
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if strings.Split(f.Tag.Get("json"), ",")[0] == name {
			return f.Type
		}
	}
	return t
}
func expectedType(t reflect.Type) string {
	t = indirectType(t)
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Uint16:
		return "u16"
	case reflect.Uint32:
		return "u32"
	case reflect.Uint64:
		return "u64"
	case reflect.Slice, reflect.Array:
		return "a sequence"
	case reflect.Map:
		return "a map"
	case reflect.Struct:
		return "struct " + t.Name()
	default:
		return t.String()
	}
}
func diagnosticPath(parent, field string) string {
	if parent == "" {
		return field
	}
	return parent + "." + field
}
func prefixPath(path, message string) string {
	if path == "" {
		return message
	}
	return path + ": " + message
}
func quotedErrorField(message, prefix string) (string, bool) {
	if !strings.HasPrefix(message, prefix) {
		return "", false
	}
	field, err := strconv.Unquote(strings.TrimPrefix(message, prefix))
	return field, err == nil
}
func variantNames(t reflect.Type) []string {
	switch t.Name() {
	case "Engine":
		return []string{"qemu", "firecracker", "bhyve", "docker", "jail"}
	case "Storage":
		return []string{"file", "zvol"}
	case "VmState":
		return []string{"running", "stopped", "paused", "creating", "failed", "deleting"}
	case "EnvState":
		return []string{"active", "stopped", "creating", "deleting", "failed"}
	case "HostState":
		return []string{"online", "offline"}
	case "BackupAction":
		return []string{"create", "restore", "delete"}
	case "BackupBackend":
		return []string{"zvol", "reflink"}
	default:
		return nil
	}
}
func expectedNames(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = "`" + name + "`"
	}
	if len(quoted) == 1 {
		return "expected " + quoted[0]
	}
	if len(quoted) == 2 {
		return "expected " + quoted[0] + " or " + quoted[1]
	}
	return "expected one of " + strings.Join(quoted, ", ")
}
func dataDiagnostic(data []byte, t reflect.Type, err error, path string, base int) (string, int, int) {
	t = indirectType(t)
	message := err.Error()
	if strings.HasPrefix(message, "field ") {
		field, _, ok := strings.Cut(strings.TrimPrefix(message, "field "), ": ")
		if ok {
			for _, m := range jsonMembers(data) {
				if m.name == field {
					cause := errors.Unwrap(err)
					if cause == nil {
						cause = err
					}
					return dataDiagnostic(m.raw, fieldType(t, field), cause, diagnosticPath(path, field), base+m.start)
				}
			}
		}
	}
	if t.Kind() == reflect.Struct && len(data) > 0 && data[0] == '[' {
		var items []json.RawMessage
		_ = json.Unmarshal(data, &items)
		if strings.HasPrefix(message, "missing ") || message == "too many sequence fields" {
			return prefixPath(path, fmt.Sprintf("invalid length %d, expected struct %s with %d elements", len(items), t.Name(), positionalFieldCount(t))), base + len(data), 422
		}
	}
	if field, ok := quotedErrorField(message, "missing field "); ok {
		return prefixPath(path, "missing field `"+field+"`"), base + len(data), 422
	}
	if field, ok := quotedErrorField(message, "duplicate field "); ok {
		seen := false
		for _, m := range jsonMembers(data) {
			if m.name == field {
				if seen {
					return prefixPath(path, "duplicate field `"+field+"`"), base + m.keyEnd, 422
				}
				seen = true
			}
		}
	}
	if field, ok := quotedErrorField(message, "unknown field "); ok {
		names := []string{}
		if t.Kind() == reflect.Struct {
			for i := 0; i < t.NumField(); i++ {
				names = append(names, strings.Split(t.Field(i).Tag.Get("json"), ",")[0])
			}
		}
		for _, m := range jsonMembers(data) {
			if m.name == field {
				return prefixPath(diagnosticPath(path, field), "unknown field `"+field+"`, "+expectedNames(names)), base + m.keyEnd, 422
			}
		}
	}
	if strings.Contains(message, "surrogate") {
		return prefixPath(path, "unexpected end of hex escape"), base + len(data), 400
	}
	if names := variantNames(t); names != nil {
		var name string
		if len(data) > 0 && data[0] == '"' && json.Unmarshal(data, &name) == nil {
			return prefixPath(path, "unknown variant `"+name+"`, "+expectedNames(names)), base + len(data), 422
		}
		if len(data) > 0 && data[0] == '{' {
			members := jsonMembers(data)
			if len(members) > 0 {
				first := members[0]
				known := false
				for _, n := range names {
					known = known || n == first.name
				}
				if !known {
					return prefixPath(path, "unknown variant `"+first.name+"`, "+expectedNames(names)), base + first.keyEnd, 422
				}
				if string(first.raw) != "null" {
					unexpected, consumed := unexpectedJSON(first.raw)
					return prefixPath(diagnosticPath(path, first.name), "invalid type: "+unexpected+", expected unit"), base + first.start + consumed, 422
				}
				return prefixPath(path, "expected value"), base + first.end, 400
			}
			return prefixPath(path, "expected value"), base + min(2, len(data)), 400
		}
		return prefixPath(path, "expected value"), base + min(1, len(data)), 400
	}
	if (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) && len(data) > 0 && data[0] == '[' {
		decoder := json.NewDecoder(bytes.NewReader(data))
		_, _ = decoder.Token()
		index := 0
		for decoder.More() {
			start := int(decoder.InputOffset())
			var raw json.RawMessage
			if decoder.Decode(&raw) != nil {
				break
			}
			end := int(decoder.InputOffset())
			start += bytes.Index(data[start:end], raw)
			if nested := fragmentError(raw, t.Elem()); nested != nil {
				return dataDiagnostic(raw, t.Elem(), nested, fmt.Sprintf("%s[%d]", path, index), base+start)
			}
			index++
		}
	}
	if t.Kind() == reflect.Map && len(data) > 0 && data[0] == '{' {
		for _, m := range jsonMembers(data) {
			if nested := fragmentError(m.raw, t.Elem()); nested != nil {
				return dataDiagnostic(m.raw, t.Elem(), nested, diagnosticPath(path, m.name), base+m.start)
			}
		}
	}
	unexpected, consumed := unexpectedJSON(data)
	category := "invalid type"
	if t.Kind() >= reflect.Uint && t.Kind() <= reflect.Uint64 && strings.HasPrefix(unexpected, "integer ") {
		category = "invalid value"
	}
	return prefixPath(path, category+": "+unexpected+", expected "+expectedType(t)), base + consumed, 422
}
func unexpectedJSON(data []byte) (string, int) {
	if len(data) == 0 {
		return "null", 0
	}
	switch data[0] {
	case '{':
		return "map", 0
	case '[':
		return "sequence", 0
	case 'n':
		return "null", len(data)
	case 't', 'f':
		return "boolean `" + string(data) + "`", len(data)
	case '"':
		var s string
		_ = json.Unmarshal(data, &s)
		return fmt.Sprintf("string %q", s), len(data)
	default:
		value := string(data)
		if strings.ContainsAny(value, ".eE") {
			if n, err := strconv.ParseFloat(value, 64); err == nil {
				value = strconv.FormatFloat(n, 'g', -1, 64)
				if !strings.ContainsAny(value, ".eE") {
					value += ".0"
				}
			}
			return "floating point `" + value + "`", len(data)
		}
		return "integer `" + value + "`", len(data)
	}
}

var unfinishedField = regexp.MustCompile(`"([^"\\]*)"\s*:\s*$`)

func syntaxDiagnostic(data []byte, err error) (string, int) {
	trimmed := bytes.TrimSpace(data)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		if match := unfinishedField.FindSubmatch(data); match != nil {
			return string(match[1]) + ": EOF while parsing a value", len(data)
		}
		kind := "value"
		if len(trimmed) > 0 {
			switch trimmed[0] {
			case '{':
				kind = "object"
			case '[':
				kind = "list"
			case '"':
				kind = "string"
			}
		}
		article := "a "
		if kind == "object" {
			article = "an "
		}
		return "EOF while parsing " + article + kind, len(data)
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		offset := int(syntax.Offset)
		if offset > 0 && offset <= len(data) {
			c := data[offset-1]
			before := bytes.TrimSpace(data[:offset-1])
			if (c == '}' || c == ']') && len(before) > 0 && before[len(before)-1] == ',' {
				return "trailing comma", offset
			}
		}
		return "expected value", offset
	}
	return err.Error(), len(data)
}

func fragmentError(data []byte, t reflect.Type) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) && t.Kind() != reflect.Pointer {
		return fmt.Errorf("unexpected null")
	}
	if indirectType(t).Kind() == reflect.String {
		if _, err := model.DecodeJSONValue(data); err != nil {
			return err
		}
	}
	return json.Unmarshal(data, reflect.New(t).Interface())
}

// Serde reports an already-observed field type error before a later EOF. This
// streaming precheck is used only when buffering a complete JSON value failed;
// it never admits a request or substitutes for the authoritative model codec.
func earlyDataDiagnostic(data []byte, t reflect.Type, path string, base int) (string, int, int, bool) {
	t = indirectType(t)
	if t.Kind() != reflect.Struct {
		return "", 0, 0, false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || (token != json.Delim('{') && token != json.Delim('[')) {
		return "", 0, 0, false
	}
	index := 0
	for d.More() {
		var typ reflect.Type
		var childPath string
		if token == json.Delim('{') {
			key, err := d.Token()
			if err != nil {
				return "", 0, 0, false
			}
			name, ok := key.(string)
			if !ok {
				return "", 0, 0, false
			}
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				if strings.Split(field.Tag.Get("json"), ",")[0] == name {
					typ = field.Type
					break
				}
			}
			childPath = diagnosticPath(path, name)
		} else {
			if index >= t.NumField() {
				return "", 0, 0, false
			}
			typ = t.Field(index).Type
			childPath = fmt.Sprintf("%s[%d]", path, index)
			index++
		}
		start := int(d.InputOffset())
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			if typ != nil {
				for start < len(data) && (data[start] == ':' || data[start] == ',' || data[start] == ' ' || data[start] == '\n' || data[start] == '\r' || data[start] == '\t') {
					start++
				}
				return earlyDataDiagnostic(data[start:], typ, childPath, base+start)
			}
			return "", 0, 0, false
		}
		end := int(d.InputOffset())
		start += bytes.Index(data[start:end], raw)
		if typ != nil {
			if err := fragmentError(raw, typ); err != nil {
				reason, offset, status := dataDiagnostic(raw, typ, err, childPath, base+start)
				return reason, offset, status, true
			}
		}
	}
	return "", 0, 0, false
}

func positionalFieldCount(t reflect.Type) int {
	switch t.Name() {
	case "CreateVmReq":
		return 13
	case "VmSpec":
		return 11
	case "VmOptions":
		return 7
	case "EnvDetail":
		return 3
	case "AgentInfo":
		return 10
	case "Host":
		return 12
	case "Env":
		return 7
	}
	if strings.HasPrefix(t.Name(), "Resp[") {
		return 3
	}
	return t.NumField()
}
