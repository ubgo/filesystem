package s3

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
)

// CreateMultipart implements filesystem.MultipartUploader. A conditional
// upload (IfNotExists) is refused with CodeUnsupported: S3 checks the
// condition at completion, where the driver no longer has the options, and
// completing unconditionally would silently overwrite. (Write honours
// IfNotExists for bodies of any size.) The context's RequestOptions apply
// as for Write; a ChecksumAlgorithm chosen there must be on the context of
// every UploadPart too, or S3 refuses the parts.
func (d *Driver) CreateMultipart(ctx context.Context, key string, opts filesystem.WriteOptions) (string, error) {
	if opts.IfNotExists {
		return "", filesystem.Errorf(filesystem.CodeUnsupported, "s3: conditional multipart uploads are not supported; use Write")
	}
	if d.cfg.ObjectLock {
		if err := d.refuseHeld(ctx, key); err != nil {
			return "", err
		}
	}
	ws, err := d.writeSettings(ctx, key)
	if err != nil {
		return "", err
	}
	in := &s3.CreateMultipartUploadInput{
		Bucket:                    d.bucket(),
		Key:                       aws.String(key),
		Metadata:                  userMetadata(opts.Metadata),
		ACL:                       d.acl(opts.Visibility),
		ServerSideEncryption:      ws.sse,
		SSEKMSKeyId:               ws.kmsKey,
		StorageClass:              ws.class,
		Tagging:                   ws.tagging,
		ObjectLockMode:            ws.lockMode,
		ObjectLockRetainUntilDate: ws.retainUntil,
		ObjectLockLegalHoldStatus: ws.legalHold,
		ChecksumAlgorithm:         ws.checksum,
		SSECustomerAlgorithm:      ws.ssec.algorithm,
		SSECustomerKey:            ws.ssec.key,
		SSECustomerKeyMD5:         ws.ssec.keyMD5,
	}
	if opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	in.CacheControl, in.ContentDisposition, in.ContentEncoding = headerPointers(opts.Headers)
	out, err := d.client.CreateMultipartUpload(ctx, in)
	if err != nil {
		return "", mapErr(err)
	}
	return aws.ToString(out.UploadId), nil
}

// UploadPart implements filesystem.MultipartUploader. size must be the
// part's exact length: S3 needs it for a body it cannot seek.
func (d *Driver) UploadPart(ctx context.Context, key, uploadID string, number int, body io.Reader, size int64) (filesystem.Part, error) {
	c := d.cfg.SSECustomerKey.headers()
	out, err := d.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:               d.bucket(),
		Key:                  aws.String(key),
		UploadId:             aws.String(uploadID),
		PartNumber:           aws.Int32(int32(number)),
		Body:                 body,
		ContentLength:        aws.Int64(size),
		ChecksumAlgorithm:    d.checksumAlgorithm(ctx, key),
		SSECustomerAlgorithm: c.algorithm,
		SSECustomerKey:       c.key,
		SSECustomerKeyMD5:    c.keyMD5,
	})
	if err != nil {
		return filesystem.Part{}, mapErr(err)
	}
	return filesystem.Part{Number: number, ETag: strings.Trim(aws.ToString(out.ETag), `"`), Size: size}, nil
}

// PresignPart implements filesystem.PartPresigner. With an SSE-C key the
// request signs it, so the client must send SignedRequest.Header.
func (d *Driver) PresignPart(ctx context.Context, key, uploadID string, number int, expires time.Duration) (filesystem.SignedRequest, error) {
	c := d.cfg.SSECustomerKey.headers()
	req, err := d.presigner.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:               d.bucket(),
		Key:                  aws.String(key),
		UploadId:             aws.String(uploadID),
		PartNumber:           aws.Int32(int32(number)),
		SSECustomerAlgorithm: c.algorithm,
		SSECustomerKey:       c.key,
		SSECustomerKeyMD5:    c.keyMD5,
	}, func(o *s3.PresignOptions) { o.Expires = expires })
	if err != nil {
		return filesystem.SignedRequest{}, mapErr(err)
	}
	return signed(req, filesystem.MethodPut, d.now().Add(expires), filesystem.HTTPHeaders{}), nil
}

// ListParts implements filesystem.MultipartUploader, following S3's
// pagination to return every part.
func (d *Driver) ListParts(ctx context.Context, key, uploadID string) ([]filesystem.Part, error) {
	var parts []filesystem.Part
	p := s3.NewListPartsPaginator(d.client, &s3.ListPartsInput{Bucket: d.bucket(), Key: aws.String(key), UploadId: aws.String(uploadID)})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, mapErr(err)
		}
		for _, part := range out.Parts {
			parts = append(parts, filesystem.Part{
				Number: int(aws.ToInt32(part.PartNumber)),
				ETag:   strings.Trim(aws.ToString(part.ETag), `"`),
				Size:   aws.ToInt64(part.Size),
			})
		}
	}
	return parts, nil
}

// CompleteMultipart implements filesystem.MultipartUploader.
func (d *Driver) CompleteMultipart(ctx context.Context, key, uploadID string, parts []filesystem.Part) (filesystem.Entry, error) {
	completed := make([]types.CompletedPart, 0, len(parts))
	for _, p := range parts {
		completed = append(completed, types.CompletedPart{PartNumber: aws.Int32(int32(p.Number)), ETag: aws.String(`"` + p.ETag + `"`)})
	}
	if d.cfg.ObjectLock {
		if err := d.refuseHeld(ctx, key); err != nil {
			return filesystem.Entry{}, err
		}
	}
	c := d.cfg.SSECustomerKey.headers()
	_, err := d.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:               d.bucket(),
		Key:                  aws.String(key),
		UploadId:             aws.String(uploadID),
		MultipartUpload:      &types.CompletedMultipartUpload{Parts: completed},
		SSECustomerAlgorithm: c.algorithm,
		SSECustomerKey:       c.key,
		SSECustomerKeyMD5:    c.keyMD5,
	})
	if err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	return d.Stat(ctx, key)
}

// AbortMultipart implements filesystem.MultipartUploader. Aborting an
// unknown upload succeeds.
func (d *Driver) AbortMultipart(ctx context.Context, key, uploadID string) error {
	_, err := d.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: d.bucket(), Key: aws.String(key), UploadId: aws.String(uploadID)})
	if errCode(err) == errNoSuchUpload {
		return nil
	}
	return mapErr(err)
}

// SweepAbandoned implements filesystem.AbandonedSweeper (declared only with
// Config.SweepUploads): it aborts every multipart upload in the bucket
// that was initiated before before, freeing the parts S3 bills for. S3
// records when an upload started, not when its last part arrived, so the
// cutoff must be well beyond the longest upload the application allows.
// An upload completed or aborted while the sweep runs is skipped and not
// counted. It returns how many uploads it aborted.
func (d *Driver) SweepAbandoned(ctx context.Context, before time.Time) (int, error) {
	n := 0
	err := d.scanAbandoned(ctx, before, func(u types.MultipartUpload) error {
		_, err := d.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: d.bucket(), Key: u.Key, UploadId: u.UploadId})
		if errCode(err) == errNoSuchUpload {
			return nil
		}
		if err != nil {
			return mapErr(err)
		}
		n++
		return nil
	})
	return n, err
}
