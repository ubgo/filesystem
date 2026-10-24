package s3

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/ubgo/filesystem"
)

// errNoSuchLifecycle is S3's answer to reading a bucket that has no
// lifecycle configuration.
const errNoSuchLifecycle = "NoSuchLifecycleConfiguration"

// LifecycleRule is one rule of a bucket's lifecycle configuration: what S3
// does by itself, on a schedule, to the objects under Prefix. Days count
// from an object's creation (from the upload's start for
// AbortIncompleteUploadsAfterDays); zero turns that action off. A rule
// must turn on at least one action.
type LifecycleRule struct {
	// ID names the rule; empty lets S3 generate one. IDs are unique per
	// bucket.
	ID string
	// Prefix is the bucket key prefix the rule covers, as stored in the
	// bucket: a disk scoped to "tenant" stores its keys under "tenant/".
	// Empty covers the whole bucket.
	Prefix string
	// ExpireAfterDays deletes objects this many days after they were
	// written.
	ExpireAfterDays int32
	// AbortIncompleteUploadsAfterDays removes multipart uploads never
	// completed, and their parts, this many days after they started.
	AbortIncompleteUploadsAfterDays int32
	// Transitions move objects to another storage class after a number of
	// days, for example to GLACIER after 90.
	Transitions []LifecycleTransition
	// Disabled keeps the rule in the configuration without S3 applying it
	// (status Disabled). The zero value is an enabled rule.
	Disabled bool
}

// LifecycleTransition moves objects to StorageClass (an S3 storage class
// such as "STANDARD_IA" or "GLACIER") AfterDays days after they were
// written.
type LifecycleTransition struct {
	AfterDays    int32
	StorageClass string
}

// SetLifecycle replaces the bucket's lifecycle configuration with rules
// (PutBucketLifecycleConfiguration). Like SetCORS it is a setup step run
// with credentials allowed s3:PutLifecycleConfiguration, not part of the
// Disk. A rule with no action, a negative day count or a transition
// without a storage class is refused before any request. Get the client
// from a disk with filesystem.As[*s3.Client].
func SetLifecycle(ctx context.Context, client *s3.Client, bucket string, rules ...LifecycleRule) error {
	if len(rules) == 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: SetLifecycle needs at least one rule")
	}
	cfg := &types.BucketLifecycleConfiguration{}
	for _, r := range rules {
		rule, err := r.toS3()
		if err != nil {
			return err
		}
		cfg.Rules = append(cfg.Rules, rule)
	}
	_, err := client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{Bucket: aws.String(bucket), LifecycleConfiguration: cfg})
	return mapErr(err)
}

// Lifecycle reads the bucket's lifecycle rules back
// (GetBucketLifecycleConfiguration). A bucket without a configuration has
// no rules: nil and no error. A rule using anything LifecycleRule cannot
// represent (a tag or size filter, dates, noncurrent-version actions,
// delete-marker cleanup) is a CodeUnsupported error naming the rule, never
// a rule with that part dropped: writing a dropped filter back with
// SetLifecycle would apply the action to more objects than before. Read
// such a bucket with the native client.
func Lifecycle(ctx context.Context, client *s3.Client, bucket string) ([]LifecycleRule, error) {
	out, err := client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(bucket)})
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() == errNoSuchLifecycle {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr(err)
	}
	rules := make([]LifecycleRule, 0, len(out.Rules))
	for _, r := range out.Rules {
		rule, err := lifecycleFromS3(r)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// lifecycleFromS3 converts a stored rule exactly, or fails naming the
// first part LifecycleRule cannot represent.
func lifecycleFromS3(r types.LifecycleRule) (LifecycleRule, error) {
	id := aws.ToString(r.ID)
	unsupported := func(part string) (LifecycleRule, error) {
		return LifecycleRule{}, filesystem.Errorf(filesystem.CodeUnsupported, "s3: lifecycle rule %q uses %s, which LifecycleRule cannot represent", id, part)
	}
	rule := LifecycleRule{ID: id, Prefix: aws.ToString(r.Prefix)}
	switch r.Status {
	case types.ExpirationStatusEnabled:
	case types.ExpirationStatusDisabled:
		rule.Disabled = true
	default:
		return unsupported("status " + string(r.Status))
	}
	if f := r.Filter; f != nil {
		switch {
		case f.Tag != nil || f.And != nil:
			return unsupported("a tag or combined filter")
		case f.ObjectSizeGreaterThan != nil || f.ObjectSizeLessThan != nil:
			return unsupported("an object size filter")
		case f.Prefix != nil && r.Prefix != nil && *f.Prefix != *r.Prefix:
			return unsupported("two different prefixes")
		case f.Prefix != nil:
			rule.Prefix = *f.Prefix
		}
	}
	if r.NoncurrentVersionExpiration != nil || len(r.NoncurrentVersionTransitions) > 0 {
		return unsupported("noncurrent-version actions")
	}
	if e := r.Expiration; e != nil {
		if e.Date != nil || e.ExpiredObjectDeleteMarker != nil || e.Days == nil {
			return unsupported("an expiry date or delete-marker cleanup")
		}
		rule.ExpireAfterDays = *e.Days
	}
	if a := r.AbortIncompleteMultipartUpload; a != nil {
		if a.DaysAfterInitiation == nil {
			return unsupported("an upload cleanup without days")
		}
		rule.AbortIncompleteUploadsAfterDays = *a.DaysAfterInitiation
	}
	for _, t := range r.Transitions {
		if t.Date != nil || t.Days == nil {
			return unsupported("a transition by date")
		}
		rule.Transitions = append(rule.Transitions, LifecycleTransition{AfterDays: *t.Days, StorageClass: string(t.StorageClass)})
	}
	return rule, nil
}

// toS3 validates r and converts it to the SDK's rule, enabled unless
// Disabled, filtered by prefix (the empty prefix matching every key).
func (r LifecycleRule) toS3() (types.LifecycleRule, error) {
	if r.ExpireAfterDays < 0 || r.AbortIncompleteUploadsAfterDays < 0 {
		return types.LifecycleRule{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: lifecycle rule %q has a negative day count", r.ID)
	}
	if r.ExpireAfterDays == 0 && r.AbortIncompleteUploadsAfterDays == 0 && len(r.Transitions) == 0 {
		return types.LifecycleRule{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: lifecycle rule %q has no action", r.ID)
	}
	rule := types.LifecycleRule{
		ID:     optional(r.ID),
		Status: types.ExpirationStatusEnabled,
		Filter: &types.LifecycleRuleFilter{Prefix: aws.String(r.Prefix)},
	}
	if r.Disabled {
		rule.Status = types.ExpirationStatusDisabled
	}
	if r.ExpireAfterDays > 0 {
		rule.Expiration = &types.LifecycleExpiration{Days: aws.Int32(r.ExpireAfterDays)}
	}
	if r.AbortIncompleteUploadsAfterDays > 0 {
		rule.AbortIncompleteMultipartUpload = &types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(r.AbortIncompleteUploadsAfterDays)}
	}
	for _, t := range r.Transitions {
		if t.StorageClass == "" || t.AfterDays < 0 {
			return types.LifecycleRule{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: lifecycle rule %q has a transition without a storage class or with negative days", r.ID)
		}
		rule.Transitions = append(rule.Transitions, types.Transition{Days: aws.Int32(t.AfterDays), StorageClass: types.TransitionStorageClass(t.StorageClass)})
	}
	return rule, nil
}
