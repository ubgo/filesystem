package s3_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestChecksumSent proves a write with a SHA-256 and a known size is one
// PutObject whose signed payload hash and x-amz-checksum-sha256 are that
// digest, and that a write of unknown size goes through the transfer
// manager without them.
func TestChecksumSent(t *testing.T) {
	ctx := t.Context()
	d, p := newDisk(t, fss3.Config{})
	if !d.Capabilities().ChecksumSHA256 {
		t.Fatal("ChecksumSHA256 not declared")
	}
	sum := sha256Hex("hello")
	raw, _ := hex.DecodeString(sum)
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("hello"), filesystem.WriteOptions{SHA256: sum}); err != nil {
		t.Fatal(err)
	}
	puts := p.sent(http.MethodPut, "x-id=PutObject")
	if len(puts) != 1 || puts[0].checksum != base64.StdEncoding.EncodeToString(raw) || puts[0].contentSHA != sum {
		t.Fatalf("PutObject sent %+v", puts)
	}
	if b, err := d.ReadAll(ctx, "a.txt"); err != nil || string(b) != "hello" {
		t.Errorf("stored = %q, %v", b, err)
	}
	// No size known in advance: the transfer manager, without the digest.
	if _, err := d.Write(ctx, "b.txt", io.MultiReader(strings.NewReader("hello")), filesystem.WriteOptions{SHA256: sum}); err != nil {
		t.Fatal(err)
	}
	for _, r := range p.sent(http.MethodPut, "x-id=PutObject") {
		if strings.HasSuffix(r.path, "/b.txt") && r.checksum != "" {
			t.Errorf("unknown-size write sent a whole-object checksum: %+v", r)
		}
	}
}

// TestChecksumMalformed calls the driver directly (the Disk would refuse
// first) with a digest that is not hex. gofakes3 checks no checksums, so
// a mismatching body is tested against real stores
// (TestChecksumAgainstARealStore).
func TestChecksumMalformed(t *testing.T) {
	client, bucket, _ := fakeS3WithProxy(t)
	direct, err := fss3.New(fss3.Config{Client: client, Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := direct.Write(t.Context(), "c.txt", strings.NewReader("hello"), filesystem.WriteOptions{SHA256: "not-hex", SizeHint: 5}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("malformed SHA-256 = %v", err)
	}
}

// TestChecksumWriteOptions proves the verified PUT keeps every other write
// option: type, stored headers, and a conditional write that S3 refuses
// the second time.
func TestChecksumWriteOptions(t *testing.T) {
	ctx := t.Context()
	d, _ := newDisk(t, fss3.Config{ConditionalWrites: true})
	opts := filesystem.WriteOptions{SHA256: sha256Hex("x"), ContentType: "text/plain", Headers: filesystem.HTTPHeaders{CacheControl: "no-store"}, IfNotExists: true, Metadata: map[string]string{"k": "v"}}
	if _, err := d.WriteBytes(ctx, "k.txt", []byte("x"), opts); err != nil {
		t.Fatal(err)
	}
	e, err := d.Stat(ctx, "k.txt")
	if err != nil || e.ContentType != "text/plain" || e.Headers.CacheControl != "no-store" || e.Metadata["k"] != "v" {
		t.Errorf("stored = %+v, %v", e, err)
	}
	_, err = d.WriteBytes(ctx, "k.txt", []byte("x"), opts)
	wantCode(t, err, filesystem.CodeExists)
}
