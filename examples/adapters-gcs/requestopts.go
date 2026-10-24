package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
)

// tenantKMSKey is the Cloud KMS key one tenant's rows name, read from the
// database at request time rather than derived from the object's key.
const tenantKMSKey = "projects/example-project/locations/eu/keyRings/tenants/cryptoKeys/acme"

// invoiceKey is the object the per-call options are bound to.
const invoiceKey = "invoices/42.pdf"

// writeInvoice writes one tenant's invoice with that tenant's KMS key and
// a colder class, chosen for this call. The options ride on the context
// bound to invoiceKey, so the same context reused for another key (a
// thumbnail, another tenant's file) never inherits them.
// ds:def id=writeinvoice-e3n7q5kw
func writeInvoice(ctx context.Context, disk *filesystem.Disk, pdf []byte) (context.Context, error) {
	ctx, err := gcs.WithRequestOptions(ctx, disk, invoiceKey, gcs.RequestOptions{
		KMSKeyName:   tenantKMSKey, // overrides KMSKeyFor and KMSKeyName for this key only
		StorageClass: gcs.ClassNearline,
	})
	if err != nil {
		return ctx, err
	}
	_, err = disk.WriteBytes(ctx, invoiceKey, pdf, filesystem.WriteOptions{ContentType: "application/pdf"})
	return ctx, err
}

// requestOptions writes an invoice with per-call options, writes another
// key with the same context, presigns a PUT for the bound key, and shows
// an unknown class refused before anything is written.
// ds:def id=requestoptions-f8w2m6tz
func requestOptions(ctx context.Context, out io.Writer, f fixture) error {
	_, disk, err := newBucketDisk(f, "tenant-files", false, gcs.Config{Signing: f.signing})
	if err != nil {
		return err
	}
	bound, err := writeInvoice(ctx, disk, []byte("%PDF-1.7 invoice"))
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(bound, "thumbs/42.png", []byte("png"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	for _, key := range []string{invoiceKey, "thumbs/42.png"} {
		e, err := disk.Stat(ctx, key)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "per call: %s stored as %s\n", key, e.StorageClass)
	}
	put, err := disk.SignedURL(bound, invoiceKey, filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: linkLifetime})
	if err != nil {
		return err
	}
	u, err := url.Parse(put.URL)
	if err != nil {
		return err
	}
	signed := u.Query().Get("X-Goog-SignedHeaders")
	fmt.Fprintf(out, "per call: presigned PUT signs the key %t and the class %t, client sends %s\n",
		strings.Contains(signed, "x-goog-encryption-kms-key-name"), strings.Contains(signed, "x-goog-storage-class"),
		put.Header.Get("X-Goog-Encryption-Kms-Key-Name"))
	bad, err := gcs.WithRequestOptions(ctx, disk, "a.txt", gcs.RequestOptions{StorageClass: "LUKEWARM"})
	if err != nil {
		return err
	}
	_, err = disk.WriteBytes(bad, "a.txt", []byte("x"), filesystem.WriteOptions{})
	exists, _ := disk.Exists(ctx, "a.txt")
	fmt.Fprintf(out, "per call: unknown class %s, stored %t\n", filesystem.CodeOf(err), exists)
	return nil
}
