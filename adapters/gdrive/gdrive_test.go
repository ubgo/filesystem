package gdrive_test

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
	"github.com/ubgo/filesystem/adapters/gdrive"
	"github.com/ubgo/filesystem/filesystemtest"
)

// staticToken holds the token in a field, the shape that would leak if a
// Config or Driver printed its source.
type staticToken struct{ token string }

func (s staticToken) AccessToken(context.Context) (string, error) { return s.token, nil }

func config(f *fake) gdrive.Config {
	return gdrive.Config{Token: staticToken{testToken}, FolderID: "appfolder", APIURL: f.apiURL(), UploadURL: f.uploadURL()}
}

func newDriver(t *testing.T, cfg gdrive.Config) *gdrive.Driver {
	t.Helper()
	drv, err := gdrive.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return drv
}

func conformance(t *testing.T, chunk int) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		cfg := config(newFake(t))
		cfg.ChunkSize = chunk
		d, err := filesystem.NewDisk("gdrive", newDriver(t, cfg))
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}

func TestConformance(t *testing.T) { conformance(t, 0) }

// TestConformanceChunked runs the suite with the smallest chunk, so the
// large-object writes span several resumable-upload requests.
func TestConformanceChunked(t *testing.T) { conformance(t, gdrive.ChunkUnit) }

// TestResumableContentRanges pins the chunk protocol: "*" as the total
// until the final chunk when the size is unknown, the size throughout when
// hinted, and "bytes */0" for an empty file.
func TestResumableContentRanges(t *testing.T) {
	ctx := context.Background()
	unit := gdrive.ChunkUnit
	body := bytes.Repeat([]byte("y"), unit+3)
	for _, hint := range []int64{0, int64(len(body))} {
		f := newFake(t)
		cfg := config(f)
		cfg.ChunkSize = unit
		drv := newDriver(t, cfg)
		if _, err := drv.Write(ctx, "big", bytes.NewReader(body), filesystem.WriteOptions{SizeHint: hint}); err != nil {
			t.Fatal(err)
		}
		total := "*"
		if hint > 0 {
			total = fmt.Sprint(len(body))
		}
		want := fmt.Sprint([]string{fmt.Sprintf("bytes 0-%d/%s", unit-1, total), fmt.Sprintf("bytes %d-%d/%d", unit, len(body)-1, len(body))})
		if got := fmt.Sprint(f.contentRanges); got != want {
			t.Errorf("hint %d: %s, want %s", hint, got, want)
		}
	}
	f := newFake(t)
	if _, err := newDriver(t, config(f)).Write(ctx, "empty", strings.NewReader(""), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(f.contentRanges) != "[bytes */0]" {
		t.Errorf("empty file: %q", f.contentRanges)
	}
}

// TestFailingBodyCancelsUpload: a body failing after the first chunk
// cancels the session; no file appears.
func TestFailingBodyCancelsUpload(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	cfg := config(f)
	cfg.ChunkSize = gdrive.ChunkUnit
	drv := newDriver(t, cfg)
	body := io.MultiReader(bytes.NewReader(make([]byte, 2*gdrive.ChunkUnit)), failing{io.ErrUnexpectedEOF})
	if _, err := drv.Write(ctx, "cut", body, filesystem.WriteOptions{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("write = %v, want the body's error", err)
	}
	if len(f.uploads) != 0 {
		t.Fatalf("upload session left open")
	}
	if _, err := drv.Stat(ctx, "cut"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("stat after a failed write: %v", err)
	}
}

type failing struct{ err error }

func (e failing) Read([]byte) (int, error) { return 0, e.err }

// TestRepeatedNames pins the duplicate-name rules: two files of one name
// are CodeConflict for Stat, Read and Write, and Delete removes both; two
// folders of one name resolve through the oldest, and listings show the
// files of both.
func TestRepeatedNames(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	f.add("dup.txt", "appfolder", "", []byte("a"))
	f.add("dup.txt", "appfolder", "", []byte("b"))
	if _, err := drv.Stat(ctx, "dup.txt"); filesystem.CodeOf(err) != filesystem.CodeConflict {
		t.Errorf("stat of a repeated name: %v", err)
	}
	if _, err := drv.Read(ctx, "dup.txt", filesystem.ByteRange{}); filesystem.CodeOf(err) != filesystem.CodeConflict {
		t.Errorf("read of a repeated name: %v", err)
	}
	if _, err := drv.Write(ctx, "dup.txt", strings.NewReader("c"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeConflict {
		t.Errorf("write of a repeated name: %v", err)
	}
	if err := drv.Delete(ctx, "dup.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Stat(ctx, "dup.txt"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("after delete: %v", err)
	}
	old := f.add("dir", "appfolder", gdrive.FolderMIME, nil)
	newer := f.add("dir", "appfolder", gdrive.FolderMIME, nil)
	f.add("in-old.txt", old.id, "", []byte("1"))
	f.add("in-new.txt", newer.id, "", []byte("2"))
	if _, err := drv.Stat(ctx, "dir/in-old.txt"); err != nil {
		t.Errorf("a key resolves through the oldest folder: %v", err)
	}
	page, err := drv.List(ctx, "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, e := range page.Entries {
		keys = append(keys, e.Key)
	}
	if fmt.Sprint(keys) != "[dir/in-new.txt dir/in-old.txt]" {
		t.Errorf("listed %q", keys)
	}
}

// TestNativeDocsAreSkipped: Google Docs have no bytes, so they are not
// objects.
func TestNativeDocsAreSkipped(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	f.add("notes", "appfolder", gdrive.NativePrefix+"document", nil)
	if _, err := drv.Stat(ctx, "notes"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("stat of a Google Doc: %v", err)
	}
	page, err := drv.List(ctx, "", filesystem.ListOptions{Recursive: true})
	if err != nil || len(page.Entries) != 0 {
		t.Errorf("listed %+v, %v", page.Entries, err)
	}
}

// TestQueryEscaping: names holding the query language's quote and escape
// characters are matched exactly.
func TestQueryEscaping(t *testing.T) {
	ctx := context.Background()
	drv := newDriver(t, config(newFake(t)))
	for _, k := range []string{`it's/a\b.txt`, `it's/plain.txt`} {
		if _, err := drv.Write(ctx, k, strings.NewReader(k), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	e, err := drv.Stat(ctx, `it's/a\b.txt`)
	if err != nil || e.Size != int64(len(`it's/a\b.txt`)) {
		t.Fatalf("stat = %+v, %v", e, err)
	}
}

// TestUpdateKeepsTheFileID: replacing a key uploads a revision of the
// same file rather than a second file.
func TestUpdateKeepsTheFileID(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	for _, body := range []string{"v1", "v2"} {
		if _, err := drv.Write(ctx, "doc.txt", strings.NewReader(body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.items) != 1 {
		t.Fatalf("%d items after an overwrite, want 1", len(f.items))
	}
}

// TestTokenOnlyToTheAPI: a redirect to another host never carries the
// token, and a failing source is CodePermission.
func TestTokenOnlyToTheAPI(t *testing.T) {
	ctx := context.Background()
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			leaked = append(leaked, a)
		}
		http.NotFound(w, r)
	}))
	defer other.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer api.Close()
	_, _ = newDriver(t, gdrive.Config{Token: staticToken{testToken}, APIURL: api.URL, UploadURL: api.URL}).Stat(ctx, "x")
	if len(leaked) != 0 {
		t.Fatalf("token reached another host: %q", leaked)
	}
	cfg := config(newFake(t))
	cfg.Token = gdrive.TokenFunc(func(context.Context) (string, error) { return "", errors.New("revoked") })
	if _, err := newDriver(t, cfg).Stat(ctx, "x"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("failing source: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := []gdrive.Config{
		{},
		{Token: staticToken{"t"}, APIURL: "http://drive.example.com"},
		{Token: staticToken{"t"}, FolderID: "a'b"},
		{Token: staticToken{"t"}, ChunkSize: 1000},
	}
	for _, c := range cases {
		if _, err := gdrive.New(c); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(%v) = %v, want CodeInvalidArgument", c, err)
		}
	}
}

func TestTokenNeverPrints(t *testing.T) {
	cfg := config(newFake(t))
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
	if strings.Contains(buf.String(), testToken) || !strings.Contains(buf.String(), "[redacted]") {
		t.Fatalf("token printed or no redaction marker:\n%s", buf.String())
	}
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		reason string
		want   filesystem.Code
	}{
		{http.StatusForbidden, "storageQuotaExceeded", filesystem.CodeQuotaExceeded},
		{http.StatusForbidden, "userRateLimitExceeded", filesystem.CodeUnavailable},
		{http.StatusForbidden, "insufficientFilePermissions", filesystem.CodePermission},
		{http.StatusTooManyRequests, "", filesystem.CodeUnavailable},
		{http.StatusBadRequest, "invalid", filesystem.CodeInvalidArgument},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = fmt.Fprintf(w, `{"error":{"errors":[{"reason":%q}]}}`, c.reason)
		}))
		_, err := newDriver(t, gdrive.Config{Token: staticToken{testToken}, APIURL: srv.URL, UploadURL: srv.URL}).Stat(context.Background(), "x")
		srv.Close()
		if filesystem.CodeOf(err) != c.want {
			t.Errorf("%d %s: %v, want %s", c.status, c.reason, err, c.want)
		}
	}
}

// TestCopyAndMoveReplaceDst: copying or moving onto an existing key
// leaves exactly one file of that name, holding the source's bytes.
func TestCopyAndMoveReplaceDst(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	put := func(k, v string) {
		if _, err := drv.Write(ctx, k, strings.NewReader(v), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	put("src", "source")
	put("dst", "old")
	if _, err := drv.Copy(ctx, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	put("dst2", "old")
	if _, err := drv.Move(ctx, "src", "dst2"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"dst", "dst2"} {
		e, err := drv.Stat(ctx, k) // CodeConflict if the old file were kept
		if err != nil || e.Size != int64(len("source")) {
			t.Fatalf("%s: %+v, %v", k, e, err)
		}
	}
}

// TestDeletePrefixLeavesAFileNamedDir: a file named like the prefix is not
// inside it.
func TestDeletePrefixLeavesAFileNamedDir(t *testing.T) {
	ctx := context.Background()
	drv := newDriver(t, config(newFake(t)))
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

// hangUp drops the connection without answering, so the client's error is
// a transport *url.Error naming the request URL.
func hangUp(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// TestTransportErrorsHideSessionIDs: when an upload session's request
// drops, the error does not hold the session URL's query (its upload_id
// lets anyone holding it continue the upload).
func TestTransportErrorsHideSessionIDs(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("upload_id") != "":
			hangUp(w, r)
		case strings.HasPrefix(r.URL.Path, "/upload/"):
			w.Header().Set("Location", srv.URL+"/upload/files?uploadType=resumable&upload_id=SESSIONSECRET")
		default:
			_, _ = io.WriteString(w, `{"files":[]}`)
		}
	}))
	defer srv.Close()
	cfg := gdrive.Config{Token: staticToken{testToken}, APIURL: srv.URL + "/drive", UploadURL: srv.URL + "/upload"}
	_, err := newDriver(t, cfg).Write(context.Background(), "a.txt", strings.NewReader("x"), filesystem.WriteOptions{})
	if err == nil || strings.Contains(err.Error(), "SESSIONSECRET") {
		t.Fatalf("write = %v; want an error without the session ID", err)
	}
}
