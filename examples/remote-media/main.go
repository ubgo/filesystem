// Command remote-media caches media from other servers with
// github.com/ubgo/filesystem/contrib/remote: a link preview of a page with
// its og:image stored, an HMAC-signed image proxy, a forged proxy URL and
// an SVG posing as a PNG refused, then a purge and a size sweep.
//
// The "other servers" run in-process on 127.0.0.1. The SSRF guard refuses
// loopback, so this demo allows that one address with AllowAddr; a real
// deployment leaves AllowAddr unset.
package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/remote"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
)

const (
	pngData = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-cover-image-body"
	svgData = `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(document.cookie)</script></svg>`
	page    = `<!DOCTYPE html><html><head><title>Fallback</title>
<meta property="og:title" content="Release notes">
<meta property="og:description" content="What shipped this week.">
<meta property="og:image" content="/img/cover.png">
</head><body>…</body></html>`
	// proxyLifetime is how long a signed proxy URL stays valid.
	proxyLifetime = 10 * time.Minute
)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// newCache keeps remote media under remote/ on disk: images only, at most
// 5 MiB, the type decided from the bytes; link previews stay fresh for a
// day. allow is the demo's exception to the SSRF guard (nil in production).
// ds:def id=newcache-p8tqkkv4
func newCache(disk *filesystem.Disk, allow func(netip.Addr) bool) (*remote.Cache, error) {
	return remote.New(disk, remote.Options{
		Dir:        "remote",
		Policy:     upload.Policy{MaxSize: 5 << 20, Allowed: []string{"image/*"}},
		Fetch:      upload.FetchOptions{Timeout: 10 * time.Second, AllowAddr: allow},
		PreviewTTL: 24 * time.Hour,
	})
}

// newProxy mounts the image proxy at base with its own signing key.
// ds:def id=newproxy-uq85q2ww
func newProxy(cache *remote.Cache, base string) (*remote.Proxy, error) {
	secret := make([]byte, signedurl.MinKeyBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	signer, err := signedurl.New(base, []signedurl.Key{{ID: "proxy-1", Secret: secret}})
	if err != nil {
		return nil, err
	}
	return remote.NewProxy(cache, remote.ProxyOptions{Base: base, Signer: signer})
}

// card is what a chat or feed renders for a link someone posted: the
// page's own text (escape it where you render it) and its image served
// through the proxy, never hotlinked, so the visitor's browser never
// contacts the remote site.
type card struct {
	Title, Description, ImageSrc string
}

// linkCard builds the card of a posted link: LinkPreview fetches and
// caches the page's metadata and image through the SSRF guard, and the
// proxy signs a short-lived URL for the image. A page whose image could
// not be cached still gets a card, without the image.
// ds:def id=linkcard-t5m2w8kq
func linkCard(ctx context.Context, cache *remote.Cache, proxy *remote.Proxy, postURL string) (card, error) {
	p, err := cache.LinkPreview(ctx, postURL)
	if err != nil {
		return card{}, err
	}
	c := card{Title: p.Title, Description: p.Description}
	if p.ImageURL == "" || p.ImageError != "" {
		return c, nil
	}
	c.ImageSrc, err = proxy.URL(ctx, p.ImageURL, proxyLifetime)
	if err != nil {
		return card{}, err
	}
	return c, nil
}

func run(ctx context.Context, out io.Writer) error {
	site := http.NewServeMux()
	site.HandleFunc("/post", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, page) })
	site.HandleFunc("/img/cover.png", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, pngData) })
	site.HandleFunc("/img/evil.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = io.WriteString(w, svgData)
	})
	remoteSite := httptest.NewServer(site)
	defer remoteSite.Close()

	dir, err := os.MkdirTemp("", "remote-media-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	driver, err := local.New(dir)
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("cache", driver)
	if err != nil {
		return err
	}
	loopback := netip.MustParseAddr("127.0.0.1")
	cache, err := newCache(disk, func(a netip.Addr) bool { return a == loopback })
	if err != nil {
		return err
	}

	p, err := cache.LinkPreview(ctx, remoteSite.URL+"/post")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "preview   %q / %q\n", p.Title, p.Description)
	fmt.Fprintf(out, "image     %s %d bytes, stored under the URL's hash: %t\n", p.ImageType, p.ImageSize, p.ImageKey != "")

	app := http.NewServeMux()
	appServer := httptest.NewServer(app)
	defer appServer.Close()
	proxy, err := newProxy(cache, appServer.URL+"/media/proxy")
	if err != nil {
		return err
	}
	app.Handle("/media/proxy", proxy)
	c, err := linkCard(ctx, cache, proxy, remoteSite.URL+"/post")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "card      %q, image through the proxy: %t\n", c.Title, strings.HasPrefix(c.ImageSrc, appServer.URL+"/media/proxy?"))

	signed, err := proxy.URL(ctx, p.ImageURL, proxyLifetime)
	if err != nil {
		return err
	}
	if err := show(out, "signed", signed); err != nil {
		return err
	}
	if err := show(out, "forged", appServer.URL+"/media/proxy?u=http://169.254.169.254/latest/meta-data/"); err != nil {
		return err
	}
	evil, err := proxy.URL(ctx, remoteSite.URL+"/img/evil.png", proxyLifetime)
	if err != nil {
		return err
	}
	if err := show(out, "svg-png", evil); err != nil {
		return err
	}

	if err := cache.Purge(ctx, remoteSite.URL+"/post"); err != nil {
		return err
	}
	res, err := cache.Sweep(ctx, remote.SweepOptions{MaxBytes: 1 << 30, MaxAge: 30 * 24 * time.Hour})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sweep     kept %d entries, %d bytes; deleted %d\n", res.Kept, res.Bytes, res.Deleted)
	return oembedPreviews(ctx, out, disk, func(a netip.Addr) bool { return a == loopback })
}

// show requests u and prints the status and the headers that matter.
func show(out io.Writer, label, u string) error {
	resp, err := http.Get(u)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	n, _ := io.Copy(io.Discard, resp.Body)
	fmt.Fprintf(out, "%-9s %d", label, resp.StatusCode)
	if resp.StatusCode == http.StatusOK {
		fmt.Fprintf(out, " %s %d bytes, %s, %s", resp.Header.Get("Content-Type"), n, resp.Header.Get("X-Content-Type-Options"), resp.Header.Get("Cache-Control"))
	}
	fmt.Fprintln(out)
	return nil
}
