package onedrive_test

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
	"github.com/ubgo/filesystem/adapters/onedrive"
	"github.com/ubgo/filesystem/filesystemtest"
)

// staticToken holds the token in a field, the shape that would leak if a
// Config or Driver printed its source.
type staticToken struct{ token string }

func (s staticToken) AccessToken(context.Context) (string, error) { return s.token, nil }

func config(f *fake) onedrive.Config {
	return onedrive.Config{Token: staticToken{testToken}, Root: "Apps/Test", BaseURL: f.graph.URL + "/v1.0"}
}

func newDriver(t *testing.T, cfg onedrive.Config) *onedrive.Driver {
	t.Helper()
	drv, err := onedrive.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return drv
}

func conformance(t *testing.T, chunk int) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		f := newFake(t)
		t.Cleanup(func() {
			if len(f.leaked) > 0 {
				t.Errorf("token sent to a pre-authenticated URL: %v", f.leaked)
			}
		})
		cfg := config(f)
		cfg.ChunkSize = chunk
		d, err := filesystem.NewDisk("onedrive", newDriver(t, cfg))
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}

func TestConformance(t *testing.T) { conformance(t, 0) }

// TestConformanceChunked runs the suite with the smallest fragment, so the
// large-object writes go through upload sessions.
func TestConformanceChunked(t *testing.T) { conformance(t, onedrive.FragmentUnit) }

// TestSessionContentRanges pins the session protocol: with no size hint
// the total is "*" until the final fragment states it; with one, every
// fragment states it; nothing is committed before the final fragment, and
// the session URL never gets the token.
func TestSessionContentRanges(t *testing.T) {
	ctx := context.Background()
	unit := onedrive.FragmentUnit
	body := bytes.Repeat([]byte("x"), 2*unit+5)
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
		want := []string{
			fmt.Sprintf("bytes 0-%d/%s", unit-1, total),
			fmt.Sprintf("bytes %d-%d/%s", unit, 2*unit-1, total),
			fmt.Sprintf("bytes %d-%d/%d", 2*unit, len(body)-1, len(body)),
		}
		if fmt.Sprint(f.contentRanges) != fmt.Sprint(want) {
			t.Errorf("hint %d: Content-Range %q, want %q", hint, f.contentRanges, want)
		}
		if len(f.leaked) > 0 {
			t.Errorf("token sent to the session URL: %v", f.leaked)
		}
		rc, err := drv.Read(ctx, "big", filesystem.ByteRange{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("read back %d bytes, %v", len(got), err)
		}
	}
}

// TestSessionCancelledOnBodyFailure: a body failing after the session
// started cancels it, so nothing is committed.
func TestSessionCancelledOnBodyFailure(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	cfg := config(f)
	cfg.ChunkSize = onedrive.FragmentUnit
	drv := newDriver(t, cfg)
	body := io.MultiReader(bytes.NewReader(make([]byte, 2*onedrive.FragmentUnit)), failing{io.ErrUnexpectedEOF})
	if _, err := drv.Write(ctx, "cut", body, filesystem.WriteOptions{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("write = %v, want the body's error", err)
	}
	if len(f.sessions) != 0 {
		t.Fatalf("session left open: %d", len(f.sessions))
	}
	if _, err := drv.Stat(ctx, "cut"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("stat after a failed write: %v", err)
	}
}

type failing struct{ err error }

func (e failing) Read([]byte) (int, error) { return 0, e.err }

// TestReadFollowsRedirectWithRangeNoToken: a ranged read asks Graph with
// the Range header and gets the bytes from the download host, which never
// sees the token.
func TestReadFollowsRedirectWithRangeNoToken(t *testing.T) {
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
	if string(got) != "234" || len(f.ranges) != 1 || f.ranges[0] != "bytes=2-4" || len(f.leaked) != 0 {
		t.Fatalf("read %q, ranges %q, leaked %v", got, f.ranges, f.leaked)
	}
}

// TestNamesAreEscaped: characters OneDrive refuses reach Graph escaped and
// come back decoded in listings.
func TestNamesAreEscaped(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	if _, err := drv.Write(ctx, `q?/a:b.`, strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.items[strings.ToLower("Apps/Test/q%3F/a%3Ab%2E")] == nil {
		var have []string
		for k := range f.items {
			have = append(have, k)
		}
		t.Fatalf("stored names %q", have)
	}
	page, err := drv.List(ctx, "", filesystem.ListOptions{Recursive: true})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Key != `q?/a:b.` {
		t.Fatalf("listed %+v, %v", page.Entries, err)
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

// TestTokenOnlyToGraph: a nextLink or redirect to another host never gets
// the token, and a failing token source is CodePermission.
func TestTokenOnlyToGraph(t *testing.T) {
	ctx := context.Background()
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			leaked = append(leaked, a)
		}
		http.NotFound(w, r)
	}))
	defer other.Close()
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer graph.Close()
	drv := newDriver(t, onedrive.Config{Token: staticToken{testToken}, BaseURL: graph.URL})
	_, _ = drv.Stat(ctx, "x")
	_, _ = drv.Read(ctx, "x", filesystem.ByteRange{})
	if len(leaked) != 0 {
		t.Fatalf("token reached another host: %q", leaked)
	}
	cfg := config(newFake(t))
	cfg.Token = onedrive.TokenFunc(func(context.Context) (string, error) { return "", errors.New("consent revoked") })
	if _, err := newDriver(t, cfg).Stat(ctx, "x"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("failing source: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := []onedrive.Config{
		{},
		{Token: staticToken{"t"}, BaseURL: "http://graph.example.com"},
		{Token: staticToken{"t"}, Drive: "me/drive"},
		{Token: staticToken{"t"}, Drive: "/drives/../x"},
		{Token: staticToken{"t"}, ChunkSize: 1000},
		{Token: staticToken{"t"}, ChunkSize: onedrive.MaxChunkSize + onedrive.FragmentUnit},
	}
	for _, c := range cases {
		if _, err := onedrive.New(c); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
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
	cases := map[int]filesystem.Code{
		http.StatusInsufficientStorage:   filesystem.CodeQuotaExceeded,
		http.StatusRequestEntityTooLarge: filesystem.CodeTooLarge,
		http.StatusTooManyRequests:       filesystem.CodeUnavailable,
		http.StatusForbidden:             filesystem.CodePermission,
		http.StatusBadRequest:            filesystem.CodeInvalidArgument,
	}
	for status, want := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"code":"x"}}`)
		}))
		_, err := newDriver(t, onedrive.Config{Token: staticToken{testToken}, BaseURL: srv.URL}).Stat(context.Background(), "x")
		srv.Close()
		if filesystem.CodeOf(err) != want {
			t.Errorf("%d: %v, want %s", status, err, want)
		}
	}
}

// TestSessionConditions: IfNotExists holds for bodies longer than one
// chunk (Graph re-checks conflictBehavior when a session commits), and
// IfMatch is refused for them before anything is written, since Graph
// checks If-Match only when the session starts.
func TestSessionConditions(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	cfg := config(f)
	cfg.ChunkSize = onedrive.FragmentUnit
	drv := newDriver(t, cfg)
	body := func() io.Reader { return bytes.NewReader(make([]byte, 2*onedrive.FragmentUnit)) }
	first, err := drv.Write(ctx, "big", body(), filesystem.WriteOptions{IfNotExists: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Write(ctx, "big", body(), filesystem.WriteOptions{IfNotExists: true}); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Fatalf("second IfNotExists session: %v, want CodeExists", err)
	}
	sessions := f.requests["POST /createUploadSession"]
	if _, err := drv.Write(ctx, "big", body(), filesystem.WriteOptions{IfMatch: first.ETag}); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Fatalf("IfMatch on a session-sized body: %v, want CodeUnsupported", err)
	}
	if f.requests["POST /createUploadSession"] != sessions {
		t.Fatal("a refused IfMatch write started an upload session")
	}
	small := strings.NewReader("swap")
	if _, err := drv.Write(ctx, "big", small, filesystem.WriteOptions{IfMatch: first.ETag}); err != nil {
		t.Fatalf("IfMatch on a single-request body: %v", err)
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

// TestTransportErrorsHidePreauthenticatedURLs: when the download host or
// an upload session's host drops the connection, the error holds neither
// the path nor the query of those pre-authenticated URLs.
func TestTransportErrorsHidePreauthenticatedURLs(t *testing.T) {
	ctx := context.Background()
	dead := httptest.NewServer(http.HandlerFunc(hangUp))
	defer dead.Close()
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ":/content"):
			http.Redirect(w, r, dead.URL+"/dl/SESSIONPATH?tempauth=TEMPAUTH", http.StatusFound)
		case strings.HasSuffix(r.URL.Path, ":/createUploadSession"):
			_, _ = fmt.Fprintf(w, `{"uploadUrl": %q}`, dead.URL+"/up/SESSIONPATH?sig=SIGNATURE")
		default:
			http.NotFound(w, r)
		}
	}))
	defer graph.Close()
	drv := newDriver(t, onedrive.Config{Token: staticToken{testToken}, BaseURL: graph.URL, ChunkSize: onedrive.FragmentUnit})
	_, readErr := drv.Read(ctx, "a.txt", filesystem.ByteRange{})
	_, writeErr := drv.Write(ctx, "b.bin", bytes.NewReader(make([]byte, 2*onedrive.FragmentUnit)), filesystem.WriteOptions{})
	for _, err := range []error{readErr, writeErr} {
		if err == nil {
			t.Fatal("a request to a dropped connection succeeded")
		}
		for _, secret := range []string{"SESSIONPATH", "TEMPAUTH", "SIGNATURE"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error carries %q: %v", secret, err)
			}
		}
	}
}
