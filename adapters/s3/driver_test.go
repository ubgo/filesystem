package s3_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

func newDisk(t *testing.T, cfg fss3.Config) (*filesystem.Disk, *s3Proxy) {
	t.Helper()
	client, bucket, proxy := fakeS3WithProxy(t)
	cfg.Client, cfg.Bucket = client, bucket
	drv, err := fss3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("s3", drv)
	if err != nil {
		t.Fatal(err)
	}
	return d, proxy
}

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if got := filesystem.CodeOf(err); got != code {
		t.Fatalf("code %q (err %v), want %q", got, err, code)
	}
}

func TestNewValidation(t *testing.T) {
	client, bucket := fakeS3(t)
	for _, cfg := range []fss3.Config{
		{Bucket: bucket},
		{Client: client},
		{Client: client, Bucket: bucket, Visibility: "public"},
		{Client: client, Bucket: bucket, PartSize: -1},
	} {
		if _, err := fss3.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(%+v) = %v", cfg, err)
		}
	}
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("s3", drv)
	if c, ok := filesystem.As[*s3.Client](d); !ok || c != client {
		t.Error("As did not return the S3 client")
	}
	if caps := drv.Capabilities(); caps.SetVisibility || caps.ConditionalWrite || !caps.PresignPart {
		t.Errorf("default capabilities = %v", caps)
	}
	if len(fss3.VisibilityModeValues) != 2 {
		t.Error("VisibilityModeValues incomplete")
	}
}

// get fetches a URL and returns status and body.
func get(t *testing.T, method, u string, body io.Reader, h http.Header) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, u, body)
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPresignedURLsWork(t *testing.T) {
	ctx := t.Context()
	d, _ := newDisk(t, fss3.Config{})
	put, err := d.SignedURL(ctx, "direct/a.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if put.Header.Get("Content-Type") != "text/plain" || put.Header.Get("Host") != "" || !strings.Contains(put.URL, "X-Amz-Signature") {
		t.Errorf("signed PUT = %+v", put)
	}
	if status, body := get(t, http.MethodPut, put.URL, strings.NewReader("uploaded"), put.Header); status != 200 {
		t.Fatalf("PUT to signed URL = %d %s", status, body)
	}
	g, err := d.SignedURL(ctx, "direct/a.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, ContentType: "text/x-forced", ContentDisposition: "attachment"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.URL, "response-content-disposition=attachment") || g.Header != nil {
		t.Errorf("signed GET = %+v", g)
	}
	if status, body := get(t, http.MethodGet, g.URL, nil, nil); status != 200 || body != "uploaded" {
		t.Errorf("GET signed URL = %d %q", status, body)
	}
}

func TestPresignedPartsComplete(t *testing.T) {
	ctx := t.Context()
	d, _ := newDisk(t, fss3.Config{})
	id, err := d.CreateMultipart(ctx, "mp.bin", filesystem.WriteOptions{ContentType: "application/octet-stream", Metadata: map[string]string{"a": "b"}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := d.PresignPart(ctx, "mp.bin", id, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := get(t, http.MethodPut, req.URL, strings.NewReader("browser part"), req.Header)
	if status != 200 {
		t.Fatalf("PUT part = %d", status)
	}
	parts, err := d.ListParts(ctx, "mp.bin", id)
	if err != nil || len(parts) != 1 {
		t.Fatalf("parts = %+v, %v", parts, err)
	}
	if _, err := d.CompleteMultipart(ctx, "mp.bin", id, parts); err != nil {
		t.Fatal(err)
	}
	if b, _ := d.ReadAll(ctx, "mp.bin"); string(b) != "browser part" {
		t.Errorf("assembled %q", b)
	}
	cond, _ := newDisk(t, fss3.Config{ConditionalWrites: true})
	_, err = cond.CreateMultipart(ctx, "x", filesystem.WriteOptions{IfNotExists: true})
	wantCode(t, err, filesystem.CodeUnsupported)
}

func TestPresignFailsWithoutCredentials(t *testing.T) {
	client := s3.New(s3.Options{
		Region:      "us-east-1",
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return aws.Credentials{}, errors.New("no credentials") }),
	})
	drv, _ := fss3.New(fss3.Config{Client: client, Bucket: "b"})
	d, _ := filesystem.NewDisk("s3", drv)
	ctx := t.Context()
	if _, err := d.SignedURL(ctx, "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute}); err == nil {
		t.Error("presign without credentials succeeded")
	}
	if _, err := d.PresignPart(ctx, "k", "id", 1, time.Minute); err == nil {
		t.Error("part presign without credentials succeeded")
	}
	if _, err := d.PresignPost(ctx, filesystem.PostPolicy{Key: "k", MaxSize: 1, Expires: time.Minute}); err == nil {
		t.Error("POST presign without credentials succeeded")
	}
}

func TestListWithoutLimitUsesThePageSize(t *testing.T) {
	client, bucket := fakeS3(t)
	drv, _ := fss3.New(fss3.Config{Client: client, Bucket: bucket})
	if _, err := drv.List(t.Context(), "", filesystem.ListOptions{}); err != nil {
		t.Errorf("driver List with no limit: %v", err)
	}
}

func TestDeletePrefixBatches(t *testing.T) {
	ctx := t.Context()
	d, _ := newDisk(t, fss3.Config{})
	const n = 1003
	for i := range n {
		if _, err := d.WriteBytes(ctx, fmt.Sprintf("bulk/%04d", i), []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = d.WriteBytes(ctx, "keep", []byte("x"), filesystem.WriteOptions{})
	if err := d.DeletePrefix(ctx, "bulk"); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, err := range d.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 1 {
		t.Errorf("%d keys left, want 1", count)
	}
}

func TestReadPastEnd(t *testing.T) {
	ctx := t.Context()
	d, proxy := newDisk(t, fss3.Config{})
	_, _ = d.WriteBytes(ctx, "k", []byte("abc"), filesystem.WriteOptions{})
	proxy.fail(http.MethodGet, "/k", http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
	rc, err := d.ReadRange(ctx, "k", filesystem.ByteRange{Offset: 100})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(rc); len(b) != 0 {
		t.Errorf("read past end = %q", b)
	}
	proxy.fail(http.MethodGet, "/missing", http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
	_, err = d.ReadRange(ctx, "missing", filesystem.ByteRange{Offset: 100})
	wantCode(t, err, filesystem.CodeNotFound)
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   filesystem.Code
	}{
		{404, "NoSuchKey", filesystem.CodeNotFound},
		{404, "NoSuchUpload", filesystem.CodeNotFound},
		{412, "PreconditionFailed", filesystem.CodeExists},
		{403, "AccessDenied", filesystem.CodePermission},
		{400, "InvalidPart", filesystem.CodeInvalidArgument},
		{400, "EntityTooSmall", filesystem.CodeInvalidArgument},
		{400, "BadDigest", filesystem.CodeInvalidArgument},
		{400, "XAmzContentChecksumMismatch", filesystem.CodeInvalidArgument},
		{400, "XAmzContentSHA256Mismatch", filesystem.CodeInvalidArgument},
		{503, "SlowDown", filesystem.CodeUnavailable},
		{503, "ServiceUnavailable", filesystem.CodeUnavailable},
		{429, "TooManyRequests", filesystem.CodeUnavailable},
		{403, "SignatureDoesNotMatch", filesystem.CodePermission},
		{404, "SomethingNew", filesystem.CodeNotFound},
		{412, "OtherCondition", filesystem.CodeExists},
		{400, "MalformedXML", filesystem.CodeUnknown},
	}
	for _, c := range cases {
		d, proxy := newDisk(t, fss3.Config{})
		// Retries off: the SDK retries 5xx itself, which would consume the
		// single injected failure.
		proxy.fail(http.MethodDelete, "/victim", c.status, c.code)
		err := d.Delete(t.Context(), "victim")
		if filesystem.CodeOf(err) != c.want {
			t.Errorf("%d %s => %q (%v), want %q", c.status, c.code, filesystem.CodeOf(err), err, c.want)
		}
	}
}

// TestRetryAfter proves a 429 or 503 carrying Retry-After reports the
// wait on the error, and that one without it reports none.
func TestRetryAfter(t *testing.T) {
	for _, c := range []struct {
		status     int
		code       string
		header     string
		want       time.Duration
		wantHasOne bool
	}{
		{http.StatusServiceUnavailable, "SlowDown", "7", 7 * time.Second, true},
		{http.StatusTooManyRequests, "TooManyRequests", "2", 2 * time.Second, true},
		{http.StatusServiceUnavailable, "ServiceUnavailable", "", 0, false},
		{http.StatusServiceUnavailable, "ServiceUnavailable", "soon", 0, false},
	} {
		d, proxy := newDisk(t, fss3.Config{})
		proxy.failRetryAfter(http.MethodDelete, "/victim", c.status, c.code, c.header)
		err := d.Delete(t.Context(), "victim")
		wantCode(t, err, filesystem.CodeUnavailable)
		got, ok := filesystem.RetryAfterOf(err)
		if ok != c.wantHasOne || got != c.want {
			t.Errorf("%d %s Retry-After %q => %v, %t; want %v", c.status, c.code, c.header, got, ok, c.want)
		}
	}
}

// TestEveryOperationReportsFailures injects an error into the request each
// operation makes and requires it to surface.
func TestEveryOperationReportsFailures(t *testing.T) {
	ctx := t.Context()
	type step struct {
		name, method, match string
		run                 func(d *filesystem.Disk) error
	}
	steps := []step{
		{"stat", http.MethodHead, "/k", func(d *filesystem.Disk) error { _, err := d.Stat(ctx, "k"); return err }},
		{"stat acl", http.MethodGet, "acl", func(d *filesystem.Disk) error { _, err := d.Stat(ctx, "k"); return err }},
		{"read", http.MethodGet, "/k", func(d *filesystem.Disk) error { _, err := d.Read(ctx, "k"); return err }},
		{"write", http.MethodPut, "/w", func(d *filesystem.Disk) error {
			_, err := d.WriteBytes(ctx, "w", []byte("x"), filesystem.WriteOptions{})
			return err
		}},
		{"list", http.MethodGet, "list-type", func(d *filesystem.Disk) error { _, err := d.ListPage(ctx, "", filesystem.ListOptions{}); return err }},
		{"copy acl", http.MethodGet, "acl", func(d *filesystem.Disk) error { _, err := d.Copy(ctx, "k", "c"); return err }},
		{"copy", http.MethodPut, "/c", func(d *filesystem.Disk) error { _, err := d.Copy(ctx, "k", "c"); return err }},
		{"set visibility", http.MethodPut, "acl", func(d *filesystem.Disk) error { return d.SetVisibility(ctx, "k", filesystem.VisibilityPublic) }},
		{"delete prefix list", http.MethodGet, "list-type", func(d *filesystem.Disk) error { return d.DeletePrefix(ctx, "dir") }},
		{"delete prefix delete", http.MethodPost, "delete", func(d *filesystem.Disk) error { return d.DeletePrefix(ctx, "dir") }},
		{"create multipart", http.MethodPost, "uploads", func(d *filesystem.Disk) error {
			_, err := d.CreateMultipart(ctx, "m", filesystem.WriteOptions{})
			return err
		}},
		{"upload part", http.MethodPut, "partNumber", func(d *filesystem.Disk) error {
			_, err := d.UploadPart(ctx, "m", "id", 1, strings.NewReader("x"), 1)
			return err
		}},
		{"list parts", http.MethodGet, "uploadId", func(d *filesystem.Disk) error { _, err := d.ListParts(ctx, "m", "id"); return err }},
		{"complete", http.MethodPost, "uploadId", func(d *filesystem.Disk) error {
			_, err := d.CompleteMultipart(ctx, "m", "id", []filesystem.Part{{Number: 1, ETag: "e"}})
			return err
		}},
		{"abort", http.MethodDelete, "uploadId", func(d *filesystem.Disk) error { return d.AbortMultipart(ctx, "m", "id") }},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			d, proxy := newDisk(t, fss3.Config{Visibility: fss3.VisibilityACL})
			_, _ = d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{})
			_, _ = d.WriteBytes(ctx, "dir/a", []byte("x"), filesystem.WriteOptions{})
			proxy.fail(s.method, s.match, http.StatusForbidden, "AccessDenied")
			if err := s.run(d); filesystem.CodeOf(err) != filesystem.CodePermission {
				t.Errorf("%s: %v", s.name, err)
			}
		})
	}
}

// partialDelete answers a DeleteObjects request with per-key errors.
func TestDeletePrefixReportsPerKeyErrors(t *testing.T) {
	ctx := t.Context()
	client, bucket, proxy := fakeS3WithProxy(t)
	proxy.next = deleteErrors{proxy.next}
	drv, _ := fss3.New(fss3.Config{Client: client, Bucket: bucket})
	d, _ := filesystem.NewDisk("s3", drv)
	_, _ = d.WriteBytes(ctx, "dir/a", []byte("x"), filesystem.WriteOptions{})
	err := d.DeletePrefix(ctx, "dir")
	var batch *filesystem.BatchError
	if !errors.As(err, &batch) || batch.Failures[0].Key != "dir/a" || filesystem.CodeOf(err) != filesystem.CodeUnavailable || !strings.Contains(err.Error(), "InternalError boom") {
		t.Errorf("per-key delete errors = %v", err)
	}
	err = d.DeleteKeys(ctx, []string{"dir/a", "other"})
	if !errors.As(err, &batch) || len(batch.Failures) != 2 || batch.Failures[0].Key != "dir/a" ||
		filesystem.CodeOf(batch.Failures[0].Err) != filesystem.CodeUnavailable || filesystem.CodeOf(batch.Failures[1].Err) != filesystem.CodeUnknown {
		t.Errorf("DeleteKeys per-key errors = %v %+v", err, batch.Failures)
	}
}

func TestDeleteKeys(t *testing.T) {
	ctx := t.Context()
	d, proxy := newDisk(t, fss3.Config{})
	if !d.Capabilities().DeleteKeys {
		t.Fatal("the S3 driver does not declare DeleteKeys")
	}
	keys := make([]string, 1001) // two requests
	for i := range keys {
		keys[i] = fmt.Sprintf("many/%04d", i)
		if _, err := d.WriteBytes(ctx, keys[i], []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	proxy.fail(http.MethodPost, "delete", http.StatusForbidden, "AccessDenied")
	err := d.DeleteKeys(ctx, keys)
	var batch *filesystem.BatchError
	if !errors.As(err, &batch) || len(batch.Failures) != len(keys) || filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("a refused request = %d failures, %v", len(batch.Failures), filesystem.CodeOf(err))
	}
	if err := d.DeleteKeys(ctx, append(keys, "never-existed")); err != nil {
		t.Fatal(err)
	}
	page, _ := d.ListPage(ctx, "many", filesystem.ListOptions{Recursive: true})
	if len(page.Entries) != 0 {
		t.Fatalf("%d keys left", len(page.Entries))
	}
}

type deleteErrors struct{ next http.Handler }

func (h deleteErrors) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.URL.Query()["delete"]; ok && r.Method == http.MethodPost {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><DeleteResult><Error><Key>dir/a</Key><Code>InternalError</Code><Message>boom</Message></Error><Error><Key>other</Key><Code>SomethingNew</Code><Message>odd</Message></Error></DeleteResult>`)
		return
	}
	h.next.ServeHTTP(w, r)
}

func TestAbortUnknownUploadSucceeds(t *testing.T) {
	d, proxy := newDisk(t, fss3.Config{})
	proxy.fail(http.MethodDelete, "uploadId", http.StatusNotFound, "NoSuchUpload")
	if err := d.AbortMultipart(t.Context(), "k", "unknown"); err != nil {
		t.Errorf("abort of an unknown upload = %v", err)
	}
}

func TestOpener(t *testing.T) {
	ctx := t.Context()
	_, bucket, proxy := fakeS3WithProxy(t)
	srv := serverURL(t, proxy)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		t.Fatal(err)
	}
	dsn := "s3://" + bucket + "?region=us-east-1&endpoint=" + url.QueryEscape(srv) + "&path_style=true&acl=true&conditional_write=true&sse=&storage_class=&part_size=5242880&prefix=tenant"
	d, err := mux.Open(ctx, "s3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("via dsn"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}); err != nil {
		t.Fatal(err)
	}
	if c := d.Capabilities(); !c.SetVisibility || !c.ConditionalWrite {
		t.Errorf("DSN flags ignored: %v", c)
	}
	if !d.Capabilities().PresignHeaders {
		t.Error("presign_headers does not default to true")
	}
	if off, err := mux.Open(ctx, "s3", dsn+"&presign_headers=false"); err != nil || off.Capabilities().PresignHeaders {
		t.Errorf("presign_headers=false = %v, %v", off, err)
	}
	for _, bad := range []string{
		"s3://?region=x",
		"s3://b?nope=1",
		"s3://b?path_style=maybe",
		"s3://b?part_size=big",
		"s3://b?part_size=-5",
		"s3://b?presign_headers=maybe",
	} {
		if _, err := mux.Open(ctx, "s3", bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("Open(%q) = %v", bad, err)
		}
	}
	t.Setenv("AWS_CONFIG_FILE", t.TempDir())
	if _, err := mux.Open(ctx, "s3", "s3://b?region=us-east-1"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("unreadable AWS config = %v", err)
	}
}

func TestPruneEmptyPrefixes(t *testing.T) {
	ctx := t.Context()
	client, bucket, proxy := fakeS3WithProxy(t)
	// Directory markers stand in for the empty folders a filer-backed store
	// keeps after their last object is deleted: "marker/" is listed as an
	// object by some stores, "empty/sub/" rolls up into the prefix "empty/".
	for _, k := range []string{"marker/", "empty/sub/"} {
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(k), Body: strings.NewReader("")}); err != nil {
			t.Fatal(err)
		}
	}
	for _, cfg := range []struct {
		prune bool
		want  string
	}{{false, "empty/,full/"}, {true, "full/"}} {
		drv, _ := fss3.New(fss3.Config{Client: client, Bucket: bucket, PruneEmptyPrefixes: cfg.prune})
		d, _ := filesystem.NewDisk("s3", drv)
		_, _ = d.WriteBytes(ctx, "full/a.txt", []byte("x"), filesystem.WriteOptions{})
		var got []string
		for e, err := range d.List(ctx, "", filesystem.ListOptions{}) {
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, e.Key+"/")
		}
		if strings.Join(got, ",") != cfg.want {
			t.Errorf("prune=%v listed %v, want %s", cfg.prune, got, cfg.want)
		}
	}
	drv, _ := fss3.New(fss3.Config{Client: client, Bucket: bucket, PruneEmptyPrefixes: true})
	d, _ := filesystem.NewDisk("s3", drv)
	proxy.fail(http.MethodGet, "prefix=empty", http.StatusForbidden, "AccessDenied")
	if _, err := d.ListPage(ctx, "", filesystem.ListOptions{}); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("prefix check failure = %v", err)
	}
}

// TestCompleteMultipartKeepsContentType proves the object a multipart
// upload completes into is stored with the type given to CreateMultipart.
// upload.Confirm, which the Uppy complete route runs, refuses an object
// whose stored type differs from its sniffed bytes, so a lost type would
// refuse every browser multipart upload.
func TestCompleteMultipartKeepsContentType(t *testing.T) {
	ctx := t.Context()
	d, _ := newDisk(t, fss3.Config{})
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	id, err := d.CreateMultipart(ctx, "photo.png", filesystem.WriteOptions{ContentType: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := d.UploadPart(ctx, "photo.png", id, 1, bytes.NewReader(png), int64(len(png)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CompleteMultipart(ctx, "photo.png", id, []filesystem.Part{p}); err != nil {
		t.Fatal(err)
	}
	if e, err := d.Stat(ctx, "photo.png"); err != nil || e.ContentType != "image/png" {
		t.Fatalf("stored type = %q, %v; want image/png", e.ContentType, err)
	}
}

// TestPresignPinsHeaders proves a presigned PUT signs the stored headers
// it was asked to pin (SigV4 lists them in X-Amz-SignedHeaders, so a store
// that verifies signatures refuses an upload without them) and hands them
// back for the client to send. The fake does not verify signatures; the
// refusal itself is checked by TestPresignHeadersAgainstARealStore.
func TestPresignPinsHeaders(t *testing.T) {
	ctx := t.Context()
	d, _ := newDisk(t, fss3.Config{})
	if !d.Capabilities().PresignHeaders {
		t.Fatal("PresignHeaders not declared")
	}
	h := filesystem.HTTPHeaders{CacheControl: "no-store", ContentDisposition: "attachment", ContentEncoding: "gzip"}
	put, err := d.SignedURL(ctx, "pinned.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, Headers: h})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(put.URL)
	signedHeaders := u.Query().Get("X-Amz-SignedHeaders")
	for _, name := range []string{"cache-control", "content-disposition", "content-encoding"} {
		if !strings.Contains(signedHeaders, name) {
			t.Errorf("%s not signed: %s", name, signedHeaders)
		}
	}
	if filesystem.HTTPHeadersFrom(put.Header) != h {
		t.Errorf("returned headers = %v", put.Header)
	}
	plain, err := d.SignedURL(ctx, "plain.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if err != nil || strings.Contains(plain.URL, "cache-control") || plain.Header.Get(filesystem.HeaderCacheControl) != "" {
		t.Errorf("PUT without headers = %+v, %v", plain, err)
	}
	if c, _ := newDisk(t, fss3.Config{UnverifiedSignedHeaders: true}); c.Capabilities().PresignHeaders {
		t.Error("PresignHeaders claimed on a store that does not verify signatures")
	}
}
