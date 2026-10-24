// Command records-locks shows the locks plugin of the records layer on
// SQLite and an in-memory disk: Alice checks a document out, her
// replacement goes through while Bob's, his trash and an actor-less
// purge are refused, the lock expires by itself on the plugin's clock,
// an administrator breaks a lock, and Sweep tidies the expired ones. The
// lock-free alternative, records.IfUnchanged, is in records-replace.
//
//	go run ./records-locks
//
// It prints what each step returned; main_test.go runs the same steps.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/locks"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	diskName = "docs"
	tenant   = "acme"
	alice    = "user:alice"
	bob      = "user:bob"
	// editWindow is how long a check-out lasts unless renewed.
	editWindow = 30 * time.Minute
	// maxLock is the longest check-out the app allows.
	maxLock = 2 * time.Hour
)

// docs accepts plain-text documents.
var docs = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"text/plain"}, Key: upload.RandomKey("docs")}

// clock is the plugin's clock, moved by the demo past a lock's expiry.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

// newStore installs the locks plugin. The owner of a request is the actor
// set with records.WithActor; WithMaxTTL refuses longer check-outs.
// ds:def id=newlocksstore-p8w3k6nc
func newStore(ctx context.Context, db *sql.DB, now func() time.Time) (*records.Store, *locks.Plugin, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	lp := locks.New(locks.WithMaxTTL(maxLock), locks.WithClock(now))
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(lp))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, lp, nil
}

// checkOut locks a document for its editor. Locking again extends the
// editor's own lock; another owner's live lock is locks.ErrLocked.
// ds:def id=checkout-v2n7q4hx
func checkOut(ctx context.Context, s *records.Store, lp *locks.Plugin, fileID, user string) (locks.Lock, error) {
	return lp.Lock(ctx, s, tenant, fileID, user, editWindow)
}

// save replaces a document's content as user. While someone else holds
// the lock it fails with locks.ErrLocked and nothing changes.
// ds:def id=save-k3m8t5wz
func save(ctx context.Context, s *records.Store, fileID, user, body string) error {
	ctx = records.WithActor(ctx, user)
	_, err := s.Replace(ctx, tenant, fileID, strings.NewReader(body), "", docs)
	return err
}

// checkIn releases the editor's lock. Unlocking a lock you do not hold
// changes nothing.
// ds:def id=checkin-r6c9w2tm
func checkIn(ctx context.Context, s *records.Store, lp *locks.Plugin, fileID, user string) error {
	return lp.Unlock(ctx, s, tenant, fileID, user)
}

// outcome names an error for the printout.
func outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, locks.ErrLocked):
		return "locked (" + string(filesystem.CodeOf(err)) + ")"
	default:
		return string(filesystem.CodeOf(err))
	}
}

// run performs every step and prints what it got.
func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "records.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	c := &clock{now: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)}
	s, lp, err := newStore(ctx, db, c.Now)
	if err != nil {
		return err
	}
	f, err := s.Upload(records.WithActor(ctx, alice), tenant, diskName, strings.NewReader("draft 1"), "plan.txt", docs)
	if err != nil {
		return err
	}
	l, err := checkOut(ctx, s, lp, f.ID, alice)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "alice checked out until %s\n", l.ExpiresAt.Format("15:04"))
	_, err = checkOut(ctx, s, lp, f.ID, bob)
	fmt.Fprintf(w, "bob check out: %s\n", outcome(err))
	fmt.Fprintf(w, "alice saves: %s\n", outcome(save(ctx, s, f.ID, alice, "draft 2")))
	fmt.Fprintf(w, "bob saves: %s\n", outcome(save(ctx, s, f.ID, bob, "bob's draft")))
	fmt.Fprintf(w, "bob trashes: %s\n", outcome(s.Trash(records.WithActor(ctx, bob), tenant, f.ID)))
	fmt.Fprintf(w, "purge with no actor: %s\n", outcome(s.Purge(ctx, tenant, f.ID)))
	_, err = lp.Lock(ctx, s, tenant, f.ID, alice, 3*time.Hour)
	fmt.Fprintf(w, "alice asks for 3h: %s\n", outcome(err))
	holder, held, err := lp.LockOf(ctx, s, tenant, f.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "held %t by %s\n", held, holder.Owner)
	c.Add(editWindow + time.Minute)
	fmt.Fprintf(w, "31 minutes later, bob saves: %s\n", outcome(save(ctx, s, f.ID, bob, "bob's draft")))
	if _, err := checkOut(ctx, s, lp, f.ID, bob); err != nil {
		return err
	}
	fmt.Fprintf(w, "alice checks in bob's lock: %s, still held %t\n", outcome(checkIn(ctx, s, lp, f.ID, alice)), mustHeld(ctx, s, lp, f.ID))
	if err := lp.Break(ctx, s, tenant, f.ID); err != nil {
		return err
	}
	fmt.Fprintf(w, "admin breaks it: held %t\n", mustHeld(ctx, s, lp, f.ID))
	if _, err := checkOut(ctx, s, lp, f.ID, alice); err != nil {
		return err
	}
	if err := checkIn(ctx, s, lp, f.ID, alice); err != nil {
		return err
	}
	if _, err := checkOut(ctx, s, lp, f.ID, bob); err != nil {
		return err
	}
	c.Add(time.Hour)
	n, err := lp.Sweep(ctx, s, c.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "sweep removed %d expired lock(s)\n", n)
	if err := tokenDemo(ctx, w, dir); err != nil {
		return err
	}
	return sharedDemo(ctx, w, dir)
}

// mustHeld reports whether a live lock holds the file; an error reads as
// not held, which the printout makes visible.
func mustHeld(ctx context.Context, s *records.Store, lp *locks.Plugin, fileID string) bool {
	_, held, err := lp.LockOf(ctx, s, tenant, fileID)
	return err == nil && held
}

func main() {
	dir, err := os.MkdirTemp("", "records-locks")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
