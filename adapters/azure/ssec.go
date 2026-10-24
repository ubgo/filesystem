package azure

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/ubgo/filesystem"
)

// CustomerKeySize is the length of a customer-provided key (CPK): an
// AES-256 key.
const CustomerKeySize = 32

// redactedKey is what a CustomerKey prints in place of its bytes.
const redactedKey = "azure.CustomerKey{[REDACTED]}"

// CustomerKey is a customer-provided encryption key (CPK): Azure encrypts
// a blob with it and keeps only its hash, so every read, write and
// property call on that blob must present the same key, and a lost key is
// lost data. The bytes sit in an unexported field and every printer (fmt
// verbs, %#v, slog, encoding/json) shows a redacted placeholder, so a key
// logged by mistake, alone or inside a Config, never leaks. Azure accepts
// a CPK only over HTTPS. Build it with NewCustomerKey.
type CustomerKey struct {
	info *blob.CPKInfo
}

// NewCustomerKey checks that key is CustomerKeySize bytes and prepares the
// headers Azure needs (the key and its SHA-256, base64).
func NewCustomerKey(key []byte) (CustomerKey, error) {
	if len(key) != CustomerKeySize {
		return CustomerKey{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: a customer-provided key is %d bytes, got %d", CustomerKeySize, len(key))
	}
	sum := sha256.Sum256(key)
	enc, hash := base64.StdEncoding.EncodeToString(key), base64.StdEncoding.EncodeToString(sum[:])
	algo := blob.EncryptionAlgorithmTypeAES256
	return CustomerKey{info: &blob.CPKInfo{EncryptionKey: &enc, EncryptionKeySHA256: &hash, EncryptionAlgorithm: &algo}}, nil
}

// IsZero reports whether no key is set.
func (k CustomerKey) IsZero() bool { return k.info == nil }

// String implements fmt.Stringer with the redacted placeholder.
func (k CustomerKey) String() string { return redactedKey }

// GoString implements fmt.GoStringer with the redacted placeholder.
func (k CustomerKey) GoString() string { return redactedKey }

// Format implements fmt.Formatter, so every verb prints the redacted
// placeholder.
func (k CustomerKey) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redactedKey) }

// LogValue implements slog.LogValuer with the redacted placeholder.
func (k CustomerKey) LogValue() slog.Value { return slog.StringValue(redactedKey) }

// MarshalText implements encoding.TextMarshaler with the redacted
// placeholder, so JSON or YAML dumps of a Config hide the key too.
func (k CustomerKey) MarshalText() ([]byte, error) { return []byte(redactedKey), nil }

// cpk is Config.CustomerKey for an SDK request, nil when unset.
func (d *Driver) cpk() *blob.CPKInfo { return d.cfg.CustomerKey.info }
