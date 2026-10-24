package crypt_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/crypt"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/memory"
)

const sealed = crypt.ChunkSize + crypt.TagSize

var masterKey = bytes.Repeat([]byte{0x5a}, crypt.DataKeySize)

// setup returns a Disk over crypt over a memory driver, and the memory
// driver itself so tests can see and tamper with the ciphertext.
func setup(t *testing.T) (*filesystem.Disk, *memory.Driver, *crypt.MemoryKMS) {
	t.Helper()
	kms := crypt.NewMemoryKMS()
	if err := kms.Add("k1", masterKey); err != nil {
		t.Fatal(err)
	}
	inner := memory.New()
	drv, err := crypt.New(inner, crypt.NewKMSKeyring(kms, func(context.Context, string) (string, error) { return "k1", nil }))
	if err != nil {
		t.Fatal(err)
	}
	disk, err := filesystem.NewDisk("enc", drv)
	if err != nil {
		t.Fatal(err)
	}
	return disk, inner, kms
}

// body is n bytes that differ from chunk to chunk.
func body(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i/crypt.ChunkSize)
	}
	return b
}

func raw(t *testing.T, inner *memory.Driver, key string) []byte {
	t.Helper()
	rc, err := inner.Read(t.Context(), key, filesystem.ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func putRaw(t *testing.T, inner *memory.Driver, key string, b []byte) {
	t.Helper()
	if _, err := inner.Write(t.Context(), key, bytes.NewReader(b), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		d, _, _ := setup(t)
		return d
	})
}

// Only capabilities that hold for ciphertext are declared, although the
// memory driver underneath has them all.
func TestCapabilities(t *testing.T) {
	d, inner, _ := setup(t)
	c := d.Capabilities()
	ic := inner.Capabilities()
	if !ic.Copy || !ic.Move || !ic.Multipart || !ic.Append {
		t.Fatal("the memory driver no longer has the capabilities this test narrows")
	}
	if c.Copy || c.Move || c.Multipart || c.Append || c.Presign || c.PublicURL || c.PresignPost || c.PresignPart || c.ConditionalRead || c.Versions || c.VersionRead || c.ChecksumSHA256 {
		t.Fatalf("declared a capability that does not hold on ciphertext: %+v", c)
	}
	if !c.CompareAndSwap || !c.ConditionalWrite || !c.DeletePrefix || !c.Metadata || !c.Tags {
		t.Fatalf("lost a byte-independent capability: %+v", c)
	}
}

func TestRoundTripAndSizes(t *testing.T) {
	d, inner, _ := setup(t)
	for _, n := range []int{0, 1, crypt.ChunkSize - 1, crypt.ChunkSize, crypt.ChunkSize + 1, 3*crypt.ChunkSize + 5} {
		key := fmt.Sprintf("f%d", n)
		want := body(n)
		e, err := d.WriteBytes(t.Context(), key, want, filesystem.WriteOptions{})
		if err != nil || e.Size != int64(n) {
			t.Fatalf("%d: write %+v, %v", n, e, err)
		}
		got, err := d.ReadAll(t.Context(), key)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%d: read %d bytes, %v", n, len(got), err)
		}
		if st, err := d.Stat(t.Context(), key); err != nil || st.Size != int64(n) {
			t.Fatalf("%d: stat %+v, %v", n, st, err)
		}
		stored := raw(t, inner, key)
		if int64(len(stored)) != crypt.CiphertextSize(int64(n)) {
			t.Fatalf("%d: stored %d bytes, want %d", n, len(stored), crypt.CiphertextSize(int64(n)))
		}
		if n >= 64 && bytes.Contains(stored, want[:64]) {
			t.Fatalf("%d: plaintext visible in the stored object", n)
		}
	}
	for e, err := range d.List(t.Context(), "", filesystem.ListOptions{}) {
		if err != nil {
			t.Fatal(err)
		}
		var n int
		_, _ = fmt.Sscanf(e.Key, "f%d", &n)
		if e.Size != int64(n) {
			t.Errorf("list %s: size %d", e.Key, e.Size)
		}
	}
}

// Ranged reads return the right bytes, and read only the chunks they
// need: the other chunks are corrupted and the reads still succeed.
func TestRangedReadsDecryptOnlyTheirChunks(t *testing.T) {
	d, inner, _ := setup(t)
	want := body(5*crypt.ChunkSize + 100)
	if _, err := d.WriteBytes(t.Context(), "r", want, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	stored := raw(t, inner, "r")
	for _, chunk := range []int{0, 4, 5} {
		stored[crypt.HeaderSize+chunk*sealed+7] ^= 0xff
	}
	putRaw(t, inner, "r", stored)
	for _, rng := range []filesystem.ByteRange{
		{Offset: crypt.ChunkSize, Length: 10},
		{Offset: crypt.ChunkSize - 0 + 5, Length: 2*crypt.ChunkSize + 3},
		{Offset: 4*crypt.ChunkSize - 1, Length: 1},
	} {
		rc, err := d.ReadRange(t.Context(), "r", rng)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || !bytes.Equal(got, want[rng.Offset:rng.Offset+rng.Length]) {
			t.Fatalf("%+v: %d bytes, %v", rng, len(got), err)
		}
	}
	for _, rng := range []filesystem.ByteRange{{Offset: 10, Length: 5}, {Offset: 4 * crypt.ChunkSize}, {}} {
		rc, err := d.ReadRange(t.Context(), "r", rng)
		if err == nil {
			_, err = io.ReadAll(rc)
			_ = rc.Close()
		}
		if !errors.Is(err, crypt.ErrTampered) {
			t.Fatalf("%+v over a corrupted chunk: %v", rng, err)
		}
	}
}

func TestRangeEdges(t *testing.T) {
	d, _, _ := setup(t)
	want := body(2*crypt.ChunkSize + 9)
	if _, err := d.WriteBytes(t.Context(), "e", want, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, rng := range []filesystem.ByteRange{
		{Offset: 0, Length: 1}, {Offset: 2 * crypt.ChunkSize}, {Offset: 2*crypt.ChunkSize + 8, Length: 100},
		{Offset: int64(len(want))}, {Offset: int64(len(want)) + 50, Length: 3}, {Offset: 3},
	} {
		rc, err := d.ReadRange(t.Context(), "e", rng)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		lo := min(rng.Offset, int64(len(want)))
		hi := int64(len(want))
		if rng.Length > 0 {
			hi = min(hi, rng.Offset+rng.Length)
		}
		if err != nil || !bytes.Equal(got, want[lo:max(lo, hi)]) {
			t.Fatalf("%+v: %d bytes, %v", rng, len(got), err)
		}
	}
}

func readErr(t *testing.T, d *filesystem.Disk, key string) error {
	t.Helper()
	_, err := d.ReadAll(t.Context(), key)
	return err
}

func TestTampering(t *testing.T) {
	d, inner, _ := setup(t)
	want := body(3*crypt.ChunkSize + 10)
	if _, err := d.WriteBytes(t.Context(), "a", want, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(t.Context(), "b", want, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	orig := raw(t, inner, "a")
	other := raw(t, inner, "b")
	chunk := func(b []byte, i int) []byte { return b[crypt.HeaderSize+i*sealed : crypt.HeaderSize+(i+1)*sealed] }
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	head := orig[:crypt.HeaderSize]
	cases := map[string][]byte{
		"flipped bit":             func() []byte { b := bytes.Clone(orig); b[crypt.HeaderSize+100] ^= 1; return b }(),
		"reordered chunks":        join(head, chunk(orig, 1), chunk(orig, 0), orig[crypt.HeaderSize+2*sealed:]),
		"chunk from other object": join(head, chunk(other, 0), orig[crypt.HeaderSize+sealed:]),
		"truncated at a boundary": orig[:crypt.HeaderSize+3*sealed],
		"last chunk dropped":      join(head, chunk(orig, 0), chunk(orig, 1), orig[crypt.HeaderSize+3*sealed:]),
		"extra chunk appended":    join(orig, chunk(orig, 0)),
		"header of other object":  join(other[:crypt.HeaderSize], orig[crypt.HeaderSize:]),
		"no header":               []byte("plain bytes written around the wrapper"),
		"header only":             bytes.Clone(head),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			putRaw(t, inner, "a", b)
			if err := readErr(t, d, "a"); !errors.Is(err, crypt.ErrTampered) {
				t.Fatalf("read: %v", err)
			}
		})
	}
	// An object moved to another key, wrapped key and all, does not
	// decrypt there.
	putRaw(t, inner, "moved", other)
	if err := readErr(t, d, "moved"); !errors.Is(err, crypt.ErrTampered) {
		t.Fatalf("moved object: %v", err)
	}
	// Truncation inside a chunk shows in Stat as an impossible size.
	putRaw(t, inner, "a", orig[:crypt.HeaderSize+sealed+5])
	if _, err := d.Stat(t.Context(), "a"); !errors.Is(err, crypt.ErrTampered) {
		t.Fatalf("stat of a cut object: %v", err)
	}
}

// Nonces never repeat: identical plaintext chunks seal to different
// ciphertext within an object, and the same content written twice shares
// nothing.
func TestNoncesNeverRepeat(t *testing.T) {
	d, inner, _ := setup(t)
	same := bytes.Repeat([]byte("same"), 3*crypt.ChunkSize/4)
	if _, err := d.WriteBytes(t.Context(), "s", same, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	first := raw(t, inner, "s")
	c0 := first[crypt.HeaderSize : crypt.HeaderSize+crypt.ChunkSize]
	c1 := first[crypt.HeaderSize+sealed : crypt.HeaderSize+sealed+crypt.ChunkSize]
	if bytes.Equal(c0, c1) {
		t.Fatal("two identical chunks sealed to the same ciphertext: the nonce repeated")
	}
	if _, err := d.WriteBytes(t.Context(), "s", same, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	second := raw(t, inner, "s")
	if bytes.Equal(first[crypt.HeaderSize:crypt.HeaderSize+64], second[crypt.HeaderSize:crypt.HeaderSize+64]) {
		t.Fatal("rewriting the same content reused the data key")
	}
}

// keyIDs is a KeyIDFunc the tests switch: per tenant (first key segment),
// with a generation that rotation bumps.
type keyIDs struct {
	mu  sync.Mutex
	gen map[string]int
}

func (k *keyIDs) id(_ context.Context, key string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	tenant, _, _ := strings.Cut(key, "/")
	return fmt.Sprintf("%s-%d", tenant, k.gen[tenant]), nil
}

func (k *keyIDs) bump(tenant string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.gen[tenant]++
}

func tenantDisk(t *testing.T) (*filesystem.Disk, *crypt.Driver, *memory.Driver, *crypt.MemoryKMS, *keyIDs) {
	t.Helper()
	kms := crypt.NewMemoryKMS()
	for _, id := range []string{"acme-0", "acme-1", "globex-0"} {
		if err := kms.Add(id, bytes.Repeat([]byte(id[:1]), crypt.DataKeySize)); err != nil {
			t.Fatal(err)
		}
	}
	ids := &keyIDs{gen: map[string]int{}}
	inner := memory.New()
	drv, err := crypt.New(inner, crypt.NewKMSKeyring(kms, ids.id))
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("t", drv)
	if err != nil {
		t.Fatal(err)
	}
	return d, drv, inner, kms, ids
}

// Rotation re-wraps the data key only: the chunks are copied byte for
// byte, the header names the new key, and the old key can go.
func TestRotateRewrapsOnly(t *testing.T) {
	d, drv, inner, kms, ids := tenantDisk(t)
	want := body(2*crypt.ChunkSize + 3)
	if _, err := d.WriteBytes(t.Context(), "acme/report.pdf", want, filesystem.WriteOptions{ContentType: "application/pdf", Metadata: map[string]string{"owner": "7"}}); err != nil {
		t.Fatal(err)
	}
	before := raw(t, inner, "acme/report.pdf")
	ids.bump("acme")
	if err := drv.Rotate(t.Context(), "acme/report.pdf"); err != nil {
		t.Fatal(err)
	}
	after := raw(t, inner, "acme/report.pdf")
	if !bytes.Equal(before[crypt.HeaderSize:], after[crypt.HeaderSize:]) {
		t.Fatal("rotation changed the chunks")
	}
	if !bytes.Contains(after[:crypt.HeaderSize], []byte("acme-1")) || bytes.Contains(after[:crypt.HeaderSize], []byte("acme-0")) {
		t.Fatal("the header does not name the new key")
	}
	kms.Remove("acme-0")
	got, err := d.ReadAll(t.Context(), "acme/report.pdf")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("read after retiring the old key: %v", err)
	}
	st, err := d.Stat(t.Context(), "acme/report.pdf")
	if err != nil || st.ContentType != "application/pdf" || st.Metadata["owner"] != "7" {
		t.Fatalf("rotation lost attributes: %+v, %v", st, err)
	}
}

// Rotation refuses to overwrite a write that landed after it read.
func TestRotateConflict(t *testing.T) {
	inner := &racingDriver{Driver: memory.New()}
	kms := crypt.NewMemoryKMS()
	_ = kms.Add("k", masterKey)
	drv, err := crypt.New(inner, crypt.NewKMSKeyring(kms, func(context.Context, string) (string, error) { return "k", nil }))
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("r", drv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(t.Context(), "x", []byte("v1"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	inner.race = func() { _, _ = d.WriteBytes(context.Background(), "x", []byte("v2"), filesystem.WriteOptions{}) }
	if err := drv.Rotate(t.Context(), "x"); filesystem.CodeOf(err) != filesystem.CodeConflict {
		t.Fatalf("rotate over a concurrent write: %v", err)
	}
	if got, _ := d.ReadAll(t.Context(), "x"); string(got) != "v2" {
		t.Fatalf("the concurrent write was lost: %q", got)
	}
}

// racingDriver runs race once, after the first full Read, to land a write
// between Rotate's read and its write.
type racingDriver struct {
	*memory.Driver
	race func()
}

func (r *racingDriver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	rc, err := r.Driver.Read(ctx, key, rng)
	if r.race != nil && rng == (filesystem.ByteRange{}) {
		f := r.race
		r.race = nil
		f()
	}
	return rc, err
}

// Per-tenant keys: deleting one tenant's master key makes its objects
// unreadable (crypto-shredding) and leaves other tenants alone.
func TestPerTenantKeys(t *testing.T) {
	d, _, inner, kms, _ := tenantDisk(t)
	for _, k := range []string{"acme/a.txt", "globex/b.txt"} {
		if _, err := d.WriteBytes(t.Context(), k, []byte("secret of "+k), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Contains(raw(t, inner, "globex/b.txt")[:crypt.HeaderSize], []byte("globex-0")) {
		t.Fatal("globex's object is not wrapped under globex's key")
	}
	kms.Remove("acme-0")
	if err := readErr(t, d, "acme/a.txt"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("acme after its key was deleted: %v", err)
	}
	if got, err := d.ReadAll(t.Context(), "globex/b.txt"); err != nil || string(got) != "secret of globex/b.txt" {
		t.Fatalf("globex: %q, %v", got, err)
	}
}

// spyKeyring is a second Keyring: it wraps by XOR with a fixed pad and
// records every data key, so the test can look for them in output.
type spyKeyring struct {
	mu   sync.Mutex
	deks [][]byte
}

func (s *spyKeyring) WrapKey(_ context.Context, _ string, dek []byte) (crypt.WrappedKey, error) {
	s.mu.Lock()
	s.deks = append(s.deks, bytes.Clone(dek))
	s.mu.Unlock()
	out := bytes.Clone(dek)
	for i := range out {
		out[i] ^= 0xa5
	}
	return crypt.WrappedKey{KeyID: "spy", Ciphertext: out}, nil
}

func (s *spyKeyring) UnwrapKey(_ context.Context, _ string, w crypt.WrappedKey) ([]byte, error) {
	out := bytes.Clone(w.Ciphertext)
	for i := range out {
		out[i] ^= 0xa5
	}
	return out, nil
}

// Keys appear in no printed, logged or returned value.
func TestKeysRedacted(t *testing.T) {
	spy := &spyKeyring{}
	inner := memory.New()
	drv, err := crypt.New(inner, spy)
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("s", drv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(t.Context(), "x", body(crypt.ChunkSize+1), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, err := d.ReadAll(t.Context(), "x"); err != nil || len(got) != crypt.ChunkSize+1 {
		t.Fatalf("custom keyring round trip: %v", err)
	}
	stored := raw(t, inner, "x")
	stored[crypt.HeaderSize+3] ^= 1
	putRaw(t, inner, "x", stored)
	readFailure := readErr(t, d, "x")

	kms := crypt.NewMemoryKMS()
	_ = kms.Add("m", masterKey)
	_, decryptFailure := kms.Decrypt(t.Context(), "m", bytes.Repeat([]byte{1}, 40), nil)

	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("state", "kms", kms, "driver", drv)
	printed := strings.Join([]string{
		fmt.Sprintf("%v %+v %#v %s", kms, kms, kms, kms),
		fmt.Sprintf("%v %+v %#v %s", drv, drv, drv, drv),
		logs.String(), readFailure.Error(), decryptFailure.Error(),
	}, "\n")
	secrets := append([][]byte{masterKey}, spy.deks...)
	for _, s := range secrets {
		for _, form := range []string{string(s), hex.EncodeToString(s), fmt.Sprint(s), fmt.Sprintf("%x", s)} {
			if strings.Contains(printed, form) {
				t.Fatalf("a key appears in output:\n%s", printed)
			}
		}
	}
	if !strings.Contains(printed, "redacted") {
		t.Fatalf("output does not say it redacted:\n%s", printed)
	}
}

func TestNewRefusesMissingParts(t *testing.T) {
	if _, err := crypt.New(nil, &spyKeyring{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatal(err)
	}
	if _, err := crypt.New(memory.New(), nil); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatal(err)
	}
	if err := crypt.NewMemoryKMS().Add("short", []byte("16 bytes of key!")); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatal(err)
	}
}

func TestSizes(t *testing.T) {
	for _, n := range []int64{0, 1, crypt.ChunkSize, crypt.ChunkSize + 1, 10*crypt.ChunkSize - 1} {
		if got, err := crypt.PlaintextSize(crypt.CiphertextSize(n)); err != nil || got != n {
			t.Errorf("%d: %d, %v", n, got, err)
		}
	}
	for _, bad := range []int64{0, crypt.HeaderSize, crypt.HeaderSize + crypt.TagSize - 1, crypt.HeaderSize + sealed + 3} {
		if _, err := crypt.PlaintextSize(bad); !errors.Is(err, crypt.ErrTampered) {
			t.Errorf("%d: %v", bad, err)
		}
	}
}

// The storage key is bound in every chunk, not only by a keyring that
// binds it when wrapping: under spyKeyring, which does not, an object
// moved to another key still fails.
func TestMovedObjectFailsWithAnyKeyring(t *testing.T) {
	inner := memory.New()
	drv, err := crypt.New(inner, &spyKeyring{})
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("s", drv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(t.Context(), "tenant-a/invoice.pdf", []byte("a's invoice"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	putRaw(t, inner, "tenant-b/invoice.pdf", raw(t, inner, "tenant-a/invoice.pdf"))
	if err := readErr(t, d, "tenant-b/invoice.pdf"); !errors.Is(err, crypt.ErrTampered) {
		t.Fatalf("moved object: %v", err)
	}
}
