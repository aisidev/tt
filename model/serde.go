package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// SerdeJSON preserves serde_json's string escaping for byte-sensitive generated
// configuration and boot-option witnesses. Struct field order must match Rust;
// numeric maps need their own numeric ordering if used in a byte contract.
func SerdeJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); {
		if data[i] == '\\' && i+1 < len(data) {
			if data[i+1] == 'u' && i+6 <= len(data) {
				var replacement string
				switch string(data[i+2 : i+6]) {
				case "003c":
					replacement = "<"
				case "003e":
					replacement = ">"
				case "0026":
					replacement = "&"
				case "2028":
					replacement = "\u2028"
				case "2029":
					replacement = "\u2029"
				}
				if replacement != "" {
					out = append(out, replacement...)
					i += 6
					continue
				}
			}
			out = append(out, data[i], data[i+1])
			i += 2
			continue
		}
		out = append(out, data[i])
		i++
	}
	return out, nil
}
func QuoteJSONString(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("invalid UTF-8 string")
	}
	data, err := SerdeJSON(value)
	return string(data), err
}

// DecodeJSONValue follows serde's Unicode rejection while retaining integer
// precision in unknown fields that an offline database conversion must preserve.
func DecodeJSONValue(data []byte) (any, error) {
	if err := validStrings(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}
