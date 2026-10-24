package vision

import "log/slog"

// redacted is what a Secret prints in place of its value.
const redacted = "[redacted]"

// Secret holds a labeler's credential (an API key) so that it never
// prints. Formatted directly, or logged through slog, it shows a
// placeholder; held in an unexported field (a client's config, where fmt
// does not call methods) its value sits behind a pointer, which fmt
// prints as an address. So a logged client or config does not leak it.
// The zero Secret is empty.
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

// Reveal returns the value, for the request that needs it; never log it.
func (s Secret) Reveal() string {
	if s.v == nil {
		return ""
	}
	return string(*s.v)
}

// String implements fmt.Stringer without the value.
func (s Secret) String() string { return redacted }

// GoString implements fmt.GoStringer without the value.
func (s Secret) GoString() string { return redacted }

// LogValue implements slog.LogValuer without the value.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }
