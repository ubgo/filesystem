package s3_test

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// headerACL is the header a presigned PUT signs a visibility as.
const headerACL = "X-Amz-Acl"

// postConditions returns a POST form's exact-match policy conditions.
func postConditions(t *testing.T, fields map[string]string) map[string]string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(fields["policy"])
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Conditions []json.RawMessage `json:"conditions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, c := range doc.Conditions {
		var m map[string]string
		if json.Unmarshal(c, &m) == nil {
			for k, v := range m {
				out[k] = v
			}
		}
	}
	return out
}

// TestPresignVisibilitySignsACL proves that in ACL mode a presigned PUT
// signs the asked visibility as x-amz-acl and a POST form pins it as the
// acl field and policy condition, and that nothing is signed when none is
// asked, as before.
func TestPresignVisibilitySignsACL(t *testing.T) {
	ctx := t.Context()
	d, _ := newDisk(t, fss3.Config{Visibility: fss3.VisibilityACL})
	if !d.Capabilities().PresignVisibility {
		t.Fatal("an ACL-mode disk does not declare PresignVisibility")
	}
	for _, c := range []struct {
		ask  filesystem.Visibility
		want string
	}{
		{"", ""},
		{filesystem.VisibilityPublic, "public-read"},
		{filesystem.VisibilityPrivate, "private"},
	} {
		put, err := d.SignedURL(ctx, "a.png", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, Visibility: c.ask})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(put.URL)
		signed := strings.Contains(u.Query().Get("X-Amz-SignedHeaders"), strings.ToLower(headerACL))
		if put.Header.Get(headerACL) != c.want || signed != (c.want != "") {
			t.Errorf("PUT %q: header %q signed %t, want %q", c.ask, put.Header.Get(headerACL), signed, c.want)
		}
		post, err := d.PresignPost(ctx, filesystem.PostPolicy{Key: "a.png", MaxSize: 1, Expires: time.Minute, Visibility: c.ask})
		if err != nil {
			t.Fatal(err)
		}
		if post.Fields["acl"] != c.want || postConditions(t, post.Fields)["acl"] != c.want {
			t.Errorf("POST %q: field %q condition %q, want %q", c.ask, post.Fields["acl"], postConditions(t, post.Fields)["acl"], c.want)
		}
	}
}

// TestPresignVisibilityRefusedWithoutACLs proves a bucket whose ACLs are
// off (bucket owner enforced, the S3 default) never issues an upload that
// claims a visibility it cannot set, and that a store that checks no
// signature does not claim to pin one.
func TestPresignVisibilityRefusedWithoutACLs(t *testing.T) {
	ctx := t.Context()
	d, _ := newDisk(t, fss3.Config{})
	if d.Capabilities().PresignVisibility {
		t.Fatal("a disk without ACLs declares PresignVisibility")
	}
	_, err := d.SignedURL(ctx, "a.png", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, Visibility: filesystem.VisibilityPublic})
	wantCode(t, err, filesystem.CodeUnsupported)
	_, err = d.PresignPost(ctx, filesystem.PostPolicy{Key: "a.png", MaxSize: 1, Expires: time.Minute, Visibility: filesystem.VisibilityPublic})
	wantCode(t, err, filesystem.CodeUnsupported)
	// The driver itself refuses too, for a caller that bypasses the Disk.
	client, bucket, _ := fakeS3WithProxy(t)
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	_, err = drv.Presign(ctx, "a.png", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, Visibility: filesystem.VisibilityPublic})
	wantCode(t, err, filesystem.CodeUnsupported)
	_, err = drv.PresignPost(ctx, filesystem.PostPolicy{Key: "a.png", MaxSize: 1, Expires: time.Minute, Visibility: filesystem.VisibilityPublic})
	wantCode(t, err, filesystem.CodeUnsupported)

	u, _ := newDisk(t, fss3.Config{Visibility: fss3.VisibilityACL, UnverifiedSignedHeaders: true})
	if u.Capabilities().PresignVisibility {
		t.Fatal("a store that checks no signature declares PresignVisibility")
	}
}
