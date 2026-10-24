package audit_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// rotatingRing is a Keyring of the application's whose current key the
// test moves, as a KMS rotation would; fail makes every call fail.
type rotatingRing struct {
	mu      sync.Mutex
	current string
	keys    map[string][]byte
	fail    error
}

func (r *rotatingRing) Current(ctx context.Context) (string, []byte, error) {
	r.mu.Lock()
	id := r.current
	r.mu.Unlock()
	key, err := r.Key(ctx, id)
	return id, key, err
}

func (r *rotatingRing) Key(_ context.Context, id string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return nil, r.fail
	}
	k, ok := r.keys[id]
	if !ok {
		return nil, errors.New("no key " + id)
	}
	return k, nil
}

var errKMS = errors.New("kms down")

// TestKeyringRotation: entries chained under the old key (ID "") and under
// a rotated one verify together; each entry records its key ID.
func TestKeyringRotation(t *testing.T) {
	ring := &rotatingRing{keys: map[string][]byte{"": chainKey}}
	e := newEnv(t, audit.WithHashChainKeyring(ring))
	e.file(t.Context(), t, scope, "a")
	e.file(t.Context(), t, scope, "b")
	mustVerify(t, e, 2)

	ring.mu.Lock()
	ring.keys["k2"], ring.current = []byte("the rotated key"), "k2"
	ring.mu.Unlock()
	e.file(t.Context(), t, scope, "c")
	mustVerify(t, e, 3)
	page := e.entries(t, audit.Query{Scope: scope})
	if page.Entries[0].KeyID != "k2" || page.Entries[2].KeyID != "" {
		t.Fatalf("key IDs %q .. %q", page.Entries[0].KeyID, page.Entries[2].KeyID)
	}
	// WithHashChain's single key cannot check the rotated head.
	if _, err := audit.New(audit.WithHashChain(chainKey)).Verify(t.Context(), e.store, scope); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("Verify with the old key alone = %v", err)
	}
	// A ring that lost the old key cannot verify the old entries.
	_, err := audit.New(audit.WithHashChainKeyring(audit.StaticKeyring{CurrentID: "k2", Keys: map[string][]byte{"k2": []byte("the rotated key")}})).
		Verify(t.Context(), e.store, scope)
	if err == nil {
		t.Fatal("Verify without the old key passed")
	}
	// A static ring holding both verifies.
	both := audit.StaticKeyring{CurrentID: "k2", Keys: map[string][]byte{"": chainKey, "k2": []byte("the rotated key")}}
	if n, err := audit.New(audit.WithHashChainKeyring(both)).Verify(t.Context(), e.store, scope); err != nil || n != 3 {
		t.Fatalf("Verify with both keys = %d, %v", n, err)
	}
	// The key ID is hashed: moving an entry to another key breaks it.
	testdb.Exec(t, e.db, "UPDATE fs_audit SET key_id = '' WHERE seq = 3")
	mustBreak(t, e.p, e, "entry 3")
}

// TestKeyringFailsClosed: a keyring that errors fails the change (nothing
// logged, nothing committed) and fails Verify.
func TestKeyringFailsClosed(t *testing.T) {
	ring := &rotatingRing{keys: map[string][]byte{"": chainKey}}
	e := newEnv(t, audit.WithHashChainKeyring(ring))
	e.file(t.Context(), t, scope, "a")
	ring.mu.Lock()
	ring.fail = errKMS
	ring.mu.Unlock()
	if _, err := e.disk.WriteBytes(t.Context(), "x", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "x"}); !errors.Is(err, errKMS) {
		t.Fatalf("CreateFile = %v", err)
	}
	if _, err := e.p.Verify(t.Context(), e.store, scope); !errors.Is(err, errKMS) {
		t.Fatalf("Verify = %v", err)
	}
	if e.count(t, "fs_audit") != 1 || e.count(t, "fs_files") != 1 {
		t.Fatal("a change went through without its entry")
	}
	ring.mu.Lock()
	ring.fail, ring.current = nil, strings.Repeat("k", audit.MaxKeyIDBytes+1)
	ring.keys[ring.current] = []byte("x")
	ring.mu.Unlock()
	if _, err := e.disk.WriteBytes(t.Context(), "y", []byte("y"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "y"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a too-long key ID = %v", err)
	}
}

// TestWithHashChainUnchanged: WithHashChain still writes entries and heads
// with no key ID, which a StaticKeyring holding the key under "" verifies.
func TestWithHashChainUnchanged(t *testing.T) {
	e := chained(t)
	if e.entries(t, audit.Query{Scope: scope}).Entries[0].KeyID != "" {
		t.Fatal("WithHashChain wrote a key ID")
	}
	ring := audit.StaticKeyring{Keys: map[string][]byte{"": chainKey}}
	if n, err := audit.New(audit.WithHashChainKeyring(ring)).Verify(t.Context(), e.store, scope); err != nil || n != 3 {
		t.Fatalf("Verify = %d, %v", n, err)
	}
}

// TestSource: WithSource logs channel and client, WithSourceIP the address
// too; without either nothing; with the chain the source is hashed.
func TestSource(t *testing.T) {
	src := records.Source{Channel: "ios", Client: "app/2.1", IP: "203.0.113.9"}
	for _, tc := range []struct {
		name string
		opts []audit.Option
		want records.Source
	}{
		{"off", nil, records.Source{}},
		{"source", []audit.Option{audit.WithSource()}, records.Source{Channel: "ios", Client: "app/2.1"}},
		{"source ip", []audit.Option{audit.WithSourceIP()}, src},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, append([]audit.Option{audit.WithHashChain(chainKey)}, tc.opts...)...)
			e.file(records.WithSource(t.Context(), src), t, scope, "a")
			if got := e.entries(t, audit.Query{Scope: scope}).Entries[0].Source; got != tc.want {
				t.Fatalf("source = %+v, want %+v", got, tc.want)
			}
			mustVerify(t, e, 1)
			testdb.Exec(t, e.db, "UPDATE fs_audit SET source_channel = 'web'")
			mustBreak(t, e.p, e, "entry 1")
		})
	}
	e := newEnv(t, audit.WithSource())
	wide := records.WithSource(t.Context(), records.Source{Channel: strings.Repeat("c", records.MaxSourceChannelBytes+1)})
	f := e.file(t.Context(), t, scope, "b")
	if err := e.store.Trash(wide, scope, f.ID); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a too-wide source = %v", err)
	}
}

// failingAt is a Keyring that answers like ring but fails its Nth Key
// call, counted from 1 after arm: a KMS that goes down mid-operation.
type failingAt struct {
	ring  audit.Keyring
	mu    sync.Mutex
	calls int
	at    int
}

func (f *failingAt) arm(at int) { f.mu.Lock(); f.calls, f.at = 0, at; f.mu.Unlock() }

func (f *failingAt) Current(ctx context.Context) (string, []byte, error) { return f.ring.Current(ctx) }

func (f *failingAt) Key(ctx context.Context, id string) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	fail := f.at != 0 && f.calls == f.at
	f.mu.Unlock()
	if fail {
		return nil, errKMS
	}
	return f.ring.Key(ctx, id)
}

// TestKeyringHeadKeyFails: the key of the chain head failing (after the
// current key was read) fails the change, and fails a Prune of the
// chained entries (nothing deleted); a nil ring chains with the empty key.
func TestKeyringHeadKeyFails(t *testing.T) {
	ring := &failingAt{ring: audit.StaticKeyring{Keys: map[string][]byte{"": chainKey}}}
	e := newEnv(t, audit.WithHashChainKeyring(ring))
	e.file(t.Context(), t, scope, "a")
	ring.arm(1)
	if _, err := e.disk.WriteBytes(t.Context(), "x", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "x"}); !errors.Is(err, errKMS) {
		t.Fatalf("CreateFile with the head key failing = %v", err)
	}
	ring.arm(1)
	if _, err := e.p.Prune(t.Context(), e.store, time.Now().Add(time.Hour)); !errors.Is(err, errKMS) {
		t.Fatalf("Prune with the head key failing = %v", err)
	}
	ring.arm(0)
	mustVerify(t, e, 1)

	nilRing := newEnv(t, audit.WithHashChainKeyring(nil))
	nilRing.file(t.Context(), t, scope, "a")
	mustVerify(t, nilRing, 1)
}
