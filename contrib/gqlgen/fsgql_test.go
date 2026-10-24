package fsgql_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/99designs/gqlgen/graphql"
	"github.com/ubgo/sqlb"
	"github.com/vektah/gqlparser/v2/gqlerror"
	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	fsgql "github.com/ubgo/filesystem/contrib/gqlgen"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

const (
	textBody = "quarterly numbers\n"
	htmlBody = "<!doctype html><script>alert(1)</script>"
	maxSize  = 64
)

var policy = upload.Policy{MaxSize: maxSize, Allowed: []string{"text/plain"}, Key: upload.RandomKey("incoming")}

func newDisk(t *testing.T) *filesystem.Disk {
	t.Helper()
	d, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// file is an upload as gqlgen's transport hands it to a resolver: size
// and content type are what the client claimed.
func file(name, body, claimedType string, claimedSize int64) graphql.Upload {
	return graphql.Upload{File: strings.NewReader(body), Filename: name, Size: claimedSize, ContentType: claimedType}
}

// count returns how many objects disk holds.
func count(t *testing.T, d *filesystem.Disk) int {
	t.Helper()
	n := 0
	for _, err := range d.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	return n
}

func TestReceive_StoresThroughThePolicy(t *testing.T) {
	d := newDisk(t)
	res, err := fsgql.Receive(t.Context(), d, file("../../notes.txt", textBody, "image/png", int64(len(textBody))), policy)
	if err != nil {
		t.Fatal(err)
	}
	if res.ContentType != "text/plain; charset=utf-8" || res.Size != int64(len(textBody)) || strings.Contains(res.Filename, "/") {
		t.Fatalf("result = %+v: the type must come from the bytes and the name be made safe", res)
	}
	if got, err := d.ReadAll(t.Context(), res.Key); err != nil || string(got) != textBody {
		t.Fatalf("stored = %q, %v", got, err)
	}
}

func TestReceive_Refuses(t *testing.T) {
	for name, tc := range map[string]struct {
		up   graphql.Upload
		code filesystem.Code
	}{
		"html claiming to be text": {file("page.txt", htmlBody, "text/plain", int64(len(htmlBody))), filesystem.CodeRejected},
		"declared too large":       {file("big.txt", textBody, "text/plain", maxSize+1), filesystem.CodeTooLarge},
		"lying about its size":     {file("big.txt", strings.Repeat("a", maxSize+1), "text/plain", 1), filesystem.CodeTooLarge},
		"no file":                  {graphql.Upload{Filename: "x.txt"}, filesystem.CodeInvalidArgument},
	} {
		d := newDisk(t)
		if _, err := fsgql.Receive(t.Context(), d, tc.up, policy); filesystem.CodeOf(err) != tc.code {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
		if n := count(t, d); n != 0 {
			t.Errorf("%s: %d objects stored", name, n)
		}
	}
}

func TestReceiveAll_AllOrNone(t *testing.T) {
	d := newDisk(t)
	ok := file("a.txt", textBody, "", 0)
	bad := file("b.html", htmlBody, "", 0)
	if _, err := fsgql.ReceiveAll(t.Context(), d, []*graphql.Upload{&ok, &bad}, policy, 5); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("err = %v", err)
	}
	if n := count(t, d); n != 0 {
		t.Fatalf("%d objects left after a failed list", n)
	}
	one, two, three := file("1.txt", textBody, "", 0), file("2.txt", textBody, "", 0), file("3.txt", textBody, "", 0)
	if _, err := fsgql.ReceiveAll(t.Context(), d, []*graphql.Upload{&one, &two, &three}, policy, 2); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || count(t, d) != 0 {
		t.Fatalf("over maxFiles: err = %v, %d stored", err, count(t, d))
	}
	if _, err := fsgql.ReceiveAll(t.Context(), d, []*graphql.Upload{&one, nil}, policy, 2); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || count(t, d) != 0 {
		t.Fatalf("nil upload: err = %v, %d stored", err, count(t, d))
	}
	a, b := file("a.txt", textBody, "", 0), file("b.txt", textBody, "", 0)
	got, err := fsgql.ReceiveAll(t.Context(), d, []*graphql.Upload{&a, &b}, policy, 0)
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || got != nil {
		t.Fatalf("maxFiles 0 must mean 1: %v", err)
	}
	got, err = fsgql.ReceiveAll(t.Context(), d, []*graphql.Upload{&a, &b}, policy, 2)
	if err != nil || len(got) != 2 || count(t, d) != 2 {
		t.Fatalf("got %d, %v", len(got), err)
	}
}

func TestReceiveRecord_RecordsTheActor(t *testing.T) {
	d := newDisk(t)
	disks, err := filesystem.NewManager(d)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx := records.WithActor(t.Context(), "alice")
	f, err := fsgql.ReceiveRecord(ctx, store, "acme", "media", file("notes.txt", textBody, "", int64(len(textBody))), policy)
	if err != nil || f.UploadedBy != "alice" || f.Scope != "acme" || f.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("file = %+v, %v", f, err)
	}
	if _, err := fsgql.ReceiveRecord(ctx, store, "acme", "media", file("big.txt", textBody, "", maxSize+1), policy); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("declared too large: %v", err)
	}
}

func TestErrorPresenter(t *testing.T) {
	ctx := graphql.WithPathContext(context.Background(), graphql.NewPathWithField("upload"))
	present := fsgql.ErrorPresenter(nil)
	driver := filesystem.NewError(filesystem.CodeUnavailable, errors.New("s3: secret-bucket/tenants/acme/x: refused"))
	for name, tc := range map[string]struct {
		err     error
		message string
		code    filesystem.Code
	}{
		"driver":    {driver, http.StatusText(http.StatusServiceUnavailable), filesystem.CodeUnavailable},
		"rejected":  {filesystem.NewError(filesystem.CodeRejected, errors.New("type text/html is not allowed")), "type text/html is not allowed", filesystem.CodeRejected},
		"max bytes": {&http.MaxBytesError{Limit: 1}, (&http.MaxBytesError{Limit: 1}).Error(), filesystem.CodeTooLarge},
	} {
		got := present(ctx, tc.err)
		if got.Message != tc.message || fsgql.Code(got) != tc.code || got.Path.String() != "upload" {
			t.Errorf("%s = %q %q %v", name, got.Message, fsgql.Code(got), got.Path)
		}
	}
	app := errors.New("your own error")
	if got := present(ctx, app); got.Message != app.Error() || fsgql.Code(got) != "" {
		t.Errorf("other errors go to next: %+v", got)
	}
	custom := fsgql.ErrorPresenter(func(context.Context, error) *gqlerror.Error { return gqlerror.Errorf("custom") })
	if got := custom(ctx, app); got.Message != "custom" {
		t.Errorf("next not called: %+v", got)
	}
	if fsgql.Code(nil) != "" || fsgql.Code(&gqlerror.Error{Extensions: map[string]any{fsgql.CodeExtension: "too_large"}}) != filesystem.CodeTooLarge {
		t.Error("Code does not read a code decoded from JSON")
	}
}

func TestMultipartForm(t *testing.T) {
	if f := fsgql.MultipartForm(policy, 3); f.MaxUploadSize != 3*maxSize+fsgql.FormOverhead || f.MaxMemory != fsgql.MaxMemory {
		t.Fatalf("form = %+v", f)
	}
	if f := fsgql.MultipartForm(policy, 0); f.MaxUploadSize != maxSize+fsgql.FormOverhead {
		t.Fatalf("maxFiles 0 = %+v", f)
	}
}

func TestSchema_DeclaresTheScalarAndTheType(t *testing.T) {
	for _, want := range []string{"scalar Upload", "type StoredUpload", "sha256: String!"} {
		if !strings.Contains(fsgql.Schema, want) {
			t.Errorf("schema lacks %q", want)
		}
	}
}
