package s3

import (
	"context"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
)

// Presign implements filesystem.Presigner with SigV4 query signing. A GET
// can override the response's content type and disposition; a PUT signs the
// content type and opts.Headers (Cache-Control, Content-Disposition,
// Content-Encoding), so the client must send exactly the headers in
// SignedRequest.Header, a store that verifies signatures refuses the upload
// otherwise, and S3 stores them with the object (PresignHeaders).
//
// A PUT also signs the disk's per-write settings (storage class, tags,
// object-lock mode, retain-until date counted from signing, legal hold;
// RequestOptions for key apply), which come back in SignedRequest.Header.
// The checksum of a body not yet seen cannot be signed: with a
// Config.ChecksumAlgorithm Presign is declared only with
// Config.PresignWithoutChecksum, whose PUTs leave the checksum out, and a
// PUT under RequestOptions.ChecksumAlgorithm is CodeUnsupported.
//
// A PUT asked for a visibility (PresignOptions.Visibility, ACL mode only)
// signs it as x-amz-acl, so the client must send that header unchanged and
// cannot make the object public on its own (PresignVisibility).
//
// Under Config.SSECustomerKey it is declared only with
// Config.PresignCustomerKey: both methods sign the key, so the client must
// send SignedRequest.Header, which then holds the disk-wide key.
func (d *Driver) Presign(ctx context.Context, key string, opts filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	expires := func(o *s3.PresignOptions) { o.Expires = opts.Expires }
	c := d.cfg.SSECustomerKey.headers()
	var (
		req *v4.PresignedHTTPRequest
		err error
	)
	if opts.Method == filesystem.MethodPut {
		ws, werr := d.writeSettings(ctx, key)
		if werr != nil {
			return filesystem.SignedRequest{}, werr
		}
		// A checksum header needs the body's checksum, unknown when
		// signing; S3 would refuse every upload against the one the SDK
		// signs for an empty body. The disk's own algorithm is left out
		// only when Config.PresignWithoutChecksum says so; one a call asks
		// for is refused.
		if requestOptions(ctx, key).ChecksumAlgorithm != "" || (d.cfg.ChecksumAlgorithm != "" && !d.cfg.PresignWithoutChecksum) {
			return filesystem.SignedRequest{}, filesystem.Errorf(filesystem.CodeUnsupported, "s3: a presigned PUT cannot pin checksum algorithm %s; upload through Write", ws.checksum)
		}
		in := &s3.PutObjectInput{Bucket: d.bucket(), Key: aws.String(key)}
		if opts.ContentType != "" {
			in.ContentType = aws.String(opts.ContentType)
		}
		h := opts.Headers
		in.CacheControl, in.ContentDisposition, in.ContentEncoding = optional(h.CacheControl), optional(h.ContentDisposition), optional(h.ContentEncoding)
		// The disk's encryption is signed too, so a direct upload is
		// encrypted the same way as one through Write.
		in.ServerSideEncryption, in.SSEKMSKeyId = ws.sse, ws.kmsKey
		in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5 = c.algorithm, c.key, c.keyMD5
		// So are the per-write settings Write applies (storage class,
		// tags, object lock), so a direct upload cannot skip a retention.
		in.StorageClass, in.Tagging = ws.class, ws.tagging
		in.ObjectLockMode, in.ObjectLockRetainUntilDate, in.ObjectLockLegalHoldStatus = ws.lockMode, ws.retainUntil, ws.legalHold
		acl, aerr := d.presignACL(opts.Visibility)
		if aerr != nil {
			return filesystem.SignedRequest{}, aerr
		}
		in.ACL = acl
		req, err = d.presigner.PresignPutObject(ctx, in, expires)
	} else {
		in := &s3.GetObjectInput{Bucket: d.bucket(), Key: aws.String(key), SSECustomerAlgorithm: c.algorithm, SSECustomerKey: c.key, SSECustomerKeyMD5: c.keyMD5}
		if opts.ContentType != "" {
			in.ResponseContentType = aws.String(opts.ContentType)
		}
		if opts.ContentDisposition != "" {
			in.ResponseContentDisposition = aws.String(opts.ContentDisposition)
		}
		req, err = d.presigner.PresignGetObject(ctx, in, expires)
	}
	if err != nil {
		return filesystem.SignedRequest{}, mapErr(err)
	}
	var pinned filesystem.HTTPHeaders
	if opts.Method == filesystem.MethodPut {
		pinned = opts.Headers
	}
	return signed(req, opts.Method, d.now().Add(opts.Expires), pinned), nil
}

// presignACL is the canned ACL a presigned PUT or POST form pins for the
// visibility the Disk passed (PresignOptions.Visibility,
// PostPolicy.Visibility): none for "", which leaves the bucket's default
// exactly as before visibility could be asked for, and public-read or the
// disk's private ACL in ACL mode. Outside ACL mode the bucket refuses
// ACLs (bucket owner enforced), so a visibility is CodeUnsupported rather
// than a request S3 would reject at upload time; the Disk never passes
// one then, because the driver declares no PresignVisibility.
func (d *Driver) presignACL(v filesystem.Visibility) (types.ObjectCannedACL, error) {
	if v == "" {
		return "", nil
	}
	if d.cfg.Visibility != VisibilityACL {
		return "", filesystem.Errorf(filesystem.CodeUnsupported, "s3: a presigned upload can pin a visibility only in ACL mode; this bucket's ACLs are off (bucket owner enforced)")
	}
	return d.acl(v), nil
}

// optional is nil for an empty string, so an unset header is not signed.
func optional(v string) *string {
	if v == "" {
		return nil
	}
	return aws.String(v)
}

// signed converts the SDK's presigned request, dropping the Host header
// (clients set it from the URL). pinned are the stored headers a PUT signs:
// SigV4 already lists them in SignedHeader, and setting them from the
// options as well keeps the contract independent of the SDK.
func signed(req *v4.PresignedHTTPRequest, method filesystem.HTTPMethod, expiresAt time.Time, pinned filesystem.HTTPHeaders) filesystem.SignedRequest {
	h := http.Header{}
	for k, v := range req.SignedHeader {
		if http.CanonicalHeaderKey(k) != "Host" {
			h[http.CanonicalHeaderKey(k)] = v
		}
	}
	pinned.SetOn(h)
	if len(h) == 0 {
		h = nil
	}
	return filesystem.SignedRequest{Method: method, URL: req.URL, Header: h, ExpiresAt: expiresAt}
}
