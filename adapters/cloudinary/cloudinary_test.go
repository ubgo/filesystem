package cloudinary_test

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
	"github.com/ubgo/filesystem/adapters/cloudinary"
	"github.com/ubgo/filesystem/filesystemtest"
)

func config(f *fake, typ cloudinary.Type) cloudinary.Config {
	return cloudinary.Config{CloudName: testCloud, APIKey: testKey, APISecret: testSecret, Type: typ, APIURL: f.URL + "/v1_1", DeliveryURL: "https://res.example.com"}
}

func newDriver(t *testing.T, cfg cloudinary.Config) *cloudinary.Driver {
	t.Helper()
	drv, err := cloudinary.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return drv
}

func TestConformance(t *testing.T) {
	for _, typ := range []cloudinary.Type{cloudinary.TypeAuthenticated, cloudinary.TypeUpload} {
		t.Run(string(typ), func(t *testing.T) {
			filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
				d, err := filesystem.NewDisk("cloudinary", newDriver(t, config(newFake(t), typ)))
				if err != nil {
					t.Fatal(err)
				}
				return d
			})
		})
	}
}

// TestPublicIDsAreEscaped: bytes Cloudinary refuses in a public ID are
// stored as "~XX" and listed back decoded; "~" itself is escaped, so the
// mapping is one to one.
func TestPublicIDsAreEscaped(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f, cloudinary.TypeAuthenticated))
	key := "a b/x?y%z~é.txt"
	if _, err := drv.Write(ctx, key, strings.NewReader("1"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	const id = "a~20b/x~3Fy~25z~7E~C3~A9.txt"
	if f.res[id] == nil {
		t.Fatalf("stored IDs %v, want %s", f.res, id)
	}
	// Not written by the driver: an invalid escape, and "~41" for "A",
	// which would make two IDs decode to one key.
	f.res["foreign~zz"] = f.res[id]
	f.res["~41.txt"] = f.res[id]
	page, err := drv.List(ctx, "", filesystem.ListOptions{Recursive: true})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Key != key {
		t.Fatalf("listed %+v, %v", page.Entries, err)
	}
}

// TestPublicURL: only upload-type resources have delivery URLs.
func TestPublicURL(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	got, err := newDriver(t, config(f, cloudinary.TypeUpload)).PublicURL(ctx, "docs/a b.pdf")
	if want := "https://res.example.com/demo/raw/upload/docs/a~20b.pdf"; err != nil || got != want {
		t.Fatalf("PublicURL = %q, %v; want %q", got, err, want)
	}
	auth := newDriver(t, config(f, cloudinary.TypeAuthenticated))
	if _, err := auth.PublicURL(ctx, "x"); filesystem.CodeOf(err) != filesystem.CodeUnsupported || auth.Capabilities().PublicURL {
		t.Fatalf("authenticated resources offered a public URL: %v", err)
	}
}

// TestSHA256Signatures: an account set to SHA-256 gets SHA-256 signatures
// (the fake checks SHA-1, so it must refuse them).
func TestSHA256Signatures(t *testing.T) {
	cfg := config(newFake(t), cloudinary.TypeAuthenticated)
	cfg.Algorithm = cloudinary.SHA256
	_, err := newDriver(t, cfg).Write(context.Background(), "k", strings.NewReader("x"), filesystem.WriteOptions{})
	if filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("a SHA-256 signature passed a SHA-1 check: %v", err)
	}
}

// TestReadSendsRange: ranged reads ask for the range.
func TestReadSendsRange(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f, cloudinary.TypeAuthenticated))
	if _, err := drv.Write(ctx, "r", strings.NewReader("0123456789"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	rc, err := drv.Read(ctx, "r", filesystem.ByteRange{Offset: 2, Length: 3})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "234" || fmt.Sprint(f.ranges) != "[bytes=2-4]" {
		t.Fatalf("read %q with ranges %q", got, f.ranges)
	}
}

// TestFailingBodyStoresNothing: a body failing mid-stream aborts the
// upload; its own error is returned, not CodeUnavailable.
func TestFailingBodyStoresNothing(t *testing.T) {
	f := newFake(t)
	drv := newDriver(t, config(f, cloudinary.TypeAuthenticated))
	body := io.MultiReader(strings.NewReader("partial"), failing{io.ErrUnexpectedEOF})
	_, err := drv.Write(context.Background(), "cut", body, filesystem.WriteOptions{})
	if !errors.Is(err, io.ErrUnexpectedEOF) || filesystem.CodeOf(err) == filesystem.CodeUnavailable || len(f.res) != 0 {
		t.Fatalf("write = %v, %d stored", err, len(f.res))
	}
}

type failing struct{ err error }

func (e failing) Read([]byte) (int, error) { return 0, e.err }

func TestConfigValidation(t *testing.T) {
	ok := cloudinary.Config{CloudName: "c", APIKey: "k", APISecret: "s"}
	bad := []func(*cloudinary.Config){
		func(c *cloudinary.Config) { c.APISecret = "" },
		func(c *cloudinary.Config) { c.CloudName = "a/b" },
		func(c *cloudinary.Config) { c.Type = "fetch" },
		func(c *cloudinary.Config) { c.ResourceType = "auto" },
		func(c *cloudinary.Config) { c.Algorithm = "md5" },
		func(c *cloudinary.Config) { c.APIURL = "http://api.example.com" },
	}
	for i, mutate := range bad {
		c := ok
		mutate(&c)
		if _, err := cloudinary.New(c); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("case %d: %v, want CodeInvalidArgument", i, err)
		}
	}
}

func TestSecretNeverPrints(t *testing.T) {
	cfg := config(newFake(t), cloudinary.TypeAuthenticated)
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
	if strings.Contains(buf.String(), testSecret) || !strings.Contains(buf.String(), "[redacted]") {
		t.Fatalf("secret printed or no redaction marker:\n%s", buf.String())
	}
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		msg    string
		want   filesystem.Code
	}{
		{420, "Rate Limit Exceeded", filesystem.CodeUnavailable},
		{http.StatusBadRequest, "File size too large. Got 20000000. Maximum is 10485760.", filesystem.CodeTooLarge},
		{http.StatusUnauthorized, "Invalid Signature", filesystem.CodePermission},
		{http.StatusBadRequest, "Invalid public_id", filesystem.CodeInvalidArgument},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = fmt.Fprintf(w, `{"error":{"message":%q}}`, c.msg)
		}))
		cfg := cloudinary.Config{CloudName: testCloud, APIKey: testKey, APISecret: testSecret, APIURL: srv.URL}
		_, err := newDriver(t, cfg).Stat(context.Background(), "x")
		srv.Close()
		if filesystem.CodeOf(err) != c.want {
			t.Errorf("%d %q: %v, want %s", c.status, c.msg, err, c.want)
		}
	}
}

// TestBatchAndPrefixDeletes: DeleteKeys sends encoded public IDs; a prefix
// delete repeats while Cloudinary reports it partial; Move replaces dst.
func TestBatchAndPrefixDeletes(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f, cloudinary.TypeAuthenticated))
	put := func(k string) {
		if _, err := drv.Write(ctx, k, strings.NewReader(k), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"d/1", "d/2", "d/3", "d/4", "d/5", "keep", "odd name"} {
		put(k)
	}
	if err := drv.DeletePrefix(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	if err := drv.DeleteKeys(ctx, []string{"odd name"}); err != nil {
		t.Fatal(err)
	}
	if len(f.res) != 1 || f.res["keep"] == nil {
		t.Fatalf("left %v, want only keep", f.res)
	}
	put("src")
	if _, err := drv.Move(ctx, "src", "keep"); err != nil {
		t.Fatalf("move onto an existing key: %v", err)
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

// TestTransportErrorsHideSignedURLs: a read whose connection drops fails
// with an error that holds none of the signed URL's query: the API key,
// the signature and the expiry would replay the read from any log.
func TestTransportErrorsHideSignedURLs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(hangUp))
	defer srv.Close()
	cfg := cloudinary.Config{CloudName: testCloud, APIKey: testKey, APISecret: testSecret, APIURL: srv.URL}
	_, err := newDriver(t, cfg).Read(context.Background(), "private/report.pdf", filesystem.ByteRange{})
	if err == nil {
		t.Fatal("read through a dropped connection succeeded")
	}
	for _, secret := range []string{testKey, "signature", "expires_at", "timestamp"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error carries %q: %v", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "/raw/download") {
		t.Fatalf("error lost the API path it may keep: %v", err)
	}
}
