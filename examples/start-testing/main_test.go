package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/layer"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/sqlb"
)

// gif is the smallest prefix the content sniffer recognises as a GIF.
const gif = "GIF89a"

// TestAvatarSaved hands the service a Fake disk and asserts on what it
// stored.
// ds:def id=testing-avatarsaved-fx8nkktf
func TestAvatarSaved(t *testing.T) {
	disk := filesystemtest.NewFake(t, "media")
	key, err := NewAvatars(disk.Disk).Save(t.Context(), "u1", strings.NewReader(gif))
	if err != nil {
		t.Fatal(err)
	}
	disk.AssertExists(key)
	disk.AssertContent("avatars/u1", gif)
	disk.AssertCount("avatars", 1)
}

// TestAvatarRefused checks the refusal by its code, never by its message,
// and that nothing was stored.
// ds:def id=testing-avatarrefused-pm2dhgbn
func TestAvatarRefused(t *testing.T) {
	disk := filesystemtest.NewFake(t, "media")
	_, err := NewAvatars(disk.Disk).Save(t.Context(), "u1", strings.NewReader("<svg onload=alert(1)>"))
	if !errors.Is(err, filesystem.ErrRejected) {
		t.Fatalf("err = %v, want CodeRejected", err)
	}
	disk.AssertEmpty()
}

// TestAvatarRemoved checks a delete, and that deleting twice is fine.
func TestAvatarRemoved(t *testing.T) {
	disk := filesystemtest.NewFake(t, "media")
	avatars := NewAvatars(disk.Disk)
	if _, err := avatars.Save(t.Context(), "u1", strings.NewReader(gif)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := avatars.Remove(t.Context(), "u1"); err != nil {
			t.Fatal(err)
		}
	}
	disk.AssertMissing("avatars/u1")
}

// TestFromDSN opens the disk the way production configuration does, with
// a memory DSN, so DSN parameters are under test too.
// ds:def id=testing-fromdsn-jyf9ghyk
func TestFromDSN(t *testing.T) {
	mux := filesystem.NewMux()
	if err := mux.Register(memory.Scheme, memory.Opener); err != nil {
		t.Fatal(err)
	}
	disk, err := mux.Open(t.Context(), "media", "mem://?readonly=true")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAvatars(disk).Save(t.Context(), "u1", strings.NewReader(gif)); !errors.Is(err, filesystem.ErrReadOnly) {
		t.Fatalf("err = %v, want CodeReadOnly", err)
	}
}

// newStore returns a migrated records store on a fresh SQLite file in the
// test's temporary directory, reaching its bytes through disk. A file,
// not ":memory:", because each connection to ":memory:" is a separate
// empty database.
// ds:def id=testing-newstore-6vkdpbv5
func newStore(t *testing.T, disk *filesystem.Disk, opts ...records.Option) *records.Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, append([]records.Option{records.WithOwnerTypes(ownerUser)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestGallery checks rows and bytes together: the store lists what the
// disk holds.
// ds:def id=testing-gallery-x5rhck7j
func TestGallery(t *testing.T) {
	disk := filesystemtest.NewFake(t, mediaDisk)
	gallery := NewGallery(newStore(t, disk.Disk))
	for _, name := range []string{"a.gif", "b.gif"} {
		if _, err := gallery.Add(t.Context(), "u1", strings.NewReader(gif), name); err != nil {
			t.Fatal(err)
		}
	}
	photos, err := gallery.Photos(t.Context(), "u1")
	if err != nil || len(photos) != 2 || photos[0].File.Filename != "a.gif" {
		t.Fatalf("photos = %+v, %v", photos, err)
	}
	disk.AssertCount(rolePhotos, 2)
}

// TestRolledBackUploadLeavesNoBytes makes the database step fail (a quota
// of one file) and checks the uploaded object was deleted again.
// ds:def id=testing-rollback-w3zc6n3s
func TestRolledBackUploadLeavesNoBytes(t *testing.T) {
	disk := filesystemtest.NewFake(t, mediaDisk)
	oneFile, err := quota.New(func(context.Context, string) (quota.Limit, error) { return quota.Limit{Files: 1}, nil })
	if err != nil {
		t.Fatal(err)
	}
	gallery := NewGallery(newStore(t, disk.Disk, records.WithPlugins(oneFile)))
	if _, err := gallery.Add(t.Context(), "u1", strings.NewReader(gif), "a.gif"); err != nil {
		t.Fatal(err)
	}
	if _, err := gallery.Add(t.Context(), "u1", strings.NewReader(gif), "b.gif"); !errors.Is(err, filesystem.ErrQuotaExceeded) {
		t.Fatalf("err = %v, want CodeQuotaExceeded", err)
	}
	disk.AssertCount(rolePhotos, 1)
}

// failFirst is an interceptor that fails the first n operations of kind
// with CodeUnavailable, the code of a transient backend failure, without
// reaching the driver.
// ds:def id=testing-failfirst-gg62jgzr
func failFirst(kind filesystem.OpKind, n int) filesystem.Interceptor {
	var mu sync.Mutex
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		mu.Lock()
		fail := op.Kind == kind && n > 0
		if fail {
			n--
		}
		mu.Unlock()
		if fail {
			return filesystem.Errorf(filesystem.CodeUnavailable, "injected outage")
		}
		return call(ctx)
	}
}

// noSleep makes Retry retry at once, so the test does not wait.
func noSleep(context.Context, time.Duration) error { return nil }

// TestRetryRidesOutOutage injects two failed writes under a Retry
// interceptor: a write of bytes is retried and lands, a streamed upload is
// not retried (its body cannot be replayed) and stores nothing.
// ds:def id=testing-retry-5yd7p44f
func TestRetryRidesOutOutage(t *testing.T) {
	disk := filesystemtest.NewFake(t, "media", filesystem.WithInterceptors(
		layer.Retry(layer.RetryOptions{Attempts: 3, Sleep: noSleep}),
		failFirst(filesystem.OpWrite, 2),
	))
	if _, err := disk.WriteBytes(t.Context(), "a.txt", []byte("a"), filesystem.WriteOptions{}); err != nil {
		t.Fatalf("retried write: %v", err)
	}
	disk.AssertContent("a.txt", "a")

	streamed := filesystemtest.NewFake(t, "media", filesystem.WithInterceptors(
		layer.Retry(layer.RetryOptions{Attempts: 3, Sleep: noSleep}),
		failFirst(filesystem.OpWrite, 1),
	))
	if _, err := NewAvatars(streamed.Disk).Save(t.Context(), "u1", bytes.NewReader([]byte(gif))); !errors.Is(err, filesystem.ErrUnavailable) {
		t.Fatalf("streamed upload: err = %v, want CodeUnavailable", err)
	}
	streamed.AssertEmpty()
}

// TestReadOutage uses layer.Guard to fail every read, as a backend that
// is down would, and checks the error the caller sees.
// ds:def id=testing-guard-6us897vb
func TestReadOutage(t *testing.T) {
	down := layer.Guard(func(_ context.Context, op *filesystem.Op) error {
		if op.Kind == filesystem.OpRead {
			return filesystem.Errorf(filesystem.CodeUnavailable, "injected outage")
		}
		return nil
	})
	disk := filesystemtest.NewFake(t, "media", filesystem.WithInterceptors(down))
	if _, err := disk.WriteBytes(t.Context(), "a.txt", []byte("a"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.ReadAll(t.Context(), "a.txt"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("err = %v", err)
	}
}

// clock is a movable test clock.
// ds:def id=testing-clock-q9v8qeg7
type clock struct {
	mu  sync.Mutex
	now time.Time
}

// Now returns the clock's time.
func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

// Advance moves the clock forward by d.
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

// TestTrashRetention moves the store's clock instead of sleeping: a file
// trashed 31 days ago is purged by a 30-day sweep, bytes included.
// ds:def id=testing-trash-fsxm6286
func TestTrashRetention(t *testing.T) {
	const retention = 30 * 24 * time.Hour
	c := &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	disk := filesystemtest.NewFake(t, mediaDisk)
	store := newStore(t, disk.Disk, records.WithClock(c.Now))
	f, err := NewGallery(store).Add(t.Context(), "u1", strings.NewReader(gif), "a.gif")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Trash(t.Context(), "u1", f.ID); err != nil {
		t.Fatal(err)
	}
	c.Advance(retention + 24*time.Hour)
	if n, err := store.SweepTrash(t.Context(), c.Now().Add(-retention)); err != nil || n != 1 {
		t.Fatalf("swept %d, %v", n, err)
	}
	disk.AssertEmpty()
}

// TestConformance runs the suite every built-in driver passes. Put your
// own driver in the factory; a new, empty disk per case.
// ds:def id=testing-conformance-584pkj8j
func TestConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		drv, err := local.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		disk, err := filesystem.NewDisk("conformance", drv)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = disk.Close() })
		return disk
	})
}
