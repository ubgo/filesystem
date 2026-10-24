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

// errNoReplication is S3's answer to reading a bucket that has no
// replication configuration.
const errNoReplication = "ReplicationConfigurationNotFoundError"

// ReplicationConfig is a bucket's replication configuration: S3 copies new
// objects, asynchronously, to other buckets, typically in another region
// (cross-region replication). Prerequisites S3 enforces, not this package:
// versioning on the source and every destination bucket, and Role, an IAM
// role S3 can assume that may read the source and replicate into the
// destinations. Objects written before the configuration are not copied
// (that is S3 Batch Replication).
type ReplicationConfig struct {
	// Role is the ARN of the IAM role S3 assumes to replicate.
	Role string
	// Rules say what goes where; at least one.
	Rules []ReplicationRule
}

// ReplicationRule replicates the objects under Prefix to one destination.
type ReplicationRule struct {
	// ID names the rule; empty lets S3 generate one.
	ID string
	// Priority orders rules whose prefixes overlap: the higher wins. Each
	// rule of a configuration needs a distinct priority.
	Priority int32
	// Prefix is the bucket key prefix replicated, as stored in the bucket;
	// empty replicates the whole bucket.
	Prefix string
	// DestinationBucket is the destination bucket's ARN,
	// "arn:aws:s3:::name".
	DestinationBucket string
	// StorageClass is the class replicas are stored in; empty keeps the
	// source object's class.
	StorageClass string
	// ReplicateDeletes copies delete markers too, so deleting a key on
	// the source hides it on the destination. Off, the destination keeps
	// everything, which makes it a backup rather than a mirror.
	ReplicateDeletes bool
	// Disabled keeps the rule in the configuration without S3 applying it.
	// The zero value is an enabled rule.
	Disabled bool
}

// SetReplication replaces the bucket's replication configuration
// (PutBucketReplication). Like SetCORS it is a setup step run with
// credentials allowed s3:PutReplicationConfiguration and iam:PassRole on
// Role, not part of the Disk. A configuration without a role or rules, or
// a rule without a destination, is refused before any request. Get the
// client from a disk with filesystem.As[*s3.Client].
func SetReplication(ctx context.Context, client *s3.Client, bucket string, r ReplicationConfig) error {
	if r.Role == "" || len(r.Rules) == 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: replication needs a role and at least one rule")
	}
	cfg := &types.ReplicationConfiguration{Role: aws.String(r.Role)}
	for _, rule := range r.Rules {
		if rule.DestinationBucket == "" {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: replication rule %q has no destination bucket", rule.ID)
		}
		deletes := types.DeleteMarkerReplicationStatusDisabled
		if rule.ReplicateDeletes {
			deletes = types.DeleteMarkerReplicationStatusEnabled
		}
		status := types.ReplicationRuleStatusEnabled
		if rule.Disabled {
			status = types.ReplicationRuleStatusDisabled
		}
		cfg.Rules = append(cfg.Rules, types.ReplicationRule{
			ID:                      optional(rule.ID),
			Priority:                aws.Int32(rule.Priority),
			Status:                  status,
			Filter:                  &types.ReplicationRuleFilter{Prefix: aws.String(rule.Prefix)},
			DeleteMarkerReplication: &types.DeleteMarkerReplication{Status: deletes},
			Destination:             &types.Destination{Bucket: aws.String(rule.DestinationBucket), StorageClass: types.StorageClass(rule.StorageClass)},
		})
	}
	_, err := client.PutBucketReplication(ctx, &s3.PutBucketReplicationInput{Bucket: aws.String(bucket), ReplicationConfiguration: cfg})
	return mapErr(err)
}

// Replication reads the bucket's replication configuration back
// (GetBucketReplication). A bucket without one returns the zero
// ReplicationConfig and no error. A rule using anything ReplicationRule
// cannot represent (a tag filter, the older schema without a filter,
// existing-object or replica-modification settings, a destination
// account, encryption, metrics or a replication time) is a
// CodeUnsupported error naming the rule, never a rule with that part
// dropped, which SetReplication would write back wider or different.
func Replication(ctx context.Context, client *s3.Client, bucket string) (ReplicationConfig, error) {
	out, err := client.GetBucketReplication(ctx, &s3.GetBucketReplicationInput{Bucket: aws.String(bucket)})
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() == errNoReplication {
		return ReplicationConfig{}, nil
	}
	if err != nil {
		return ReplicationConfig{}, mapErr(err)
	}
	if out.ReplicationConfiguration == nil {
		return ReplicationConfig{}, nil
	}
	r := ReplicationConfig{Role: aws.ToString(out.ReplicationConfiguration.Role)}
	for _, rule := range out.ReplicationConfiguration.Rules {
		got, err := replicationFromS3(rule)
		if err != nil {
			return ReplicationConfig{}, err
		}
		r.Rules = append(r.Rules, got)
	}
	return r, nil
}

// replicationFromS3 converts a stored rule exactly, or fails naming the
// first part ReplicationRule cannot represent.
func replicationFromS3(rule types.ReplicationRule) (ReplicationRule, error) {
	id := aws.ToString(rule.ID)
	unsupported := func(part string) (ReplicationRule, error) {
		return ReplicationRule{}, filesystem.Errorf(filesystem.CodeUnsupported, "s3: replication rule %q uses %s, which ReplicationRule cannot represent", id, part)
	}
	got := ReplicationRule{ID: id, Priority: aws.ToInt32(rule.Priority)}
	switch rule.Status {
	case types.ReplicationRuleStatusEnabled:
	case types.ReplicationRuleStatusDisabled:
		got.Disabled = true
	default:
		return unsupported("status " + string(rule.Status))
	}
	f := rule.Filter
	switch {
	case f == nil || rule.Prefix != nil || rule.Priority == nil:
		// The older schema replicates delete markers by default, which
		// SetReplication's schema would not.
		return unsupported("the older schema without a filter and priority")
	case f.Tag != nil || f.And != nil:
		return unsupported("a tag or combined filter")
	}
	got.Prefix = aws.ToString(f.Prefix)
	if rule.ExistingObjectReplication != nil || rule.SourceSelectionCriteria != nil {
		return unsupported("existing-object or replica-modification settings")
	}
	d := rule.Destination
	if d == nil || d.Bucket == nil {
		return unsupported("no destination bucket")
	}
	if d.AccessControlTranslation != nil || d.Account != nil || d.EncryptionConfiguration != nil || d.Metrics != nil || d.ReplicationTime != nil {
		return unsupported("destination account, encryption, metrics or replication time settings")
	}
	got.DestinationBucket, got.StorageClass = *d.Bucket, string(d.StorageClass)
	dm := rule.DeleteMarkerReplication
	if dm == nil {
		return unsupported("no delete-marker setting")
	}
	switch dm.Status {
	case types.DeleteMarkerReplicationStatusEnabled:
		got.ReplicateDeletes = true
	case types.DeleteMarkerReplicationStatusDisabled:
	default:
		return unsupported("delete-marker status " + string(dm.Status))
	}
	return got, nil
}
