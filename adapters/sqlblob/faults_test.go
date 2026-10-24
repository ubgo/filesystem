package sqlblob_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/sqlblob"
	"github.com/ubgo/filesystem/records/recordstest"
)

// faultyDriver opens a migrated driver over a pool whose database steps
// (begin, exec, query, commit) can be made to fail one at a time.
func faultyDriver(t *testing.T) (*sqlblob.Driver, *sql.DB, *recordstest.Faults) {
	t.Helper()
	db, faults := recordstest.OpenFaulty(t)
	drv, err := sqlblob.New(sqlblob.Config{DB: db, Dialect: recordstest.Dialect()})
	if err != nil {
		t.Fatal(err)
	}
	if err := drv.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return drv, db, faults
}

// seed writes the objects every swept operation starts from.
func seed(t *testing.T, drv *sqlblob.Driver) {
	t.Helper()
	ctx := context.Background()
	for _, k := range []string{"src", "dir/a", "dir/b"} {
		if _, err := drv.Write(ctx, k, strings.NewReader(k), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestEveryDatabaseStepFailsCleanly measures how many database steps each
// operation takes, then reruns it on a fresh database once per step with
// that step failing: every run must return the injected error, never
// swallow it or report success.
func TestEveryDatabaseStepFailsCleanly(t *testing.T) {
	ctx := context.Background()
	// Each op gets the ETag of "src", read before the faults are armed.
	ops := map[string]func(d *sqlblob.Driver, etag string) error{
		"migrate": func(d *sqlblob.Driver, _ string) error { return d.Migrate(ctx) },
		"stat":    func(d *sqlblob.Driver, _ string) error { _, err := d.Stat(ctx, "src"); return err },
		"read": func(d *sqlblob.Driver, _ string) error {
			_, err := d.Read(ctx, "src", filesystem.ByteRange{})
			return err
		},
		"write": func(d *sqlblob.Driver, _ string) error {
			_, err := d.Write(ctx, "new", strings.NewReader("x"), filesystem.WriteOptions{})
			return err
		},
		"write if-not-exists": func(d *sqlblob.Driver, _ string) error {
			_, err := d.Write(ctx, "new", strings.NewReader("x"), filesystem.WriteOptions{IfNotExists: true})
			return err
		},
		"write if-match": func(d *sqlblob.Driver, etag string) error {
			_, err := d.Write(ctx, "src", strings.NewReader("y"), filesystem.WriteOptions{IfMatch: etag})
			return err
		},
		"delete":      func(d *sqlblob.Driver, _ string) error { return d.Delete(ctx, "src") },
		"delete keys": func(d *sqlblob.Driver, _ string) error { return d.DeleteKeys(ctx, []string{"src", "dir/a"}) },
		"list": func(d *sqlblob.Driver, _ string) error {
			_, err := d.List(ctx, "dir", filesystem.ListOptions{})
			return err
		},
		"copy": func(d *sqlblob.Driver, _ string) error { _, err := d.Copy(ctx, "src", "dst"); return err },
		"move": func(d *sqlblob.Driver, _ string) error { _, err := d.Move(ctx, "src", "dst"); return err },
		"set visibility": func(d *sqlblob.Driver, _ string) error {
			return d.SetVisibility(ctx, "src", filesystem.VisibilityPublic)
		},
		"delete prefix": func(d *sqlblob.Driver, _ string) error { return d.DeletePrefix(ctx, "dir") },
	}
	// prepared returns a seeded driver, its faults switch and src's ETag.
	prepared := func(t *testing.T) (*sqlblob.Driver, *recordstest.Faults, string) {
		drv, _, faults := faultyDriver(t)
		seed(t, drv)
		e, err := drv.Stat(ctx, "src")
		if err != nil {
			t.Fatal(err)
		}
		return drv, faults, e.ETag
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			drv, faults, etag := prepared(t)
			faults.Arm(0)
			if err := op(drv, etag); err != nil {
				t.Fatalf("unfaulted run: %v", err)
			}
			steps := faults.Steps()
			if steps == 0 {
				t.Fatal("the operation took no database step")
			}
			for n := 1; n <= steps; n++ {
				drv, faults, etag := prepared(t)
				faults.Arm(n)
				if err := op(drv, etag); !errors.Is(err, recordstest.ErrInjected) {
					t.Errorf("step %d of %d failing: %v, want the injected error", n, steps, err)
				}
			}
		})
	}
}

// TestBadMetadataIsReported pins that a row whose metadata column does not
// decode (edited by hand) fails Stat and List instead of being served
// without its metadata.
func TestBadMetadataIsReported(t *testing.T) {
	ctx := context.Background()
	drv, db := newDriver(t, sqlblob.Config{})
	if _, err := drv.Write(ctx, "dir/m", strings.NewReader("x"), filesystem.WriteOptions{Metadata: map[string]string{"a": "b"}}); err != nil {
		t.Fatal(err)
	}
	recordstest.Exec(t, db, "UPDATE "+sqlblob.DefaultTable+" SET metadata = '{not json'")
	if _, err := drv.Stat(ctx, "dir/m"); filesystem.CodeOf(err) != filesystem.CodeUnknown {
		t.Fatalf("Stat: %v, want CodeUnknown", err)
	}
	if _, err := drv.List(ctx, "dir", filesystem.ListOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnknown {
		t.Fatalf("List: %v, want CodeUnknown", err)
	}
}

// TestRecursiveListDefaultsThePageSize pins that a recursive listing with
// no Limit is paged at filesystem.DefaultPageSize: one key over it yields
// a full page and a cursor.
func TestRecursiveListDefaultsThePageSize(t *testing.T) {
	ctx := context.Background()
	drv, _ := newDriver(t, sqlblob.Config{})
	keys := make([]string, filesystem.DefaultPageSize+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("p/%04d", i)
		if _, err := drv.Write(ctx, keys[i], strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := drv.List(ctx, "p", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != filesystem.DefaultPageSize || p.NextCursor == "" {
		t.Fatalf("got %d entries, cursor %q; want %d and a cursor", len(p.Entries), p.NextCursor, filesystem.DefaultPageSize)
	}
}

// TestCopyOfAnEmptyObject pins that copying an empty object stores an
// empty value, not NULL, under the new key.
func TestCopyOfAnEmptyObject(t *testing.T) {
	ctx := context.Background()
	drv, _ := newDriver(t, sqlblob.Config{})
	if _, err := drv.Write(ctx, "empty", strings.NewReader(""), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	e, err := drv.Copy(ctx, "empty", "copy")
	if err != nil {
		t.Fatal(err)
	}
	if e.Size != 0 {
		t.Fatalf("copy size %d", e.Size)
	}
	rc, err := drv.Read(ctx, "copy", filesystem.ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if b, _ := io.ReadAll(rc); len(b) != 0 {
		t.Fatalf("read %q", b)
	}
}

// TestSetVisibilityOfAMissingKey pins that changing nothing because the key
// is absent is CodeNotFound, not a silent success.
func TestSetVisibilityOfAMissingKey(t *testing.T) {
	drv, _ := newDriver(t, sqlblob.Config{})
	if err := drv.SetVisibility(context.Background(), "nope", filesystem.VisibilityPublic); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("got %v, want CodeNotFound", err)
	}
}

// TestDeletePrefixOfTheRoot pins that the root prefix deletes every row.
func TestDeletePrefixOfTheRoot(t *testing.T) {
	ctx := context.Background()
	drv, _ := newDriver(t, sqlblob.Config{})
	seed(t, drv)
	if err := drv.DeletePrefix(ctx, ""); err != nil {
		t.Fatal(err)
	}
	p, err := drv.List(ctx, "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 0 {
		t.Fatalf("%d rows left", len(p.Entries))
	}
}

// cancellingBody is an endless body that cancels its context on the first
// read and counts the reads that follow.
type cancellingBody struct {
	cancel context.CancelFunc
	reads  int
}

func (b *cancellingBody) Read(p []byte) (int, error) {
	b.reads++
	b.cancel()
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// TestWriteStopsWhenTheContextEnds pins that the body stops being read as
// soon as the context ends (not at MaxObjectSize), and nothing is stored.
func TestWriteStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	drv, _ := newDriver(t, sqlblob.Config{})
	body := &cancellingBody{cancel: cancel}
	if _, err := drv.Write(ctx, "k", body, filesystem.WriteOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if body.reads != 1 {
		t.Fatalf("body read %d times after the context ended, want 1", body.reads)
	}
	if _, err := drv.Stat(context.Background(), "k"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a cancelled write left a row: %v", err)
	}
}

// skipUpdates makes the database silently skip every UPDATE of the table
// (no error, no row counted), which is how MySQL reports an update that
// changed no value. SQLite and Postgres do it with a trigger; MySQL needs
// none, it does so natively for an unchanged row, and reports false.
func skipUpdates(t *testing.T, db *sql.DB) bool {
	t.Helper()
	switch recordstest.Engine() {
	case recordstest.SQLite:
		recordstest.Exec(t, db, "CREATE TRIGGER skip_updates BEFORE UPDATE ON "+sqlblob.DefaultTable+" BEGIN SELECT RAISE(IGNORE); END")
		return true
	case recordstest.Postgres:
		recordstest.Exec(t, db, "CREATE FUNCTION skip_update() RETURNS trigger AS $$ BEGIN RETURN NULL; END $$ LANGUAGE plpgsql")
		recordstest.Exec(t, db, "CREATE TRIGGER skip_updates BEFORE UPDATE ON "+sqlblob.DefaultTable+" FOR EACH ROW EXECUTE FUNCTION skip_update()")
		return true
	}
	return false
}

// TestCompareAndSwapWhenNoRowIsCounted pins swap's fallback for an update
// the database did not count: a row still carrying the expected ETag that
// already holds the new bytes is a success (MySQL's unchanged row), while
// a row still carrying the ETag but not the new bytes is a conflict,
// because the write did not land.
func TestCompareAndSwapWhenNoRowIsCounted(t *testing.T) {
	ctx := context.Background()
	drv, db := newDriver(t, sqlblob.Config{})
	e, err := drv.Write(ctx, "k", strings.NewReader("same"), filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	skipped := skipUpdates(t, db)
	if _, err := drv.Write(ctx, "k", strings.NewReader("same"), filesystem.WriteOptions{IfMatch: e.ETag}); err != nil {
		t.Fatalf("same bytes, uncounted update: %v, want success", err)
	}
	if !skipped {
		return // MySQL: an update with different bytes is counted, nothing more to see
	}
	if _, err := drv.Write(ctx, "k", strings.NewReader("other"), filesystem.WriteOptions{IfMatch: e.ETag}); filesystem.CodeOf(err) != filesystem.CodeConflict {
		t.Fatalf("other bytes, uncounted update: %v, want CodeConflict", err)
	}
}
