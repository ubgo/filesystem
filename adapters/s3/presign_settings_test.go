package s3_test

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// TestPresignPinsWriteSettings proves a presigned PUT signs, and a
// presigned POST pins as policy conditions and form fields, every
// per-write setting a Write applies: storage class, tags, object-lock
// mode, retain-until date and legal hold, so a direct upload cannot skip
// a retention.
func TestPresignPinsWriteSettings(t *testing.T) {
	ctx := t.Context()
	d, _ := lockedDisk(t, fss3.Config{Tagging: true, ObjectLockDays: 30, StorageClass: types.StorageClassStandardIa})
	rctx := withOpts(t, ctx, d, "up.bin", fss3.RequestOptions{LegalHold: true, Tags: map[string]string{"case": "7"}})
	day := time.Now().AddDate(0, 0, 30).UTC().Format("2006-01-02")
	want := map[string]string{
		"X-Amz-Storage-Class":                 "STANDARD_IA",
		"X-Amz-Object-Lock-Mode":              "GOVERNANCE",
		"X-Amz-Object-Lock-Retain-Until-Date": day,
		"X-Amz-Object-Lock-Legal-Hold":        "ON",
		"X-Amz-Tagging":                       "case=7",
	}
	put, err := d.SignedURL(rctx, "up.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(put.URL)
	signed := u.Query().Get("X-Amz-SignedHeaders")
	for h, v := range want {
		if !strings.Contains(signed, strings.ToLower(h)) || !strings.HasPrefix(put.Header.Get(h), v) {
			t.Errorf("presigned PUT: %s signed=%t value=%q, want %q", h, strings.Contains(signed, strings.ToLower(h)), put.Header.Get(h), v)
		}
	}

	post, err := d.PresignPost(rctx, filesystem.PostPolicy{Key: "up.bin", MaxSize: 1 << 20, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// The policy's exact-match conditions, by field name.
	pinned := map[string]string{}
	for k, v := range post.Fields {
		if !strings.EqualFold(k, "policy") {
			continue
		}
		raw, _ := base64.StdEncoding.DecodeString(v)
		var doc struct {
			Conditions []json.RawMessage `json:"conditions"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, c := range doc.Conditions {
			var m map[string]string
			if json.Unmarshal(c, &m) == nil {
				maps.Copy(pinned, m)
			}
		}
	}
	fields := map[string]string{
		"x-amz-storage-class":                 "STANDARD_IA",
		"x-amz-object-lock-mode":              "GOVERNANCE",
		"x-amz-object-lock-retain-until-date": day,
		"x-amz-object-lock-legal-hold":        "ON",
		"tagging":                             "<Tagging><TagSet><Tag><Key>case</Key><Value>7</Value></Tag></TagSet></Tagging>",
	}
	for f, v := range fields {
		if !strings.HasPrefix(post.Fields[f], v) || pinned[f] != post.Fields[f] {
			t.Errorf("presigned POST: field %s = %q, policy %q, want %q", f, post.Fields[f], pinned[f], v)
		}
	}
	// Another key does not get the per-key options.
	other, _ := d.SignedURL(rctx, "other.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if other.Header.Get("X-Amz-Object-Lock-Legal-Hold") != "" || other.Header.Get("X-Amz-Object-Lock-Mode") != "GOVERNANCE" {
		t.Errorf("other key headers = %v", other.Header)
	}
}

// TestPresignWithChecksumAlgorithm proves a disk with a checksum
// algorithm declares no presigning it cannot honour: none at all by
// default, and with PresignWithoutChecksum a Presign whose PUT leaves the
// checksum out, while parts and forms stay off. A per-call algorithm is
// refused.
func TestPresignWithChecksumAlgorithm(t *testing.T) {
	ctx := t.Context()
	d, _ := newDisk(t, fss3.Config{ChecksumAlgorithm: types.ChecksumAlgorithmCrc32c})
	if c := d.Capabilities(); c.Presign || c.PresignPart || c.PresignPost || c.PresignHeaders {
		t.Errorf("capabilities = %+v", c)
	}
	_, err := d.SignedURL(ctx, "a.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	wantCode(t, err, filesystem.CodeUnsupported)
	opted, _ := newDisk(t, fss3.Config{ChecksumAlgorithm: types.ChecksumAlgorithmCrc32c, PresignWithoutChecksum: true})
	if c := opted.Capabilities(); !c.Presign || !c.PresignHeaders || c.PresignPart || c.PresignPost {
		t.Errorf("opted-in capabilities = %+v", c)
	}
	put, err := opted.SignedURL(ctx, "a.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := url.Parse(put.URL); strings.Contains(u.Query().Get("X-Amz-SignedHeaders"), "checksum") {
		t.Errorf("opted-in PUT signs a checksum: %s", u.Query().Get("X-Amz-SignedHeaders"))
	}
	if _, err := opted.SignedURL(ctx, "a.bin", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute}); err != nil {
		t.Errorf("presigned GET: %v", err)
	}
	plain, _ := newDisk(t, fss3.Config{})
	_, err = plain.SignedURL(withOpts(t, ctx, plain, "a.bin", fss3.RequestOptions{ChecksumAlgorithm: types.ChecksumAlgorithmSha1}), "a.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	wantCode(t, err, filesystem.CodeUnsupported)
}
