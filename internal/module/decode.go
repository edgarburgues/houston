package module

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// DecodeReply is the hardened reply decoder shared by every surface. The
// traps are PowerShell's: a silent UTF-8 BOM from default file encodings, or
// full UTF-16 from PS 5.1 `>` redirection. Policy: BOM stripped silently,
// UTF-16 rejected with the actionable message, leading whitespace/CRLF
// trimmed, EMPTY stdout is a valid no-op (v is left untouched: zero patches,
// no sections, hidden segment, generic "done"), one JSON object decoded,
// trailing bytes tolerated (a stray Write-Host after the object), leading
// garbage rejected. Unknown reply fields are ignored by encoding/json;
// wrong-typed known fields error.
func DecodeReply(raw []byte, v any) error {
	b := bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if len(b) >= 2 && ((b[0] == 0xFF && b[1] == 0xFE) || (b[0] == 0xFE && b[1] == 0xFF)) {
		return errors.New("reply is UTF-16, emit UTF-8")
	}
	b = bytes.TrimLeft(b, " \t\r\n")
	if len(b) == 0 {
		return nil
	}
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(v); err != nil {
		return fmt.Errorf("invalid reply: %v (starts %q)", err, snippet(b, 200))
	}
	return nil
}

// snippet returns the first n bytes of b, cut on a rune boundary.
func snippet(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	for n > 0 && !utf8.RuneStart(b[n]) {
		n--
	}
	return string(b[:n])
}
