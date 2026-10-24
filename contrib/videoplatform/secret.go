package videoplatform

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"time"
)

// redacted is what a Secret prints in place of its value.
const redacted = "[redacted]"

// Secret holds a credential (an API token, a webhook signing secret) so
// that it never prints. Formatted directly, or logged through slog, it
// shows a placeholder; held in an unexported field (a Client's config,
// where fmt does not call methods) its value sits behind a pointer, which
// fmt prints as an address. So a logged Client or Config does not leak
// it. The zero Secret is empty.
type Secret struct{ v *[]byte }

// NewSecret returns a Secret holding value; an empty value is the zero
// Secret.
func NewSecret(value string) Secret {
	if value == "" {
		return Secret{}
	}
	b := []byte(value)
	return Secret{v: &b}
}

// IsZero reports whether s is empty.
func (s Secret) IsZero() bool { return s.v == nil }

// Reveal returns a copy of the value, for the request or signature that
// needs it; never log it.
func (s Secret) Reveal() []byte {
	if s.v == nil {
		return nil
	}
	return append([]byte(nil), *s.v...)
}

// String implements fmt.Stringer without the value.
func (s Secret) String() string { return redacted }

// GoString implements fmt.GoStringer without the value.
func (s Secret) GoString() string { return redacted }

// Format implements fmt.Formatter, so no verb or flag reaches the value.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue implements slog.LogValuer without the value.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// DurationOf converts a platform's duration in seconds to a
// time.Duration: zero, negative and NaN are 0 (unknown, as Cloudflare's
// -1), and a figure past the longest Duration is the longest Duration
// rather than a value that wraps negative on conversion.
func DurationOf(seconds float64) time.Duration {
	if !(seconds > 0) {
		return 0
	}
	ns := seconds * float64(time.Second)
	if ns >= math.MaxInt64 {
		return math.MaxInt64
	}
	return time.Duration(ns)
}
