package dropbox_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/dropbox"
	"github.com/ubgo/filesystem/filesystemtest"
)

// staticToken is a TokenSource holding the token in a field, the shape
// that would leak if a Config or Driver printed its source.
type staticToken struct{ token string }

func (s staticToken) AccessToken(context.Context) (string, error) { return s.token, nil }

func config(f *fake) dropbox.Config {
	return dropbox.Config{Token: staticToken{testToken}, Root: "/Apps/Test", APIURL: f.URL + "/2", ContentURL: f.URL + "/2"}
}

func newDriver(t *testing.T, cfg dropbox.Config) *dropbox.Driver {
	t.Helper()
	drv, err := dropbox.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return drv
}

func TestConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		d, err := filesystem.NewDisk("dropbox", newDriver(t, config(newFake(t))))
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}

// TestConformanceChunked runs the suite with a chunk far smaller than its
// bodies, so every multi-chunk write goes through an upload session.
func TestConformanceChunked(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		cfg := config(newFake(t))
		cfg.ChunkSize = 1024
		d, err := filesystem.NewDisk("dropbox", newDriver(t, cfg))
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}

// TestWriteUsesSessionPastOneChunk pins the chunking: one request for a
// short body, start + appends + finish for a long one, nothing committed
// before the finish.
func TestWriteUsesSessionPastOneChunk(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	cfg := config(f)
	cfg.ChunkSize = 10
	drv := newDriver(t, cfg)
	if _, err := drv.Write(ctx, "small", strings.NewReader("short"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("0123456789", 4) + "tail"
	if _, err := drv.Write(ctx, "big", strings.NewReader(body), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"files/upload": 1, "files/upload_session/start": 1, "files/upload_session/append_v2": 3, "files/upload_session/finish": 1}
	for k, n := range want {
		if f.requests[k] != n {
			t.Errorf("%s called %d times, want %d (all: %v)", k, f.requests[k], n, f.requests)
		}
	}
	rc, err := drv.Read(ctx, "big", filesystem.ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != body {
		t.Fatalf("read back %q", got)
	}
}

// TestFailingBodyMidSessionStoresNothing: a body failing after the
// session started never reaches a finish, so no file appears.
func TestFailingBodyMidSessionStoresNothing(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	cfg := config(f)
	cfg.ChunkSize = 4
	drv := newDriver(t, cfg)
	body := io.MultiReader(strings.NewReader("12345678"), iotestErr{io.ErrUnexpectedEOF})
	if _, err := drv.Write(ctx, "cut", body, filesystem.WriteOptions{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("write = %v, want the body's io.ErrUnexpectedEOF", err)
	}
	if f.requests["files/upload_session/finish"] != 0 || len(f.files) != 0 {
		t.Fatalf("a failed body committed: %v, %d files", f.requests, len(f.files))
	}
}

type iotestErr struct{ err error }

func (e iotestErr) Read([]byte) (int, error) { return 0, e.err }

// TestPathsAndEscaping pins the key mapping: keys live under Root, and a
// segment ending in a dot or space is stored escaped and listed back
// decoded.
func TestPathsAndEscaping(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	for _, k := range []string{"dir./file ", "plain.txt"} {
		if _, err := drv.Write(ctx, k, strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"/apps/test/dir%2e/file%20", "/apps/test/plain.txt"} {
		if f.files[p] == nil {
			t.Errorf("no file at %s; have %v", p, keysOf(f.files))
		}
	}
	page, err := drv.List(ctx, "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 2 || page.Entries[0].Key != "dir./file " || page.Entries[1].Key != "plain.txt" {
		t.Fatalf("listed %+v", page.Entries)
	}
}

func keysOf(m map[string]*fakeFile) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestTokenOnlyToTheAPI: every request carries the bearer token, and a
// redirect from the API is not followed, so the token never reaches
// another host.
func TestTokenOnlyToTheAPI(t *testing.T) {
	ctx := context.Background()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("redirect followed to another host (Authorization %q)", r.Header.Get("Authorization"))
	}))
	defer other.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	cfg := dropbox.Config{Token: staticToken{testToken}, APIURL: redirecting.URL, ContentURL: redirecting.URL}
	drv := newDriver(t, cfg)
	if _, err := drv.Stat(ctx, "x"); err == nil {
		t.Fatal("stat through a redirect succeeded")
	}
	if _, err := drv.Read(ctx, "x", filesystem.ByteRange{}); err == nil {
		t.Fatal("read through a redirect succeeded")
	}
}

// TestConfigValidation pins what New refuses.
func TestConfigValidation(t *testing.T) {
	cases := []dropbox.Config{
		{},
		{Token: staticToken{"t"}, APIURL: "http://api.example.com/2"},
		{Token: staticToken{"t"}, ContentURL: "https://user:pw@example.com"},
		{Token: staticToken{"t"}, Root: "no-slash"},
		{Token: staticToken{"t"}, ChunkSize: dropbox.MaxChunkSize + 1},
	}
	for _, c := range cases {
		if _, err := dropbox.New(c); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(%v) = %v, want CodeInvalidArgument", c, err)
		}
	}
}

// TestTokenErrorsArePermission: a source that fails (a revoked refresh
// token) is CodePermission, and so is a token the API refuses.
func TestTokenErrorsArePermission(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	cfg := config(f)
	cfg.Token = dropbox.TokenFunc(func(context.Context) (string, error) { return "", errors.New("refresh token revoked") })
	if _, err := newDriver(t, cfg).Stat(ctx, "x"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("failing source: %v", err)
	}
	cfg.Token = staticToken{"wrong"}
	if _, err := newDriver(t, cfg).Stat(ctx, "x"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("refused token: %v", err)
	}
}

// TestTokenNeverPrints pins the redaction of Config and Driver.
func TestTokenNeverPrints(t *testing.T) {
	f := newFake(t)
	cfg := config(f)
	drv := newDriver(t, cfg)
	var buf bytes.Buffer
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		fmt.Fprintf(&buf, verb+"\n", cfg)
		fmt.Fprintf(&buf, verb+"\n", drv)
	}
	// Format answers every verb, so fmt never reaches GoString; call it
	// directly so a direct caller cannot see the secret either.
	buf.WriteString(cfg.GoString())
	buf.WriteString("\n")
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("open", "cfg", cfg, "driver", drv)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("open", "cfg", cfg)
	if strings.Contains(buf.String(), testToken) {
		t.Fatalf("token printed:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "[redacted]") {
		t.Fatalf("no redaction marker:\n%s", buf.String())
	}
}

// TestStatusMapping pins the classification of Dropbox error answers.
func TestStatusMapping(t *testing.T) {
	cases := []struct {
		status  int
		summary string
		want    filesystem.Code
	}{
		{http.StatusConflict, "path/insufficient_space/..", filesystem.CodeQuotaExceeded},
		{http.StatusConflict, "path/malformed_path/..", filesystem.CodeInvalidKey},
		{http.StatusConflict, "too_many_write_operations/..", filesystem.CodeUnavailable},
		{http.StatusTooManyRequests, "", filesystem.CodeUnavailable},
		{http.StatusServiceUnavailable, "", filesystem.CodeUnavailable},
		{http.StatusBadRequest, "", filesystem.CodeInvalidArgument},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(c.status)
			_, _ = fmt.Fprintf(w, `{"error_summary": %q}`, c.summary)
		}))
		drv := newDriver(t, dropbox.Config{Token: staticToken{testToken}, APIURL: srv.URL, ContentURL: srv.URL})
		_, err := drv.Stat(context.Background(), "x")
		srv.Close()
		if filesystem.CodeOf(err) != c.want {
			t.Errorf("%d %q: %v, want %s", c.status, c.summary, err, c.want)
		}
	}
}

// TestDeletePrefixLeavesAFileNamedDir: a file named like the prefix is not
// inside it.
func TestDeletePrefixLeavesAFileNamedDir(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	if _, err := drv.Write(ctx, "tmp", strings.NewReader("keep"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := drv.DeletePrefix(ctx, "tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Stat(ctx, "tmp"); err != nil {
		t.Fatalf("the file named like the prefix is gone: %v", err)
	}
}

// TestReadSendsRange: ranged reads ask the server for the range rather
// than downloading the whole file.
func TestReadSendsRange(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	if _, err := drv.Write(ctx, "r", strings.NewReader("0123456789"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	rc, err := drv.Read(ctx, "r", filesystem.ByteRange{Offset: 2, Length: 3})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "234" || len(f.ranges) != 1 || f.ranges[0] != "bytes=2-4" {
		t.Fatalf("read %q with Range headers %q", got, f.ranges)
	}
}

// TestIfNotExistsRefusesIdenticalContent: IfNotExists fails against an
// existing file even when the new bytes are the same.
func TestIfNotExistsRefusesIdenticalContent(t *testing.T) {
	ctx := context.Background()
	drv := newDriver(t, config(newFake(t)))
	opts := filesystem.WriteOptions{IfNotExists: true}
	if _, err := drv.Write(ctx, "once", strings.NewReader("same"), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Write(ctx, "once", strings.NewReader("same"), opts); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Fatalf("identical IfNotExists write: %v, want CodeExists", err)
	}
}
