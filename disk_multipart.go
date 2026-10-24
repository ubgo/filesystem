package filesystem

import (
	"context"
	"io"
	"time"
)

// multipart returns the driver's MultipartUploader after checking the
// capability and cleaning the key.
func (d *Disk) multipart(kind OpKind, key string) (MultipartUploader, string, error) {
	k, err := d.CleanKey(key)
	if err != nil {
		return nil, "", annotate(err, kind, d.name, key)
	}
	if !d.caps.Multipart {
		return nil, "", d.unsupported(kind, k)
	}
	return d.driver.(MultipartUploader), k, nil
}

// MinPartNumber and MaxPartNumber bound part numbers. They are S3's limits,
// adopted by every S3-compatible backend and by browser uploaders.
const (
	MinPartNumber = 1
	MaxPartNumber = 10000
)

func validPart(kind OpKind, d *Disk, k string, number int) error {
	if number < MinPartNumber || number > MaxPartNumber {
		return annotate(Errorf(CodeInvalidArgument, "part number %d is not in [%d, %d]", number, MinPartNumber, MaxPartNumber), kind, d.name, k)
	}
	return nil
}

func validUploadID(kind OpKind, d *Disk, k, uploadID string) error {
	if uploadID == "" {
		return annotate(Errorf(CodeInvalidArgument, "empty upload ID"), kind, d.name, k)
	}
	return nil
}

// CreateMultipart starts a multipart upload of key and returns its upload
// ID. Metadata, content type and visibility are taken from opts and checked
// against the capabilities as for Write. Needs the Multipart capability.
func (d *Disk) CreateMultipart(ctx context.Context, key string, opts WriteOptions) (string, error) {
	mp, k, err := d.multipart(OpMultipartCreate, key)
	if err != nil {
		return "", err
	}
	if err := d.prepareWrite(&opts); err != nil {
		return "", annotate(err, OpMultipartCreate, d.name, k)
	}
	if opts.IfMatch != "" {
		return "", annotate(Errorf(CodeUnsupported, "IfMatch applies to Write, not to multipart uploads"), OpMultipartCreate, d.name, k)
	}
	if opts.SHA256 != "" {
		// A multipart object's checksum is not the whole body's SHA-256 on
		// the backends that have one; check the assembled object instead.
		return "", annotate(Errorf(CodeUnsupported, "SHA256 applies to Write, not to multipart uploads"), OpMultipartCreate, d.name, k)
	}
	if len(opts.Checksums) > 0 {
		// Parts arrive separately, may be retried and may go straight to
		// the bucket through PresignPart, so the Disk never sees the whole
		// body in order, and no backend verifies a caller-chosen hash
		// across parts. Computing them now would report digests nobody
		// checked; refuse instead and point at the path that can.
		return "", annotate(Errorf(CodeUnsupported, "Checksums cannot be verified across multipart parts: compute them from the assembled object after CompleteMultipart (upload.Confirm with upload.Policy.Checksums does), or use Write"), OpMultipartCreate, d.name, k)
	}
	var id string
	op := &Op{Kind: OpMultipartCreate, Key: k, Mutating: true, Replayable: false}
	err = d.do(ctx, op, func(ctx context.Context) error {
		var err error
		id, err = mp.CreateMultipart(ctx, k, opts)
		return err
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// UploadPart uploads one part through the server. A retry interceptor may
// retry it only when body implements io.Seeker and can actually seek; a
// body whose Seek fails (an *os.File on a pipe) is uploaded once as a
// stream, as in Write.
func (d *Disk) UploadPart(ctx context.Context, key, uploadID string, number int, body io.Reader, size int64) (Part, error) {
	mp, k, err := d.multipart(OpMultipartUpload, key)
	if err != nil {
		return Part{}, err
	}
	if err := validUploadID(OpMultipartUpload, d, k, uploadID); err != nil {
		return Part{}, err
	}
	if err := validPart(OpMultipartUpload, d, k, number); err != nil {
		return Part{}, err
	}
	seeker, seekable := body.(io.Seeker)
	var start int64
	if seekable {
		if start, err = seeker.Seek(0, io.SeekCurrent); err != nil {
			seekable = false
		}
	}
	var part Part
	op := &Op{Kind: OpMultipartUpload, Key: k, Mutating: true, Replayable: seekable}
	err = d.do(ctx, op, func(ctx context.Context) error {
		if op.Attempt > 1 && seekable {
			if _, err := seeker.Seek(start, io.SeekStart); err != nil {
				return err
			}
		}
		var err error
		part, err = mp.UploadPart(ctx, k, uploadID, number, op.wrapBody(body), size)
		return err
	})
	if err != nil {
		return Part{}, err
	}
	return part, nil
}

// PresignPart returns a signed PUT for one part, for clients that upload
// parts directly to the backend. Needs the PresignPart capability.
func (d *Disk) PresignPart(ctx context.Context, key, uploadID string, number int, expires time.Duration) (SignedRequest, error) {
	k, err := d.CleanKey(key)
	if err != nil {
		return SignedRequest{}, annotate(err, OpMultipartPresign, d.name, key)
	}
	if !d.caps.PresignPart {
		return SignedRequest{}, d.unsupported(OpMultipartPresign, k)
	}
	pp := d.driver.(PartPresigner)
	if err := validUploadID(OpMultipartPresign, d, k, uploadID); err != nil {
		return SignedRequest{}, err
	}
	if err := validPart(OpMultipartPresign, d, k, number); err != nil {
		return SignedRequest{}, err
	}
	if err := validateExpiry(expires); err != nil {
		return SignedRequest{}, annotate(err, OpMultipartPresign, d.name, k)
	}
	var req SignedRequest
	op := &Op{Kind: OpMultipartPresign, Key: k, Mutating: true, Replayable: true}
	err = d.do(ctx, op, func(ctx context.Context) error {
		var err error
		req, err = pp.PresignPart(ctx, k, uploadID, number, expires)
		return err
	})
	if err != nil {
		return SignedRequest{}, err
	}
	return req, nil
}

// ListParts lists the parts uploaded so far, in part-number order, so a
// client can resume an interrupted upload.
func (d *Disk) ListParts(ctx context.Context, key, uploadID string) ([]Part, error) {
	mp, k, err := d.multipart(OpMultipartListParts, key)
	if err != nil {
		return nil, err
	}
	if err := validUploadID(OpMultipartListParts, d, k, uploadID); err != nil {
		return nil, err
	}
	var parts []Part
	op := &Op{Kind: OpMultipartListParts, Key: k, Replayable: true}
	err = d.do(ctx, op, func(ctx context.Context) error {
		var err error
		parts, err = mp.ListParts(ctx, k, uploadID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return parts, nil
}

// CompleteMultipart assembles the parts into the final object. parts must
// list every part with the ETag the backend returned for it.
func (d *Disk) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) (Entry, error) {
	mp, k, err := d.multipart(OpMultipartComplete, key)
	if err != nil {
		return Entry{}, err
	}
	if err := validUploadID(OpMultipartComplete, d, k, uploadID); err != nil {
		return Entry{}, err
	}
	if len(parts) == 0 {
		return Entry{}, annotate(Errorf(CodeInvalidArgument, "no parts to complete"), OpMultipartComplete, d.name, k)
	}
	var e Entry
	op := &Op{Kind: OpMultipartComplete, Key: k, Mutating: true, Replayable: false}
	err = d.do(ctx, op, func(ctx context.Context) error {
		var err error
		e, err = mp.CompleteMultipart(ctx, k, uploadID, parts)
		return err
	})
	if err != nil {
		return Entry{}, err
	}
	e.Key = k
	return e, nil
}

// AbortMultipart discards an unfinished upload and its parts. Aborting an
// unknown upload succeeds, so it is safe in cleanup paths.
func (d *Disk) AbortMultipart(ctx context.Context, key, uploadID string) error {
	mp, k, err := d.multipart(OpMultipartAbort, key)
	if err != nil {
		return err
	}
	if err := validUploadID(OpMultipartAbort, d, k, uploadID); err != nil {
		return err
	}
	op := &Op{Kind: OpMultipartAbort, Key: k, Mutating: true, Replayable: true}
	return d.do(ctx, op, func(ctx context.Context) error {
		return mp.AbortMultipart(ctx, k, uploadID)
	})
}
