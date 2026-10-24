package s3

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Tags implements filesystem.Tagger (declared with Config.Tagging) with
// GetObjectTagging. An object without tags returns an empty map; a missing
// one CodeNotFound.
func (d *Driver) Tags(ctx context.Context, key string) (map[string]string, error) {
	out, err := d.client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: d.bucket(), Key: aws.String(key)})
	if err != nil {
		return nil, mapErr(err)
	}
	tags := make(map[string]string, len(out.TagSet))
	for _, t := range out.TagSet {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return tags, nil
}

// SetTags implements filesystem.Tagger: PutObjectTagging replaces every
// tag, and an empty map is DeleteObjectTagging. S3's limits (ten tags,
// 128-character keys, 256-character values) come back from S3 as errors.
func (d *Driver) SetTags(ctx context.Context, key string, tags map[string]string) error {
	if len(tags) == 0 {
		_, err := d.client.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{Bucket: d.bucket(), Key: aws.String(key)})
		return mapErr(err)
	}
	set := make([]types.Tag, 0, len(tags))
	for k, v := range tags {
		set = append(set, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	_, err := d.client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: d.bucket(), Key: aws.String(key), Tagging: &types.Tagging{TagSet: set}})
	return mapErr(err)
}
