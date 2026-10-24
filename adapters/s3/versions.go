package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
)

// Versions implements filesystem.VersionLister (declared with
// Config.Versioning) with ListObjectVersions, following its pagination.
// Versions of other keys sharing the prefix are skipped; delete markers
// are included, newest first.
func (d *Driver) Versions(ctx context.Context, key string) ([]filesystem.Version, error) {
	var vs []filesystem.Version
	p := s3.NewListObjectVersionsPaginator(d.client, &s3.ListObjectVersionsInput{Bucket: d.bucket(), Prefix: aws.String(key)})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, mapErr(err)
		}
		for _, v := range out.Versions {
			if aws.ToString(v.Key) == key {
				vs = append(vs, filesystem.Version{ID: aws.ToString(v.VersionId), Size: aws.ToInt64(v.Size), ModTime: aws.ToTime(v.LastModified), ETag: strings.Trim(aws.ToString(v.ETag), `"`), Latest: aws.ToBool(v.IsLatest)})
			}
		}
		for _, m := range out.DeleteMarkers {
			if aws.ToString(m.Key) == key {
				vs = append(vs, filesystem.Version{ID: aws.ToString(m.VersionId), ModTime: aws.ToTime(m.LastModified), Latest: aws.ToBool(m.IsLatest), DeleteMarker: true})
			}
		}
	}
	// S3 lists versions and delete markers separately; the latest goes
	// first even when its timestamp ties with an older one.
	slices.SortStableFunc(vs, func(a, b filesystem.Version) int {
		if a.Latest != b.Latest {
			if a.Latest {
				return -1
			}
			return 1
		}
		return b.ModTime.Compare(a.ModTime)
	})
	return vs, nil
}

// ReadVersion implements filesystem.VersionReader (declared with
// Config.Versioning) with a GetObject of that version, sending the disk's
// SSE-C key. A missing version, or a delete marker (S3 answers 405), is
// CodeNotFound.
func (d *Driver) ReadVersion(ctx context.Context, key, id string) (io.ReadCloser, error) {
	c := d.cfg.SSECustomerKey.headers()
	out, err := d.client.GetObject(ctx, &s3.GetObjectInput{Bucket: d.bucket(), Key: aws.String(key), VersionId: aws.String(id), SSECustomerAlgorithm: c.algorithm, SSECustomerKey: c.key, SSECustomerKeyMD5: c.keyMD5})
	if err != nil {
		return nil, versionErr(err)
	}
	return out.Body, nil
}

// RestoreVersion implements filesystem.VersionReader: it copies version id
// onto key, so its content becomes a new latest version and every version
// is kept. It undoes an overwrite, and a delete. It checks the version
// with a HEAD first, because S3 answers a copy from a delete marker with a
// generic InvalidRequest; a missing version or a delete marker is
// CodeNotFound, and with Config.ObjectLock a held key is refused with
// CodePermission. The copy gets the context's write settings (see Write);
// objects over 5 GiB fail with S3's copy limit.
func (d *Driver) RestoreVersion(ctx context.Context, key, id string) error {
	version := aws.String(id)
	if _, err := d.head(ctx, key, version); err != nil {
		return versionErr(err)
	}
	if d.cfg.ObjectLock {
		if err := d.refuseHeld(ctx, key); err != nil {
			return err
		}
	}
	ws, err := d.writeSettings(ctx, key)
	if err != nil {
		return err
	}
	_, err = d.copyObject(ctx, key, version, key, ws)
	return err
}

// versionErr classifies an error about one version: a delete marker, which
// S3 answers with 405 Method Not Allowed (HEAD has no body to carry an
// error code, so the status decides), is CodeNotFound like a missing
// version (NoSuchVersion, a 404 that mapErr already maps).
func versionErr(err error) error {
	var resp *awshttp.ResponseError
	if errors.As(err, &resp) && httpResponse(resp) != nil && httpResponse(resp).StatusCode == http.StatusMethodNotAllowed {
		return filesystem.NewError(filesystem.CodeNotFound, err)
	}
	return mapErr(err)
}

// errNoLockConfiguration is S3's answer for a bucket without Object Lock.
const errNoLockConfiguration = "ObjectLockConfigurationNotFoundError"

// CheckBucket verifies that the bucket has what the Config declares about
// it: versioning enabled for Versioning, Object Lock enabled for
// ObjectLock. A declaration the bucket does not back is CodeInvalidArgument,
// so a disk never claims Versions or Hold on a bucket that would silently
// keep no history or refuse every hold. New makes no request, so call it
// once after New (the Opener does, for the versioning and object_lock
// parameters); it needs s3:GetBucketVersioning and
// s3:GetBucketObjectLockConfiguration.
func (d *Driver) CheckBucket(ctx context.Context) error {
	if d.cfg.Versioning {
		out, err := d.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: d.bucket()})
		if err != nil {
			return mapErr(err)
		}
		if out.Status != types.BucketVersioningStatusEnabled {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: Versioning is declared but bucket %s has versioning %q", d.cfg.Bucket, out.Status)
		}
	}
	if d.cfg.ObjectLock {
		out, err := d.client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: d.bucket()})
		if err != nil && errCode(err) != errNoLockConfiguration {
			return mapErr(err)
		}
		if err != nil || out.ObjectLockConfiguration == nil || out.ObjectLockConfiguration.ObjectLockEnabled != types.ObjectLockEnabledEnabled {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: ObjectLock is declared but bucket %s does not have Object Lock enabled", d.cfg.Bucket)
		}
	}
	return nil
}
