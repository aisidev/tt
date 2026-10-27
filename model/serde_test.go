package model

import "testing"

func TestSerdeJSONEscapesOnlyJSONSyntax(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"<&>\u2028\u2029", "\"<&>\u2028\u2029\""},
		{`\u2028\u003c`, `"\\u2028\\u003c"`},
		{"\n\r\t\x00\"\\", `"\n\r\t\u0000\"\\"`},
		{"\\\u2028", "\"\\\\\u2028\""},
	} {
		got, err := QuoteJSONString(tc.value)
		if err != nil || got != tc.want {
			t.Errorf("%q: %q, %v; want %q", tc.value, got, err, tc.want)
		}
	}
	if _, err := QuoteJSONString(string([]byte{255})); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestNumericMapsUseRustBTreeOrderingForCLIJSON(t *testing.T) {
	data, err := SerdeJSON(Map[uint16, uint16]{443: 20443, 80: 20080, 22: 20022})
	if err != nil || string(data) != `{"22":20022,"80":20080,"443":20443}` {
		t.Fatal(string(data), err)
	}
}
