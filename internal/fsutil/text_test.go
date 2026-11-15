package fsutil

import (
	"errors"
	"testing"
)

func TestDecodeText(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
		err  error
	}{
		{"plain", []byte(`{"a":"ü"}`), `{"a":"ü"}`, nil},
		{"utf8 bom", append([]byte{0xEF, 0xBB, 0xBF}, `{"a":1}`...), `{"a":1}`, nil},
		// PowerShell 5.1 Out-File: UTF-16 LE with BOM, "{"ü"}\r\n"
		{"utf16le", []byte{0xFF, 0xFE, '{', 0, '"', 0, 0xFC, 0, '"', 0, '}', 0, '\r', 0, '\n', 0}, "{\"ü\"}\r\n", nil},
		{"utf16be", []byte{0xFE, 0xFF, 0, '[', 0, '1', 0, ']'}, "[1]", nil},
		{"ansi", []byte{'"', 0xFC, '"'}, "", ErrNotUTF8},
		{"empty", nil, "", nil},
	}
	for _, c := range cases {
		got, err := DecodeText(c.in)
		if !errors.Is(err, c.err) || (err == nil && string(got) != c.want) {
			t.Errorf("%s: got %q, %v; want %q, %v", c.name, got, err, c.want, c.err)
		}
	}
	if _, err := DecodeText([]byte{0xFF, 0xFE, 'x'}); err == nil {
		t.Error("odd UTF-16 length accepted")
	}
}
