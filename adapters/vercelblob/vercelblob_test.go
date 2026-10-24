package vercelblob_test

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
	"github.com/ubgo/filesystem/adapters/vercelblob"
	"github.com/ubgo/filesystem/filesystemtest"
)

func config(f *fake, access vercelblob.Access) vercelblob.Config {
	return vercelblob.Config{Token: testToken, Access: access, APIURL: f.api.URL + "/api/blob", BlobURL: f.blobs.URL}
}

func newDriver(t *testing.T, cfg vercelblob.Config) *vercelblob.Driver {
	t.Helper()
	drv, err := vercelblob.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return drv
}

func TestConformance(t *testing.T) {
	for _, access := range []vercelblob.Access{vercelblob.AccessPrivate, vercelblob.AccessPublic} {
		t.Run(string(access), func(t *testing.T) {
			filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
				f := newFake(t, access == vercelblob.AccessPublic)
				t.Cleanup(func() {
					if len(f.leaked) > 0 {
						t.Errorf("token sent to a public store's blob host: %v", f.leaked)
					}
				})
				d, err := filesystem.NewDisk("vercel", newDriver(t, config(f, access)))
				if err != nil {
					t.Fatal(err)
				}
				return d
			})
		})
	}
}

// TestStoreHostFromToken pins the default blob host: the store ID from the
// read-write token, lower-cased, and the access mode.
func TestStoreHostFromToken(t *testing.T) {
	drv, err := vercelblob.New(vercelblob.Config{Token: testToken, Access: vercelblob.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}
	got, err := drv.PublicURL(context.Background(), "a b/c.png")
	if want := "https://storeabc123.public.blob.vercel-storage.com/a%20b/c.png"; err != nil || got != want {
		t.Fatalf("PublicURL = %q, %v; want %q", got, err, want)
	}
	priv, _ := vercelblob.New(vercelblob.Config{Token: testToken, Access: vercelblob.AccessPrivate})
	if _, err := priv.PublicURL(context.Background(), "x"); filesystem.CodeOf(err) != filesystem.CodeUnsupported || priv.Capabilities().PublicURL {
		t.Fatalf("a private store offered a public URL: %v", err)
	}
}

// TestDeleteKeysBatches: DeleteKeys sends DeleteBatch URLs per request.
func TestDeleteKeysBatches(t *testing.T) {
	ctx := context.Background()
	f := newFake(t, false)
	drv := newDriver(t, config(f, vercelblob.AccessPrivate))
	keys := make([]string, vercelblob.DeleteBatch+3)
	for i := range keys {
		keys[i] = fmt.Sprintf("k/%03d", i)
		if _, err := drv.Write(ctx, keys[i], strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := drv.DeleteKeys(ctx, keys); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(f.deletes) != fmt.Sprintf("[%d 3]", vercelblob.DeleteBatch) || len(f.store) != 0 {
		t.Fatalf("delete requests %v, %d blobs left", f.deletes, len(f.store))
	}
}

// TestFailingBodyAbortsThePut: a body failing mid-stream aborts the PUT,
// so the API never receives a whole body, and the body's own error is
// returned, not mistaken for an unavailable server (which a caller would
// retry).
func TestFailingBodyAbortsThePut(t *testing.T) {
	ctx := context.Background()
	f := newFake(t, false)
	drv := newDriver(t, config(f, vercelblob.AccessPrivate))
	body := io.MultiReader(strings.NewReader("partial"), failing{io.ErrUnexpectedEOF})
	if _, err := drv.Write(ctx, "cut", body, filesystem.WriteOptions{}); !errors.Is(err, io.ErrUnexpectedEOF) || filesystem.CodeOf(err) == filesystem.CodeUnavailable {
		t.Fatalf("write = %v, want the body's error", err)
	}
	if len(f.store) != 0 {
		t.Fatalf("a failed body was stored")
	}
}

type failing struct{ err error }

func (e failing) Read([]byte) (int, error) { return 0, e.err }

// TestTokenOnlyToItsHosts: a redirect is not followed, so the token never
// reaches another host.
func TestTokenOnlyToItsHosts(t *testing.T) {
	ctx := context.Background()
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = append(leaked, r.Header.Get("Authorization"))
	}))
	defer other.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer redirecting.Close()
	drv := newDriver(t, vercelblob.Config{Token: testToken, Access: vercelblob.AccessPrivate, APIURL: redirecting.URL, BlobURL: redirecting.URL})
	_, _ = drv.Stat(ctx, "x")
	_, _ = drv.Read(ctx, "x", filesystem.ByteRange{})
	if len(leaked) != 0 {
		t.Fatalf("a redirect was followed: %q", leaked)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := []vercelblob.Config{
		{},
		{Token: testToken},
		{Token: testToken, Access: "secret"},
		{Token: "not-a-blob-token", Access: vercelblob.AccessPublic},
		{Token: testToken, Access: vercelblob.AccessPublic, APIURL: "http://vercel.example.com"},
	}
	for _, c := range cases {
		if _, err := vercelblob.New(c); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(%v) = %v, want CodeInvalidArgument", c, err)
		}
	}
}

func TestTokenNeverPrints(t *testing.T) {
	cfg := config(newFake(t, false), vercelblob.AccessPrivate)
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
	if strings.Contains(buf.String(), "secretsecret") || !strings.Contains(buf.String(), "[redacted]") {
		t.Fatalf("token printed or no redaction marker:\n%s", buf.String())
	}
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   filesystem.Code
	}{
		{http.StatusBadRequest, "file_too_large", filesystem.CodeTooLarge},
		{http.StatusBadRequest, "content_type_not_allowed", filesystem.CodeRejected},
		{http.StatusTooManyRequests, "rate_limited", filesystem.CodeUnavailable},
		{http.StatusForbidden, "store_suspended", filesystem.CodePermission},
		{http.StatusBadRequest, "bad_request", filesystem.CodeInvalidArgument},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = fmt.Fprintf(w, `{"error":{"code":%q}}`, c.code)
		}))
		_, err := newDriver(t, vercelblob.Config{Token: testToken, Access: vercelblob.AccessPrivate, APIURL: srv.URL, BlobURL: srv.URL}).Stat(context.Background(), "x")
		srv.Close()
		if filesystem.CodeOf(err) != c.want {
			t.Errorf("%s: %v, want %s", c.code, err, c.want)
		}
	}
}
