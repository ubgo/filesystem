// Command security-crypt encrypts every file before it reaches the
// backend, with a master key per tenant: it writes two tenants' files
// through an encrypting disk, shows the backend holds only ciphertext,
// serves a ranged read, rotates one tenant's master key (re-wrapping data
// keys only), and deletes a tenant's key to make its files unreadable.
//
//	go run ./security-crypt
//
// The KMS is crypt.MemoryKMS with keys generated at start-up; in
// production an adapter over AWS KMS, Cloud KMS or Vault implements
// crypt.KMS. Files go to the memory driver.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/crypt"
	"github.com/ubgo/filesystem/memory"
)

// diskName names the encrypting disk.
const diskName = "vault"

// tenantKeys chooses each tenant's current master key: the tenant is the
// first key segment, and the version goes up when the tenant's key is
// rotated. This is the application's decision, passed to the keyring.
type tenantKeys struct {
	mu      sync.Mutex
	version map[string]int
}

// keyID implements crypt.KeyIDFunc.
func (k *tenantKeys) keyID(_ context.Context, objectKey string) (string, error) {
	tenant, _, ok := strings.Cut(objectKey, "/")
	if !ok {
		return "", filesystem.Errorf(filesystem.CodeInvalidKey, "key %q has no tenant segment", objectKey)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return fmt.Sprintf("tenant-%s-v%d", tenant, k.version[tenant]), nil
}

// rotate makes the next version current for tenant.
func (k *tenantKeys) rotate(tenant string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.version[tenant]++
}

// addKey generates a master key under id. A real KMS generates and keeps
// it; it never reaches the application.
func addKey(kms *crypt.MemoryKMS, id string) error {
	key := make([]byte, crypt.DataKeySize)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	defer clear(key)
	return kms.Add(id, key)
}

// newVault wraps a backend driver so everything written is encrypted
// under the tenant's master key.
// ds:def id=newvault-hmqdk5pv
func newVault(backend filesystem.Driver, kms crypt.KMS, keys *tenantKeys) (*filesystem.Disk, *crypt.Driver, error) {
	drv, err := crypt.New(backend, crypt.NewKMSKeyring(kms, keys.keyID))
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv)
	if err != nil {
		return nil, nil, err
	}
	return disk, drv, nil
}

// rotateTenant re-wraps every object of tenant under its next master key.
// ds:def id=rotatetenant-ns8jhjmy
func rotateTenant(ctx context.Context, disk *filesystem.Disk, drv *crypt.Driver, keys *tenantKeys, tenant string) (int, error) {
	keys.rotate(tenant)
	n := 0
	for e, err := range disk.List(ctx, tenant, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return n, err
		}
		if err := drv.Rotate(ctx, e.Key); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// writeEncrypted writes body through the encrypting disk, then looks at
// what the backend actually holds: the size the disk reports (the
// plaintext's), the size stored (plaintext plus a header and one tag per
// 64 KiB chunk), and whether the plaintext is anywhere in it.
// ds:def id=writeencrypted-m5c9w2tq
func writeEncrypted(ctx context.Context, disk *filesystem.Disk, backend filesystem.Driver, key string, body []byte) (written, stored int64, plaintext bool, err error) {
	if _, err := disk.WriteBytes(ctx, key, body, filesystem.WriteOptions{}); err != nil {
		return 0, 0, false, err
	}
	st, err := disk.Stat(ctx, key)
	if err != nil {
		return 0, 0, false, err
	}
	raw, err := backend.Stat(ctx, key)
	if err != nil {
		return 0, 0, false, err
	}
	rc, err := backend.Read(ctx, key, filesystem.ByteRange{})
	if err != nil {
		return 0, 0, false, err
	}
	defer func() { _ = rc.Close() }()
	onDisk, err := io.ReadAll(rc)
	if err != nil {
		return 0, 0, false, err
	}
	return st.Size, raw.Size, bytes.Contains(onDisk, body[:min(len(body), 16)]), nil
}

func run(ctx context.Context, out io.Writer) error {
	kms := crypt.NewMemoryKMS()
	for _, id := range []string{"tenant-acme-v0", "tenant-acme-v1", "tenant-globex-v0"} {
		if err := addKey(kms, id); err != nil {
			return err
		}
	}
	keys := &tenantKeys{version: map[string]int{}}
	backend := memory.New()
	disk, drv, err := newVault(backend, kms, keys)
	if err != nil {
		return err
	}

	report := strings.Repeat("Q3 revenue: confidential. ", 10_000)
	written, stored, plain, err := writeEncrypted(ctx, disk, backend, "acme/finance/q3.txt", []byte(report))
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "globex/hr/salaries.csv", []byte("name,salary\nada,1\n"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	fmt.Fprintf(out, "acme/finance/q3.txt: %d bytes written, %d bytes stored\n", written, stored)
	fmt.Fprintf(out, "backend holds the plaintext: %v\n", plain)

	part, err := disk.ReadRange(ctx, "acme/finance/q3.txt", filesystem.ByteRange{Offset: 200_000, Length: 26})
	if err != nil {
		return err
	}
	b, err := io.ReadAll(part)
	_ = part.Close()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "ranged read at 200000: %q\n", b)

	n, err := rotateTenant(ctx, disk, drv, keys, "acme")
	if err != nil {
		return err
	}
	kms.Remove("tenant-acme-v0")
	got, err := disk.ReadAll(ctx, "acme/finance/q3.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "rotated %d acme object(s); readable without the old key: %v\n", n, string(got) == report)

	kms.Remove("tenant-globex-v0")
	_, err = disk.ReadAll(ctx, "globex/hr/salaries.csv")
	fmt.Fprintf(out, "globex after its key was deleted: %v\n", err)
	fmt.Fprintf(out, "printing the KMS: %v\n", kms)
	return nil
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}
