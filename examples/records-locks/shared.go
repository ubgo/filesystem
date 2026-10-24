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

// carol is a third user, who joins neither the shared lock nor the edit.
const carol = "user:carol"

// newSharedStore installs the locks plugin with WithSharedLocks, WebDAV's
// shared write lock, and TokenHolder: several editors hold a document at
// once, each with a token of their own, and everyone else is kept out.
// ds:def id=newsharedstore-m5r8k2wd
func newSharedStore(ctx context.Context, db *sql.DB, now func() time.Time, opts ...locks.Option) (*records.Store, *locks.Plugin, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	lp := locks.New(append([]locks.Option{locks.WithHolderFunc(locks.TokenHolder), locks.WithClock(now)}, opts...)...)
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(lp))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, lp, nil
}

// coEdit gives Alice and Bob a shared lock each on one document, so both
// may save it (each with their own token) while Carol may neither save
// nor take the document for herself.
// ds:def id=coedit-x3q7n9vb
func coEdit(ctx context.Context, s *records.Store, lp *locks.Plugin, fileID string) (aliceLock, bobLock locks.Lock, err error) {
	if aliceLock, err = lp.LockShared(ctx, s, tenant, fileID, alice, editWindow); err != nil {
		return locks.Lock{}, locks.Lock{}, err
	}
	if bobLock, err = lp.LockShared(ctx, s, tenant, fileID, bob, editWindow); err != nil {
		return locks.Lock{}, locks.Lock{}, err
	}
	return aliceLock, bobLock, nil
}

// sharedDemo runs a co-editing session on its own database.
func sharedDemo(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "shared.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	c := &clock{now: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)}
	s, lp, err := newSharedStore(ctx, db, c.Now, locks.WithSharedLocks())
	if err != nil {
		return err
	}
	f, err := s.Upload(records.WithActor(ctx, alice), tenant, diskName, strings.NewReader("outline"), "design.txt", docs)
	if err != nil {
		return err
	}
	a, b, err := coEdit(ctx, s, lp, f.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "shared lock: alice and bob hold design.txt, own tokens: %t\n", a.Token != b.Token)
	fmt.Fprintf(w, "shared lock: alice saves with her token: %s\n", outcome(saveWithToken(ctx, s, f.ID, alice, a.Token, "outline + intro")))
	fmt.Fprintf(w, "shared lock: bob saves with his token: %s\n", outcome(saveWithToken(ctx, s, f.ID, bob, b.Token, "outline + intro + api")))
	fmt.Fprintf(w, "shared lock: bob saves with alice's token: %s\n", outcome(saveWithToken(ctx, s, f.ID, bob, a.Token, "x")))
	fmt.Fprintf(w, "shared lock: carol saves: %s\n", outcome(saveWithToken(ctx, s, f.ID, carol, "", "carol's draft")))
	_, err = checkOut(ctx, s, lp, f.ID, carol)
	fmt.Fprintf(w, "shared lock: carol checks out alone: %s\n", outcome(err))
	if err := lp.UnlockShared(ctx, s, tenant, f.ID, alice); err != nil {
		return err
	}
	held, err := lp.SharedLocksOf(ctx, s, tenant, f.ID)
	if err != nil {
		return err
	}
	owners := make([]string, len(held))
	for i, l := range held {
		owners[i] = l.Owner
	}
	fmt.Fprintf(w, "shared lock: after alice unlocks, held by %s\n", strings.Join(owners, ","))

	plain, plainLocks, err := newSharedStore(ctx, db, c.Now)
	if err != nil {
		return err
	}
	_, err = plainLocks.LockShared(ctx, plain, tenant, f.ID, carol, editWindow)
	fmt.Fprintf(w, "shared lock: without WithSharedLocks: %s\n", outcome(err))
	return nil
}
