package records_test

import (
	"context"
	"database/sql"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

const pngData = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-png-body"

var images = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}

// env is a migrated SQLite store over a memory disk named "media".
type env struct {
	db    *sql.DB
	disk  *filesystem.Disk
	store *records.Store
	now   time.Time
}

func engine() string                          { return testdb.Engine() }
func testDialect() sqlb.Dialect               { return testdb.Dialect() }
func openDB(t *testing.T) *sql.DB             { return testdb.Open(t) }
func exec(t *testing.T, db *sql.DB, q string) { testdb.Exec(t, db, q) }
func onlySQLite(t *testing.T)                 { testdb.OnlySQLite(t) }

func newEnv(t *testing.T, drv filesystem.Driver, opts ...records.Option) *env {
	t.Helper()
	return newEnvOn(t, openDB(t), drv, opts...)
}

// newEnvOn is newEnv on a given database, such as one from
// testdb.OpenFaulty.
func newEnvOn(t *testing.T, db *sql.DB, drv filesystem.Driver, opts ...records.Option) *env {
	t.Helper()
	e := &env{db: db, now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	if drv == nil {
		drv = memory.New()
	}
	disk, err := filesystem.NewDisk("media", drv, filesystem.WithURLSigner(signer{}), filesystem.WithPublicURL("https://cdn.test"))
	if err != nil {
		t.Fatal(err)
	}
	e.disk = disk
	mgr, _ := filesystem.NewManager(disk)
	all := append([]records.Option{
		records.WithClock(func() time.Time { e.now = e.now.Add(time.Second); return e.now }),
		records.WithOwnerTypes("user", "post"),
	}, opts...)
	e.store, err = records.New(e.db, testDialect(), mgr, all...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

// put stores an object and records it.
func (e *env) put(t *testing.T, scope, key string) records.File {
	t.Helper()
	if _, err := e.disk.WriteBytes(t.Context(), key, []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: key, Filename: key})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if got := filesystem.CodeOf(err); got != code {
		t.Fatalf("code %q (err %v), want %q", got, err, code)
	}
}

type signer struct{}

func (signer) SignURL(_ context.Context, disk, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	u := "https://app.test/files/" + disk + "/" + key + "?sig"
	if o.ContentDisposition != "" {
		u += "&cd=" + o.ContentDisposition
	}
	return filesystem.SignedRequest{Method: o.Method, URL: u}, nil
}

type undeletable struct{ *memory.Driver }

func (undeletable) Delete(context.Context, string) error { return filesystem.ErrPermission }

func reader(s string) io.Reader { return strings.NewReader(s) }

// plainDriver is a shared memory driver without per-object visibility.
type plainDriver struct{}

var plainStore = memory.New()

func (plainDriver) Stat(ctx context.Context, k string) (filesystem.Entry, error) {
	e, err := plainStore.Stat(ctx, k)
	e.Visibility = ""
	return e, err
}
func (plainDriver) Read(ctx context.Context, k string, r filesystem.ByteRange) (io.ReadCloser, error) {
	return plainStore.Read(ctx, k, r)
}
func (plainDriver) Write(ctx context.Context, k string, b io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	return plainStore.Write(ctx, k, b, o)
}
func (plainDriver) Delete(ctx context.Context, k string) error { return plainStore.Delete(ctx, k) }
func (plainDriver) List(ctx context.Context, d string, o filesystem.ListOptions) (filesystem.Page, error) {
	return plainStore.List(ctx, d, o)
}
func (plainDriver) Capabilities() filesystem.Capabilities { return filesystem.Capabilities{} }
