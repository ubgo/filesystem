package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// The two-disk layout: names, URLs and lifetimes of this app.
const (
	// publicDisk is the bucket behind the CDN; privateDisk is reached only
	// through signed links.
	publicDisk  = "cdn"
	privateDisk = "vault"
	// cdnBase and appBase are never fetched by the demo.
	cdnBase = "https://cdn.example.com"
	appBase = "https://app.example.com/files"
	// linkLifetime is how long a private file's link lives.
	linkLifetime = 10 * time.Minute
	// memoryDB is a private in-memory SQLite database.
	memoryDB = "file::memory:?_pragma=foreign_keys(ON)"
)

// demoSecret is a fixed signing secret so the demo is repeatable.
var demoSecret = bytes.Repeat([]byte("v"), signedurl.MinKeyBytes)

// newLayout builds both disks: the public one serves from the CDN base
// and defaults to public, the private one signs links.
// ds:def id=newlayout-rnmz9jyg
func newLayout() (*filesystem.Manager, error) {
	signer, err := signedurl.New(appBase, []signedurl.Key{{ID: "k1", Secret: demoSecret}})
	if err != nil {
		return nil, err
	}
	cdn, err := filesystem.NewDisk(publicDisk, bucketWide{memory.New()},
		filesystem.WithDefaultVisibility(filesystem.VisibilityPublic), filesystem.WithPublicURL(cdnBase))
	if err != nil {
		return nil, err
	}
	vault, err := filesystem.NewDisk(privateDisk, bucketWide{memory.New()}, filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, err
	}
	return filesystem.NewManager(cdn, vault)
}

// describe prints where a file lives and the link the Store gives for it.
func describe(ctx context.Context, store *records.Store, f records.File, out io.Writer) error {
	u, err := store.URL(ctx, f, linkLifetime)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s on %s: %s\n", f.Visibility, f.Disk, u[:strings.IndexAny(u+"?", "?")])
	return nil
}

// openStore opens SQLite and a Store that keeps public files on the
// public disk and private files on the private one.
func openStore(ctx context.Context, disks *filesystem.Manager) (*records.Store, *sql.DB, error) {
	db, err := sql.Open("sqlite", memoryDB)
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithVisibilityDisks(publicDisk, privateDisk))
	if err == nil {
		_, err = store.Migrate(ctx)
	}
	if err != nil {
		return nil, nil, errors.Join(err, db.Close())
	}
	return store, db, nil
}

// twoDisks uploads a public file, then makes it private. The Store picks
// the disk by visibility, and making the file private moves its object to
// the private disk, so nothing of it stays reachable through the CDN.
// ds:def id=twodisks-j35tq85s
func twoDisks(ctx context.Context, out io.Writer) error {
	disks, err := newLayout()
	if err != nil {
		return err
	}
	store, db, err := openStore(ctx, disks)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	p := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}, Visibility: filesystem.VisibilityPublic}
	f, err := store.Upload(ctx, "acme", "", strings.NewReader("\x89PNG\r\n\x1a\n"), "banner.png", p)
	if err != nil {
		return err
	}
	if err := describe(ctx, store, f, out); err != nil {
		return err
	}
	if err := store.SetVisibility(ctx, "acme", f.ID, filesystem.VisibilityPrivate); err != nil {
		return err
	}
	if f, err = store.File(ctx, "acme", f.ID); err != nil {
		return err
	}
	if err := describe(ctx, store, f, out); err != nil {
		return err
	}
	cdn, err := disks.Disk(publicDisk)
	if err != nil {
		return err
	}
	onCDN, err := cdn.Exists(ctx, f.Key)
	fmt.Fprintf(out, "still on the CDN disk: %t\n", onCDN)
	return err
}
