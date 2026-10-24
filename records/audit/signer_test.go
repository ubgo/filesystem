package audit_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
)

// kmsSigner stands in for a KMS whose key never leaves it: it answers MAC
// requests by key ID and never hands out a key. sha512 picks HMAC-SHA512 (else
// HMAC-SHA256, what a KMS HMAC_SHA_256 key computes); calls counts them; fail
// fails every call; short truncates every tag to that many bytes.
type kmsSigner struct {
	mu      sync.Mutex
	current string
	keys    map[string][]byte
	sha512  bool
	calls   int
	fail    error
	short   int
}

func (k *kmsSigner) CurrentID(context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.current, k.fail
}

func (k *kmsSigner) MAC(_ context.Context, id string, msg []byte) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls++
	if k.fail != nil {
		return nil, k.fail
	}
	key, ok := k.keys[id]
	if !ok {
		return nil, errors.New("kms: no key " + id)
	}
	h := hmac.New(sha256.New, key)
	if k.sha512 {
		h = hmac.New(sha512.New, key)
	}
	h.Write(msg)
	tag := h.Sum(nil)
	if k.short > 0 {
		tag = tag[:k.short]
	}
	return tag, nil
}

// TestSignerIsUsed: a Signer computes every MAC (the key never reaches the
// plugin), and an HMAC-SHA256 signer under the same key agrees with
// WithHashChain in both directions, so a log moves between them.
func TestSignerIsUsed(t *testing.T) {
	kms := &kmsSigner{keys: map[string][]byte{"": chainKey}}
	e := newEnv(t, audit.WithHashChainMAC(kms))
	e.file(t.Context(), t, scope, "a")
	e.file(t.Context(), t, scope, "b")
	if kms.calls == 0 {
		t.Fatal("the signer was never asked")
	}
	mustVerify(t, e, 2)
	if n, err := audit.New(audit.WithHashChain(chainKey)).Verify(t.Context(), e.store, scope); err != nil || n != 2 {
		t.Fatalf("WithHashChain verifying the signer's log = %d, %v", n, err)
	}
	other := chained(t)
	if n, err := audit.New(audit.WithHashChainMAC(kms)).Verify(t.Context(), other.store, scope); err != nil || n != 3 {
		t.Fatalf("the signer verifying WithHashChain's log = %d, %v", n, err)
	}
}

// TestSignerOtherAlgorithm: a signer with another MAC (HMAC-SHA512, within
// MaxMACBytes) chains and verifies; a key it does not hold fails Verify.
func TestSignerOtherAlgorithm(t *testing.T) {
	kms := &kmsSigner{current: "hsm-1", keys: map[string][]byte{"hsm-1": []byte("inside the hsm")}, sha512: true}
	e := newEnv(t, audit.WithHashChainMAC(kms))
	e.file(t.Context(), t, scope, "a")
	mustVerify(t, e, 1)
	if _, err := audit.New(audit.WithHashChain(chainKey)).Verify(t.Context(), e.store, scope); err == nil {
		t.Fatal("WithHashChain verified a SHA-512 chain under another key")
	}
	lost := &kmsSigner{keys: map[string][]byte{"": chainKey}}
	if _, err := audit.New(audit.WithHashChainMAC(lost)).Verify(t.Context(), e.store, scope); err == nil {
		t.Fatal("a signer without the entry's key verified it")
	}
}

// TestSignerFailsClosed: a failing signer fails the change (nothing
// logged, nothing committed) and Verify; a tag shorter than MinMACBytes is
// refused the same way rather than written.
func TestSignerFailsClosed(t *testing.T) {
	kms := &kmsSigner{keys: map[string][]byte{"": chainKey}}
	e := newEnv(t, audit.WithHashChainMAC(kms))
	e.file(t.Context(), t, scope, "a")
	kms.mu.Lock()
	kms.fail = errKMS
	kms.mu.Unlock()
	if _, err := e.disk.WriteBytes(t.Context(), "x", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "x"}); !errors.Is(err, errKMS) {
		t.Fatalf("CreateFile = %v", err)
	}
	if _, err := e.p.Verify(t.Context(), e.store, scope); !errors.Is(err, errKMS) {
		t.Fatalf("Verify = %v", err)
	}
	kms.mu.Lock()
	kms.fail, kms.short = nil, audit.MinMACBytes-1
	kms.mu.Unlock()
	if _, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "x"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("CreateFile with a short MAC = %v", err)
	}
	if _, err := e.p.Verify(t.Context(), e.store, scope); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("Verify with a short MAC = %v", err)
	}
	if e.count(t, "fs_audit") != 1 || e.count(t, "fs_files") != 1 {
		t.Fatal("a change went through without its entry")
	}
}
