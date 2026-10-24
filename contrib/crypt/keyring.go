package crypt

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"log/slog"
	"sync"

	"github.com/ubgo/filesystem"
)

// WrappedKey is an object's data key encrypted under a master key, as
// stored in the object's header. KeyID names the master key so the
// object can be decrypted after the current key has moved on.
type WrappedKey struct {
	KeyID      string
	Ciphertext []byte
}

// Keyring wraps and unwraps per-object data keys. It is where key policy
// lives: which master key an object gets (one for the app, one per
// tenant, a customer's own), and where master keys are kept.
//
// WrapKey must wrap with the master key that is current for objectKey;
// UnwrapKey must accept any master key a stored object may still name, so
// rotation can proceed object by object. Implementations must never log or
// return the data key or a master key in an error.
type Keyring interface {
	WrapKey(ctx context.Context, objectKey string, dataKey []byte) (WrappedKey, error)
	UnwrapKey(ctx context.Context, objectKey string, w WrappedKey) ([]byte, error)
}

// KMS is the shape of a key management service's encrypt and decrypt
// calls (AWS KMS, Google Cloud KMS, Azure Key Vault, HashiCorp Vault
// transit): small payloads encrypted under a key the service holds and
// never exports, with additional data that must match on decrypt. An
// adapter over a real service is a few lines in the application; MemoryKMS
// is the in-process implementation.
type KMS interface {
	Encrypt(ctx context.Context, keyID string, plaintext, aad []byte) ([]byte, error)
	Decrypt(ctx context.Context, keyID string, ciphertext, aad []byte) ([]byte, error)
}

// KeyIDFunc chooses the master key for an object: a constant for one key,
// or one derived from the key's tenant segment for per-tenant keys
// (bring-your-own-key keeps each tenant's key in their KMS account).
type KeyIDFunc func(ctx context.Context, objectKey string) (string, error)

// kmsKeyring is the Keyring over a KMS.
type kmsKeyring struct {
	kms   KMS
	keyID KeyIDFunc
}

// NewKMSKeyring returns a Keyring that wraps data keys with kms under the
// master key keyID chooses, binding the object's storage key as
// additional data so a wrapped key copied into another object's header
// does not unwrap there.
func NewKMSKeyring(kms KMS, keyID KeyIDFunc) Keyring {
	return kmsKeyring{kms: kms, keyID: keyID}
}

// wrapAAD is the additional data a data key is wrapped with.
func wrapAAD(objectKey string) []byte {
	return append(append([]byte(nil), aadLabel...), objectKey...)
}

// WrapKey implements Keyring.
func (k kmsKeyring) WrapKey(ctx context.Context, objectKey string, dataKey []byte) (WrappedKey, error) {
	id, err := k.keyID(ctx, objectKey)
	if err != nil {
		return WrappedKey{}, err
	}
	ct, err := k.kms.Encrypt(ctx, id, dataKey, wrapAAD(objectKey))
	if err != nil {
		return WrappedKey{}, err
	}
	return WrappedKey{KeyID: id, Ciphertext: ct}, nil
}

// UnwrapKey implements Keyring.
func (k kmsKeyring) UnwrapKey(ctx context.Context, objectKey string, w WrappedKey) ([]byte, error) {
	return k.kms.Decrypt(ctx, w.KeyID, w.Ciphertext, wrapAAD(objectKey))
}

// MemoryKMS is a KMS whose master keys live in process memory: for tests,
// for development, and for applications that load one master key from a
// secret manager at start-up. Keys are AES-256 and never leave it; its
// String, GoString and LogValue print only how many keys it holds. Safe
// for concurrent use.
type MemoryKMS struct {
	mu   sync.RWMutex
	keys map[string]cipher.AEAD
}

// NewMemoryKMS returns an empty MemoryKMS.
func NewMemoryKMS() *MemoryKMS {
	return &MemoryKMS{keys: map[string]cipher.AEAD{}}
}

// Add stores master key under id, replacing any key with that id. key must
// be DataKeySize (32) bytes; it is not retained beyond the cipher built
// from it, so the caller may clear it afterwards.
func (m *MemoryKMS) Add(id string, key []byte) error {
	if id == "" || len(key) != DataKeySize {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "crypt: a master key needs an id and %d bytes", DataKeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[id] = aead
	return nil
}

// Remove forgets the key under id. Objects whose data key is still wrapped
// under it no longer decrypt, which is how a retired key is proven unused
// after rotation, and how deleting a tenant's key shreds their data.
func (m *MemoryKMS) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keys, id)
}

// key returns the cipher under id.
func (m *MemoryKMS) key(id string) (cipher.AEAD, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keys[id]
	if !ok {
		return nil, filesystem.Errorf(filesystem.CodePermission, "crypt: no master key %q", id)
	}
	return k, nil
}

// Encrypt implements KMS: a random nonce followed by the sealed bytes.
func (m *MemoryKMS) Encrypt(_ context.Context, keyID string, plaintext, aad []byte) ([]byte, error) {
	k, err := m.key(keyID)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, k.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return k.Seal(nonce, nonce, plaintext, aad), nil
}

// Decrypt implements KMS.
func (m *MemoryKMS) Decrypt(_ context.Context, keyID string, ciphertext, aad []byte) ([]byte, error) {
	k, err := m.key(keyID)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < k.NonceSize() {
		return nil, tampered("wrapped key")
	}
	pt, err := k.Open(nil, ciphertext[:k.NonceSize()], ciphertext[k.NonceSize():], aad)
	if err != nil {
		return nil, tampered("wrapped key does not unwrap under %q", keyID)
	}
	return pt, nil
}

// String describes m without its keys.
func (m *MemoryKMS) String() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return fmt.Sprintf("crypt.MemoryKMS{%d keys, redacted}", len(m.keys))
}

// GoString implements fmt.GoStringer so %#v redacts too.
func (m *MemoryKMS) GoString() string { return m.String() }

// LogValue implements slog.LogValuer so structured logs redact too.
func (m *MemoryKMS) LogValue() slog.Value { return slog.StringValue(m.String()) }
