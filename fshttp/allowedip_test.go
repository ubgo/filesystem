package fshttp_test

import (
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
)

// loopback is where httptest's clients connect from.
var loopback = netip.MustParsePrefix("127.0.0.0/8")

// TestSignedAllowedIP proves an IP-restricted URL answers only requests
// from inside its range, for downloads and uploads; that X-Forwarded-For is
// ignored unless the connecting address is a trusted proxy; and that behind
// a trusted proxy the client is the first untrusted hop from the right, so
// a client cannot prepend an allowed address to get in.
func TestSignedAllowedIP(t *testing.T) {
	ctx := t.Context()
	direct := setup(t)
	if _, err := direct.disk.Write(ctx, "a.txt", strings.NewReader("hello"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	restricted := func(e *env, method filesystem.HTTPMethod, cidr string) string {
		return e.sign(t, e.disk, "a.txt", filesystem.PresignOptions{Method: method, Expires: time.Minute, AllowedIP: cidr})
	}
	xff := func(v string) http.Header { return http.Header{"X-Forwarded-For": {v}} }

	if resp, body := do(t, http.MethodGet, restricted(direct, filesystem.MethodGet, "127.0.0.1/32"), nil, nil); resp.StatusCode != http.StatusOK || body != "hello" {
		t.Errorf("from inside the range: %d %q", resp.StatusCode, body)
	}
	outside := restricted(direct, filesystem.MethodGet, "203.0.113.0/24")
	if resp, _ := do(t, http.MethodGet, outside, nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("from outside the range: %d", resp.StatusCode)
	}
	// Without trusted proxies a forged header changes nothing.
	if resp, _ := do(t, http.MethodGet, outside, nil, xff("203.0.113.9")); resp.StatusCode != http.StatusForbidden {
		t.Errorf("forged X-Forwarded-For was believed: %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodPut, restricted(direct, filesystem.MethodPut, "203.0.113.0/24"), strings.NewReader("x"), nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("upload from outside the range: %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodPut, restricted(direct, filesystem.MethodPut, "127.0.0.0/8"), strings.NewReader("x"), nil); resp.StatusCode != http.StatusOK {
		t.Errorf("upload from inside the range: %d", resp.StatusCode)
	}

	// An IPv4-mapped prefix covers the IPv4 clients it names: clients are
	// compared unmapped, so the prefix is unmapped when parsed.
	if resp, _ := do(t, http.MethodGet, restricted(direct, filesystem.MethodGet, "::ffff:127.0.0.1/128"), nil, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("IPv4-mapped prefix from inside it: %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodGet, restricted(direct, filesystem.MethodGet, "::ffff:203.0.113.0/120"), nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("IPv4-mapped prefix from outside it: %d", resp.StatusCode)
	}
	for in, want := range map[string]string{"::ffff:127.0.0.1/128": "127.0.0.1/32", "::ffff:203.0.113.0/120": "203.0.113.0/24", "::ffff:0.0.0.0/96": "0.0.0.0/0"} {
		if p, err := filesystem.ParseAllowedIP(in); err != nil || p.String() != want {
			t.Errorf("ParseAllowedIP(%q) = %v, %v; want %s", in, p, err, want)
		}
	}
	if _, err := filesystem.ParseAllowedIP("::ffff:0:0/95"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("mapped prefix shorter than /96: %v", err)
	}

	proxied := setup(t, fshttp.SignedOptions{TrustedProxies: []netip.Prefix{loopback}})
	if _, err := proxied.disk.Write(ctx, "a.txt", strings.NewReader("hello"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	inRange := restricted(proxied, filesystem.MethodGet, "203.0.113.0/24")
	for _, c := range []struct {
		name   string
		header http.Header
		want   int
	}{
		{"client in range behind the proxy", xff("203.0.113.9"), http.StatusOK},
		{"trusted hops are skipped", xff("203.0.113.9, 127.0.0.2"), http.StatusOK},
		{"client prepends an allowed address", xff("203.0.113.9, 198.51.100.7"), http.StatusForbidden},
		{"client out of range", xff("198.51.100.7"), http.StatusForbidden},
		{"only trusted hops", xff("127.0.0.3"), http.StatusForbidden},
		{"no header: the proxy itself", nil, http.StatusForbidden},
		{"unparsable hop", xff("not-an-ip"), http.StatusForbidden},
	} {
		if resp, _ := do(t, http.MethodGet, inRange, nil, c.header); resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
}
