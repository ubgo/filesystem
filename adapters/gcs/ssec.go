package gcs

import (
	"fmt"
	"io"
	"log/slog"

	"cloud.google.com/go/storage"

	"github.com/ubgo/filesystem"
)

// CustomerKeySize is the length of a customer-supplied encryption key
// (CSEK): an AES-256 key.
const CustomerKeySize = 32

// redactedKey is what a CustomerKey prints in place of its bytes.
const redactedKey = "gcs.CustomerKey{[REDACTED]}"

// CustomerKey is a customer-supplied encryption key (CSEK): GCS encrypts an
// object with it and keeps only its hash, so every read, write, Stat and
// copy of that object must present the same key, and a lost key is lost
// data. The bytes sit in an unexported field and every printer (fmt verbs,
// %#v, slog, encoding/json) shows a redacted placeholder, so a key logged
// by mistake, alone or inside a Config, never leaks. Build it with
// NewCustomerKey.
type CustomerKey struct {
	key []byte
}

// NewCustomerKey checks that key is CustomerKeySize bytes and copies it.
func NewCustomerKey(key []byte) (CustomerKey, error) {
	if len(key) != CustomerKeySize {
		return CustomerKey{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: a customer-supplied key is %d bytes, got %d", CustomerKeySize, len(key))
	}
	return CustomerKey{key: append([]byte(nil), key...)}, nil
}

// IsZero reports whether no key is set.
func (k CustomerKey) IsZero() bool { return len(k.key) == 0 }

// String implements fmt.Stringer with the redacted placeholder.
func (k CustomerKey) String() string { return redactedKey }

// GoString implements fmt.GoStringer with the redacted placeholder.
func (k CustomerKey) GoString() string { return redactedKey }

// Format implements fmt.Formatter, so every verb (%x and %v included)
// prints the redacted placeholder.
func (k CustomerKey) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redactedKey) }

// LogValue implements slog.LogValuer with the redacted placeholder.
func (k CustomerKey) LogValue() slog.Value { return slog.StringValue(redactedKey) }

// MarshalText implements encoding.TextMarshaler with the redacted
// placeholder, so JSON or YAML dumps of a Config hide the key too.
func (k CustomerKey) MarshalText() ([]byte, error) { return []byte(redactedKey), nil }

// object is the handle every call on key uses: with Config.CustomerKey it
// carries the key, so GCS can encrypt or decrypt the object.
func (d *Driver) object(key string) *storage.ObjectHandle {
	o := d.bucket.Object(key)
	if !d.cfg.CustomerKey.IsZero() {
		o = o.Key(d.cfg.CustomerKey.key)
	}
	return o
}
