package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/locks"
	"github.com/ubgo/sqlb"
)

// newTokenStore installs the locks plugin with locks.TokenHolder: a
// change needs the lock's owner and the token the locking client got, as
// WebDAV's If header does, so Alice's second client cannot overwrite the
// check-out of her first.
// ds:def id=newtokenstore-t4k9w2mx
func newTokenStore(ctx context.Context, db *sql.DB, now func() time.Time) (*records.Store, *locks.Plugin, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	lp := locks.New(locks.WithHolderFunc(locks.TokenHolder), locks.WithClock(now))
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(lp))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, lp, nil
}

// saveWithToken is a WebDAV PUT: the actor from the session and the lock
// token from the request's If header, both on the context.
// ds:def id=savewithtoken-h7n3c8qv
func saveWithToken(ctx context.Context, s *records.Store, fileID, user, token, body string) error {
	ctx = records.WithActor(ctx, user)
	ctx = locks.WithLockToken(ctx, token)
	_, err := s.Replace(ctx, tenant, fileID, strings.NewReader(body), "", docs)
	return err
}

// tokenDemo checks a document out from Alice's laptop and saves from the
// laptop (with the token) and from her tablet (without it).
func tokenDemo(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "tokens.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	c := &clock{now: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)}
	s, lp, err := newTokenStore(ctx, db, c.Now)
	if err != nil {
		return err
	}
	f, err := s.Upload(records.WithActor(ctx, alice), tenant, diskName, strings.NewReader("draft 1"), "spec.txt", docs)
	if err != nil {
		return err
	}
	laptop, err := checkOut(ctx, s, lp, f.ID, alice)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "token lock: alice's laptop got a token starting %s\n", laptop.Token[:len(locks.TokenPrefix)])
	fmt.Fprintf(w, "token lock: laptop saves with its token: %s\n", outcome(saveWithToken(ctx, s, f.ID, alice, laptop.Token, "draft 2")))
	fmt.Fprintf(w, "token lock: tablet saves as alice without it: %s\n", outcome(saveWithToken(ctx, s, f.ID, alice, "", "tablet draft")))
	again, err := checkOut(ctx, s, lp, f.ID, alice)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "token lock: extending keeps the token: %t\n", again.Token == laptop.Token)
	return nil
}
