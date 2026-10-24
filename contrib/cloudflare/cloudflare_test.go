package cloudflare_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/cloudflare"
	"github.com/ubgo/filesystem/memory"
)

const (
	base = "https://downloads.example.test/files"
	ttl  = 3 * time.Hour
)

var (
	now    = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	secret = bytes.Repeat([]byte("s"), cloudflare.MinSecretBytes)
)

func newSigner(t *testing.T, mutate func(*cloudflare.Config)) *cloudflare.Signer {
	t.Helper()
	cfg := cloudflare.Config{BaseURL: base, Secret: secret, TTL: ttl, Clock: func() time.Time { return now }}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := cloudflare.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// messageMAC is the pattern Cloudflare documents for the MessageMAC
// argument: message, separator, a 10-digit timestamp, "-", the MAC.
var messageMAC = regexp.MustCompile(`^(.+)(\d{10})-(.{43,})$`)

// edgeValid is an independent port of is_timed_hmac_valid_v0 as
// Cloudflare documents it: split http.request.uri into message, separator
// (sepLen bytes), timestamp and MAC; recompute HMAC-SHA256(key, message +
// timestamp); accept when it matches and at - timestamp is within ttl.
func edgeValid(key []byte, uri string, ttlSeconds int64, at time.Time, sepLen int, urlSafe bool) bool {
	m := messageMAC.FindStringSubmatch(uri)
	if m == nil || len(m[1]) <= sepLen {
		return false
	}
	message, ts, encoded := m[1][:len(m[1])-sepLen], m[2], m[3]
	var mac []byte
	var err error
	if urlSafe {
		mac, err = base64.RawURLEncoding.DecodeString(encoded)
	} else {
		var unescaped string
		if unescaped, err = url.QueryUnescape(encoded); err == nil {
			mac, err = base64.StdEncoding.DecodeString(unescaped)
		}
	}
	if err != nil {
		return false
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte(message + ts))
	issued, _ := strconv.ParseInt(ts, 10, 64)
	return hmac.Equal(mac, h.Sum(nil)) && at.Unix()-issued <= ttlSeconds
}

// documentedToken is Cloudflare's own Python example, transcribed:
// quote_plus(b64encode(hmac(secret, message + timestamp, sha256))).
func documentedToken(key []byte, message string, ts int64) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(message + strconv.FormatInt(ts, 10)))
	return strconv.FormatInt(ts, 10) + "-" + url.QueryEscape(base64.StdEncoding.EncodeToString(h.Sum(nil)))
}

// requestURI is what http.request.uri holds for raw: path and query.
func requestURI(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.RequestURI()
}

func TestTokenMatchesDocumentedAlgorithm(t *testing.T) {
	s := newSigner(t, nil)
	// Expires == TTL issues the token at now, as the documented example
	// does.
	req, err := s.SignURL(context.Background(), "downloads", "images/cat.jpg", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: ttl})
	if err != nil {
		t.Fatal(err)
	}
	want := base + "/images/cat.jpg?verify=" + documentedToken(secret, "/files/images/cat.jpg", now.Unix())
	if req.URL != want {
		t.Fatalf("URL = %q\nwant  %q", req.URL, want)
	}
}

func TestEdgeAcceptsUntilExpiry(t *testing.T) {
	for _, urlSafe := range []bool{false, true} {
		t.Run(strconv.FormatBool(urlSafe), func(t *testing.T) {
			s := newSigner(t, func(c *cloudflare.Config) { c.URLSafeMAC = urlSafe })
			req, err := s.SignURL(context.Background(), "downloads", "reports/q3 2026+final.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: 10 * time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if !req.ExpiresAt.Equal(now.Add(10 * time.Minute)) {
				t.Fatalf("ExpiresAt = %v", req.ExpiresAt)
			}
			uri := requestURI(t, req.URL)
			sep := len("?" + cloudflare.DefaultParam + "=")
			ttlSec := int64(ttl / time.Second)
			if !edgeValid(secret, uri, ttlSec, now, sep, urlSafe) {
				t.Fatalf("edge rejects a fresh token: %s", uri)
			}
			if !edgeValid(secret, uri, ttlSec, req.ExpiresAt, sep, urlSafe) {
				t.Fatal("edge rejects the token at its expiry")
			}
			if edgeValid(secret, uri, ttlSec, req.ExpiresAt.Add(time.Second), sep, urlSafe) {
				t.Fatal("edge accepts the token after its expiry")
			}
			if edgeValid(secret, strings.Replace(uri, "reports", "secrets", 1), ttlSec, now, sep, urlSafe) {
				t.Fatal("edge accepts the token for another path")
			}
			if edgeValid(bytes.Repeat([]byte("x"), 32), uri, ttlSec, now, sep, urlSafe) {
				t.Fatal("edge accepts the token under another secret")
			}
		})
	}
}

func TestCustomParam(t *testing.T) {
	s := newSigner(t, func(c *cloudflare.Config) { c.Param = "tok" })
	req, err := s.SignURL(context.Background(), "downloads", "a.jpg", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !edgeValid(secret, requestURI(t, req.URL), int64(ttl/time.Second), now, len("?tok="), false) {
		t.Fatalf("edge rejects %s", req.URL)
	}
}

func TestSignURLRefusals(t *testing.T) {
	s := newSigner(t, nil)
	cases := []struct {
		name string
		opts filesystem.PresignOptions
		code filesystem.Code
	}{
		{"no expiry", filesystem.PresignOptions{Method: filesystem.MethodGet}, filesystem.CodeInvalidArgument},
		{"beyond ttl", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: ttl + time.Second}, filesystem.CodeInvalidArgument},
		{"put", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute}, filesystem.CodeUnsupported},
		{"override", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, ContentType: "image/png"}, filesystem.CodeUnsupported},
	}
	for _, c := range cases {
		if _, err := s.SignURL(context.Background(), "downloads", "a.jpg", c.opts); filesystem.CodeOf(err) != c.code {
			t.Errorf("%s: err = %v, want %s", c.name, err, c.code)
		}
	}
}

// TestActiveTypeNeverInline signs through a Disk, which forces an
// attachment for SVG; the signer refuses rather than drop it.
func TestActiveTypeNeverInline(t *testing.T) {
	disk, err := filesystem.NewDisk("downloads", memory.New(), filesystem.WithURLSigner(newSigner(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = disk.SignedURL(context.Background(), "logo.svg", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, ContentType: "image/svg+xml"})
	if filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Fatalf("err = %v, want unsupported", err)
	}
}

func TestNewRefusesBadConfig(t *testing.T) {
	good := cloudflare.Config{BaseURL: base, Secret: secret, TTL: ttl}
	cases := map[string]func(*cloudflare.Config){
		"http base":      func(c *cloudflare.Config) { c.BaseURL = "http://downloads.example.test" },
		"query base":     func(c *cloudflare.Config) { c.BaseURL = base + "?a=b" },
		"short secret":   func(c *cloudflare.Config) { c.Secret = secret[:cloudflare.MinSecretBytes-1] },
		"no ttl":         func(c *cloudflare.Config) { c.TTL = 0 },
		"fractional ttl": func(c *cloudflare.Config) { c.TTL = 1500 * time.Millisecond },
		"huge ttl":       func(c *cloudflare.Config) { c.TTL = filesystem.MaxPresignExpiry + time.Second },
		"bad param":      func(c *cloudflare.Config) { c.Param = "a&b" },
	}
	for name, mutate := range cases {
		cfg := good
		mutate(&cfg)
		if _, err := cloudflare.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestSecretRedacted(t *testing.T) {
	cfg := cloudflare.Config{BaseURL: base, Secret: []byte("0123456789abcdef0123456789abcdef"), TTL: ttl}
	s, err := cloudflare.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var outputs []string
	for _, v := range []any{cfg, s} {
		var logs bytes.Buffer
		slog.New(slog.NewJSONHandler(&logs, nil)).Info("cfg", "value", v)
		outputs = append(outputs, logs.String())
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%d"} {
			outputs = append(outputs, fmt.Sprintf(verb, v))
		}
	}
	// Format answers every verb, so fmt never reaches GoString; call it
	// directly so a direct caller cannot see the secret either.
	outputs = append(outputs, cfg.GoString(), s.GoString())
	leaks := []string{string(cfg.Secret), fmt.Sprintf("%x", cfg.Secret), fmt.Sprint(cfg.Secret[:4]), base64.StdEncoding.EncodeToString(cfg.Secret)}
	for _, out := range outputs {
		for _, leak := range leaks {
			if strings.Contains(out, leak) {
				t.Fatalf("output leaks the secret: %s", out)
			}
		}
		if !strings.Contains(out, "REDACTED") {
			t.Fatalf("output is not the redacted form: %s", out)
		}
	}
}
