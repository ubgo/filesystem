package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/layer"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// signedLinks issues a link on a scoped disk. The link names the scoped
// disk, so fshttp can serve it only if that disk is in the Manager it
// resolves names with.
// ds:def id=signedlinks-vm866gh6
func signedLinks(ctx context.Context, out io.Writer) error {
	routes := http.NewServeMux()
	srv := httptest.NewServer(routes)
	defer srv.Close()
	signer, err := signedurl.New(srv.URL+signedPrefix, []signedurl.Key{{ID: "k1", Secret: demoSecret}})
	if err != nil {
		return err
	}
	shared, err := filesystem.NewDisk("shared", memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return err
	}
	acme, err := tenantDisk(shared, "acme")
	if err != nil {
		return err
	}
	if _, err := acme.WriteBytes(ctx, "report.pdf", []byte("%PDF-1.7 acme"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	disks, err := filesystem.NewManager(shared, acme)
	if err != nil {
		return err
	}
	routes.Handle(signedPrefix+"/", http.StripPrefix(signedPrefix, fshttp.Signed(disks, signer)))
	link, err := acme.SignedURL(ctx, "report.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	resp, err := http.Get(link.URL)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	path := strings.TrimPrefix(link.URL[:strings.IndexByte(link.URL, '?')], srv.URL)
	fmt.Fprintf(out, "signed link %s: %d\n", path, resp.StatusCode)
	return withoutScopedDisk(ctx, shared, signer, link.URL, srv.URL, out)
}

// withoutScopedDisk serves the same link from a handler whose Manager
// holds only the shared disk: the name in the link resolves to nothing.
// ds:def id=withoutscopeddisk-w2gz9zvj
func withoutScopedDisk(ctx context.Context, shared *filesystem.Disk, signer *signedurl.Signer, link, base string, out io.Writer) error {
	disks, err := filesystem.NewManager(shared)
	if err != nil {
		return err
	}
	h := http.StripPrefix(signedPrefix, fshttp.Signed(disks, signer))
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, strings.TrimPrefix(link, base), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	fmt.Fprintf(out, "same link, scoped disk not in the Manager: %d\n", rec.Code)
	return nil
}

// tenantKey is the context key under which the middleware stores the
// request's tenant.
type tenantKey struct{}

// sameTenant is a Guard check for a disk that every tenant shares
// unscoped: every key, and the target of a copy or move, must lie under
// the tenant of the request.
// ds:def id=sametenant-4fket7z9
func sameTenant(ctx context.Context, op *filesystem.Op) error {
	tenant, _ := ctx.Value(tenantKey{}).(string)
	if tenant == "" {
		return errors.New("no tenant in context")
	}
	dir := filesystem.JoinKey(tenantsDir, tenant)
	for _, k := range append([]string{op.Key, op.Target}, op.Keys...) {
		if k != "" && !filesystem.KeyInDir(k, dir) {
			return fmt.Errorf("key %q is not in %s", k, dir)
		}
	}
	return nil
}

// guardKeys runs the check on a disk where the application builds keys
// itself, so a bug that builds another tenant's key fails instead of
// leaking.
// ds:def id=guardkeys-u2scjnm4
func guardKeys(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("shared", memory.New(), filesystem.WithInterceptors(layer.Guard(sameTenant)))
	if err != nil {
		return err
	}
	ctx = context.WithValue(ctx, tenantKey{}, "acme")
	_, err = disk.WriteBytes(ctx, "tenants/acme/a.txt", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "guard own key: %v\n", err)
	_, err = disk.Copy(ctx, "tenants/acme/a.txt", "tenants/globex/a.txt")
	fmt.Fprintf(out, "guard copy into another tenant: %s\n", filesystem.CodeOf(err))
	return nil
}

// recordScopes shows the scope argument of the records Store: every query
// filters by it, so a file ID from one tenant is not found in another.
// ds:def id=recordscopes-r5r3w6ux
func recordScopes(ctx context.Context, out io.Writer) error {
	db, err := sql.Open("sqlite", memoryDB)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		return err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return err
	}
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks)
	if err != nil {
		return err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return err
	}
	p := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"application/pdf"}}
	f, err := store.Upload(ctx, "acme", "media", strings.NewReader("%PDF-1.7 acme"), "plan.pdf", p)
	if err != nil {
		return err
	}
	_, err = store.File(ctx, "globex", f.ID)
	fmt.Fprintf(out, "globex asks for acme's file: %s\n", filesystem.CodeOf(err))
	return nil
}
