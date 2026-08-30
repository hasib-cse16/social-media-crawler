package tiktok

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// TikTok is inconsistent about whether a numeric field arrives as a JSON number
// or a quoted string: createTime has been observed both ways, and within a
// single stats object collectCount is a string while playCount is a number.
// These types accept either form so a type flip upstream cannot break parsing.

// flexUint64 decodes an unsigned integer from a JSON number or string.
type flexUint64 struct {
	Value uint64
	Set   bool
}

func (f *flexUint64) UnmarshalJSON(data []byte) error {
	raw := unquoteJSON(data)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	n, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil {
		// Counters are best-effort: a value we cannot read is reported as absent
		// rather than failing the whole response.
		return nil
	}
	f.Value, f.Set = n, true
	return nil
}

// flexInt64 decodes a signed integer from a JSON number or string.
type flexInt64 struct {
	Value int64
	Set   bool
}

func (f *flexInt64) UnmarshalJSON(data []byte) error {
	raw := unquoteJSON(data)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return fmt.Errorf("flexInt64: %q is neither a number nor a numeric string", raw)
	}
	f.Value, f.Set = n, true
	return nil
}

// unquoteJSON strips surrounding quotes from a JSON string token, leaving
// numbers untouched.
func unquoteJSON(data []byte) []byte {
	data = bytes.TrimSpace(data)
	if len(data) >= 2 && data[0] == '"' && data[len(data)-1] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err == nil {
			return []byte(s)
		}
		return data[1 : len(data)-1]
	}
	return data
}

// ptr returns the value as a pointer, or nil when it was absent.
func (f flexUint64) ptr() *uint64 {
	if !f.Set {
		return nil
	}
	v := f.Value
	return &v
}
