package postgres

import (
	"fmt"
	"strconv"
	"time"
)

// Conversions between the domain's types and what Postgres actually stores.
//
// Counters are *uint64 in the domain because the providers report them that
// way, and bigint in the database because Postgres has no unsigned integer
// type. The columns carry CHECK (>= 0) constraints, so reading one back can
// never produce a negative, and the conversions below are total rather than
// lossy in practice.

// countArg renders a domain counter for a query argument. nil stays nil, so an
// unreported counter is written as NULL rather than 0.
func countArg(v *uint64) *int64 {
	if v == nil {
		return nil
	}
	// Above math.MaxInt64 the value is not storable. No real counter is within
	// nine orders of magnitude of this, so clamping quietly is safer than
	// failing the whole write: the alternative is losing a video's entire
	// snapshot because one field was absurd.
	const maxInt64 = uint64(1)<<63 - 1
	n := *v
	if n > maxInt64 {
		n = maxInt64
	}
	i := int64(n)
	return &i
}

// countValue converts a column back into a domain counter.
func countValue(v *int64) *uint64 {
	if v == nil {
		return nil
	}
	if *v < 0 {
		// Only reachable if a CHECK constraint were dropped. Report absent
		// rather than wrapping around to something enormous.
		return nil
	}
	n := uint64(*v)
	return &n
}

// secondsToDuration converts an EXTRACT(EPOCH FROM interval) result.
//
// Intervals are read as seconds in SQL rather than scanned as pgtype.Interval
// because an interval's month component has no fixed length, and a Duration
// implies one. Every interval this schema stores is measured in hours, so the
// conversion is exact — and doing it in SQL keeps that assumption in one place.
func secondsToDuration(seconds int64) time.Duration {
	return time.Duration(seconds) * time.Second
}

// nullableString flattens a nullable text column.
func nullableString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// emptyToNil renders an empty string as NULL, so "no error recorded" is a NULL
// rather than an empty string that looks like a recorded empty error.
func emptyToNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// errNoRowsAffected reports that a write matched nothing, which for an update
// keyed on a primary key means the row is gone.
func errNoRowsAffected(what string, id any) error {
	return fmt.Errorf("%s %v: no such row", what, id)
}

// strconvSeconds renders a Duration as a whole number of seconds.
func strconvSeconds(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Second), 10)
}
