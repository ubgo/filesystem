package filesystem

import (
	"context"
	"time"
)

// Form field names of a presigned POST, the HTML-form upload S3 and GCS
// accept (UP-04). A driver's PresignPost returns its fields under these
// names where the backend defines them, so a client builds the form the
// same way for every backend: every field of PresignedPost.Fields, in any
// order, then the file in PostFileField, last.
const (
	PostFieldKey                = "key"
	PostFieldContentType        = "Content-Type"
	PostFieldCacheControl       = HeaderCacheControl
	PostFieldContentDisposition = HeaderContentDisposition
	PostFieldContentEncoding    = HeaderContentEncoding
	// PostFileField is the form field holding the file; backends require
	// it to be the last field.
	PostFileField = "file"
	// PostFilenamePlaceholder in a key prefix upload's key field is
	// replaced by the backend with the uploaded file's name.
	PostFilenamePlaceholder = "${filename}"
)

// PostPolicy is what a presigned POST lets a client upload. Unlike a
// presigned PUT, the backend itself enforces every condition: the size
// range, the key, the content type, the stored headers and the expiry,
// so an oversized or mistyped upload is refused before it is stored.
type PostPolicy struct {
	// Key is the exact key the upload is stored under. Set Key or
	// KeyPrefix, not both.
	Key string
	// KeyPrefix lets the client choose any key under this directory; the
	// key field is then KeyPrefix + "/" + PostFilenamePlaceholder, which
	// the client may keep (the backend substitutes the file's name) or
	// replace with any key under the prefix. Never the root.
	KeyPrefix string
	// ContentType, when set, is the only Content-Type accepted; the
	// client sends it as the PostFieldContentType field. Empty accepts any.
	ContentType string
	// MinSize and MaxSize bound the upload in bytes: MaxSize is required,
	// and 0 ≤ MinSize ≤ MaxSize.
	MinSize, MaxSize int64
	// Expires is the form's lifetime, in (0, MaxPresignExpiry].
	Expires time.Duration
	// Headers are stored with the object (and pinned by the policy); they
	// need Capabilities.Headers.
	Headers HTTPHeaders
	// Visibility is the visibility the uploaded object is stored with,
	// pinned by the policy (S3's and GCS's acl field), under the rules of
	// PresignOptions.Visibility: empty pins nothing (unchanged), and a
	// value the disk must pin needs Capabilities.PresignVisibility.
	Visibility Visibility
}

// PresignedPost is a form a client posts to URL: every field in Fields,
// then the file in PostFileField. It expires at ExpiresAt.
type PresignedPost struct {
	URL       string
	Fields    map[string]string
	ExpiresAt time.Time
}

// PostPresigner signs presigned POST forms. Options arrive validated and
// cleaned by the Disk. The returned Fields must include PostFieldKey, and
// PostFieldContentType when the policy fixes a type, plus whatever the
// backend needs (policy document, signature, credential).
type PostPresigner interface {
	PresignPost(ctx context.Context, p PostPolicy) (PresignedPost, error)
}

// PresignPost returns a form that lets a client upload straight to the
// backend under p, with every condition enforced by the backend. It needs
// the PresignPost capability (S3 and GCS); without it, CodeUnsupported.
// It counts as a mutation, so a read-only disk refuses it. The upload is
// still the client's: confirm it (upload.Confirm) before trusting it.
func (d *Disk) PresignPost(ctx context.Context, p PostPolicy) (PresignedPost, error) {
	clean, err := d.validatePost(p)
	if err != nil {
		return PresignedPost{}, annotate(err, OpPresignPost, d.name, p.Key+p.KeyPrefix)
	}
	if !d.caps.PresignPost {
		return PresignedPost{}, d.unsupported(OpPresignPost, clean.Key+clean.KeyPrefix)
	}
	signer := d.driver.(PostPresigner)
	var out PresignedPost
	op := &Op{Kind: OpPresignPost, Key: clean.Key + clean.KeyPrefix, Mutating: true, Replayable: true}
	err = d.do(ctx, op, func(ctx context.Context) error {
		var err error
		out, err = signer.PresignPost(ctx, clean)
		return err
	})
	if err != nil {
		return PresignedPost{}, err
	}
	return out, nil
}

// validatePost checks a PostPolicy and returns it with its key or prefix
// cleaned.
func (d *Disk) validatePost(p PostPolicy) (PostPolicy, error) {
	switch {
	case (p.Key == "") == (p.KeyPrefix == ""):
		return p, Errorf(CodeInvalidArgument, "a POST policy needs exactly one of Key and KeyPrefix")
	case p.MaxSize <= 0 || p.MinSize < 0 || p.MinSize > p.MaxSize:
		return p, Errorf(CodeInvalidArgument, "POST size range [%d, %d] is not 0 ≤ MinSize ≤ MaxSize with MaxSize > 0", p.MinSize, p.MaxSize)
	}
	if err := validateExpiry(p.Expires); err != nil {
		return p, err
	}
	if err := p.Headers.validate(); err != nil {
		return p, err
	}
	if !p.Headers.IsZero() && !d.caps.Headers {
		return p, Errorf(CodeUnsupported, "driver %T does not store HTTP headers", d.driver)
	}
	var err error
	if p.Visibility, err = d.presignVisibility(p.Visibility); err != nil {
		return p, err
	}
	if p.Visibility != "" && !d.caps.PresignVisibility {
		return p, Errorf(CodeUnsupported, "driver %T cannot pin visibility in a POST form", d.driver)
	}
	if p.Key != "" {
		p.Key, err = d.CleanKey(p.Key)
		return p, err
	}
	if p.KeyPrefix, err = d.cleanDir(p.KeyPrefix); err == nil && p.KeyPrefix == "" {
		err = Errorf(CodeInvalidArgument, "a POST key prefix cannot be the root of the disk")
	}
	return p, err
}
