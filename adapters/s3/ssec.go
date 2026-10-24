package s3

import (
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/ubgo/filesystem"
)

// customerKeySize is the length of an SSE-C key: S3 encrypts with AES-256.
const customerKeySize = 32

// customerKeyAlgorithm is the only SSE-C algorithm S3 accepts.
const customerKeyAlgorithm = "AES256"

// redactedCustomerKey is what a CustomerKey prints in place of its bytes.
const redactedCustomerKey = "s3.CustomerKey{[redacted]}"

// CustomerKey is an SSE-C key: a 256-bit key the application keeps and S3
// never stores, sent with every request that writes or reads an object's
// bytes. Losing it loses the objects written with it.
//
// It never formats its bytes, the same way the DSN's static credentials do:
// printed directly (any verb, %+v and %#v included) it shows a placeholder,
// and inside the Driver's unexported configuration the bytes sit behind a
// pointer, which fmt prints as an address. A logged Config or Driver does
// not leak it. The zero CustomerKey means none.
type CustomerKey struct{ key *[customerKeySize]byte }

// NewCustomerKey returns a CustomerKey for raw, which must be exactly 32
// bytes (CodeInvalidArgument otherwise; the error never contains the key).
// raw is copied.
func NewCustomerKey(raw []byte) (CustomerKey, error) {
	if len(raw) != customerKeySize {
		return CustomerKey{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: an SSE-C key is %d bytes, not %d", customerKeySize, len(raw))
	}
	var k [customerKeySize]byte
	copy(k[:], raw)
	return CustomerKey{key: &k}, nil
}

// IsZero reports whether no key is set.
func (k CustomerKey) IsZero() bool { return k.key == nil }

// String implements fmt.Stringer without the key.
func (k CustomerKey) String() string { return redactedCustomerKey }

// GoString implements fmt.GoStringer without the key.
func (k CustomerKey) GoString() string { return redactedCustomerKey }

// Format implements fmt.Formatter, so no verb or flag reaches the bytes.
func (k CustomerKey) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redactedCustomerKey) }

// customerHeaders are the three SSE-C request fields: the algorithm, the
// base64 key, and the base64 MD5 of the key, which S3 uses to check the key
// arrived intact (the protocol fixes MD5; it protects nothing secret). All
// nil without a key.
type customerHeaders struct{ algorithm, key, keyMD5 *string }

// headers renders k for a request.
func (k CustomerKey) headers() customerHeaders {
	if k.key == nil {
		return customerHeaders{}
	}
	sum := md5.Sum(k.key[:])
	return customerHeaders{
		algorithm: aws.String(customerKeyAlgorithm),
		key:       aws.String(base64.StdEncoding.EncodeToString(k.key[:])),
		keyMD5:    aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	}
}
