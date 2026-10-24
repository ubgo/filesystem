package s3_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// TestWriteSettingsRefusedEverywhere proves every path that resolves a
// write's settings (SetStorageClass, CreateMultipart, Copy, a presigned
// PUT, a presigned POST and RestoreVersion) refuses RequestOptions the
// disk cannot honour before sending anything that writes, rather than
// writing the object without them: here tags on a disk without
// Config.Tagging.
func TestWriteSettingsRefusedEverywhere(t *testing.T) {
	ctx := t.Context()
	d, p := lockedDisk(t, fss3.Config{})
	if _, err := d.WriteBytes(ctx, "doc.txt", []byte("one"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	vs, err := d.Versions(ctx, "doc.txt")
	if err != nil || len(vs) != 1 {
		t.Fatalf("Versions = %+v, %v", vs, err)
	}
	tagged := fss3.RequestOptions{Tags: map[string]string{"team": "ops"}}
	src := withOpts(t, ctx, d, "doc.txt", tagged)
	dst := withOpts(t, ctx, d, "copy.txt", tagged)
	up := withOpts(t, ctx, d, "up.bin", tagged)
	before := len(p.sent(http.MethodPut, "")) + len(p.sent(http.MethodPost, ""))
	_, mpErr := d.CreateMultipart(up, "up.bin", filesystem.WriteOptions{})
	_, copyErr := d.Copy(dst, "doc.txt", "copy.txt")
	_, putErr := d.SignedURL(up, "up.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	_, postErr := d.PresignPost(up, filesystem.PostPolicy{Key: "up.bin", MaxSize: 1 << 20, Expires: time.Minute})
	for name, err := range map[string]error{
		"SetStorageClass": d.SetStorageClass(src, "doc.txt", "STANDARD_IA"),
		"CreateMultipart": mpErr,
		"Copy":            copyErr,
		"presigned PUT":   putErr,
		"presigned POST":  postErr,
		"RestoreVersion":  d.RestoreVersion(src, "doc.txt", vs[0].ID),
	} {
		if filesystem.CodeOf(err) != filesystem.CodeUnsupported || !strings.Contains(err.Error(), "Config.Tagging") {
			t.Errorf("%s = %v, want the Config.Tagging refusal", name, err)
		}
	}
	if after := len(p.sent(http.MethodPut, "")) + len(p.sent(http.MethodPost, "")); after != before {
		t.Errorf("refused calls still sent %d writes", after-before)
	}
}

// TestEmptyTagKeyRefused proves a tag with an empty key on a write is
// refused with CodeInvalidArgument, as Disk.SetTags refuses it, instead of
// being encoded into x-amz-tagging for S3 to reject or mangle.
func TestEmptyTagKeyRefused(t *testing.T) {
	ctx := t.Context()
	d, p := newDisk(t, fss3.Config{Tagging: true})
	bad := withOpts(t, ctx, d, "a.txt", fss3.RequestOptions{Tags: map[string]string{"": "x"}})
	_, err := d.WriteBytes(bad, "a.txt", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	for _, r := range p.sent(http.MethodPut, "") {
		if strings.HasSuffix(r.path, "/a.txt") {
			t.Errorf("refused write sent %+v", r)
		}
	}
}

// TestStatReportsEveryChecksumAlgorithm proves Stat reports the stored
// checksum of each algorithm a write can choose under its own metadata
// key, and that an algorithm the driver does not know reports nothing
// rather than an empty or wrong value.
func TestStatReportsEveryChecksumAlgorithm(t *testing.T) {
	ctx := t.Context()
	d, _ := newDisk(t, fss3.Config{})
	for _, alg := range []types.ChecksumAlgorithm{types.ChecksumAlgorithmCrc32, types.ChecksumAlgorithmCrc32c, types.ChecksumAlgorithmCrc64nvme, types.ChecksumAlgorithmSha1, types.ChecksumAlgorithmSha256} {
		key := "sum-" + strings.ToLower(string(alg)) + ".txt"
		octx := withOpts(t, ctx, d, key, fss3.RequestOptions{ChecksumAlgorithm: alg})
		if _, err := d.WriteBytes(octx, key, []byte("hello"), filesystem.WriteOptions{}); err != nil {
			t.Fatalf("%s: %v", alg, err)
		}
		e, err := d.Stat(octx, key)
		if err != nil || e.Metadata[fss3.MetaChecksumPrefix+strings.ToLower(string(alg))] == "" {
			t.Errorf("%s: Stat metadata = %v, %v", alg, e.Metadata, err)
		}
	}
	unknown := withOpts(t, ctx, d, "sum-crc32.txt", fss3.RequestOptions{ChecksumAlgorithm: "MD5"})
	e, err := d.Stat(unknown, "sum-crc32.txt")
	if err != nil {
		t.Fatal(err)
	}
	for k := range e.Metadata {
		if strings.HasPrefix(k, fss3.MetaChecksumPrefix) {
			t.Errorf("unknown algorithm reported %s = %q", k, e.Metadata[k])
		}
	}
}

// TestHoldFailures proves Hold, SetHold and the held-object checks in
// front of SetStorageClass and DeleteKeys pass S3's failures through with
// their codes, never reading a failed check as "not held".
func TestHoldFailures(t *testing.T) {
	ctx := t.Context()
	d, p := lockedDisk(t, fss3.Config{})
	_, err := d.Hold(ctx, "missing.txt")
	wantCode(t, err, filesystem.CodeNotFound)
	wantCode(t, d.SetHold(ctx, "missing.txt", filesystem.Hold{Legal: true}), filesystem.CodeNotFound)
	for _, k := range []string{"a.txt", "b.txt"} {
		if _, err := d.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// Clearing a retention reads the object first; that read failing is
	// the call's error.
	p.fail(http.MethodHead, "/a.txt", http.StatusServiceUnavailable, "ServiceUnavailable")
	wantCode(t, d.SetHold(ctx, "a.txt", filesystem.Hold{}), filesystem.CodeUnavailable)
	// A failed hold check refuses the rewrite instead of assuming no hold.
	p.fail(http.MethodHead, "/a.txt", http.StatusServiceUnavailable, "ServiceUnavailable")
	wantCode(t, d.SetStorageClass(ctx, "a.txt", "STANDARD_IA"), filesystem.CodeUnavailable)
	// In a batch delete, a key whose check fails for any reason other
	// than a hold fails the whole call, and nothing is deleted.
	p.fail(http.MethodHead, "/b.txt", http.StatusServiceUnavailable, "ServiceUnavailable")
	wantCode(t, d.DeleteKeys(ctx, []string{"a.txt", "b.txt"}), filesystem.CodeUnavailable)
	if len(p.sent(http.MethodPost, "delete")) != 0 {
		t.Error("DeleteObjects sent after a failed hold check")
	}
	for _, k := range []string{"a.txt", "b.txt"} {
		if ok, err := d.Exists(ctx, k); err != nil || !ok {
			t.Errorf("%s after the refused delete: %t, %v", k, ok, err)
		}
	}
	// Every key held: each is reported, and no DeleteObjects is sent.
	for _, k := range []string{"a.txt", "b.txt"} {
		if err := d.SetHold(ctx, k, filesystem.Hold{Legal: true}); err != nil {
			t.Fatal(err)
		}
	}
	var be *filesystem.BatchError
	if err := d.DeleteKeys(ctx, []string{"a.txt", "b.txt"}); !errors.As(err, &be) || len(be.Failures) != 2 {
		t.Fatalf("DeleteKeys of held keys = %v", err)
	}
	for _, f := range be.Failures {
		wantCode(t, f.Err, filesystem.CodePermission)
	}
	if len(p.sent(http.MethodPost, "delete")) != 0 {
		t.Error("DeleteObjects sent with every key held")
	}
}

// TestCheckBucketFailures proves CheckBucket reports a failed read of the
// bucket's versioning or Object Lock configuration as that failure, not as
// "not enabled" and not as success.
func TestCheckBucketFailures(t *testing.T) {
	ctx := t.Context()
	client, bucket, p := fakeS3WithProxy(t)
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: bucket, Versioning: true, ObjectLock: true})
	if err != nil {
		t.Fatal(err)
	}
	p.fail(http.MethodGet, "versioning", http.StatusForbidden, "AccessDenied")
	wantCode(t, drv.CheckBucket(ctx), filesystem.CodePermission)
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(bucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		t.Fatal(err)
	}
	p.lockEnable()
	p.fail(http.MethodGet, "object-lock", http.StatusForbidden, "AccessDenied")
	wantCode(t, drv.CheckBucket(ctx), filesystem.CodePermission)
	if err := drv.CheckBucket(ctx); err != nil {
		t.Errorf("CheckBucket once the reads succeed: %v", err)
	}
}

// TestVersionsFailureAndOrder proves a failed ListObjectVersions is the
// call's error, and that versions and delete markers, which S3 lists
// separately, merge newest first with the latest on top: here a write
// after a delete, so the latest is a version listed before an older
// delete marker.
func TestVersionsFailureAndOrder(t *testing.T) {
	ctx := t.Context()
	d, p := lockedDisk(t, fss3.Config{})
	p.fail(http.MethodGet, "versions", http.StatusServiceUnavailable, "ServiceUnavailable")
	_, err := d.Versions(ctx, "doc.txt")
	wantCode(t, err, filesystem.CodeUnavailable)

	for _, step := range []string{"one", "two", "", "three"} {
		if step == "" {
			err = d.Delete(ctx, "doc.txt")
		} else {
			_, err = d.WriteBytes(ctx, "doc.txt", []byte(step), filesystem.WriteOptions{})
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	vs, err := d.Versions(ctx, "doc.txt")
	if err != nil || len(vs) != 4 || !vs[0].Latest || vs[0].DeleteMarker || !vs[1].DeleteMarker || vs[1].Latest || vs[2].Latest || vs[3].Latest {
		t.Fatalf("Versions = %+v, %v", vs, err)
	}
	for i, want := range map[int]string{0: "three", 2: "two", 3: "one"} {
		rc, err := d.ReadVersion(ctx, "doc.txt", vs[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(b) != want {
			t.Errorf("version %d holds %q, want %q", i, b, want)
		}
	}
}

// failingConfig answers every request with S3's AccessDenied.
func failingConfig(t *testing.T) *s3.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeS3Error(w, http.StatusForbidden, "AccessDenied")
	}))
	t.Cleanup(srv.Close)
	return s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true, RetryMaxAttempts: 1,
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	})
}

// TestBucketConfigReadFailures proves Lifecycle, Replication and
// ObjectGrants report a refused read as CodePermission rather than as an
// empty configuration, which a caller could write back over the real one.
func TestBucketConfigReadFailures(t *testing.T) {
	ctx := t.Context()
	client := failingConfig(t)
	rules, err := fss3.Lifecycle(ctx, client, "b")
	if filesystem.CodeOf(err) != filesystem.CodePermission || rules != nil {
		t.Errorf("Lifecycle = %+v, %v", rules, err)
	}
	cfg, err := fss3.Replication(ctx, client, "b")
	if filesystem.CodeOf(err) != filesystem.CodePermission || cfg.Role != "" {
		t.Errorf("Replication = %+v, %v", cfg, err)
	}
	grants, err := fss3.ObjectGrants(ctx, client, "b", "docs/a.pdf")
	if filesystem.CodeOf(err) != filesystem.CodePermission || grants != nil {
		t.Errorf("ObjectGrants = %+v, %v", grants, err)
	}
}

// TestLifecycleReadBackRefusesOddRules covers the remaining shapes
// lifecycleFromS3 refuses: a status other than Enabled or Disabled, a rule
// whose two prefixes disagree, and an upload cleanup without days.
func TestLifecycleReadBackRefusesOddRules(t *testing.T) {
	ctx := t.Context()
	client, store := newConfigClient(t)
	for name, rule := range map[string]string{
		"status":         `<Rule><ID>odd</ID><Filter><Prefix>a/</Prefix></Filter><Status>Paused</Status><Expiration><Days>1</Days></Expiration></Rule>`,
		"two prefixes":   `<Rule><ID>odd</ID><Prefix>a/</Prefix><Filter><Prefix>b/</Prefix></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule>`,
		"cleanup no day": `<Rule><ID>odd</ID><Filter><Prefix>a/</Prefix></Filter><Status>Enabled</Status><AbortIncompleteMultipartUpload></AbortIncompleteMultipartUpload></Rule>`,
	} {
		store.mu.Lock()
		store.docs["/b?lifecycle"] = []byte(`<LifecycleConfiguration>` + rule + `</LifecycleConfiguration>`)
		store.mu.Unlock()
		rules, err := fss3.Lifecycle(ctx, client, "b")
		if filesystem.CodeOf(err) != filesystem.CodeUnsupported || !strings.Contains(err.Error(), `"odd"`) {
			t.Errorf("%s: read back %+v, %v", name, rules, err)
		}
	}
}

// TestReplicationReadBackRefusesOddRules covers the remaining shapes
// replicationFromS3 refuses (an unknown rule or delete-marker status, a
// destination without a bucket), and proves an answer without a
// configuration document reads as none.
func TestReplicationReadBackRefusesOddRules(t *testing.T) {
	ctx := t.Context()
	client, store := newConfigClient(t)
	const filter = `<Filter><Prefix></Prefix></Filter><Priority>1</Priority>`
	const dest = `<Destination><Bucket>arn:aws:s3:::d</Bucket></Destination>`
	const deletes = `<DeleteMarkerReplication><Status>Disabled</Status></DeleteMarkerReplication>`
	for name, rule := range map[string]string{
		"status":           `<Status>Paused</Status>` + filter + deletes + dest,
		"no bucket":        `<Status>Enabled</Status>` + filter + deletes + `<Destination><StorageClass>STANDARD</StorageClass></Destination>`,
		"delete status":    `<Status>Enabled</Status>` + filter + `<DeleteMarkerReplication><Status>Sometimes</Status></DeleteMarkerReplication>` + dest,
		"disabled deletes": `<Status>Disabled</Status>` + filter + deletes + dest,
	} {
		store.mu.Lock()
		store.docs["/b?replication"] = []byte(`<ReplicationConfiguration><Role>r</Role><Rule><ID>odd</ID>` + rule + `</Rule></ReplicationConfiguration>`)
		store.mu.Unlock()
		got, err := fss3.Replication(ctx, client, "b")
		if name == "disabled deletes" {
			if err != nil || len(got.Rules) != 1 || !got.Rules[0].Disabled || got.Rules[0].ReplicateDeletes {
				t.Errorf("%s: read back %+v, %v", name, got, err)
			}
			continue
		}
		if filesystem.CodeOf(err) != filesystem.CodeUnsupported || !strings.Contains(err.Error(), `"odd"`) {
			t.Errorf("%s: read back %+v, %v", name, got, err)
		}
	}
	store.mu.Lock()
	store.docs["/b?replication"] = []byte{}
	store.mu.Unlock()
	if got, err := fss3.Replication(ctx, client, "b"); err != nil || got.Role != "" || got.Rules != nil {
		t.Errorf("empty answer read back %+v, %v", got, err)
	}
}

// TestObjectACLWriteACPAndOddGrantees proves a WRITE_ACP grant travels
// as its own header, and that a stored grant naming two kinds of grantee
// at once is refused on read instead of being reduced to one of them.
func TestObjectACLWriteACPAndOddGrantees(t *testing.T) {
	ctx := t.Context()
	client, store := newConfigClient(t)
	acl := fss3.ObjectACL{Grants: []fss3.Grant{{Grantee: fss3.Grantee{ID: "admin"}, Permission: fss3.GrantWriteACP}}}
	if err := fss3.SetObjectACL(ctx, client, "b", "docs/w.pdf", acl); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	h := store.headers["/b/docs/w.pdf?acl"]
	store.docs["/b/docs/two.pdf?acl"] = []byte(`<AccessControlPolicy><AccessControlList>` +
		`<Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser"><ID>x</ID><URI>http://acs.amazonaws.com/groups/global/AllUsers</URI></Grantee><Permission>READ</Permission></Grant>` +
		`</AccessControlList></AccessControlPolicy>`)
	store.mu.Unlock()
	if got := h.Get("X-Amz-Grant-Write-Acp"); got != `id="admin"` {
		t.Errorf("X-Amz-Grant-Write-Acp = %q", got)
	}
	if g, err := fss3.ObjectGrants(ctx, client, "b", "docs/two.pdf"); filesystem.CodeOf(err) != filesystem.CodeUnsupported || g != nil {
		t.Errorf("grant with two grantees read back as %+v, %v", g, err)
	}
}

// failingSTS answers AssumeRole with a fixed output or error.
type failingSTS struct {
	out *sts.AssumeRoleOutput
	err error
}

func (f failingSTS) AssumeRole(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	return f.out, f.err
}

// TestDelegatePrefixFailures proves a refused AssumeRole is the call's
// error, and an answer missing any part of the credentials is an error
// rather than credentials that cannot sign.
func TestDelegatePrefixFailures(t *testing.T) {
	ctx := t.Context()
	o := fss3.DelegateOptions{Role: "arn:aws:iam::1:role/uploader", SessionName: "user-42", Bucket: "media", Prefix: "users/42", Duration: time.Hour}
	if _, err := fss3.DelegatePrefix(ctx, failingSTS{err: errors.New("sts down")}, o); err == nil || !strings.Contains(err.Error(), "sts down") {
		t.Errorf("refused AssumeRole = %v", err)
	}
	exp := time.Now().Add(time.Hour)
	for name, c := range map[string]*ststypes.Credentials{
		"none":      nil,
		"no key":    {SecretAccessKey: aws.String("s"), Expiration: &exp},
		"no secret": {AccessKeyId: aws.String("k"), Expiration: &exp},
		"no expiry": {AccessKeyId: aws.String("k"), SecretAccessKey: aws.String("s")},
	} {
		creds, err := fss3.DelegatePrefix(ctx, failingSTS{out: &sts.AssumeRoleOutput{Credentials: c}}, o)
		if filesystem.CodeOf(err) != filesystem.CodeUnknown || creds.AccessKeyID != "" {
			t.Errorf("%s: %+v, %v", name, creds, err)
		}
	}
}

// TestOpenerCustomerKeyNotBase64 proves an SSE-C variable that is not
// base64 is refused naming the variable, never echoing its value.
func TestOpenerCustomerKeyNotBase64(t *testing.T) {
	_, bucket, p := fakeS3WithProxy(t)
	t.Setenv("FS_TEST_AK", "test")
	t.Setenv("FS_TEST_SK", "test")
	t.Setenv("FS_TEST_SSEC_RAW", "not base64 at all!")
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		t.Fatal(err)
	}
	dsn := "s3://" + bucket + "?region=us-east-1&path_style=true&endpoint=" + url.QueryEscape(serverURL(t, p)) + "&access_key_env=FS_TEST_AK&secret_key_env=FS_TEST_SK&sse_c_key_env=FS_TEST_SSEC_RAW"
	_, err := mux.Open(t.Context(), "raw", dsn)
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if err == nil || !strings.Contains(err.Error(), "FS_TEST_SSEC_RAW") || !strings.Contains(err.Error(), "base64") || strings.Contains(err.Error(), "not base64 at all") {
		t.Errorf("error = %v", err)
	}
}
