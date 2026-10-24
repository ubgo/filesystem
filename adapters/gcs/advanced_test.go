package gcs_test

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
)

// advanced returns a driver and its disk on a fresh bucket of a proxied
// fake, with cfg's Client and Bucket filled in.
func advanced(t *testing.T, cfg gcs.Config, versioned bool) (*gcs.Driver, *filesystem.Disk, *proxy) {
	t.Helper()
	client, fake, p, _ := proxied(t)
	cfg.Client, cfg.Bucket = client, "adv"
	fake.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: cfg.Bucket, VersioningEnabled: versioned})
	drv, err := gcs.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("gcs", drv)
	if err != nil {
		t.Fatal(err)
	}
	return drv, d, p
}

func testKey(t *testing.T) gcs.CustomerKey {
	t.Helper()
	k, err := gcs.NewCustomerKey(bytes.Repeat([]byte{0xAB}, gcs.CustomerKeySize))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestCustomerKeyIsRedacted(t *testing.T) {
	k := testKey(t)
	cfg := gcs.Config{Bucket: "b", CustomerKey: k}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("cfg", "key", k, "cfg", cfg)
	asJSON, err := json.Marshal(struct{ Key gcs.CustomerKey }{k})
	if err != nil {
		t.Fatal(err)
	}
	printed := fmt.Sprintf("%v %+v %#v %s %x %q", k, cfg, cfg, k, k, k) + logged.String() + string(asJSON)
	for _, leak := range []string{"abab", "ABAB", "\xab\xab", "171, 171", "q6ur"} {
		if strings.Contains(printed, leak) {
			t.Fatalf("key bytes leaked (%q) in %s", leak, printed)
		}
	}
	if _, err := gcs.NewCustomerKey(make([]byte, 16)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a 16-byte key = %v, want CodeInvalidArgument", err)
	}
}

func TestCustomerKeyOnEveryObjectCall(t *testing.T) {
	ctx := t.Context()
	k := testKey(t)
	signing := &storage.SignedURLOptions{GoogleAccessID: "x@y", PrivateKey: pemKey(t)}
	drv, d, p := advanced(t, gcs.Config{CustomerKey: k, Signing: signing}, false)
	if c := drv.Capabilities(); c.Presign || c.PresignPost || c.PresignHeaders {
		t.Fatalf("a CSEK disk declares signed URLs: %+v", c)
	}
	if _, err := d.WriteBytes(ctx, "secret.txt", []byte("hush"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(ctx, "secret.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadAll(ctx, "secret.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Copy(ctx, "secret.txt", "copy.txt"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bytes.Repeat([]byte{0xAB}, gcs.CustomerKeySize))
	want := base64.StdEncoding.EncodeToString(sum[:])
	p.mu.Lock()
	defer p.mu.Unlock()
	checked := 0
	for i, u := range p.urls {
		if strings.HasPrefix(u, http.MethodDelete) {
			continue
		}
		if got := p.headers[i].Get("X-Goog-Encryption-Key-Sha256"); got != want {
			t.Errorf("%s sent key hash %q, want %q", u, got, want)
		}
		if strings.Contains(u, "/rewriteTo/") && p.headers[i].Get("X-Goog-Copy-Source-Encryption-Key-Sha256") != want {
			t.Errorf("copy did not present the source's key: %v", p.headers[i])
		}
		checked++
	}
	// Write, Stat, Read and Copy.
	if checked != 4 {
		t.Fatalf("only %d requests checked: %v", checked, p.urls)
	}
	if _, err := gcs.New(gcs.Config{Client: drv.Native(), Bucket: "b", CustomerKey: k, KMSKeyName: "projects/p/locations/l/keyRings/r/cryptoKeys/c"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("CustomerKey with KMSKeyName = %v, want CodeInvalidArgument", err)
	}
}

func TestStorageClass(t *testing.T) {
	ctx := t.Context()
	drv, d, p := advanced(t, gcs.Config{StorageClass: gcs.ClassNearline}, false)
	body := []byte("cold data")
	if _, err := d.WriteBytes(ctx, "c.txt", body, filesystem.WriteOptions{Metadata: map[string]string{"owner": "ops"}}); err != nil {
		t.Fatal(err)
	}
	if e, err := d.Stat(ctx, "c.txt"); err != nil || e.StorageClass != string(gcs.ClassNearline) {
		t.Fatalf("class after write = %q (%v), want NEARLINE", e.StorageClass, err)
	}
	if err := d.SetStorageClass(ctx, "c.txt", string(gcs.ClassColdline)); err != nil {
		t.Fatal(err)
	}
	// The fake ignores a rewrite's class, so check what was sent: the class,
	// the metadata kept, and the generation condition.
	if len(p.sent("POST", "/rewriteTo/", "ifGenerationMatch=", "ifMetagenerationMatch=", "sourceGeneration=")) != 1 {
		t.Fatalf("rewrite not conditioned on the generation it read: %v", p.urls)
	}
	if b := strings.Join(p.bodies, ""); !strings.Contains(b, `"storageClass":"COLDLINE"`) || !strings.Contains(b, `"owner":"ops"`) {
		t.Fatalf("rewrite body %s", b)
	}
	if got, err := d.ReadAll(ctx, "c.txt"); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("after the rewrite: %q, %v", got, err)
	}
	p.fail(http.MethodPost, "/rewriteTo/", http.StatusPreconditionFailed)
	wantCode(t, d.SetStorageClass(ctx, "c.txt", string(gcs.ClassArchive)), filesystem.CodeConflict)
	if !drv.Capabilities().StorageClass {
		t.Fatal("StorageClass not declared")
	}
}

func TestHoldAndRetention(t *testing.T) {
	ctx := t.Context()
	drv, d, p := advanced(t, gcs.Config{RetainFor: time.Hour}, false)
	before := time.Now()
	if _, err := d.WriteBytes(ctx, "kept.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := drv.ObjectInfo(ctx, "kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.RetentionMode != gcs.RetentionUnlocked || info.Hold.RetainUntil.Before(before.Add(time.Hour-time.Minute)) {
		t.Fatalf("retention after a write with RetainFor: %+v", info)
	}
	until := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	if err := d.SetHold(ctx, "kept.txt", filesystem.Hold{Legal: true, RetainUntil: until}); err != nil {
		t.Fatal(err)
	}
	if b := strings.Join(p.bodies, ""); !strings.Contains(b, `"temporaryHold":true`) || !strings.Contains(b, `"mode":"Unlocked"`) {
		t.Fatalf("set hold sent %s", b)
	}
	if len(p.sent("PATCH", "overrideUnlockedRetention=true")) != 1 {
		t.Fatalf("a changed retention must carry the override: %v", p.urls)
	}
	h, err := d.Hold(ctx, "kept.txt")
	if err != nil || !h.RetainUntil.Equal(until) {
		t.Fatalf("hold = %+v, %v; want retention until %v", h, err, until)
	}
	// Releasing only the legal hold leaves the retention alone.
	p.bodies = nil
	if err := d.SetHold(ctx, "kept.txt", filesystem.Hold{RetainUntil: until}); err != nil {
		t.Fatal(err)
	}
	if b := strings.Join(p.bodies, ""); !strings.Contains(b, `"temporaryHold":false`) || strings.Contains(b, "retention") {
		t.Fatalf("release of the legal hold sent %s", b)
	}
	// GCS refuses to delete a held or retained object with 403.
	p.fail(http.MethodDelete, "kept.txt", http.StatusForbidden)
	wantCode(t, d.Delete(ctx, "kept.txt"), filesystem.CodePermission)
	wantCode(t, d.SetHold(ctx, "missing.txt", filesystem.Hold{Legal: true}), filesystem.CodeNotFound)
	if _, err := gcs.New(gcs.Config{Client: drv.Native(), Bucket: "b", RetentionMode: "Forever"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("unknown retention mode = %v", err)
	}
}

func TestVersions(t *testing.T) {
	ctx := t.Context()
	drv, d, _ := advanced(t, gcs.Config{Versioned: true}, true)
	for _, body := range []string{"v1", "v2"} {
		if _, err := d.WriteBytes(ctx, "doc.txt", []byte(body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.WriteBytes(ctx, "doc.txt.bak", []byte("other"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	vs, err := d.Versions(ctx, "doc.txt")
	if err != nil || len(vs) != 2 || !vs[0].Latest || vs[1].Latest {
		t.Fatalf("versions = %+v, %v; want two, newest first and latest", vs, err)
	}
	r, err := d.ReadVersion(ctx, "doc.txt", vs[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := io.ReadAll(r)
	_ = r.Close()
	if string(old) != "v1" {
		t.Fatalf("old version reads %q", old)
	}
	if err := d.RestoreVersion(ctx, "doc.txt", vs[1].ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.ReadAll(ctx, "doc.txt"); string(got) != "v1" {
		t.Fatalf("after restore the object reads %q", got)
	}
	if err := d.Delete(ctx, "doc.txt"); err != nil {
		t.Fatal(err)
	}
	vs, err = d.Versions(ctx, "doc.txt")
	if err != nil || len(vs) != 3 || vs[0].Latest {
		t.Fatalf("versions after delete = %+v, %v; want three noncurrent", vs, err)
	}
	if err := d.RestoreVersion(ctx, "doc.txt", vs[0].ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.ReadAll(ctx, "doc.txt"); string(got) != "v1" {
		t.Fatalf("a deleted key restored reads %q", got)
	}
	_, err = d.Versions(ctx, "missing.txt")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = d.ReadVersion(ctx, "doc.txt", "latest")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = d.ReadVersion(ctx, "doc.txt", "999999999")
	wantCode(t, err, filesystem.CodeNotFound)
	if !drv.Capabilities().VersionRead {
		t.Fatal("VersionRead not declared with Versioned")
	}

	_, plain, _ := advanced(t, gcs.Config{}, true)
	_, err = plain.Versions(ctx, "doc.txt")
	wantCode(t, err, filesystem.CodeUnsupported)
	_, err = plain.ReadVersion(ctx, "doc.txt", vs[0].ID)
	wantCode(t, err, filesystem.CodeUnsupported)
}

func TestChecksumsReported(t *testing.T) {
	ctx := t.Context()
	drv, d, _ := advanced(t, gcs.Config{}, false)
	body := []byte("checksummed bytes")
	if _, err := d.WriteBytes(ctx, "sum.bin", body, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := drv.ObjectInfo(ctx, "sum.bin")
	if err != nil {
		t.Fatal(err)
	}
	md := md5.Sum(body)
	if info.CRC32C != crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli)) || !bytes.Equal(info.MD5, md[:]) || info.VersionID == "" {
		t.Fatalf("object info %+v does not carry the body's checksums", info)
	}
	_, err = drv.ObjectInfo(ctx, "missing.bin")
	wantCode(t, err, filesystem.CodeNotFound)
}

func TestOpenerAdvancedParams(t *testing.T) {
	ctx := t.Context()
	_, fake, _, base := proxied(t)
	fake.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: "dsn", VersioningEnabled: true})
	t.Setenv("GCS_TEST_CSEK", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, gcs.CustomerKeySize)))
	mux := filesystem.NewMux()
	_ = mux.Register(gcs.Scheme, gcs.Opener)
	prefix := "gs://dsn?no_auth=true&endpoint=" + url.QueryEscape(base+"/storage/v1/")
	d, err := mux.Open(ctx, "gcs", prefix+"&storage_class=NEARLINE&versioned=true&customer_key_env=GCS_TEST_CSEK")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Capabilities().Versions {
		t.Fatal("versioned=true did not declare Versions")
	}
	if _, err := d.WriteBytes(ctx, "o.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	obj, err := fake.GetObject("dsn", "o.txt")
	if err != nil || obj.StorageClass != string(gcs.ClassNearline) {
		t.Fatalf("stored object %+v, %v; want class NEARLINE", obj.ObjectAttrs, err)
	}
	t.Setenv("GCS_TEST_SHORT", base64.StdEncoding.EncodeToString([]byte("short")))
	t.Setenv("GCS_TEST_JUNK", "***")
	for _, bad := range []string{"&customer_key_env=GCS_TEST_SHORT", "&customer_key_env=GCS_TEST_JUNK", "&versioned=sometimes"} {
		if _, err := mux.Open(ctx, "gcs", prefix+bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("Open(%s) = %v, want CodeInvalidArgument", bad, err)
		}
	}
}

// TestRetainForOnEveryWrite checks that every path creating an object
// sends Config.RetainFor's retention, and that signed uploads, which
// cannot pin it, are not offered.
func TestRetainForOnEveryWrite(t *testing.T) {
	ctx := t.Context()
	signing := &storage.SignedURLOptions{GoogleAccessID: "x@y", PrivateKey: pemKey(t)}
	drv, d, p := advanced(t, gcs.Config{RetainFor: time.Hour, Versioned: true, Signing: signing}, true)
	if c := drv.Capabilities(); c.Presign || c.PresignPost {
		t.Fatalf("a disk with RetainFor offers signed uploads: %+v", c)
	}
	plain, err := gcs.New(gcs.Config{Client: drv.Native(), Bucket: "adv"})
	if err != nil {
		t.Fatal(err)
	}
	plainDisk, _ := filesystem.NewDisk("plain", plain)
	if _, err := plainDisk.WriteBytes(ctx, "unretained.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("v1"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	vs, err := d.Versions(ctx, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"copy", func() error { _, err := d.Copy(ctx, "a.txt", "b.txt"); return err }},
		{"restore version", func() error { return d.RestoreVersion(ctx, "a.txt", vs[0].ID) }},
		{"storage class of an object without retention", func() error {
			return d.SetStorageClass(ctx, "unretained.txt", string(gcs.ClassColdline))
		}},
	} {
		p.mu.Lock()
		p.bodies = nil
		p.mu.Unlock()
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		p.mu.Lock()
		body := strings.Join(p.bodies, "")
		p.mu.Unlock()
		if !strings.Contains(body, `"retention":{`) || !strings.Contains(body, `"mode":"Unlocked"`) {
			t.Errorf("%s sent no retention: %s", step.name, body)
		}
	}
}
