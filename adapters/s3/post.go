package s3

import (
	"context"
	"encoding/xml"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ubgo/filesystem"
)

// POST policy form fields S3 defines beyond the core's (filesystem.PostField*).
const (
	postFieldSSE          = "x-amz-server-side-encryption"
	postFieldKMSKeyID     = "x-amz-server-side-encryption-aws-kms-key-id"
	postFieldStorageClass = "x-amz-storage-class"
	postFieldTagging      = "tagging"
	postFieldLockMode     = "x-amz-object-lock-mode"
	postFieldRetainUntil  = "x-amz-object-lock-retain-until-date"
	postFieldLegalHold    = "x-amz-object-lock-legal-hold"
	postFieldACL          = "acl"
	postCondLengthRange   = "content-length-range"
	postCondStartsWith    = "starts-with"
	postKeyVariable       = "$key"
)

// PresignPost implements filesystem.PostPresigner with an S3 POST policy
// signed with SigV4: a form a browser posts straight to the bucket, where
// S3 itself refuses an upload outside the policy. The policy pins the
// size range (content-length-range), the exact key or a key prefix
// (starts-with $key), the Content-Type when one is set, the stored
// headers, and the disk's encryption, storage class, tags and object-lock
// settings (mode, retain-until date counted from signing, legal hold;
// RequestOptions for the exact key apply), so a form upload is stored
// exactly like a Write. Not declared with a ChecksumAlgorithm, whose value
// a form cannot carry before the browser reads the file. Objects get the
// bucket's default ACL unless the policy asks for a visibility
// (PostPolicy.Visibility, ACL mode only), pinned as the acl field. Every
// condition is also a
// form field the client must send unchanged (Fields).
func (d *Driver) PresignPost(ctx context.Context, p filesystem.PostPolicy) (filesystem.PresignedPost, error) {
	key := p.Key
	// The SDK's policy conditions are untyped JSON values ([]any), its API
	// boundary; each one built here is a string map or a fixed-shape list.
	conds := []any{[]any{postCondLengthRange, p.MinSize, p.MaxSize}}
	fields := map[string]string{}
	if p.KeyPrefix != "" {
		key = p.KeyPrefix + "/" + filesystem.PostFilenamePlaceholder
		conds = append(conds, []any{postCondStartsWith, postKeyVariable, p.KeyPrefix + "/"})
	}
	pin := func(name, value string) {
		if value != "" {
			fields[name] = value
			conds = append(conds, map[string]string{name: value})
		}
	}
	pin(filesystem.PostFieldContentType, p.ContentType)
	pin(filesystem.PostFieldCacheControl, p.Headers.CacheControl)
	pin(filesystem.PostFieldContentDisposition, p.Headers.ContentDisposition)
	pin(filesystem.PostFieldContentEncoding, p.Headers.ContentEncoding)
	ws, err := d.writeSettings(ctx, key)
	if err != nil {
		return filesystem.PresignedPost{}, err
	}
	pin(postFieldSSE, string(ws.sse))
	pin(postFieldKMSKeyID, aws.ToString(ws.kmsKey))
	pin(postFieldStorageClass, string(ws.class))
	pin(postFieldLockMode, string(ws.lockMode))
	if ws.retainUntil != nil {
		pin(postFieldRetainUntil, ws.retainUntil.Format(time.RFC3339))
	}
	pin(postFieldLegalHold, string(ws.legalHold))
	if ws.tagging != nil {
		pin(postFieldTagging, tagsXML(*ws.tagging))
	}
	acl, err := d.presignACL(p.Visibility)
	if err != nil {
		return filesystem.PresignedPost{}, err
	}
	pin(postFieldACL, string(acl))
	expires := d.now().Add(p.Expires)
	req, perr := d.presigner.PresignPostObject(ctx, &s3.PutObjectInput{Bucket: d.bucket(), Key: aws.String(key)}, func(o *s3.PresignPostOptions) {
		o.Expires = p.Expires
		o.Conditions = conds
		if d.cfg.Clock != nil {
			o.PostPresigner = clockPost{next: o.PostPresigner, now: d.cfg.Clock}
		}
	})
	if perr != nil {
		return filesystem.PresignedPost{}, mapErr(perr)
	}
	for k, v := range req.Values {
		fields[k] = v
	}
	return filesystem.PresignedPost{URL: req.URL, Fields: fields, ExpiresAt: expires}, nil
}

// tagsXML renders an x-amz-tagging query (encodeTags) as the Tagging XML
// document a POST form's tagging field takes.
func tagsXML(query string) string {
	vals, _ := url.ParseQuery(query)
	var b strings.Builder
	b.WriteString("<Tagging><TagSet>")
	for _, k := range slices.Sorted(maps.Keys(vals)) {
		b.WriteString("<Tag><Key>")
		_ = xml.EscapeText(&b, []byte(k))
		b.WriteString("</Key><Value>")
		_ = xml.EscapeText(&b, []byte(vals.Get(k)))
		b.WriteString("</Value></Tag>")
	}
	b.WriteString("</TagSet></Tagging>")
	return b.String()
}
