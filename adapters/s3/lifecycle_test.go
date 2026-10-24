package s3_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

const fakePath = "/test-bucket/"

// withOpts is fss3.WithRequestOptions for a key the test knows is valid.
func withOpts(t *testing.T, ctx context.Context, d *filesystem.Disk, key string, o fss3.RequestOptions) context.Context {
	t.Helper()
	ctx, err := fss3.WithRequestOptions(ctx, d, key, o)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// TestTags proves Tags/SetTags round-trip, that tags set on write through
// RequestOptions are stored, that a copy keeps or replaces them, and that
// a disk without Config.Tagging refuses both.
func TestTags(t *testing.T) {
	ctx := t.Context()
	d, _ := newDisk(t, fss3.Config{Tagging: true})
	if !d.Capabilities().Tags {
		t.Fatal("Tags not declared")
	}
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetTags(ctx, "a.txt", map[string]string{"team": "billing", "tier": "hot"}); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Tags(ctx, "a.txt"); err != nil || !maps.Equal(got, map[string]string{"team": "billing", "tier": "hot"}) {
		t.Fatalf("Tags = %v, %v", got, err)
	}
	if err := d.SetTags(ctx, "a.txt", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Tags(ctx, "a.txt"); err != nil || len(got) != 0 {
		t.Fatalf("cleared Tags = %v, %v", got, err)
	}
	_, err := d.Tags(ctx, "missing.txt")
	wantCode(t, err, filesystem.CodeNotFound)

	tagged := withOpts(t, ctx, d, "b.txt", fss3.RequestOptions{Tags: map[string]string{"owner": "ops"}})
	if _, err := d.WriteBytes(tagged, "b.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.Tags(ctx, "b.txt"); got["owner"] != "ops" {
		t.Errorf("write tags = %v", got)
	}
	if _, err := d.Copy(ctx, "b.txt", "c.txt"); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.Tags(ctx, "c.txt"); got["owner"] != "ops" {
		t.Errorf("copy kept tags = %v", got)
	}
	if _, err := d.Copy(withOpts(t, ctx, d, "d.txt", fss3.RequestOptions{Tags: map[string]string{"owner": "dev"}}), "b.txt", "d.txt"); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.Tags(ctx, "d.txt"); !maps.Equal(got, map[string]string{"owner": "dev"}) {
		t.Errorf("copy replaced tags = %v", got)
	}

	plain, _ := newDisk(t, fss3.Config{})
	_, err = plain.WriteBytes(withOpts(t, ctx, plain, "e.txt", fss3.RequestOptions{Tags: map[string]string{"owner": "ops"}}), "e.txt", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeUnsupported)
	_, err = plain.Tags(ctx, "e.txt")
	wantCode(t, err, filesystem.CodeUnsupported)
}

// TestObjectLock proves legal holds and retention: Hold reads them, a held
// object refuses Delete and DeleteKeys with CodePermission while the rest
// is deleted, lifting the hold allows the delete, and a write gets the
// disk's default retention or a per-write hold.
func TestObjectLock(t *testing.T) {
	ctx := t.Context()
	client, bucket, p := fakeS3WithProxy(t)
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: bucket, ObjectLock: true})
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, drv.CheckBucket(ctx), filesystem.CodeInvalidArgument)
	p.lockEnable()
	if err := drv.CheckBucket(ctx); err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("s3", drv)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"held.txt", "free.txt"} {
		if _, err := d.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.SetHold(ctx, "held.txt", filesystem.Hold{Legal: true}); err != nil {
		t.Fatal(err)
	}
	if h, err := d.Hold(ctx, "held.txt"); err != nil || !h.Legal || !h.RetainUntil.IsZero() {
		t.Fatalf("Hold = %+v, %v", h, err)
	}
	wantCode(t, d.Delete(ctx, "held.txt"), filesystem.CodePermission)
	if ok, _ := d.Exists(ctx, "held.txt"); !ok {
		t.Fatal("held object was deleted")
	}
	var be *filesystem.BatchError
	if err := d.DeleteKeys(ctx, []string{"held.txt", "free.txt"}); !errors.As(err, &be) || len(be.Failures) != 1 || be.Failures[0].Key != "held.txt" || filesystem.CodeOf(be.Failures[0].Err) != filesystem.CodePermission {
		t.Fatalf("DeleteKeys = %v", err)
	}
	if ok, _ := d.Exists(ctx, "free.txt"); ok {
		t.Error("the free key was not deleted")
	}

	until := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := d.SetHold(ctx, "held.txt", filesystem.Hold{RetainUntil: until}); err != nil {
		t.Fatal(err)
	}
	if h, _ := d.Hold(ctx, "held.txt"); h.Legal || !h.RetainUntil.Equal(until) {
		t.Fatalf("Hold after retention = %+v", h)
	}
	wantCode(t, d.Delete(ctx, "held.txt"), filesystem.CodePermission)
	// Lifting a governance retention sends the bypass header.
	if err := d.SetHold(ctx, "held.txt", filesystem.Hold{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "held.txt"); err != nil {
		t.Fatalf("Delete after the hold was lifted: %v", err)
	}

	locked, err := fss3.New(fss3.Config{Client: client, Bucket: bucket, ObjectLock: true, ObjectLockMode: types.ObjectLockModeCompliance, ObjectLockDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	ld, _ := filesystem.NewDisk("locked", locked)
	// A plain write goes through the transfer manager, one with a SHA-256
	// through the verified PutObject: both get the retention.
	for i, opts := range []filesystem.WriteOptions{{}, {SHA256: sha256Hex("x")}} {
		key := []string{"worm.txt", "worm2.txt"}[i]
		if _, err := ld.WriteBytes(ctx, key, []byte("x"), opts); err != nil {
			t.Fatal(err)
		}
		puts := p.sent(http.MethodPut, "x-id=PutObject")
		last := puts[len(puts)-1].header
		if last.Get("X-Amz-Object-Lock-Mode") != "COMPLIANCE" || !strings.HasPrefix(last.Get("X-Amz-Object-Lock-Retain-Until-Date"), time.Now().AddDate(0, 0, 30).UTC().Format("2006-01-02")) {
			t.Fatalf("write (SHA-256 %t) lock headers = %v", opts.SHA256 != "", last)
		}
	}
	// COMPLIANCE cannot be lifted, by anyone.
	wantCode(t, ld.SetHold(ctx, "worm.txt", filesystem.Hold{}), filesystem.CodePermission)
	wantCode(t, ld.Delete(ctx, "worm.txt"), filesystem.CodePermission)

	if _, err := d.WriteBytes(withOpts(t, ctx, d, "case.txt", fss3.RequestOptions{LegalHold: true}), "case.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if h, _ := d.Hold(ctx, "case.txt"); !h.Legal {
		t.Error("per-write legal hold not set")
	}

	plain, _ := newDisk(t, fss3.Config{})
	_, err = plain.WriteBytes(withOpts(t, ctx, plain, "x.txt", fss3.RequestOptions{LegalHold: true}), "x.txt", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeUnsupported)
	_, err = fss3.New(fss3.Config{Client: client, Bucket: bucket, ObjectLockDays: 1})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = fss3.New(fss3.Config{Client: client, Bucket: bucket, ObjectLock: true, ObjectLockMode: "FOREVER"})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestStorageClass proves the disk's class and a per-write override are
// sent, Stat reports the class, and Disk.SetStorageClass rewrites the
// object in place keeping its bytes.
func TestStorageClass(t *testing.T) {
	ctx := t.Context()
	client, bucket, p := fakeS3WithProxy(t)
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: bucket, StorageClass: types.StorageClassStandardIa, StorageClasses: true})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("s3", drv)
	if !d.Capabilities().StorageClass {
		t.Fatal("StorageClass not declared")
	}
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("hello"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if e, _ := d.Stat(ctx, "a.txt"); e.StorageClass != "STANDARD_IA" {
		t.Errorf("Stat class = %q", e.StorageClass)
	}
	if _, err := d.WriteBytes(withOpts(t, ctx, d, "b.txt", fss3.RequestOptions{StorageClass: types.StorageClassGlacierIr}), "b.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if e, _ := d.Stat(ctx, "b.txt"); e.StorageClass != "GLACIER_IR" {
		t.Errorf("per-write class = %q", e.StorageClass)
	}
	if err := d.SetStorageClass(ctx, "a.txt", "ONEZONE_IA"); err != nil {
		t.Fatal(err)
	}
	copies := p.sent(http.MethodPut, "x-id=CopyObject")
	if len(copies) != 1 || copies[0].path != fakePath+"a.txt" || copies[0].header.Get("X-Amz-Storage-Class") != "ONEZONE_IA" {
		t.Fatalf("SetStorageClass sent %+v", copies)
	}
	if e, _ := d.Stat(ctx, "a.txt"); e.StorageClass != "ONEZONE_IA" {
		t.Errorf("class after SetStorageClass = %q", e.StorageClass)
	}
	if b, err := d.ReadAll(ctx, "a.txt"); err != nil || string(b) != "hello" {
		t.Errorf("bytes after SetStorageClass = %q, %v", b, err)
	}
	wantCode(t, d.SetStorageClass(ctx, "missing.txt", "STANDARD"), filesystem.CodeNotFound)
}

// TestArchiveRestore proves a read of an unrestored archived object is
// CodeConflict with ErrArchived, Restore sends the configured tier (or
// the per-call one) and is idempotent, Stat reports the progress, and the
// object reads once the restore is done.
func TestArchiveRestore(t *testing.T) {
	ctx := t.Context()
	client, bucket, p := fakeS3WithProxy(t)
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: bucket, Restore: true, RestoreTier: types.TierBulk, StorageClass: types.StorageClassGlacier})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("s3", drv)
	if !d.Capabilities().Restore {
		t.Fatal("Restore not declared")
	}
	if _, err := d.WriteBytes(ctx, "cold.txt", []byte("frozen"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = d.ReadAll(ctx, "cold.txt")
	if filesystem.CodeOf(err) != filesystem.CodeConflict || !errors.Is(err, fss3.ErrArchived) {
		t.Fatalf("read of archived = %v", err)
	}
	if err := d.Restore(ctx, "cold.txt", filesystem.RestoreOptions{Days: 2}); err != nil {
		t.Fatal(err)
	}
	if p.tier(fakePath+"cold.txt") != "Bulk" {
		t.Errorf("tier = %q", p.tier(fakePath+"cold.txt"))
	}
	if err := d.Restore(ctx, "cold.txt", filesystem.RestoreOptions{Days: 2}); err != nil {
		t.Fatalf("second Restore while one runs: %v", err)
	}
	if e, _ := d.Stat(ctx, "cold.txt"); e.Metadata[fss3.MetaRestore] != fss3.RestoreOngoing {
		t.Errorf("Stat while restoring = %v", e.Metadata)
	}
	p.finishRestore(fakePath + "cold.txt")
	e, _ := d.Stat(ctx, "cold.txt")
	if e.Metadata[fss3.MetaRestore] != fss3.RestoreDone || e.Metadata[fss3.MetaRestoreExpiry] != "2040-12-21T00:00:00Z" {
		t.Errorf("Stat after restore = %v", e.Metadata)
	}
	if b, err := d.ReadAll(ctx, "cold.txt"); err != nil || string(b) != "frozen" {
		t.Fatalf("read after restore = %q, %v", b, err)
	}

	if _, err := d.WriteBytes(ctx, "cold2.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Restore(withOpts(t, ctx, d, "cold2.txt", fss3.RequestOptions{RestoreTier: types.TierExpedited}), "cold2.txt", filesystem.RestoreOptions{Days: 1}); err != nil {
		t.Fatal(err)
	}
	if p.tier(fakePath+"cold2.txt") != "Expedited" {
		t.Errorf("per-call tier = %q", p.tier(fakePath+"cold2.txt"))
	}
	_, err = fss3.New(fss3.Config{Client: client, Bucket: bucket, RestoreTier: "Instant"})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestStateKeysNotStored proves Write drops the driver's own MetaPrefix
// keys, so an Entry from Stat can be written back (a mirror does).
func TestStateKeysNotStored(t *testing.T) {
	ctx := t.Context()
	d, _ := newDisk(t, fss3.Config{ChecksumAlgorithm: types.ChecksumAlgorithmCrc32})
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{Metadata: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	e, _ := d.Stat(ctx, "a.txt")
	if _, err := d.WriteBytes(ctx, "b.txt", []byte("x"), filesystem.WriteOptions{Metadata: e.Metadata}); err != nil {
		t.Fatal(err)
	}
	got, _ := d.Stat(ctx, "b.txt")
	if _, ok := e.Metadata[fss3.MetaChecksumPrefix+"crc32"]; !ok {
		t.Fatalf("no state key to write back: %v", e.Metadata)
	}
	if got.Metadata["k"] != "v" || len(got.Metadata) != 2 {
		t.Errorf("metadata = %v", got.Metadata)
	}
}

// lockedDisk is a disk with Object Lock (and versioning, so RestoreVersion
// works) on a fresh fake bucket.
func lockedDisk(t *testing.T, cfg fss3.Config) (*filesystem.Disk, *s3Proxy) {
	t.Helper()
	client, bucket, p := fakeS3WithProxy(t)
	p.lockEnable()
	if _, err := client.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{Bucket: aws.String(bucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		t.Fatal(err)
	}
	cfg.Client, cfg.Bucket, cfg.ObjectLock, cfg.Versioning, cfg.StorageClasses = client, bucket, true, true, true
	drv, err := fss3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("locked", drv)
	if err != nil {
		t.Fatal(err)
	}
	return d, p
}

// TestHeldObjectIsNotOverwritten proves a held object refuses every path
// that would make a new, unprotected version current: Write (both upload
// paths), Copy and Move onto it, SetStorageClass, RestoreVersion and a
// multipart upload, while the stored bytes and the hold stay.
func TestHeldObjectIsNotOverwritten(t *testing.T) {
	ctx := t.Context()
	d, _ := lockedDisk(t, fss3.Config{})
	for _, k := range []string{"h.txt", "src.txt"} {
		if _, err := d.WriteBytes(ctx, k, []byte(k), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.WriteBytes(ctx, "h.txt", []byte("v2"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	vs, _ := d.Versions(ctx, "h.txt")
	if err := d.SetHold(ctx, "h.txt", filesystem.Hold{Legal: true}); err != nil {
		t.Fatal(err)
	}
	attempts := map[string]func() error{
		"write": func() error {
			_, err := d.WriteBytes(ctx, "h.txt", []byte("new"), filesystem.WriteOptions{})
			return err
		},
		"verified write": func() error {
			_, err := d.WriteBytes(ctx, "h.txt", []byte("new"), filesystem.WriteOptions{SHA256: sha256Hex("new")})
			return err
		},
		"copy onto":         func() error { _, err := d.Copy(ctx, "src.txt", "h.txt"); return err },
		"move onto":         func() error { _, err := d.Move(ctx, "src.txt", "h.txt"); return err },
		"set storage class": func() error { return d.SetStorageClass(ctx, "h.txt", "STANDARD_IA") },
		"restore version":   func() error { return d.RestoreVersion(ctx, "h.txt", vs[1].ID) },
		"multipart":         func() error { _, err := d.CreateMultipart(ctx, "h.txt", filesystem.WriteOptions{}); return err },
	}
	for name, try := range attempts {
		if err := try(); filesystem.CodeOf(err) != filesystem.CodePermission {
			t.Errorf("%s of a held object = %v, want CodePermission", name, err)
		}
	}
	if b, _ := d.ReadAll(ctx, "h.txt"); string(b) != "v2" {
		t.Errorf("held object now reads %q", b)
	}
	if h, _ := d.Hold(ctx, "h.txt"); !h.Legal {
		t.Error("hold lost")
	}
	if ok, _ := d.Exists(ctx, "src.txt"); !ok {
		t.Error("a refused move deleted its source")
	}
	// A multipart upload started before the hold is refused at completion.
	if err := d.SetHold(ctx, "h.txt", filesystem.Hold{}); err != nil {
		t.Fatal(err)
	}
	id, err := d.CreateMultipart(ctx, "h.txt", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	part, err := d.UploadPart(ctx, "h.txt", id, 1, strings.NewReader("part"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetHold(ctx, "h.txt", filesystem.Hold{Legal: true}); err != nil {
		t.Fatal(err)
	}
	_, err = d.CompleteMultipart(ctx, "h.txt", id, []filesystem.Part{part})
	wantCode(t, err, filesystem.CodePermission)
}

// TestRequestOptionsBoundToKey proves options attached for one key reach
// only that exact key: not a staging file, not the same name in another
// directory, and through a scoped disk only when given the driver key.
func TestRequestOptionsBoundToKey(t *testing.T) {
	ctx := t.Context()
	d, _ := lockedDisk(t, fss3.Config{Tagging: true})
	hold := withOpts(t, ctx, d, "contract.pdf", fss3.RequestOptions{LegalHold: true, Tags: map[string]string{"case": "1"}})
	if _, err := d.WriteBytes(hold, ".staging/tmp1", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if h, _ := d.Hold(ctx, ".staging/tmp1"); h.Legal {
		t.Error("an unrelated write inherited the legal hold")
	}
	if tags, _ := d.Tags(ctx, ".staging/tmp1"); len(tags) != 0 {
		t.Errorf("an unrelated write inherited tags %v", tags)
	}
	if _, err := d.WriteBytes(hold, "other/contract.pdf", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if h, _ := d.Hold(ctx, "other/contract.pdf"); h.Legal {
		t.Error("the same name in another directory inherited the legal hold")
	}
	tenant, err := d.Scope("tenant", "tenants/acme")
	if err != nil {
		t.Fatal(err)
	}
	// The caller names the key as the scoped disk knows it; the helper
	// resolves the prefix.
	scoped := withOpts(t, ctx, tenant, "contract.pdf", fss3.RequestOptions{LegalHold: true})
	_, err = fss3.WithRequestOptions(ctx, tenant, "../escape", fss3.RequestOptions{LegalHold: true})
	wantCode(t, err, filesystem.CodeInvalidKey)
	// The same name unscoped, and another name in the scope, written with
	// the same context, get nothing.
	if _, err := d.WriteBytes(scoped, "contract.pdf", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.WriteBytes(scoped, "draft.pdf", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.WriteBytes(scoped, "contract.pdf", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if h, _ := tenant.Hold(ctx, "contract.pdf"); !h.Legal {
		t.Error("the options did not reach their key through a scoped disk")
	}
	for _, k := range []struct {
		disk *filesystem.Disk
		key  string
	}{{d, "contract.pdf"}, {tenant, "draft.pdf"}} {
		if h, _ := k.disk.Hold(ctx, k.key); h.Legal {
			t.Errorf("%s on %s inherited the legal hold", k.key, k.disk.Name())
		}
	}
	// The error of a refused delete names the caller's key, not the prefix.
	err = tenant.Delete(ctx, "contract.pdf")
	wantCode(t, err, filesystem.CodePermission)
	if strings.Contains(err.Error(), "tenants/acme") {
		t.Errorf("error leaks the scope's prefix: %v", err)
	}
}

// TestMaxRetention proves retention beyond the disk's cap is refused on
// every path that sets one, and that the default cap is applied.
func TestMaxRetention(t *testing.T) {
	ctx := t.Context()
	d, _ := lockedDisk(t, fss3.Config{MaxRetention: 48 * time.Hour})
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	far := time.Now().Add(72 * time.Hour)
	wantCode(t, d.SetHold(ctx, "a.txt", filesystem.Hold{RetainUntil: far}), filesystem.CodeInvalidArgument)
	if h, _ := d.Hold(ctx, "a.txt"); h.Legal || !h.RetainUntil.IsZero() {
		t.Errorf("a refused SetHold changed the hold: %+v", h)
	}
	_, err := d.WriteBytes(withOpts(t, ctx, d, "b.txt", fss3.RequestOptions{RetainUntil: far}), "b.txt", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if err := d.SetHold(ctx, "a.txt", filesystem.Hold{RetainUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("retention within the cap: %v", err)
	}
	client, bucket, _ := fakeS3WithProxy(t)
	_, err = fss3.New(fss3.Config{Client: client, Bucket: bucket, ObjectLock: true, ObjectLockDays: 3, MaxRetention: 48 * time.Hour})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = fss3.New(fss3.Config{Client: client, Bucket: bucket, ObjectLock: true, ObjectLockDays: 11 * 365})
	wantCode(t, err, filesystem.CodeInvalidArgument) // DefaultMaxRetention
	_, err = fss3.New(fss3.Config{Client: client, Bucket: bucket, MaxRetention: -time.Hour})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestHoldChecksRunInParallel proves DeleteKeys on an Object Lock disk
// checks the keys' holds concurrently rather than one HEAD at a time.
func TestHoldChecksRunInParallel(t *testing.T) {
	ctx := t.Context()
	d, p := lockedDisk(t, fss3.Config{})
	keys := make([]string, 32)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%02d", i)
		if _, err := d.WriteBytes(ctx, keys[i], []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	p.slowHeads(20 * time.Millisecond)
	if err := d.DeleteKeys(ctx, keys); err != nil {
		t.Fatal(err)
	}
	if got := p.maxConcurrentHeads(); got < 2 {
		t.Errorf("at most %d HEAD at a time", got)
	}
}

// TestInvalidStorageClass proves S3's InvalidStorageClass is
// CodeInvalidArgument.
func TestInvalidStorageClass(t *testing.T) {
	ctx := t.Context()
	d, p := newDisk(t, fss3.Config{StorageClasses: true})
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	p.fail(http.MethodPut, "CopyObject", http.StatusBadRequest, "InvalidStorageClass")
	wantCode(t, d.SetStorageClass(ctx, "a.txt", "NOPE"), filesystem.CodeInvalidArgument)
}
