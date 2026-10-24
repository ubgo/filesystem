// Command concepts-serving shows how stored files reach browsers: signed
// download and upload links issued by signedurl and served by fshttp
// (ranges, ETags, expiry, tampering, key rotation), active content forced
// to download, stored HTTP headers, public files, a CDN base URL, links
// limited to an IP range, and HEAD requests.
//
//	go run ./concepts-serving          (from the examples directory)
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/signedurl"
)

// Mount points and lifetimes of the demo.
const (
	// signedPrefix is where fshttp.Signed is mounted; the signer's base
	// URL must point at it.
	signedPrefix = "/files"
	// publicPrefix is where fshttp.Public serves the public disk.
	publicPrefix = "/public"
	// linkLifetime is how long the demo's signed links live.
	linkLifetime = 5 * time.Minute
	// cdnBase is a CDN in front of the public bucket; never fetched.
	cdnBase = "https://cdn.example.com"
	// loopback is the one address the demo's HTTP client connects from.
	loopback = "127.0.0.1/32"
	// officeRange is a documentation range the client is not in.
	officeRange = "203.0.113.0/24"
)

// Signing secrets. Real ones come from a secret store and are at least
// signedurl.MinKeyBytes long; these are fixed so the demo is repeatable.
var (
	oldKey = signedurl.Key{ID: "2026-09", Secret: bytes.Repeat([]byte("o"), signedurl.MinKeyBytes)}
	newKey = signedurl.Key{ID: "2026-10", Secret: bytes.Repeat([]byte("n"), signedurl.MinKeyBytes)}
)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// app is the wiring every step uses.
type app struct {
	srv     *httptest.Server
	private *filesystem.Disk
	public  *filesystem.Disk
}

// run is main without the process, so the test can call it.
func run(ctx context.Context, out io.Writer) error {
	a, err := newApp()
	if err != nil {
		return err
	}
	defer a.srv.Close()
	for _, step := range []func(context.Context, *app, io.Writer) error{
		signedDownload, tampered, expired, rotation, activeContent, signedUpload, publicFiles, cdnURL, zipDownload, fsView,
		ipRestricted, headRequests, bearerTokens, cdnOverPresigning, ownHandler,
	} {
		if err := step(ctx, a, out); err != nil {
			return err
		}
	}
	return nil
}

// newApp wires two memory disks, a signer whose newest key signs and
// whose older key still verifies, and the two fshttp handlers.
// ds:def id=newapp-rwpby4bv
func newApp() (*app, error) {
	routes := http.NewServeMux()
	srv := httptest.NewServer(routes)
	signer, err := signedurl.New(srv.URL+signedPrefix, []signedurl.Key{newKey, oldKey})
	if err != nil {
		srv.Close()
		return nil, err
	}
	private, err := filesystem.NewDisk("private", memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		srv.Close()
		return nil, err
	}
	public, err := filesystem.NewDisk("public", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		srv.Close()
		return nil, err
	}
	disks, err := filesystem.NewManager(private, public)
	if err != nil {
		srv.Close()
		return nil, err
	}
	routes.Handle(signedPrefix+"/", http.StripPrefix(signedPrefix, fshttp.Signed(disks, signer)))
	routes.Handle(publicPrefix+"/", http.StripPrefix(publicPrefix, fshttp.Public(public)))
	return &app{srv: srv, private: private, public: public}, nil
}

// shape renders a signed URL's path and parameter names, leaving out the
// values (the expiry and signature change on every run).
func shape(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return err.Error()
	}
	names := slices.Sorted(maps.Keys(u.Query()))
	return u.Path + "?" + strings.Join(names, "&")
}

// fetch makes a request and returns the response with its body read.
func fetch(method, url string, body io.Reader, header http.Header) (*http.Response, string, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, "", err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp, string(b), err
}

// signedDownload issues a GET link for a private object and fetches it
// whole, as a range, and again with its ETag, which answers 304.
// ds:def id=signeddownload-7p72b6jy
func signedDownload(ctx context.Context, a *app, out io.Writer) error {
	if _, err := a.private.WriteBytes(ctx, "invoices/42.pdf", []byte("%PDF-1.7 invoice"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	link, err := a.private.SignedURL(ctx, "invoices/42.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "link: %s\n", shape(link.URL))
	resp, body, err := fetch(http.MethodGet, link.URL, nil, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "GET %d %s %q\n", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	resp, body, err = fetch(http.MethodGet, link.URL, nil, http.Header{"Range": {"bytes=0-3"}})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Range %d %q\n", resp.StatusCode, body)
	resp, _, err = fetch(http.MethodGet, link.URL, nil, http.Header{"If-None-Match": {resp.Header.Get("ETag")}})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "If-None-Match %d\n", resp.StatusCode)
	return nil
}

// tampered changes the key in a signed link. The signature covers the
// disk, key, method, expiry and response overrides, so the handler
// answers 403 without saying why.
// ds:def id=tampered-7sz65bwn
func tampered(ctx context.Context, a *app, out io.Writer) error {
	link, err := a.private.SignedURL(ctx, "invoices/42.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	resp, body, err := fetch(http.MethodGet, strings.Replace(link.URL, "42.pdf", "43.pdf", 1), nil, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "tampered %d %q\n", resp.StatusCode, strings.TrimSpace(body))
	_, err = a.private.SignedURL(ctx, "invoices/42.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: 8 * 24 * time.Hour})
	fmt.Fprintf(out, "eight-day link: %s\n", filesystem.CodeOf(err))
	return nil
}

// expired signs a link with a clock an hour behind, so it expired 55
// minutes ago, and shows the handler refusing it.
// ds:def id=expired-vc9eaxv7
func expired(ctx context.Context, a *app, out io.Writer) error {
	past := func() time.Time { return time.Now().Add(-time.Hour) }
	old, err := signedurl.New(a.srv.URL+signedPrefix, []signedurl.Key{newKey}, signedurl.WithClock(past))
	if err != nil {
		return err
	}
	link, err := old.SignURL(ctx, "private", "invoices/42.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	resp, _, err := fetch(http.MethodGet, link.URL, nil, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "expired %d\n", resp.StatusCode)
	return nil
}

// rotation shows a link signed with the previous key still working while
// that key stays in the verifier's list, newest first.
// ds:def id=rotation-dzyj6rqc
func rotation(ctx context.Context, a *app, out io.Writer) error {
	before, err := signedurl.New(a.srv.URL+signedPrefix, []signedurl.Key{oldKey})
	if err != nil {
		return err
	}
	link, err := before.SignURL(ctx, "private", "invoices/42.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	resp, _, err := fetch(http.MethodGet, link.URL, nil, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed with the old key %d\n", resp.StatusCode)
	return nil
}

// activeContent stores an HTML file, which runs script when a browser
// renders it. fshttp serves it as a download with nosniff and a sandbox
// policy, whatever the link asked for.
// ds:def id=activecontent-9jn8d9f3
func activeContent(ctx context.Context, a *app, out io.Writer) error {
	page := []byte("<html><script>alert(document.cookie)</script></html>")
	if _, err := a.private.WriteBytes(ctx, "uploads/page.html", page, filesystem.WriteOptions{}); err != nil {
		return err
	}
	link, err := a.private.SignedURL(ctx, "uploads/page.html", filesystem.PresignOptions{
		Method: filesystem.MethodGet, Expires: linkLifetime, ContentDisposition: "inline",
	})
	if err != nil {
		return err
	}
	resp, _, err := fetch(http.MethodGet, link.URL, nil, nil)
	if err != nil {
		return err
	}
	h := resp.Header
	fmt.Fprintf(out, "html: %s | %s | %s | %s\n", h.Get("Content-Type"), h.Get("Content-Disposition"), h.Get(fshttp.HeaderContentTypeOptions), h.Get(fshttp.HeaderCSP))
	return nil
}

// signedUpload issues a PUT link that pins the content type, then sends
// the file with it and with a different one.
// ds:def id=signedupload-gkp2atb5
func signedUpload(ctx context.Context, a *app, out io.Writer) error {
	link, err := a.private.SignedURL(ctx, "avatars/u1.png", filesystem.PresignOptions{
		Method: filesystem.MethodPut, Expires: linkLifetime, ContentType: "image/png",
	})
	if err != nil {
		return err
	}
	png := []byte("\x89PNG\r\n\x1a\n not really")
	resp, _, err := fetch(http.MethodPut, link.URL, bytes.NewReader(png), http.Header{"Content-Type": {"text/html"}})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "PUT with another type %d\n", resp.StatusCode)
	resp, _, err = fetch(http.MethodPut, link.URL, bytes.NewReader(png), link.Header)
	if err != nil {
		return err
	}
	e, err := a.private.Stat(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "PUT with the signed headers %d, stored %d bytes as %s\n", resp.StatusCode, e.Size, e.ContentType)
	return nil
}

// publicFiles serves the public disk by key. A private object answers 404
// like a missing one, so nobody learns that it exists; stored HTTP
// headers are sent with the file.
// ds:def id=publicfiles-94jkcbat
func publicFiles(ctx context.Context, a *app, out io.Writer) error {
	headers := filesystem.HTTPHeaders{CacheControl: "public, max-age=31536000, immutable"}
	if _, err := a.public.WriteBytes(ctx, "logo.png", []byte("\x89PNG\r\n\x1a\n"), filesystem.WriteOptions{Headers: headers}); err != nil {
		return err
	}
	if _, err := a.public.WriteBytes(ctx, "draft.png", []byte("\x89PNG\r\n\x1a\n"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPrivate}); err != nil {
		return err
	}
	for _, key := range []string{"logo.png", "draft.png", "missing.png"} {
		resp, _, err := fetch(http.MethodGet, a.srv.URL+publicPrefix+"/"+key, nil, nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "public %s %d %s\n", key, resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	return nil
}

// cdnURL builds public URLs from a configured base, with no backend call:
// the URL a CDN in front of the bucket answers.
// ds:def id=cdnurl-jevvsku7
func cdnURL(ctx context.Context, _ *app, out io.Writer) error {
	disk, err := filesystem.NewDisk("cdn", memory.New(), filesystem.WithPublicURL(cdnBase))
	if err != nil {
		return err
	}
	u, err := disk.PublicURL(ctx, "products/blue shirt.jpg")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "public URL %s\n", u)
	return nil
}

// zipDownload streams several private objects as one zip with
// fshttp.ServeZip: every key is checked and sized before the first byte,
// a name that would escape the archive is refused with 400, and the
// archive is written one object at a time without buffering.
// ds:def id=zipdownload-k3v8q2wm
func zipDownload(ctx context.Context, a *app, out io.Writer) error {
	for key, body := range map[string]string{"invoices/41.pdf": "%PDF-1.7 41", "invoices/42.pdf": "%PDF-1.7 42"} {
		if _, err := a.private.WriteBytes(ctx, key, []byte(body), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	files := []fshttp.ZipFile{{Key: "invoices/41.pdf", Name: "2026/41.pdf"}, {Key: "invoices/42.pdf", Name: "2026/42.pdf"}}
	rec := httptest.NewRecorder()
	fshttp.ServeZip(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/invoices.zip", nil), a.private, "invoices.zip", files, fshttp.ZipOptions{})
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		return err
	}
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	fmt.Fprintf(out, "zip %d %s %s\n", rec.Code, rec.Header().Get("Content-Disposition"), strings.Join(names, ","))

	rec = httptest.NewRecorder()
	evil := []fshttp.ZipFile{{Key: "invoices/41.pdf", Name: "../../etc/passwd"}}
	fshttp.ServeZip(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/evil.zip", nil), a.private, "evil.zip", evil, fshttp.ZipOptions{})
	fmt.Fprintf(out, "zip with ../ in a name %d\n", rec.Code)
	return nil
}

// fsView reads a Disk through io/fs, so standard functions such as
// fs.WalkDir, fs.ReadFile and http.FileServerFS work on any backend.
// The view is read-only.
// ds:def id=fsview-p6t4h9zr
func fsView(ctx context.Context, a *app, out io.Writer) error {
	fsys := a.private.FS(ctx)
	var files []string
	err := fs.WalkDir(fsys, "invoices", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		return err
	}
	b, err := fs.ReadFile(fsys, "invoices/41.pdf")
	if err != nil {
		return err
	}
	_, statErr := fs.Stat(fsys, "invoices/missing.pdf")
	fmt.Fprintf(out, "fs walk %s | read %q | missing is fs.ErrNotExist: %v\n", strings.Join(files, ","), b, errors.Is(statErr, fs.ErrNotExist))
	return nil
}

// ipRestricted signs two links to one object, each limited to a CIDR
// range. fshttp.Signed checks the connection's address against the range
// under the signature: the link for the loopback range works for this
// client, the one for another range answers a bare 403.
// ds:def id=iprestricted-3hmkztb6
func ipRestricted(ctx context.Context, a *app, out io.Writer) error {
	for _, allowed := range []string{loopback, officeRange} {
		link, err := a.private.SignedURL(ctx, "invoices/42.pdf", filesystem.PresignOptions{
			Method: filesystem.MethodGet, Expires: linkLifetime, AllowedIP: allowed,
		})
		if err != nil {
			return err
		}
		resp, _, err := fetch(http.MethodGet, link.URL, nil, nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "link for %s: %d\n", allowed, resp.StatusCode)
	}
	_, err := a.private.SignedURL(ctx, "invoices/42.pdf", filesystem.PresignOptions{
		Method: filesystem.MethodGet, Expires: linkLifetime, AllowedIP: "203.0.113.7/24",
	})
	fmt.Fprintf(out, "non-canonical range: %s\n", filesystem.CodeOf(err))
	return nil
}

// headRequests asks both handlers for an object's headers without its
// body: a HEAD on a GET link (no separate signature needed) and on a
// public URL. Clients use it to learn the size and type before a download.
// ds:def id=headrequests-ng53kc7b
func headRequests(ctx context.Context, a *app, out io.Writer) error {
	link, err := a.private.SignedURL(ctx, "invoices/42.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	resp, body, err := fetch(http.MethodHead, link.URL, nil, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "HEAD signed %d length=%s type=%s body=%d bytes\n",
		resp.StatusCode, resp.Header.Get("Content-Length"), resp.Header.Get("Content-Type"), len(body))
	resp, _, err = fetch(http.MethodHead, a.srv.URL+publicPrefix+"/missing.png", nil, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "HEAD public missing %d\n", resp.StatusCode)
	return nil
}
