package records_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
)

func TestText(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	if _, err := e.disk.WriteBytes(ctx, "a.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "a.png", Metadata: map[string]string{"width": "10", records.MetaTitle: "Beach"}})
	if err != nil || f.Text() != (records.Text{Title: "Beach"}) {
		t.Fatalf("CreateFile = %+v, %v", f.Text(), err)
	}

	want := records.Text{Alt: "A dog on a beach", Caption: "Day one.\nThe tide was out.", Title: "Beach"}
	got, err := e.store.SetText(ctx, "t", f.ID, want)
	if err != nil || got.Text() != want || got.Metadata["width"] != "10" {
		t.Fatalf("SetText = %+v (%v), %v", got.Text(), got.Metadata, err)
	}
	if read, _ := e.store.File(ctx, "t", f.ID); read.Text() != want || read.Metadata["width"] != "10" {
		t.Fatalf("read back %+v %v", read.Text(), read.Metadata)
	}
	// Setting the same text again changes nothing.
	if again, err := e.store.SetText(ctx, "t", f.ID, want); err != nil || again.Text() != want {
		t.Fatalf("same text = %+v, %v", again.Text(), err)
	}
	// "" removes a field; the other metadata stays.
	got, err = e.store.SetText(ctx, "t", f.ID, records.Text{Alt: "A dog"})
	if err != nil || got.Text() != (records.Text{Alt: "A dog"}) || len(got.Metadata) != 2 {
		t.Fatalf("partial = %+v (%v), %v", got.Text(), got.Metadata, err)
	}
	// A trashed file can be described too.
	_ = e.store.Trash(ctx, "t", f.ID)
	if _, err := e.store.SetText(ctx, "t", f.ID, records.Text{}); err != nil {
		t.Fatal(err)
	}
	if read, _ := e.store.File(ctx, "t", f.ID); read.Metadata[records.MetaAlt] != "" || read.Metadata["width"] != "10" {
		t.Fatalf("cleared: %v", read.Metadata)
	}

	// A file with no metadata at all.
	plain := e.put(t, "t", "b.png")
	if got, err := e.store.SetText(ctx, "t", plain.ID, records.Text{}); err != nil || got.Metadata != nil {
		t.Fatalf("empty on empty = %v, %v", got.Metadata, err)
	}
	if got, err := e.store.SetText(ctx, "t", plain.ID, records.Text{Title: "x"}); err != nil || got.Text().Title != "x" {
		t.Fatalf("title on empty = %+v, %v", got.Text(), err)
	}
	if got, err := e.store.SetText(ctx, "t", plain.ID, records.Text{}); err != nil || got.Metadata != nil {
		t.Fatalf("cleared to nothing = %v, %v", got.Metadata, err)
	}

	for _, bad := range []records.Text{
		{Alt: strings.Repeat("a", records.MaxAltChars+1)},
		{Title: strings.Repeat("t", records.MaxTitleChars+1)},
		{Caption: strings.Repeat("c", records.MaxCaptionChars+1)},
		{Alt: "two\nlines"},
		{Title: "tab\there"},
		{Caption: "bell\a"},
		{Caption: "\xff"},
	} {
		_, err := e.store.SetText(ctx, "t", f.ID, bad)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
	// The limits hold for metadata given at creation too.
	_, err = e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "a.png", Metadata: map[string]string{records.MetaAlt: "x\ny"}})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	// Within the limits, by characters rather than bytes.
	if _, err := e.store.SetText(ctx, "t", f.ID, records.Text{Title: strings.Repeat("é", records.MaxTitleChars)}); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.SetText(ctx, "t", "fil_nope", records.Text{})
	wantCode(t, err, filesystem.CodeNotFound)
}

// TestTextForeignFormat matches a row as stored, not as re-encoded: a
// row another writer formatted differently is still updated.
func TestTextForeignFormat(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.put(t, "t", "a.png")
	exec(t, e.db, "UPDATE "+records.TableNameFiles+" SET "+records.ColFileMetadata+` = '{ "width": "10" }'`)
	got, err := e.store.SetText(ctx, "t", f.ID, records.Text{Alt: "x"})
	if err != nil || got.Metadata["width"] != "10" || got.Text().Alt != "x" {
		t.Fatalf("SetText on a foreign-formatted row = %v, %v", got.Metadata, err)
	}
}

// changingConn runs change on the same connection, inside the same
// transaction, just before the first UPDATE of fs_files.metadata it sees
// while armed: a writer that changed the metadata between SetText's read
// and its write, staged without timing.
type changingConn struct {
	driver.Conn
	armed  *bool
	change string
}

func (c *changingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	exec := c.Conn.(driver.ExecerContext)
	if *c.armed && strings.HasPrefix(query, "UPDATE") && strings.Contains(query, records.TableNameFiles) && strings.Contains(query, records.ColFileMetadata) {
		*c.armed = false
		if _, err := exec.ExecContext(ctx, c.change, nil); err != nil {
			return nil, err
		}
	}
	return exec.ExecContext(ctx, query, args)
}

type changingConnector struct {
	dsn   string
	inner driver.Driver
	conn  changingConn
}

func (c *changingConnector) Connect(context.Context) (driver.Conn, error) {
	inner, err := c.inner.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &changingConn{Conn: inner, armed: c.conn.armed, change: c.conn.change}, nil
}

func (c *changingConnector) Driver() driver.Driver { return c.inner }

// TestTextRefusesAChangedRow changes the stored metadata between SetText's
// read and its write; the write, conditional on the metadata it read,
// misses, and SetText reports the race instead of overwriting the other
// writer's change. Without the condition the write would land on the
// changed row and report success. (SQLite only: the connection wrapper is the engine's.)
func TestTextRefusesAChangedRow(t *testing.T) {
	onlySQLite(t)
	ctx := t.Context()
	dsn := "file:" + filepath.Join(t.TempDir(), "race.db")
	probe, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	inner := probe.Driver()
	_ = probe.Close()
	armed := false
	db := sql.OpenDB(&changingConnector{dsn: dsn, inner: inner, conn: changingConn{
		armed: &armed, change: "UPDATE " + records.TableNameFiles + " SET " + records.ColFileMetadata + ` = '{"width":"99"}'`,
	}})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	e := newEnvOn(t, db, nil)
	f := e.put(t, "t", "a.png")

	armed = true
	_, err = e.store.SetText(ctx, "t", f.ID, records.Text{Alt: "x"})
	wantCode(t, err, filesystem.CodeUnavailable)
	// The refusal rolled back the whole transaction, the staged change
	// with it (it ran on the same transaction); nothing was written.
	if got, _ := e.store.File(ctx, "t", f.ID); got.Metadata != nil {
		t.Fatalf("a refused SetText left %v", got.Metadata)
	}
	if got, err := e.store.SetText(ctx, "t", f.ID, records.Text{Alt: "x"}); err != nil || got.Text().Alt != "x" {
		t.Fatalf("unarmed SetText = %v, %v", got.Metadata, err)
	}
}

// TestTextWriteFailure returns a failed write.
func TestTextWriteFailure(t *testing.T) {
	onlySQLite(t)
	e := newEnv(t, nil)
	f := e.put(t, "t", "a.png")
	exec(t, e.db, "CREATE TRIGGER fail BEFORE UPDATE OF "+records.ColFileMetadata+" ON "+records.TableNameFiles+" BEGIN SELECT RAISE(FAIL, 'injected'); END")
	if _, err := e.store.SetText(t.Context(), "t", f.ID, records.Text{Alt: "x"}); err == nil || filesystem.CodeOf(err) == filesystem.CodeUnavailable {
		t.Fatalf("a failed write = %v", err)
	}
}

// longAlt is a ReplaceHooks plugin that writes alt text over the limit.
type longAlt struct{}

func (longAlt) Name() string { return "longalt" }
func (longAlt) BeforeReplace(_ context.Context, _ *records.Store, _ records.File, next *records.File) error {
	next.Metadata = map[string]string{records.MetaAlt: strings.Repeat("x", records.MaxAltChars+1)}
	return nil
}

// TestTextLimitsOnReplace holds text a replace hook adds to the limits,
// as CreateFile does.
func TestTextLimitsOnReplace(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, records.WithPlugins(longAlt{}))
	f := e.put(t, "t", "a.png")
	if _, err := e.disk.WriteBytes(ctx, "b.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := e.store.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "b.png"})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if got, _ := e.store.File(ctx, "t", f.ID); got.Key != "a.png" || got.Text().Alt != "" {
		t.Fatalf("a refused replacement changed the file: %+v", got)
	}
}

func TestSHA256Normalized(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	upper := strings.Repeat("AB", 32)
	for _, k := range []string{"a.png", "b.png", "c.png"} {
		if _, err := e.disk.WriteBytes(ctx, k, []byte(pngData), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	f, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "a.png", SHA256: upper})
	if err != nil || f.SHA256 != strings.ToLower(upper) {
		t.Fatalf("CreateFile = %q, %v", f.SHA256, err)
	}
	if got, err := e.store.FileBySHA256(ctx, "t", upper); err != nil || got.ID != f.ID {
		t.Fatalf("FileBySHA256 = %v, %v", got.ID, err)
	}
	for _, bad := range []string{"abc", strings.Repeat("z", 64)} {
		_, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "b.png", SHA256: bad})
		wantCode(t, err, filesystem.CodeInvalidArgument)
		_, err = e.store.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "b.png", SHA256: bad})
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
	got, err := e.store.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "c.png", SHA256: strings.Repeat("CD", 32)})
	if err != nil || got.SHA256 != strings.Repeat("cd", 32) {
		t.Fatalf("ReplaceObject = %q, %v", got.SHA256, err)
	}
}

// oddSum is a driver reporting a digest that is not hex SHA-256.
type oddSum struct{ *memory.Driver }

func (d oddSum) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	e, err := d.Driver.Stat(ctx, key)
	e.SHA256 = "md5:abc"
	return e, err
}

// TestReportedDigestNotHex: a disk's digest that is not hex SHA-256 is
// left out, not stored and not a failure.
func TestReportedDigestNotHex(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, oddSum{memory.New()})
	if _, err := e.disk.WriteBytes(ctx, "a.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "a.png"})
	if err != nil || f.SHA256 != "" {
		t.Fatalf("CreateFile = %q, %v", f.SHA256, err)
	}
}
