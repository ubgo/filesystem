package signedurl_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/signedurl"
)

var (
	now     = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	current = signedurl.Key{ID: "k2", Secret: bytes.Repeat([]byte("a"), 32)}
	old     = signedurl.Key{ID: "k1", Secret: bytes.Repeat([]byte("b"), 32)}
)

func signer(t *testing.T, at *time.Time, keys ...signedurl.Key) *signedurl.Signer {
	t.Helper()
	s, err := signedurl.New("https://app.test/files/", keys, signedurl.WithClock(func() time.Time { return *at }))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// parse splits a signed URL into disk, key and query like a handler would.
func parse(t *testing.T, raw string) (string, string, url.Values) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	disk, key, _ := strings.Cut(strings.TrimPrefix(u.Path, "/files/"), "/")
	return disk, key, u.Query()
}

func TestRoundTrip(t *testing.T) {
	at := now
	s := signer(t, &at, current, old)
	req, err := s.SignURL(t.Context(), "uploads", "a dir/ü.pdf", filesystem.PresignOptions{
		Method: filesystem.MethodGet, Expires: time.Hour, ContentType: "application/pdf", ContentDisposition: `attachment; filename="x.pdf"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(req.URL, "https://app.test/files/uploads/a%20dir/%C3%BC.pdf?") || !req.ExpiresAt.Equal(now.Add(time.Hour)) || req.Header != nil {
		t.Errorf("signed request = %+v", req)
	}
	disk, key, q := parse(t, req.URL)
	c, err := s.Verify(disk, key, filesystem.MethodGet, q)
	if err != nil {
		t.Fatal(err)
	}
	if c.Disk != "uploads" || c.Key != "a dir/ü.pdf" || c.ContentType != "application/pdf" || c.ContentDisposition == "" || c.Method != filesystem.MethodGet {
		t.Errorf("claims = %+v", c)
	}
	put, _ := s.SignURL(t.Context(), "uploads", "k", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, ContentType: "image/png"})
	if put.Header.Get("Content-Type") != "image/png" {
		t.Errorf("PUT header = %v", put.Header)
	}
}

func TestTamperingIsRejected(t *testing.T) {
	at := now
	s := signer(t, &at, current)
	req, _ := s.SignURL(t.Context(), "d", "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, ContentType: "text/plain"})
	disk, key, q := parse(t, req.URL)
	mutate := []func(url.Values) (string, string, filesystem.HTTPMethod){
		func(url.Values) (string, string, filesystem.HTTPMethod) { return "other", key, filesystem.MethodGet },
		func(url.Values) (string, string, filesystem.HTTPMethod) { return disk, "k2", filesystem.MethodGet },
		func(url.Values) (string, string, filesystem.HTTPMethod) { return disk, key, filesystem.MethodPut },
		func(v url.Values) (string, string, filesystem.HTTPMethod) {
			v.Set(signedurl.ParamMethod, "PUT")
			return disk, key, filesystem.MethodPut
		},
		func(v url.Values) (string, string, filesystem.HTTPMethod) {
			v.Set(signedurl.ParamExpires, "9999999999")
			return disk, key, filesystem.MethodGet
		},
		func(v url.Values) (string, string, filesystem.HTTPMethod) {
			v.Set(signedurl.ParamExpires, "soon")
			return disk, key, filesystem.MethodGet
		},
		func(v url.Values) (string, string, filesystem.HTTPMethod) {
			v.Set(signedurl.ParamContentType, "text/html")
			return disk, key, filesystem.MethodGet
		},
		func(v url.Values) (string, string, filesystem.HTTPMethod) {
			v.Set(signedurl.ParamSignature, "zz")
			return disk, key, filesystem.MethodGet
		},
		func(v url.Values) (string, string, filesystem.HTTPMethod) {
			v.Del(signedurl.ParamSignature)
			return disk, key, filesystem.MethodGet
		},
		func(v url.Values) (string, string, filesystem.HTTPMethod) {
			v.Set(signedurl.ParamKeyID, "unknown")
			return disk, key, filesystem.MethodGet
		},
	}
	for i, m := range mutate {
		v := url.Values{}
		for k, vals := range q {
			v[k] = append([]string(nil), vals...)
		}
		d, k, method := m(v)
		if _, err := s.Verify(d, k, method, v); filesystem.CodeOf(err) != filesystem.CodePermission || !strings.Contains(err.Error(), "invalid or expired") {
			t.Errorf("mutation %d verified: %v", i, err)
		}
	}
	at = now.Add(2 * time.Minute)
	if _, err := s.Verify(disk, key, filesystem.MethodGet, q); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("expired URL verified: %v", err)
	}
}

func TestKeyRotation(t *testing.T) {
	at := now
	before := signer(t, &at, old)
	req, _ := before.SignURL(t.Context(), "d", "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	disk, key, q := parse(t, req.URL)
	rotated := signer(t, &at, current, old)
	if _, err := rotated.Verify(disk, key, filesystem.MethodGet, q); err != nil {
		t.Errorf("URL signed with the previous key rejected during rotation: %v", err)
	}
	retired := signer(t, &at, current)
	if _, err := retired.Verify(disk, key, filesystem.MethodGet, q); err == nil {
		t.Error("URL signed with a retired key verified")
	}
}

func TestNewValidation(t *testing.T) {
	short := signedurl.Key{ID: "s", Secret: []byte("short")}
	cases := []struct {
		base string
		keys []signedurl.Key
	}{
		{"not a url", []signedurl.Key{current}},
		{"ftp://x", []signedurl.Key{current}},
		{"https://app.test/files?x=1", []signedurl.Key{current}},
		{"https://app.test", nil},
		{"https://app.test", []signedurl.Key{short}},
		{"https://app.test", []signedurl.Key{{ID: "", Secret: current.Secret}}},
		{"https://app.test", []signedurl.Key{{ID: "a/b", Secret: current.Secret}}},
		{"https://app.test", []signedurl.Key{current, current}},
	}
	for _, c := range cases {
		if _, err := signedurl.New(c.base, c.keys); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(%q, %d keys) = %v", c.base, len(c.keys), err)
		}
	}
	s, err := signedurl.New("https://app.test", []signedurl.Key{current})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignURL(t.Context(), "d", "k", filesystem.PresignOptions{Method: "DELETE", Expires: time.Minute}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("bad method signed: %v", err)
	}
}

func TestDiskIntegration(t *testing.T) {
	s, _ := signedurl.New("https://app.test/files", []signedurl.Key{current})
	d, err := filesystem.NewDisk("uploads", memory.New(), filesystem.WithURLSigner(s))
	if err != nil {
		t.Fatal(err)
	}
	req, err := d.SignedURL(t.Context(), "/a/b.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil || !strings.HasPrefix(req.URL, "https://app.test/files/uploads/a/b.txt?") {
		t.Errorf("disk signed URL = %q, %v", req.URL, err)
	}
}

func TestStoredHeadersArePinned(t *testing.T) {
	at := now
	s := signer(t, &at, current)
	h := filesystem.HTTPHeaders{CacheControl: "public, max-age=60", ContentDisposition: "attachment; filename=a.png", ContentEncoding: "gzip"}
	if !s.PinsHeaders() {
		t.Fatal("signer does not pin headers")
	}
	req, err := s.SignURL(t.Context(), "media", "a.png", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, ContentType: "image/png", Headers: h})
	if err != nil {
		t.Fatal(err)
	}
	// The client is told every header to send.
	if filesystem.HTTPHeadersFrom(req.Header) != h || req.Header.Get("Content-Type") != "image/png" {
		t.Errorf("request headers %v", req.Header)
	}
	disk, key, q := parse(t, req.URL)
	c, err := s.Verify(disk, key, filesystem.MethodPut, q)
	if err != nil || c.Headers != h {
		t.Fatalf("verify = %+v, %v", c.Headers, err)
	}
	// Changing or dropping any stored header breaks the signature.
	for _, p := range []string{signedurl.ParamCacheControl, signedurl.ParamStoredDisposition, signedurl.ParamContentEncoding} {
		for _, mutate := range []func(url.Values){
			func(v url.Values) { v.Set(p, "no-store") },
			func(v url.Values) { v.Del(p) },
		} {
			tampered := url.Values{}
			for k, v := range q {
				tampered[k] = append([]string(nil), v...)
			}
			mutate(tampered)
			if _, err := s.Verify(disk, key, filesystem.MethodPut, tampered); filesystem.CodeOf(err) != filesystem.CodePermission {
				t.Errorf("%s tampered: %v", p, err)
			}
		}
	}
	// A PUT without headers and without a content type lists none.
	req, err = s.SignURL(t.Context(), "media", "b.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if err != nil || req.Header != nil {
		t.Errorf("bare PUT headers %v, %v", req.Header, err)
	}
}

// URLs without stored headers sign exactly as before stored headers
// existed, so links issued before an upgrade keep working after it.
func TestSignatureWithoutHeadersIsUnchanged(t *testing.T) {
	at := now
	s := signer(t, &at, current)
	req, err := s.SignURL(t.Context(), "media", "a.png", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, ContentType: "image/png", ContentDisposition: "inline"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, q := parse(t, req.URL)
	m := hmac.New(sha256.New, current.Secret)
	for _, f := range []string{"GET", "media", "a.png", strconv.FormatInt(now.Add(time.Minute).Unix(), 10), "image/png", "inline"} {
		m.Write([]byte(strconv.Itoa(len(f)) + ":" + f + "\n"))
	}
	if got, want := q.Get(signedurl.ParamSignature), hex.EncodeToString(m.Sum(nil)); got != want {
		t.Errorf("signature %s, want the pre-headers form %s", got, want)
	}
}

// A secret printed by mistake, through any verb, a containing struct, a
// slice, a Signer or slog, never appears.
func TestSecretsAreRedacted(t *testing.T) {
	secret := "do-not-leak-this-secret-0123456789"
	k := signedurl.Key{ID: "k1", Secret: []byte(secret)}
	at := now
	s := signer(t, &at, k)
	type config struct {
		Name string
		Key  signedurl.Key
		Keys []signedurl.Key
	}
	cfg := config{Name: "media", Key: k, Keys: []signedurl.Key{k}}
	var outputs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		outputs = append(outputs, fmt.Sprintf(verb, k), fmt.Sprintf(verb, cfg), fmt.Sprintf(verb, s), fmt.Sprintf(verb, *s))
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("config", "key", k, "signer", s, "keys", []signedurl.Key{k})
	textBuf := &bytes.Buffer{}
	slog.New(slog.NewTextHandler(textBuf, nil)).Info("config", "key", k, "signer", s)
	outputs = append(outputs, buf.String(), textBuf.String(), k.String(), k.GoString(), s.String(), s.GoString())
	hexSecret := fmt.Sprintf("%x", []byte(secret))
	for _, out := range outputs {
		if strings.Contains(out, secret) || strings.Contains(out, hexSecret) || strings.Contains(out, "100 111 45") {
			t.Errorf("secret leaked: %s", out)
		}
	}
	for _, want := range []string{`"k1"`, "[REDACTED]"} {
		if !strings.Contains(k.String(), want) || !strings.Contains(s.String(), want) {
			t.Errorf("String lacks %s: %s / %s", want, k, s)
		}
	}
	if !strings.Contains(buf.String(), `"secret":"[REDACTED]"`) || !strings.Contains(buf.String(), `"key_ids":["k1"]`) {
		t.Errorf("slog output %s", buf.String())
	}
}
