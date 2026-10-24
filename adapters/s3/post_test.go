package s3_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// policyDoc decodes a POST form's policy document.
type policyDoc struct {
	Expiration string            `json:"expiration"`
	Conditions []json.RawMessage `json:"conditions"`
}

func decodePolicy(t *testing.T, fields map[string]string) (policyDoc, string) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(fields["policy"])
	if err != nil {
		t.Fatalf("policy is not base64: %v", err)
	}
	var doc policyDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("policy is not JSON: %v", err)
	}
	return doc, string(raw)
}

// sigV4PostSignature computes a POST policy's signature from the secret,
// independently of the SDK: HMAC-SHA256 over the base64 policy with the
// key derived from the secret, the date, the region and "s3".
func sigV4PostSignature(secret, date, region, policy string) string {
	mac := func(key []byte, data string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(data))
		return h.Sum(nil)
	}
	k := mac([]byte("AWS4"+secret), date)
	k = mac(k, region)
	k = mac(k, "s3")
	k = mac(k, "aws4_request")
	return hex.EncodeToString(mac(k, policy))
}

// postForm posts fields and a file the way a browser form does.
func postForm(t *testing.T, url string, fields map[string]string, filename string, body []byte) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	fw, _ := w.CreateFormFile(filesystem.PostFileField, filename)
	_, _ = fw.Write(body)
	_ = w.Close()
	resp, err := http.Post(url, w.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestPresignPostPolicy proves the form's policy pins the size range, the
// key, the type, the stored headers and the disk's encryption, that every
// pinned value is also a field, and that the signature is SigV4's over the
// policy (recomputed here from the secret, not by the SDK).
func TestPresignPostPolicy(t *testing.T) {
	ctx := t.Context()
	d, _ := newDisk(t, fss3.Config{ServerSideEncryption: types.ServerSideEncryptionAes256, StorageClass: types.StorageClassStandardIa})
	if !d.Capabilities().PresignPost {
		t.Fatal("PresignPost not declared")
	}
	h := filesystem.HTTPHeaders{CacheControl: "no-store", ContentDisposition: "attachment"}
	post, err := d.PresignPost(ctx, filesystem.PostPolicy{Key: "in/a.png", ContentType: "image/png", MinSize: 1, MaxSize: 1 << 20, Expires: 10 * time.Minute, Headers: h})
	if err != nil {
		t.Fatal(err)
	}
	doc, raw := decodePolicy(t, post.Fields)
	for _, want := range []string{
		`["content-length-range",1,1048576]`, `{"key":"in/a.png"}`, `{"Content-Type":"image/png"}`,
		`{"Cache-Control":"no-store"}`, `{"Content-Disposition":"attachment"}`,
		`{"x-amz-server-side-encryption":"AES256"}`, `{"x-amz-storage-class":"STANDARD_IA"}`, `{"bucket":"test-bucket"}`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("policy lacks %s:\n%s", want, raw)
		}
	}
	for k, v := range map[string]string{"key": "in/a.png", "Content-Type": "image/png", "Cache-Control": "no-store", "x-amz-server-side-encryption": "AES256", "x-amz-storage-class": "STANDARD_IA"} {
		if post.Fields[k] != v {
			t.Errorf("field %s = %q, want %q", k, post.Fields[k], v)
		}
	}
	exp, err := time.Parse(time.RFC3339, doc.Expiration)
	if err != nil || exp.Before(time.Now().Add(9*time.Minute)) || exp.After(time.Now().Add(11*time.Minute)) {
		t.Errorf("policy expiration %q", doc.Expiration)
	}
	date := post.Fields["X-Amz-Date"][:8]
	if want := sigV4PostSignature("test", date, "us-east-1", post.Fields["policy"]); post.Fields["X-Amz-Signature"] != want {
		t.Errorf("signature %s, want %s", post.Fields["X-Amz-Signature"], want)
	}
	if !strings.HasPrefix(post.Fields["X-Amz-Credential"], "test/"+date+"/us-east-1/s3/aws4_request") {
		t.Errorf("credential %q", post.Fields["X-Amz-Credential"])
	}
}

// TestPresignPostPrefix proves a prefix form lets the client choose a key
// under the prefix only, and pins no type when none was asked for.
func TestPresignPostPrefix(t *testing.T) {
	d, _ := newDisk(t, fss3.Config{SSEKMSKeyID: testKMSKey})
	post, err := d.PresignPost(t.Context(), filesystem.PostPolicy{KeyPrefix: "users/42", MaxSize: 100, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_, raw := decodePolicy(t, post.Fields)
	if !strings.Contains(raw, `["starts-with","$key","users/42/"]`) || strings.Contains(raw, `{"key":`) || strings.Contains(raw, "Content-Type") {
		t.Errorf("prefix policy:\n%s", raw)
	}
	if !strings.Contains(raw, `"x-amz-server-side-encryption-aws-kms-key-id":"`+testKMSKey+`"`) || post.Fields["x-amz-server-side-encryption"] != "aws:kms" {
		t.Errorf("KMS key not pinned:\n%s", raw)
	}
	if post.Fields["key"] != "users/42/"+filesystem.PostFilenamePlaceholder {
		t.Errorf("key field %q", post.Fields["key"])
	}
}

// TestPresignPostUpload posts the form to the fake (which stores form
// uploads but checks no policy) and finds the object stored under the key.
func TestPresignPostUpload(t *testing.T) {
	ctx := t.Context()
	d, _ := newDisk(t, fss3.Config{})
	post, err := d.PresignPost(ctx, filesystem.PostPolicy{Key: "form/a.txt", ContentType: "text/plain", MaxSize: 100, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if status, body := postForm(t, post.URL, post.Fields, "a.txt", []byte("hello")); status >= 300 {
		t.Fatalf("form post = %d %s", status, body)
	}
	if b, err := d.ReadAll(ctx, "form/a.txt"); err != nil || string(b) != "hello" {
		t.Errorf("stored = %q, %v", b, err)
	}
}
