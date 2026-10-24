// Command concepts-delivery shows the ways a stored file can reach a
// browser without the application streaming it: a CloudFront signed URL
// and signed cookies, a Cloudflare token-authentication URL, an nginx
// X-Accel-Redirect handed off by fshttp, and a tiny file inlined as a data
// URI.
//
//	go run ./concepts-delivery          (from the examples directory)
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/cloudflare"
	"github.com/ubgo/filesystem/contrib/cloudfront"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
)

// Bases and lifetimes of the demo; the hosts are never fetched.
const (
	cloudfrontBase = "https://cdn.example.com/media"
	cloudflareBase = "https://downloads.example.com/files"
	// cloudflareRuleTTL is the ttl the WAF rule passes to
	// is_timed_hmac_valid_v0 (3 hours).
	cloudflareRuleTTL = 3 * time.Hour
	linkLifetime      = 10 * time.Minute
	// inlineLimit is the largest file the demo inlines as a data URI.
	inlineLimit = 2 << 10
	// protectedLocation is the nginx internal location the local disk's
	// directory is aliased under.
	protectedLocation = "/protected/"
)

// pixel is a 1x1 transparent PNG.
var pixel = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\x00\x01\x00\x00\x05\x00\x01\r\n-\xb4\x00\x00\x00\x00IEND\xaeB`\x82")

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, out io.Writer) error {
	for _, step := range []func(context.Context, io.Writer) error{cloudfrontLinks, cloudflareLink, sendfile, dataURI} {
		if err := step(ctx, out); err != nil {
			return err
		}
	}
	return nil
}

// cloudfrontLinks signs a download URL and a set of cookies for a
// distribution with restricted viewer access.
// ds:def id=cloudfrontlinks-p94veyrr
func cloudfrontLinks(ctx context.Context, out io.Writer) error {
	// Real code loads the key from a secret store, e.g. with
	// sign.LoadPEMPrivKey; the demo makes a throwaway one.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	signer, err := cloudfront.New(cloudfront.Config{BaseURL: cloudfrontBase, KeyPairID: "K2JCJMDEHXQW5F", PrivateKey: key})
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return err
	}
	req, err := disk.SignedURL(ctx, "videos/42/poster.jpg", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "cloudfront URL: %s\n", shape(req.URL))
	_, err = disk.SignedURL(ctx, "page.html", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime, ContentType: "text/html"})
	fmt.Fprintf(out, "cloudfront HTML link: %s\n", filesystem.CodeOf(err))
	cookies, _, err := signer.SignCookies(ctx, "videos/42", time.Hour)
	if err != nil {
		return err
	}
	names := make([]string, len(cookies))
	for i, c := range cookies {
		names[i] = c.Name
	}
	fmt.Fprintf(out, "cloudfront cookies: %s (Secure %t, HttpOnly %t)\n", strings.Join(names, ","), cookies[0].Secure, cookies[0].HttpOnly)
	fmt.Fprintf(out, "printed signer: %v\n", signer)
	return nil
}

// cloudflareLink signs a URL for a zone guarded by a token-authentication
// WAF rule.
// ds:def id=cloudflarelink-8bqds4qz
func cloudflareLink(ctx context.Context, out io.Writer) error {
	// The rule's key; real code reads it from a secret store.
	secret := bytes.Repeat([]byte("k"), cloudflare.MinSecretBytes)
	signer, err := cloudflare.New(cloudflare.Config{BaseURL: cloudflareBase, Secret: secret, TTL: cloudflareRuleTTL})
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("downloads", memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return err
	}
	req, err := disk.SignedURL(ctx, "reports/q3.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "cloudflare URL: %s, valid %s\n", shape(req.URL), req.ExpiresAt.Sub(time.Now()).Round(time.Minute))
	_, err = disk.SignedURL(ctx, "reports/q3.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: 4 * time.Hour})
	fmt.Fprintf(out, "cloudflare link longer than the rule's TTL: %s\n", filesystem.CodeOf(err))
	return nil
}

// sendfile serves a local disk's public files through fshttp.Public, which
// checks the request and sets the headers, then leaves the bytes to nginx.
// ds:def id=sendfile-fz527whq
func sendfile(ctx context.Context, out io.Writer) error {
	dir, err := os.MkdirTemp("", "concepts-delivery-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	drv, err := local.New(dir)
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("assets", drv, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		return err
	}
	if _, err := disk.Write(ctx, "logos/acme corp.png", bytes.NewReader(pixel), filesystem.WriteOptions{}); err != nil {
		return err
	}
	handler := fshttp.Public(disk, fshttp.PublicOptions{Sendfile: &fshttp.Sendfile{
		Header: fshttp.HeaderAccelRedirect,
		// Only the cleaned key reaches the mapping, never the request path.
		Path: func(_, key string) (string, bool) {
			return protectedLocation + filesystem.EscapeKey(key), true
		},
	}})
	for _, path := range []string{"/logos/acme%20corp.png", "/logos/../../etc/passwd"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "http://app.test"+path, nil))
		h := rec.Header()
		fmt.Fprintf(out, "sendfile %s: %d %s=%q body %d bytes | %s | %s\n", path, rec.Code, fshttp.HeaderAccelRedirect, h.Get(fshttp.HeaderAccelRedirect), rec.Body.Len(), h.Get("Content-Type"), h.Get(fshttp.HeaderContentTypeOptions))
	}
	return nil
}

// dataURI inlines a tiny image and shows what DataURI refuses.
// ds:def id=datauri-awmfa2av
func dataURI(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("icons", memory.New())
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"pixel.png": pixel,
		"logo.svg":  []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"hero.png":  append(slices.Clone(pixel), make([]byte, inlineLimit)...),
	}
	for _, key := range []string{"pixel.png", "logo.svg", "hero.png"} {
		if _, err := disk.Write(ctx, key, bytes.NewReader(files[key]), filesystem.WriteOptions{}); err != nil {
			return err
		}
		uri, err := disk.DataURI(ctx, key, inlineLimit)
		if err != nil {
			fmt.Fprintf(out, "data URI %s: %s\n", key, filesystem.CodeOf(err))
			continue
		}
		fmt.Fprintf(out, "data URI %s: %s… (%d chars)\n", key, uri[:len("data:image/png;base64,")], len(uri))
	}
	return nil
}

// shape prints a signed URL with its parameter values hidden, so the
// output is the same on every run.
func shape(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	names := make([]string, 0, len(u.Query()))
	for name := range u.Query() {
		names = append(names, name)
	}
	slices.Sort(names)
	u.RawQuery = ""
	return u.String() + "?" + strings.Join(names, "&")
}
