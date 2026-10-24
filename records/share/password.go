package share

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/ubgo/filesystem"
)

// Password hashing. A link's password is stored as
// "pbkdf2-sha256$<iterations>$<salt>$<key>" (salt and key in unpadded
// base64url), so the iteration count can grow later without breaking
// links made before: each hash carries its own.
const (
	passwordScheme = "pbkdf2-sha256"
	// passwordIterations follows OWASP's 2023 guidance for PBKDF2-SHA256.
	passwordIterations = 600_000
	// maxPasswordIterations bounds what a stored hash may ask for, so a
	// corrupted row cannot make an attempt run for minutes.
	maxPasswordIterations = 10 * passwordIterations
	passwordSaltBytes     = 16
	passwordKeyBytes      = 32
	// MaxPasswordBytes bounds a password, so a huge one cannot make every
	// attempt expensive.
	MaxPasswordBytes = 256
)

// passwordSep joins the fields of a stored hash.
const passwordSep = "$"

// The positions of a stored hash's fields, and hashFields their count.
// checkPassword refuses a hash with any other field count, so a new field
// means a new scheme name rather than a silently misread hash.
const (
	hashScheme = iota
	hashIterations
	hashSalt
	hashKey
	hashFields
)

// ErrPassword is the error of a link that needs a password, when none or
// a wrong one was given (CodePermission). It spends nothing. The Handler
// answers it with 401 and counts the attempt against its limiter.
var ErrPassword = filesystem.Errorf(filesystem.CodePermission, "share: this link needs its password")

// AccessOption is what a visitor presents to use a link.
type AccessOption func(*access)

type access struct{ password string }

func accessOf(opts []AccessOption) access {
	var a access
	for _, o := range opts {
		o(&a)
	}
	return a
}

// WithPassword presents a link's password.
func WithPassword(password string) AccessOption {
	return func(a *access) { a.password = password }
}

// hashPassword is the stored form of password.
func hashPassword(password string) (string, error) {
	if len(password) > MaxPasswordBytes {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "share: a password is at most %d bytes", MaxPasswordBytes)
	}
	salt := make([]byte, passwordSaltBytes)
	_, _ = rand.Read(salt) // crypto/rand.Read never fails (it aborts the process instead)
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordKeyBytes)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return strings.Join([]string{passwordScheme, strconv.Itoa(passwordIterations), enc.EncodeToString(salt), enc.EncodeToString(key)}, passwordSep), nil
}

// checkPassword reports whether password matches the stored hash, in
// constant time for the key comparison. A malformed hash matches nothing.
func checkPassword(stored, password string) bool {
	parts := strings.Split(stored, passwordSep)
	if len(parts) != hashFields || parts[hashScheme] != passwordScheme || password == "" || len(password) > MaxPasswordBytes {
		return false
	}
	iter, err := strconv.Atoi(parts[hashIterations])
	enc := base64.RawURLEncoding
	salt, err2 := enc.DecodeString(parts[hashSalt])
	want, err3 := enc.DecodeString(parts[hashKey])
	if err != nil || err2 != nil || err3 != nil || iter < 1 || iter > maxPasswordIterations {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}
