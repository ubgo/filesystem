package s3

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	tmtypes "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"

	"github.com/ubgo/filesystem"
)

// allUsersGroup is the grantee URI of a public-read ACL.
const allUsersGroup = "http://acs.amazonaws.com/groups/global/AllUsers"

// Stat implements filesystem.Driver. In ACL mode it also reads the object's
// ACL, one extra request, to report visibility. It reports the storage
// class, and adds the S3 state Entry has no field for to Metadata under
// MetaPrefix keys.
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	out, err := d.head(ctx, key, nil)
	if err != nil {
		return filesystem.Entry{}, err
	}
	e := filesystem.Entry{
		Key:         key,
		Size:        aws.ToInt64(out.ContentLength),
		ModTime:     aws.ToTime(out.LastModified),
		ContentType: aws.ToString(out.ContentType),
		ETag:        strings.Trim(aws.ToString(out.ETag), `"`),
		Metadata:    d.stateMetadata(ctx, key, out),
		// AWS omits STANDARD, so a standard object reports "".
		StorageClass: string(out.StorageClass),
		Headers: filesystem.HTTPHeaders{
			CacheControl:       aws.ToString(out.CacheControl),
			ContentDisposition: aws.ToString(out.ContentDisposition),
			ContentEncoding:    aws.ToString(out.ContentEncoding),
		},
	}
	if d.cfg.Visibility == VisibilityACL {
		if e.Visibility, err = d.visibility(ctx, key); err != nil {
			return filesystem.Entry{}, err
		}
	}
	return e, nil
}

// head is HeadObject of key (of version, when not nil), with the disk's
// SSE-C key, asking for the stored checksum when one is configured.
func (d *Driver) head(ctx context.Context, key string, version *string) (*s3.HeadObjectOutput, error) {
	c := d.cfg.SSECustomerKey.headers()
	in := &s3.HeadObjectInput{Bucket: d.bucket(), Key: aws.String(key), VersionId: version, SSECustomerAlgorithm: c.algorithm, SSECustomerKey: c.key, SSECustomerKeyMD5: c.keyMD5}
	if d.checksumAlgorithm(ctx, key) != "" {
		in.ChecksumMode = types.ChecksumModeEnabled
	}
	out, err := d.client.HeadObject(ctx, in, withEmptyLockDateDropped)
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// visibility reads an object's ACL: public when AllUsers may read it.
func (d *Driver) visibility(ctx context.Context, key string) (filesystem.Visibility, error) {
	acl, err := d.client.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: d.bucket(), Key: aws.String(key)})
	if err != nil {
		return "", mapErr(err)
	}
	for _, g := range acl.Grants {
		if g.Grantee != nil && aws.ToString(g.Grantee.URI) == allUsersGroup && (g.Permission == types.PermissionRead || g.Permission == types.PermissionFullControl) {
			return filesystem.VisibilityPublic, nil
		}
	}
	return filesystem.VisibilityPrivate, nil
}

// Read implements filesystem.Driver with a ranged GetObject. A range that
// starts past the end is answered by S3 with InvalidRange; the contract says
// that is an empty read, so it becomes one (after confirming the object
// exists).
//
// An archived object that is not restored fails with CodeConflict and
// ErrArchived.
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	c := d.cfg.SSECustomerKey.headers()
	in := &s3.GetObjectInput{Bucket: d.bucket(), Key: aws.String(key), SSECustomerAlgorithm: c.algorithm, SSECustomerKey: c.key, SSECustomerKeyMD5: c.keyMD5}
	if r := rangeHeader(rng); r != "" {
		in.Range = aws.String(r)
	}
	out, err := d.client.GetObject(ctx, in)
	if errCode(err) == errInvalidRange {
		if _, serr := d.Stat(ctx, key); serr != nil {
			return nil, serr
		}
		return io.NopCloser(strings.NewReader("")), nil
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return out.Body, nil
}

// rangeHeader renders an HTTP Range header, or "" for the whole object.
func rangeHeader(rng filesystem.ByteRange) string {
	switch {
	case rng.Offset == 0 && rng.Length == 0:
		return ""
	case rng.Length == 0:
		return "bytes=" + strconv.FormatInt(rng.Offset, 10) + "-"
	}
	return "bytes=" + strconv.FormatInt(rng.Offset, 10) + "-" + strconv.FormatInt(rng.Offset+rng.Length-1, 10)
}

// maxSinglePut is S3's limit on one PutObject: 5 GiB.
const maxSinglePut = 5 << 30

// Write implements filesystem.Driver through the transfer manager, which
// sends small bodies as one PUT and streams large or unknown-length bodies
// as a multipart upload. A write with WriteOptions.SHA256 and a known size
// (SizeHint, up to maxSinglePut) is one PutObject that S3 verifies (see
// putVerified); S3's multipart checksums are per part, so a body of
// unknown size or over 5 GiB is checked by the Disk alone, as is a write
// whose ChecksumAlgorithm is another one (S3 keeps one checksum per
// object). The context's RequestOptions add tags, storage class, object
// lock and checksum settings (see writeSettings). With Config.ObjectLock
// an overwrite of a held object is refused with CodePermission before any
// byte is sent, as Delete is: S3 would keep the held version but make a
// new, unprotected one current.
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	if d.cfg.ObjectLock {
		if err := d.refuseHeld(ctx, key); err != nil {
			return filesystem.Entry{}, err
		}
	}
	ws, err := d.writeSettings(ctx, key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	if opts.SHA256 != "" && opts.SizeHint > 0 && opts.SizeHint <= maxSinglePut && (ws.checksum == "" || ws.checksum == types.ChecksumAlgorithmSha256) {
		return d.putVerified(ctx, key, body, opts, ws)
	}
	in := &transfermanager.UploadObjectInput{
		Bucket:   d.bucket(),
		Key:      aws.String(key),
		Body:     body,
		Metadata: userMetadata(opts.Metadata),
		// The transfer manager has its own copies of the S3 enum types;
		// the wire values are identical.
		ACL:                       tmtypes.ObjectCannedACL(d.acl(opts.Visibility)),
		ServerSideEncryption:      tmtypes.ServerSideEncryption(ws.sse),
		SSEKMSKeyID:               ws.kmsKey,
		StorageClass:              tmtypes.StorageClass(ws.class),
		Tagging:                   ws.tagging,
		ObjectLockMode:            tmtypes.ObjectLockMode(ws.lockMode),
		ObjectLockRetainUntilDate: ws.retainUntil,
		ObjectLockLegalHoldStatus: tmtypes.ObjectLockLegalHoldStatus(ws.legalHold),
		ChecksumAlgorithm:         tmtypes.ChecksumAlgorithm(ws.checksum),
		SSECustomerAlgorithm:      ws.ssec.algorithm,
		SSECustomerKey:            ws.ssec.key,
		SSECustomerKeyMD5:         ws.ssec.keyMD5,
	}
	if opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	in.CacheControl, in.ContentDisposition, in.ContentEncoding = headerPointers(opts.Headers)
	if opts.IfNotExists {
		in.IfNoneMatch = aws.String("*")
	}
	if _, err := d.transfer.UploadObject(ctx, in); err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	return d.Stat(ctx, key)
}

// putVerified writes body with one PutObject that S3 checks against
// opts.SHA256 twice: the SigV4 payload hash is that digest
// (x-amz-content-sha256, so the SDK does not read the body to hash it,
// and S3 refuses a body whose hash differs), and x-amz-checksum-sha256
// carries it too, which S3 stores and returns as the object's checksum.
func (d *Driver) putVerified(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions, ws writeSettings) (filesystem.Entry, error) {
	sum, err := hex.DecodeString(opts.SHA256)
	if err != nil || len(sum) != sha256.Size {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: SHA256 %q is not a hex SHA-256", opts.SHA256)
	}
	in := &s3.PutObjectInput{
		Bucket:                    d.bucket(),
		Key:                       aws.String(key),
		Body:                      body,
		ContentLength:             aws.Int64(opts.SizeHint),
		ChecksumSHA256:            aws.String(base64.StdEncoding.EncodeToString(sum)),
		Metadata:                  userMetadata(opts.Metadata),
		ACL:                       d.acl(opts.Visibility),
		ServerSideEncryption:      ws.sse,
		SSEKMSKeyId:               ws.kmsKey,
		StorageClass:              ws.class,
		Tagging:                   ws.tagging,
		ObjectLockMode:            ws.lockMode,
		ObjectLockRetainUntilDate: ws.retainUntil,
		ObjectLockLegalHoldStatus: ws.legalHold,
		SSECustomerAlgorithm:      ws.ssec.algorithm,
		SSECustomerKey:            ws.ssec.key,
		SSECustomerKeyMD5:         ws.ssec.keyMD5,
	}
	if opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	in.CacheControl, in.ContentDisposition, in.ContentEncoding = headerPointers(opts.Headers)
	if opts.IfNotExists {
		in.IfNoneMatch = aws.String("*")
	}
	if _, err := d.client.PutObject(ctx, in, withPayloadHash(opts.SHA256)); err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	return d.Stat(ctx, key)
}

// payloadHashMiddlewareID is the SDK's middleware that hashes the body for
// SigV4 ("ComputePayloadHash"); withPayloadHash replaces it.
const payloadHashMiddlewareID = "ComputePayloadHash"

// knownPayloadHash is a SigV4 payload hash known in advance: it stands in
// for the SDK's middleware, which would read (and need to rewind) the body
// to compute the same value.
type knownPayloadHash string

// ID implements middleware.FinalizeMiddleware under the replaced ID.
func (knownPayloadHash) ID() string { return payloadHashMiddlewareID }

// HandleFinalize implements middleware.FinalizeMiddleware.
func (h knownPayloadHash) HandleFinalize(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
	return next.HandleFinalize(v4.SetPayloadHash(ctx, string(h)), in)
}

// withPayloadHash makes one call sign hexSHA256 as its payload hash.
func withPayloadHash(hexSHA256 string) func(*s3.Options) {
	return func(o *s3.Options) {
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			_, err := stack.Finalize.Swap(payloadHashMiddlewareID, knownPayloadHash(hexSHA256))
			return err
		})
	}
}

// headerPointers converts headers to the SDK's optional fields, leaving
// unset ones nil so S3 stores nothing for them.
func headerPointers(h filesystem.HTTPHeaders) (cacheControl, disposition, encoding *string) {
	optional := func(v string) *string {
		if v == "" {
			return nil
		}
		return aws.String(v)
	}
	return optional(h.CacheControl), optional(h.ContentDisposition), optional(h.ContentEncoding)
}

// Delete implements filesystem.Driver. S3 deletes of a missing key succeed.
// With Config.ObjectLock, a held object is refused with CodePermission
// (see refuseHeld).
func (d *Driver) Delete(ctx context.Context, key string) error {
	if d.cfg.ObjectLock {
		if err := d.refuseHeld(ctx, key); err != nil {
			return err
		}
	}
	_, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: d.bucket(), Key: aws.String(key)})
	return mapErr(err)
}

// List implements filesystem.Driver with ListObjectsV2. The cursor is S3's
// continuation token, opaque to callers. Directories are S3 common prefixes.
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	in := &s3.ListObjectsV2Input{Bucket: d.bucket(), MaxKeys: aws.Int32(int32(min(opts.Limit, filesystem.DefaultPageSize)))}
	if opts.Limit <= 0 {
		in.MaxKeys = aws.Int32(filesystem.DefaultPageSize)
	}
	if dir != "" {
		in.Prefix = aws.String(dir + "/")
	}
	if !opts.Recursive {
		in.Delimiter = aws.String("/")
	}
	if opts.Cursor != "" {
		in.ContinuationToken = aws.String(opts.Cursor)
	}
	out, err := d.client.ListObjectsV2(ctx, in)
	if err != nil {
		return filesystem.Page{}, mapErr(err)
	}
	entries := make([]filesystem.Entry, 0, len(out.Contents)+len(out.CommonPrefixes))
	for _, o := range out.Contents {
		// A key ending in "/" is a directory marker some tools and stores
		// create; keys in this library never end in "/", so it is not an
		// object and is not listed.
		if strings.HasSuffix(aws.ToString(o.Key), "/") {
			continue
		}
		entries = append(entries, filesystem.Entry{
			Key:     aws.ToString(o.Key),
			Size:    aws.ToInt64(o.Size),
			ModTime: aws.ToTime(o.LastModified),
			ETag:    strings.Trim(aws.ToString(o.ETag), `"`),
		})
	}
	for _, p := range out.CommonPrefixes {
		if d.cfg.PruneEmptyPrefixes {
			empty, err := d.emptyPrefix(ctx, aws.ToString(p.Prefix))
			if err != nil {
				return filesystem.Page{}, err
			}
			if empty {
				continue
			}
		}
		entries = append(entries, filesystem.Entry{Key: strings.TrimSuffix(aws.ToString(p.Prefix), "/"), IsDir: true})
	}
	slices.SortFunc(entries, func(a, b filesystem.Entry) int { return strings.Compare(a.Key, b.Key) })
	page := filesystem.Page{Entries: entries}
	if aws.ToBool(out.IsTruncated) {
		page.NextCursor = aws.ToString(out.NextContinuationToken)
	}
	return page, nil
}

// emptyPrefix reports whether no object key starts with prefix. Keys that
// end in "/" are directory markers some stores create, not objects.
func (d *Driver) emptyPrefix(ctx context.Context, prefix string) (bool, error) {
	p := s3.NewListObjectsV2Paginator(d.client, &s3.ListObjectsV2Input{Bucket: d.bucket(), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return false, mapErr(err)
		}
		for _, o := range out.Contents {
			if !strings.HasSuffix(aws.ToString(o.Key), "/") {
				return false, nil
			}
		}
	}
	return true, nil
}

// copySource renders the CopySource header: bucket and key, URL-escaped,
// and the source version when version is not nil.
func (d *Driver) copySource(key string, version *string) *string {
	src := url.PathEscape(d.cfg.Bucket) + "/" + filesystem.EscapeKey(key)
	if version != nil {
		src += "?versionId=" + url.QueryEscape(*version)
	}
	return aws.String(src)
}

// Copy implements filesystem.Copier with CopyObject, which keeps the
// metadata and content type. S3 limits a single copy to 5 GiB; larger
// objects fail with the provider's error. In ACL mode the source's
// visibility is carried over, because S3 does not copy ACLs. The copy gets
// the context's write settings (see Write); tags are copied unless
// RequestOptions.Tags replaces them. With Config.ObjectLock a held dst is
// refused with CodePermission, as Write refuses it.
func (d *Driver) Copy(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	if d.cfg.ObjectLock {
		if err := d.refuseHeld(ctx, dst); err != nil {
			return filesystem.Entry{}, err
		}
	}
	ws, err := d.writeSettings(ctx, dst)
	if err != nil {
		return filesystem.Entry{}, err
	}
	return d.copyObject(ctx, src, nil, dst, ws)
}

// copyObject copies src (at version, nil for the latest) to dst with ws,
// which may be src itself: S3 then rewrites the object in place, which is
// how SetStorageClass and RestoreVersion work.
func (d *Driver) copyObject(ctx context.Context, src string, version *string, dst string, ws writeSettings) (filesystem.Entry, error) {
	in := &s3.CopyObjectInput{
		Bucket:                         d.bucket(),
		Key:                            aws.String(dst),
		CopySource:                     d.copySource(src, version),
		ServerSideEncryption:           ws.sse,
		SSEKMSKeyId:                    ws.kmsKey,
		StorageClass:                   ws.class,
		ObjectLockMode:                 ws.lockMode,
		ObjectLockRetainUntilDate:      ws.retainUntil,
		ObjectLockLegalHoldStatus:      ws.legalHold,
		ChecksumAlgorithm:              ws.checksum,
		SSECustomerAlgorithm:           ws.ssec.algorithm,
		SSECustomerKey:                 ws.ssec.key,
		SSECustomerKeyMD5:              ws.ssec.keyMD5,
		CopySourceSSECustomerAlgorithm: ws.ssec.algorithm,
		CopySourceSSECustomerKey:       ws.ssec.key,
		CopySourceSSECustomerKeyMD5:    ws.ssec.keyMD5,
	}
	if ws.tagging != nil {
		in.TaggingDirective, in.Tagging = types.TaggingDirectiveReplace, ws.tagging
	}
	if d.cfg.Visibility == VisibilityACL {
		v, err := d.visibility(ctx, src)
		if err != nil {
			return filesystem.Entry{}, err
		}
		in.ACL = d.acl(v)
	}
	if _, err := d.client.CopyObject(ctx, in); err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	return d.Stat(ctx, dst)
}

// SetVisibility implements filesystem.VisibilitySetter in ACL mode.
func (d *Driver) SetVisibility(ctx context.Context, key string, v filesystem.Visibility) error {
	_, err := d.client.PutObjectAcl(ctx, &s3.PutObjectAclInput{Bucket: d.bucket(), Key: aws.String(key), ACL: d.acl(v)})
	return mapErr(err)
}

// deleteBatch is the most keys DeleteObjects accepts in one request.
const deleteBatch = 1000

// batchCodes classifies the per-key error codes of DeleteObjects; any
// other code is CodeUnknown.
var batchCodes = map[string]filesystem.Code{
	"AccessDenied":       filesystem.CodePermission,
	"SlowDown":           filesystem.CodeUnavailable,
	"InternalError":      filesystem.CodeUnavailable,
	"ServiceUnavailable": filesystem.CodeUnavailable,
}

// deleteObjects deletes up to deleteBatch keys in one request and returns
// the keys S3 reported as not deleted. err is the request's own failure.
// With Config.ObjectLock, held keys are reported as failed with
// CodePermission and not sent (see refuseHeld).
func (d *Driver) deleteObjects(ctx context.Context, keys []string) ([]filesystem.KeyError, error) {
	var failed []filesystem.KeyError
	if d.cfg.ObjectLock {
		held, err := d.heldKeys(ctx, keys)
		if err != nil {
			return nil, err
		}
		free := keys[:0:0]
		for _, k := range keys {
			if e, ok := held[k]; ok {
				failed = append(failed, filesystem.KeyError{Key: k, Err: e})
				continue
			}
			free = append(free, k)
		}
		if keys = free; len(keys) == 0 {
			return failed, nil
		}
	}
	ids := make([]types.ObjectIdentifier, len(keys))
	for i, k := range keys {
		ids[i] = types.ObjectIdentifier{Key: aws.String(k)}
	}
	out, err := d.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: d.bucket(), Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)}})
	if err != nil {
		return nil, mapErr(err)
	}
	for _, e := range out.Errors {
		code, ok := batchCodes[aws.ToString(e.Code)]
		if !ok {
			code = filesystem.CodeUnknown
		}
		failed = append(failed, filesystem.KeyError{Key: aws.ToString(e.Key), Err: filesystem.Errorf(code, "s3: %s %s", aws.ToString(e.Code), aws.ToString(e.Message))})
	}
	return failed, nil
}

// DeleteKeys implements filesystem.KeysDeleter with DeleteObjects, 1,000
// keys per request. A request that fails reports its keys and every later
// one, since earlier requests did delete theirs.
func (d *Driver) DeleteKeys(ctx context.Context, keys []string) error {
	var failed []filesystem.KeyError
	for start := 0; start < len(keys); start += deleteBatch {
		chunk := keys[start:min(start+deleteBatch, len(keys))]
		bad, err := d.deleteObjects(ctx, chunk)
		if err != nil {
			for _, k := range keys[start:] {
				failed = append(failed, filesystem.KeyError{Key: k, Err: err})
			}
			break
		}
		failed = append(failed, bad...)
	}
	if failed != nil {
		return &filesystem.BatchError{Failures: failed}
	}
	return nil
}

// DeletePrefix implements filesystem.PrefixDeleter: list everything under
// dir and delete it in batches of 1,000 keys per request. Keys S3 refuses
// come back in a *filesystem.BatchError.
func (d *Driver) DeletePrefix(ctx context.Context, dir string) error {
	cursor := ""
	for {
		page, err := d.List(ctx, dir, filesystem.ListOptions{Recursive: true, Cursor: cursor, Limit: deleteBatch})
		if err != nil {
			return err
		}
		if len(page.Entries) > 0 {
			keys := make([]string, len(page.Entries))
			for i, e := range page.Entries {
				keys[i] = e.Key
			}
			failed, err := d.deleteObjects(ctx, keys)
			if err != nil {
				return err
			}
			if failed != nil {
				return &filesystem.BatchError{Failures: failed}
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
}
