package s3

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
)

// FindAbandoned implements filesystem.AbandonedFinder, declared with
// SweepAbandoned (Config.SweepUploads): the multipart uploads in the
// bucket initiated before before, which SweepAbandoned would abort, with
// the key each was started for. It aborts nothing. S3 records when an
// upload started, not when its last part arrived, so that is
// LastActivity.
func (d *Driver) FindAbandoned(ctx context.Context, before time.Time) ([]filesystem.Abandoned, error) {
	var found []filesystem.Abandoned
	err := d.scanAbandoned(ctx, before, func(u types.MultipartUpload) error {
		found = append(found, filesystem.Abandoned{
			Kind:         filesystem.AbandonedMultipart,
			ID:           aws.ToString(u.UploadId),
			Key:          aws.ToString(u.Key),
			LastActivity: aws.ToTime(u.Initiated),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// scanAbandoned calls visit for every multipart upload in the bucket
// initiated before before, page by page; an error from visit ends the
// scan. It is the one definition of an abandoned upload both
// SweepAbandoned and FindAbandoned use, so the preview lists exactly what
// a sweep would abort. An upload without an initiation time is never
// abandoned: the sweep cannot tell its age.
func (d *Driver) scanAbandoned(ctx context.Context, before time.Time, visit func(types.MultipartUpload) error) error {
	pages := s3.NewListMultipartUploadsPaginator(d.client, &s3.ListMultipartUploadsInput{Bucket: d.bucket()})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return mapErr(err)
		}
		for _, u := range page.Uploads {
			if u.Initiated == nil || !u.Initiated.Before(before) {
				continue
			}
			if err := visit(u); err != nil {
				return err
			}
		}
	}
	return nil
}
