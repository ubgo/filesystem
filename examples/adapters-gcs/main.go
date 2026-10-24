// Command adapters-gcs walks through the Google Cloud Storage driver:
// opening a disk from a Config and from a DSN, per-object ACLs on a
// fine-grained bucket, V4 signed URLs from a service account key, stored
// headers (a gzip object read back exactly as stored), the capabilities
// GCS does not have, and (in features.go) the object-store features the
// fake can model.
//
// It runs against an in-process fake GCS (fake-gcs-server), so it needs no
// project and no network. Against real GCS the client comes from
// Application Default Credentials instead.
//
//	go run ./adapters-gcs            (from the examples directory)
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"hash/crc32"
	"io"
	"net/url"
	"os"
	"time"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
)

// bucketName is the bucket every disk in this program uses.
const bucketName = "media"

// serviceAccount is the signing identity of the example. On real GCS it is
// the client_email of the service account whose key signs URLs.
const serviceAccount = "uploader@example-project.iam.gserviceaccount.com"

// rsaBits is the size of the throwaway signing key; service account keys
// are 2048-bit RSA.
const rsaBits = 2048

// linkLifetime is how long the signed URLs in this program stay valid.
const linkLifetime = 10 * time.Minute

func main() {
	srv, err := startFakeGCS()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer srv.Stop()
	if err := run(context.Background(), os.Stdout, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// startFakeGCS serves an in-memory GCS API on a local port with the bucket
// already created.
func startFakeGCS() (*fakestorage.Server, error) {
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{Scheme: "http"})
	if err != nil {
		return nil, err
	}
	srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: bucketName})
	return srv, nil
}

// run is main without the process: every step prints what it saw to out.
func run(ctx context.Context, out io.Writer, srv *fakestorage.Server) error {
	key, err := signingKey()
	if err != nil {
		return err
	}
	disk, err := openWithConfig(srv.Client(), key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "capabilities: %s\n", disk.Capabilities())
	steps := []func(context.Context, io.Writer, *filesystem.Disk) error{
		publicAndPrivate, signedLinks, gzipAsStored, noMultipart,
	}
	for _, step := range steps {
		if err := step(ctx, out, disk); err != nil {
			return err
		}
	}
	drv, err := gcs.New(gcs.Config{Client: srv.Client(), Bucket: bucketName})
	if err != nil {
		return err
	}
	if err := checksums(ctx, out, disk, drv); err != nil {
		return err
	}
	viaDSN, err := openWithDSN(ctx, srv.URL())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "via DSN: %s\n", viaDSN.Capabilities())
	signing := &storage.SignedURLOptions{GoogleAccessID: serviceAccount, PrivateKey: key}
	return runFeatures(ctx, out, fixture{srv: srv, signing: signing, disk: disk})
}

// signingKey makes a throwaway RSA key in the PEM form a service account
// JSON key file carries in its private_key field.
func signingKey() ([]byte, error) {
	k, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), nil
}

// openWithConfig builds the driver for a bucket with fine-grained access
// (per-object ACLs) and a signing key, so SetVisibility and Presign are
// both declared.
// ds:def id=openwithconfig-pquvryxh
func openWithConfig(client *storage.Client, privateKey []byte) (*filesystem.Disk, error) {
	drv, err := gcs.New(gcs.Config{
		Client:     client,
		Bucket:     bucketName,
		Visibility: gcs.VisibilityACL,
		Signing:    &storage.SignedURLOptions{GoogleAccessID: serviceAccount, PrivateKey: privateKey},
	})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("media", drv)
}

// openWithDSN opens the same bucket through a Mux, pointed at the fake
// with endpoint and no_auth. Against real GCS the DSN is just
// "gs://media" (plus acl and signing_key when wanted).
// ds:def id=openwithdsn-tfh28zpd
func openWithDSN(ctx context.Context, fakeURL string) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(gcs.Scheme, gcs.Opener); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set(gcs.ParamEndpoint, fakeURL+"/storage/v1/")
	q.Set(gcs.ParamNoAuth, "true")
	return mux.Open(ctx, "media-dsn", gcs.Scheme+"://"+bucketName+"?"+q.Encode())
}

// publicAndPrivate writes one public and one private object and reads the
// visibility back from each object's ACL, then makes the public one
// private.
// ds:def id=publicandprivate-jtfcpswk
func publicAndPrivate(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	if _, err := disk.WriteBytes(ctx, "avatars/u1.png", pngHeader, filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}); err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "invoices/42.pdf", []byte("%PDF-1.7"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	for _, k := range []string{"avatars/u1.png", "invoices/42.pdf"} {
		e, err := disk.Stat(ctx, k)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: %s, %s\n", e.Key, e.ContentType, e.Visibility)
	}
	if err := disk.SetVisibility(ctx, "avatars/u1.png", filesystem.VisibilityPrivate); err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "after SetVisibility: %s\n", e.Visibility)
	return nil
}

// pngHeader is the eight-byte PNG signature, enough for content-type
// detection.
var pngHeader = []byte("\x89PNG\r\n\x1a\n")

// signedLinks issues V4 signed URLs: a download forced to an attachment,
// and an upload bound to one content type.
// ds:def id=signedlinks-9xy3dhdu
func signedLinks(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	get, err := disk.SignedURL(ctx, "invoices/42.pdf", filesystem.PresignOptions{
		Method: filesystem.MethodGet, Expires: linkLifetime, ContentDisposition: "attachment",
	})
	if err != nil {
		return err
	}
	put, err := disk.SignedURL(ctx, "uploads/new.png", filesystem.PresignOptions{
		Method: filesystem.MethodPut, Expires: linkLifetime, ContentType: "image/png",
	})
	if err != nil {
		return err
	}
	u, err := url.Parse(get.URL)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed GET: algorithm=%s disposition=%s\n", u.Query().Get("X-Goog-Algorithm"), u.Query().Get("response-content-disposition"))
	fmt.Fprintf(out, "signed PUT: client must send Content-Type: %s\n", put.Header.Get("Content-Type"))
	return nil
}

// gzipAsStored writes a pre-compressed object with Content-Encoding gzip
// and reads it back: the driver returns the compressed bytes as stored
// rather than letting GCS decompress them on the way out.
// ds:def id=gzipasstored-ywtqnjxk
func gzipAsStored(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(bytes.Repeat([]byte("log line\n"), 100)); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	_, err := disk.WriteBytes(ctx, "logs/app.log.gz", buf.Bytes(), filesystem.WriteOptions{
		ContentType: "text/plain",
		Headers:     filesystem.HTTPHeaders{ContentEncoding: "gzip", CacheControl: "private, max-age=60"},
	})
	if err != nil {
		return err
	}
	got, err := disk.ReadAll(ctx, "logs/app.log.gz")
	if err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "logs/app.log.gz")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "gzip object: stored %d bytes, read %d, identical=%t, cache-control=%q\n", buf.Len(), len(got), bytes.Equal(got, buf.Bytes()), e.Headers.CacheControl)
	return nil
}

// noMultipart shows what GCS does not offer through this driver: multipart
// uploads (large bodies stream through Write as resumable uploads).
// DeletePrefix still works, listing and deleting key by key.
// ds:def id=nomultipart-mhyeku4p
func noMultipart(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.CreateMultipart(ctx, "big.bin", filesystem.WriteOptions{})
	fmt.Fprintf(out, "multipart: %s\n", filesystem.CodeOf(err))
	if err := disk.DeletePrefix(ctx, "avatars"); err != nil {
		return err
	}
	ok, err := disk.Exists(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "after DeletePrefix: exists=%t\n", ok)
	return nil
}

// checksums shows the two layers of integrity checking. The Disk hashes
// every body and refuses one whose SHA-256 differs from
// WriteOptions.SHA256 before anything is committed; GCS itself keeps a
// CRC32C (and an MD5 for non-composite objects), which drv.ObjectInfo
// reads without downloading the object, for comparing against a checksum
// the application already holds.
// ds:def id=checksums-yggargx8
func checksums(ctx context.Context, out io.Writer, disk *filesystem.Disk, drv *gcs.Driver) error {
	body := []byte("order 42: 3 items")
	sum := sha256.Sum256(body)
	e, err := disk.WriteBytes(ctx, "orders/42.txt", body, filesystem.WriteOptions{SHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "matching SHA-256: stored, entry sha256=%s...\n", e.SHA256[:12])

	wrong := sha256.Sum256([]byte("something else"))
	_, err = disk.WriteBytes(ctx, "orders/43.txt", body, filesystem.WriteOptions{SHA256: hex.EncodeToString(wrong[:])})
	stored, existsErr := disk.Exists(ctx, "orders/43.txt")
	if existsErr != nil {
		return existsErr
	}
	fmt.Fprintf(out, "wrong SHA-256: %s, stored=%t\n", filesystem.CodeOf(err), stored)

	info, err := drv.ObjectInfo(ctx, "orders/42.txt")
	if err != nil {
		return err
	}
	md := md5.Sum(body)
	crcOK := info.CRC32C == crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli))
	fmt.Fprintf(out, "GCS CRC32C matches local=%t, MD5 matches local=%t, generation set=%t\n", crcOK, bytes.Equal(info.MD5, md[:]), info.VersionID != "")
	return nil
}
