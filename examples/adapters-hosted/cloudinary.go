package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/cloudinary"
	"github.com/ubgo/filesystem/examples/adapters-hosted/internal/fakes"
)

// The fake account's credentials. On Cloudinary they are on the dashboard;
// read the key and secret from the environment, never from source.
const (
	cloudName       = "acme"
	cloudAPIKey     = "123456789012345"
	cloudAPISecret  = "abcdEFGHsecretXYZ"
	cloudUploadSize = 1 << 10 // the fake's raw-upload limit
)

// statusRateLimited is the 420 the Admin API answers when the hourly
// limit is spent.
const statusRateLimited = 420

// runCloudinary walks the Cloudinary driver on one account with both
// delivery types.
func runCloudinary(ctx context.Context, out io.Writer) error {
	fmt.Fprintf(out, "config prints as: %v\n", cloudinary.Config{CloudName: cloudName, APIKey: cloudAPIKey, APISecret: cloudAPISecret})
	fake := fakes.NewCloudinary(cloudName, cloudAPIKey, cloudAPISecret)
	defer fake.Close()
	private, err := openCloudinary(cloudAPISecret, cloudinary.TypeAuthenticated, fake.URL)
	if err != nil {
		return err
	}
	public, err := openCloudinary(cloudAPISecret, cloudinary.TypeUpload, fake.URL)
	if err != nil {
		return err
	}
	if err := cloudinaryBasics(ctx, out, private, fake); err != nil {
		return err
	}
	if err := cloudinaryPublicIDs(ctx, out, public, fake); err != nil {
		return err
	}
	if err := cloudinaryForeign(ctx, out, private, fake); err != nil {
		return err
	}
	if err := cloudinaryRefusals(ctx, out, public, private); err != nil {
		return err
	}
	return cloudinaryErrors(ctx, out, private, fake)
}

// openCloudinary opens a disk on the account. APIURL and DeliveryURL point
// at the fake; against Cloudinary leave them empty. TypeUpload resources
// have public delivery URLs, so that disk defaults writes to public.
// ds:def id=opencloudinary-xc9n9csz
func openCloudinary(secret string, typ cloudinary.Type, fakeURL string) (*filesystem.Disk, error) {
	drv, err := cloudinary.New(cloudinary.Config{
		CloudName:   cloudName,
		APIKey:      cloudAPIKey,
		APISecret:   secret,
		Type:        typ,
		APIURL:      fakeURL + "/v1_1",
		DeliveryURL: fakeURL,
	})
	if err != nil {
		return nil, err
	}
	var opts []filesystem.Option
	if typ == cloudinary.TypeUpload {
		opts = append(opts, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	}
	return filesystem.NewDisk("cloudinary-"+string(typ), drv, opts...)
}

// cloudinaryBasics writes a PNG as a raw resource, stats it (no content
// type is kept), reads it back byte for byte, renames it, copies it (the
// Disk streams) and deletes a folder by prefix.
// ds:def id=cloudinarybasics-uesjzuv9
func cloudinaryBasics(ctx context.Context, out io.Writer, disk *filesystem.Disk, fake *fakes.Cloudinary) error {
	w, err := disk.WriteBytes(ctx, "avatars/u1.png", pngHeader, filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "write entry type %q, stat type %q, %d bytes, etag=version %s\n", w.ContentType, e.ContentType, e.Size, e.ETag)
	got, err := disk.ReadAll(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "read back identical: %t\n", bytes.Equal(got, pngHeader))
	if _, err := disk.Move(ctx, "avatars/u1.png", "avatars/user-1.png"); err != nil {
		return err
	}
	if _, err := disk.Copy(ctx, "avatars/user-1.png", "avatars/user-1-copy.png"); err != nil {
		return err
	}
	page, err := disk.ListPage(ctx, "avatars", filesystem.ListOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "after move and copy: %s\n", keysOf(page))
	if err := disk.DeletePrefix(ctx, "avatars"); err != nil {
		return err
	}
	fmt.Fprintf(out, "after DeletePrefix: %d resources; Admin API calls so far: %d\n", len(fake.PublicIDs()), fake.AdminCalls())
	return nil
}

// cloudinaryPublicIDs shows how a key maps to a public ID: bytes outside
// [A-Za-z0-9._-] are written ~XX, listings decode them, and the public
// delivery URL carries the encoded ID under /raw/upload/.
// ds:def id=cloudinarypublicids-bpnyfrv5
func cloudinaryPublicIDs(ctx context.Context, out io.Writer, disk *filesystem.Disk, fake *fakes.Cloudinary) error {
	const key = "docs/annual report (v2).pdf"
	if _, err := disk.WriteBytes(ctx, key, []byte("%PDF-1.7"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	fmt.Fprintf(out, "stored as: %s\n", strings.Join(fake.PublicIDs(), ", "))
	page, err := disk.ListPage(ctx, "docs", filesystem.ListOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "listed as: %s\n", keysOf(page))
	u, err := disk.PublicURL(ctx, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "public URL path: %s\n", strings.TrimPrefix(u, fake.URL))
	return nil
}

// cloudinaryForeign shows what the driver sees of resources it did not
// write: a raw one whose public ID decodes is listed, one that does not
// (a space) is skipped, and an image resource is never seen, since the
// driver reads and writes raw resources only.
// ds:def id=cloudinaryforeign-6f7n7a4w
func cloudinaryForeign(ctx context.Context, out io.Writer, disk *filesystem.Disk, fake *fakes.Cloudinary) error {
	fake.Seed("raw", "authenticated", "brand/terms.txt", []byte("terms"))
	fake.Seed("raw", "authenticated", "brand/My Logo.txt", []byte("logo"))
	fake.Seed("image", "authenticated", "brand/logo", pngHeader)
	page, err := disk.ListPage(ctx, "brand", filesystem.ListOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "brand/ holds 3 resources, the driver lists: %s\n", keysOf(page))
	return nil
}

// cloudinaryRefusals shows the capabilities of each delivery type and
// what the Disk refuses because Cloudinary cannot do it.
// ds:def id=cloudinaryrefusals-vcaxnkhw
func cloudinaryRefusals(ctx context.Context, out io.Writer, public, private *filesystem.Disk) error {
	fmt.Fprintf(out, "upload type capabilities: %s\n", public.Capabilities())
	fmt.Fprintf(out, "authenticated type capabilities: %s\n", private.Capabilities())
	_, err := private.PublicURL(ctx, "brand/terms.txt")
	fmt.Fprintf(out, "authenticated PublicURL: %s\n", outcome(err))
	_, err = private.SignedURL(ctx, "brand/terms.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: cloudinary.DownloadExpiry})
	fmt.Fprintf(out, "SignedURL: %s\n", outcome(err))
	_, err = public.WriteBytes(ctx, "x.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPrivate})
	fmt.Fprintf(out, "private write on the upload type: %s\n", outcome(err))
	_, err = private.WriteBytes(ctx, "x.txt", []byte("x"), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "IfNotExists: %s\n", outcome(err))
	return nil
}

// cloudinaryErrors shows how failures come back: a wrong secret, the Admin
// API's hourly limit (420) with its Retry-After, and a body over the
// plan's raw-upload limit.
// ds:def id=cloudinaryerrors-2xs8hhxw
func cloudinaryErrors(ctx context.Context, out io.Writer, disk *filesystem.Disk, fake *fakes.Cloudinary) error {
	wrong, err := openCloudinary("not-the-secret", cloudinary.TypeAuthenticated, fake.URL)
	if err != nil {
		return err
	}
	_, err = wrong.WriteBytes(ctx, "a.txt", []byte("a"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "wrong secret: %s\n", outcome(err))
	fake.FailNext(fakes.Failure{Status: statusRateLimited, Code: "Rate Limit Exceeded", RetryAfter: int(retryHint.Seconds())})
	_, err = disk.Stat(ctx, "brand/terms.txt")
	fmt.Fprintf(out, "Admin API limit: %s\n", outcome(err))
	fake.MaxBytes = cloudUploadSize
	_, err = disk.WriteBytes(ctx, "big.bin", make([]byte, cloudUploadSize+1), filesystem.WriteOptions{})
	fmt.Fprintf(out, "over the raw-upload limit: %s\n", outcome(err))
	return nil
}

// keysOf lists a page's keys, comma-separated.
func keysOf(p filesystem.Page) string {
	keys := make([]string, len(p.Entries))
	for i, e := range p.Entries {
		keys[i] = e.Key
	}
	return strings.Join(keys, ", ")
}
