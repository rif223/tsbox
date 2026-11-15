package fsutil

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"unicode/utf16"
	"unicode/utf8"
)

// ErrNotUTF8 reports a text file that is neither UTF-8 nor UTF-16 with a BOM (e.g. ANSI from PowerShell 5.1
// "Set-Content" without -Encoding).
var ErrNotUTF8 = errors.New("not UTF-8 text (save it as UTF-8, e.g. PowerShell: Set-Content -Encoding utf8)")

// DecodeText returns b as UTF-8 without a BOM. Windows tools write a UTF-8 BOM (PowerShell 5.1
// "Set-Content -Encoding utf8") or UTF-16 with a BOM ("Out-File", ">"); both are accepted.
func DecodeText(b []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		b = b[3:]
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return decodeUTF16(b[2:], binary.LittleEndian)
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return decodeUTF16(b[2:], binary.BigEndian)
	}
	if !utf8.Valid(b) {
		return nil, ErrNotUTF8
	}
	return b, nil
}

func decodeUTF16(b []byte, order binary.ByteOrder) ([]byte, error) {
	if len(b)%2 != 0 {
		return nil, errors.New("broken UTF-16 text (odd length)")
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = order.Uint16(b[2*i:])
	}
	return []byte(string(utf16.Decode(u))), nil
}

// ReadText reads a text file with DecodeText.
func ReadText(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecodeText(b)
}
