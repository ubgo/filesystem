package main

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/vercelblob"
	"github.com/ubgo/filesystem/examples/adapters-hosted/internal/fakes"
)

// vercelToken has the shape of a store's read-write token
// (vercel_blob_rw_<store id>_<secret>); the driver derives the store's
// blob host from its store ID. On Vercel it is BLOB_READ_WRITE_TOKEN.
const vercelToken = "vercel_blob_rw_StoreAbc123_secretsecret"

// vercelUploadLimit is the size limit the fake enforces, standing in for
// the plan's upload limit.
const vercelUploadLimit = 1 << 10

// runVercel walks the Vercel Blob driver on a public and a private store.
func runVercel(ctx context.Context, out io.Writer) error {
	if err := vercelTokenAndURLs(ctx, out); err != nil {
		return err
	}
	pub, priv := fakes.NewVercel(vercelToken, true), fakes.NewVercel(vercelToken, false)
	defer pub.Close()
	defer priv.Close()
	public, err := openVercel(vercelToken, vercelblob.AccessPublic, pub)
	if err != nil {
		return err
	}
	private, err := openVercel(vercelToken, vercelblob.AccessPrivate, priv)
	if err != nil {
		return err
	}
	if err := vercelBasics(ctx, out, private); err != nil {
		return err
	}
	if err := vercelVisibility(ctx, out, public, private); err != nil {
		return err
	}
	return vercelErrors(ctx, out, pub)
}

// vercelTokenAndURLs shows what the driver derives from the token alone,
// with no request: the store's blob host, the URL shape of a public blob,
// and the Config printing with the token redacted.
// ds:def id=verceltokenandurls-7qeurunm
func vercelTokenAndURLs(ctx context.Context, out io.Writer) error {
	cfg := vercelblob.Config{Token: vercelToken, Access: vercelblob.AccessPublic}
	drv, err := vercelblob.New(cfg)
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("blob", drv, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		return err
	}
	for _, key := range []string{"avatars/u1.png", "docs/annual report.pdf"} {
		u, err := disk.PublicURL(ctx, key)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "public URL: %s\n", u)
	}
	fmt.Fprintf(out, "config prints as: %v\n", cfg)
	return nil
}

// openVercel opens a disk on a store. APIURL and BlobURL point at the
// fake; against Vercel leave them empty and the blob host comes from the
// token. A public store's disk defaults writes to public, since every blob
// in it is.
// ds:def id=openvercel-5wv46szv
func openVercel(token string, access vercelblob.Access, fake *fakes.Vercel) (*filesystem.Disk, error) {
	drv, err := vercelblob.New(vercelblob.Config{
		Token:   token,
		Access:  access,
		APIURL:  fake.API.URL,
		BlobURL: fake.Blobs.URL,
	})
	if err != nil {
		return nil, err
	}
	var opts []filesystem.Option
	if access == vercelblob.AccessPublic {
		opts = append(opts, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	}
	return filesystem.NewDisk("blob-"+string(access), drv, opts...)
}

// vercelBasics writes, stats, reads a range, lists, copies on the server
// and deletes on a private store.
// ds:def id=vercelbasics-unjm7x3q
func vercelBasics(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	if _, err := disk.WriteBytes(ctx, "avatars/u1.png", pngHeader, filesystem.WriteOptions{}); err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stat: %s %d bytes %s etag=%s\n", e.Key, e.Size, e.ContentType, e.ETag)
	r, err := disk.ReadRange(ctx, "avatars/u1.png", filesystem.ByteRange{Offset: 1, Length: 3})
	if err != nil {
		return err
	}
	head, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "range read: %q\n", head)
	if _, err := disk.Copy(ctx, "avatars/u1.png", "avatars/u1-backup.png"); err != nil {
		return err
	}
	page, err := disk.ListPage(ctx, "avatars", filesystem.ListOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "list avatars: %d entries after a server-side copy\n", len(page.Entries))
	if err := disk.Delete(ctx, "avatars/u1-backup.png"); err != nil {
		return err
	}
	ok, err := disk.Exists(ctx, "avatars/u1-backup.png")
	fmt.Fprintf(out, "after delete: exists=%t (%s)\n", ok, outcome(err))
	return err
}

// vercelVisibility shows what each store declares and refuses: public
// URLs only on a public store, no per-object visibility, no create-only
// write, and a compare-and-swap that refuses a stale ETag.
// ds:def id=vercelvisibility-4dq65ubj
func vercelVisibility(ctx context.Context, out io.Writer, public, private *filesystem.Disk) error {
	fmt.Fprintf(out, "public store capabilities: %s\n", public.Capabilities())
	fmt.Fprintf(out, "private store capabilities: %s\n", private.Capabilities())
	_, err := private.PublicURL(ctx, "avatars/u1.png")
	fmt.Fprintf(out, "private store PublicURL: %s\n", outcome(err))
	_, err = public.WriteBytes(ctx, "secret.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPrivate})
	fmt.Fprintf(out, "private write on the public store: %s\n", outcome(err))
	_, err = private.WriteBytes(ctx, "avatars/u1.png", pngHeader, filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "IfNotExists: %s\n", outcome(err))
	e, err := private.Stat(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	if _, err := private.WriteBytes(ctx, "avatars/u1.png", pngHeader, filesystem.WriteOptions{IfMatch: e.ETag}); err != nil {
		return err
	}
	_, err = private.WriteBytes(ctx, "avatars/u1.png", pngHeader, filesystem.WriteOptions{IfMatch: e.ETag})
	fmt.Fprintf(out, "IfMatch with the current etag: ok; with the same etag again: %s\n", outcome(err))
	return nil
}

// vercelErrors shows how the API's error answers come back: a wrong token,
// a rate limit with its Retry-After, and a body over the size limit.
// ds:def id=vercelerrors-d9h25xtq
func vercelErrors(ctx context.Context, out io.Writer, fake *fakes.Vercel) error {
	wrong, err := openVercel("vercel_blob_rw_StoreAbc123_wrongsecret", vercelblob.AccessPublic, fake)
	if err != nil {
		return err
	}
	_, err = wrong.Stat(ctx, "avatars/u1.png")
	fmt.Fprintf(out, "wrong token: %s\n", outcome(err))
	disk, err := openVercel(vercelToken, vercelblob.AccessPublic, fake)
	if err != nil {
		return err
	}
	fake.FailNext(fakes.Failure{Status: http.StatusTooManyRequests, Code: "rate_limited", RetryAfter: int(retryHint.Seconds())})
	_, err = disk.Stat(ctx, "avatars/u1.png")
	fmt.Fprintf(out, "rate limited: %s\n", outcome(err))
	fake.MaxBytes = vercelUploadLimit
	_, err = disk.WriteBytes(ctx, "big.bin", make([]byte, vercelUploadLimit+1), filesystem.WriteOptions{})
	fmt.Fprintf(out, "over the size limit: %s\n", outcome(err))
	return nil
}
