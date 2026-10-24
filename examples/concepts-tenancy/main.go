// Command concepts-tenancy shows the ways to keep tenants apart on shared
// storage: a scoped disk per tenant, signed links for scoped disks, a
// Guard that checks every key against the request's tenant, deleting a
// tenant's files, the scope every records query filters by, and a disk per
// tenant in the tenant's region with the tenant's own domain.
//
//	go run ./concepts-tenancy          (from the examples directory)
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/signedurl"
)

// Layout decisions of this app: every tenant's objects live under
// tenantsDir/<tenant>/ on one shared disk.
const (
	tenantsDir = "tenants"
	// signedPrefix is where fshttp.Signed is mounted.
	signedPrefix = "/files"
	// linkLifetime is how long the demo's signed links live.
	linkLifetime = 5 * time.Minute
	// memoryDB is a private in-memory SQLite database.
	memoryDB = "file::memory:?_pragma=foreign_keys(ON)"
)

// demoSecret is a fixed signing secret so the demo is repeatable.
var demoSecret = bytes.Repeat([]byte("t"), signedurl.MinKeyBytes)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is main without the process, so the test can call it.
func run(ctx context.Context, out io.Writer) error {
	shared, err := filesystem.NewDisk("shared", memory.New())
	if err != nil {
		return err
	}
	if err := scopedDisks(ctx, shared, out); err != nil {
		return err
	}
	if err := deleteTenant(ctx, shared, out); err != nil {
		return err
	}
	if err := signedLinks(ctx, out); err != nil {
		return err
	}
	if err := guardKeys(ctx, out); err != nil {
		return err
	}
	if err := recordScopes(ctx, out); err != nil {
		return err
	}
	return regionsAndDomains(ctx, out)
}

// tenantDisk returns the disk one tenant's code receives: the shared disk
// seen through the tenant's prefix. Its name identifies it in logs and
// signed URLs.
// ds:def id=tenantdisk-c9d3jqwx
func tenantDisk(shared *filesystem.Disk, tenant string) (*filesystem.Disk, error) {
	return shared.Scope(tenant, filesystem.JoinKey(tenantsDir, tenant))
}

// scopedDisks writes the same key for two tenants. Each scoped disk sees
// only its own prefix, and no key, ".." included, reaches outside it.
// ds:def id=scopeddisks-y4w23nt2
func scopedDisks(ctx context.Context, shared *filesystem.Disk, out io.Writer) error {
	for _, tenant := range []string{"acme", "globex"} {
		d, err := tenantDisk(shared, tenant)
		if err != nil {
			return err
		}
		if _, err := d.WriteBytes(ctx, "logo.png", []byte(tenant+" logo"), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	acme, err := tenantDisk(shared, "acme")
	if err != nil {
		return err
	}
	b, err := acme.ReadAll(ctx, "logo.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "acme reads logo.png: %q\n", b)
	fmt.Fprintf(out, "shared disk holds: %s\n", strings.Join(keys(ctx, shared, ""), " "))
	_, err = acme.ReadAll(ctx, "../globex/logo.png")
	fmt.Fprintf(out, "acme reads ../globex/logo.png: %s\n", filesystem.CodeOf(err))
	return nil
}

// keys lists every key under dir, recursively.
func keys(ctx context.Context, d *filesystem.Disk, dir string) []string {
	var out []string
	for e, err := range d.List(ctx, dir, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return append(out, err.Error())
		}
		out = append(out, e.Key)
	}
	return out
}

// deleteTenant removes everything a departing tenant stored, with one
// prefix delete on the shared disk. A scoped disk refuses to delete its
// own root, so this is done one level up, deliberately.
// ds:def id=deletetenant-cvsd5bmn
func deleteTenant(ctx context.Context, shared *filesystem.Disk, out io.Writer) error {
	globex, err := tenantDisk(shared, "globex")
	if err != nil {
		return err
	}
	err = globex.DeletePrefix(ctx, "")
	fmt.Fprintf(out, "scoped disk deletes its root: %s\n", filesystem.CodeOf(err))
	if err := shared.DeletePrefix(ctx, filesystem.JoinKey(tenantsDir, "globex")); err != nil {
		return err
	}
	fmt.Fprintf(out, "after deleting globex: %s\n", strings.Join(keys(ctx, shared, ""), " "))
	return nil
}
