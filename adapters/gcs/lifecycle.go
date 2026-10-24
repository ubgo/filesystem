package gcs

import (
	"context"
	"reflect"

	"cloud.google.com/go/storage"

	"github.com/ubgo/filesystem"
)

// The lifecycle actions GCS runs, one per rule.
const (
	// LifecycleDelete deletes matching objects.
	LifecycleDelete = storage.DeleteAction
	// LifecycleSetStorageClass moves matching objects to StorageClass.
	LifecycleSetStorageClass = storage.SetStorageClassAction
	// LifecycleAbortUploads aborts matching multipart (XML API) uploads
	// never completed.
	LifecycleAbortUploads = storage.AbortIncompleteMPUAction
)

// LifecycleRule is one rule of a bucket's lifecycle configuration: GCS
// applies Action, by itself, to objects at least AgeDays old (an upload
// counts from its start) whose names begin with one of Prefixes. GCS
// rules carry one action each, so expiring and transitioning the same
// prefix takes two rules.
type LifecycleRule struct {
	// Action is LifecycleDelete, LifecycleSetStorageClass or
	// LifecycleAbortUploads.
	Action string
	// StorageClass is the target of LifecycleSetStorageClass, for example
	// "NEARLINE" or "ARCHIVE"; empty for the other actions.
	StorageClass string
	// AgeDays is the object's minimum age in days; it must be positive,
	// so no rule fires on every object the moment it is written.
	AgeDays int64
	// Prefixes are the object name prefixes, as stored in the bucket (a
	// scoped disk's keys are under its prefix); empty covers the whole
	// bucket.
	Prefixes []string
}

// SetLifecycle replaces the bucket's lifecycle rules. Like SetCORS it is a
// setup step run with credentials allowed storage.buckets.update, not part
// of the Disk. A rule with an unknown action, a non-positive age, or a
// storage class missing from (or given to) the wrong action is refused
// before any request. Get the client from a disk with
// filesystem.As[*storage.Client].
func SetLifecycle(ctx context.Context, client *storage.Client, bucket string, rules ...LifecycleRule) error {
	if len(rules) == 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: SetLifecycle needs at least one rule")
	}
	lc := storage.Lifecycle{}
	for _, r := range rules {
		if err := r.validate(); err != nil {
			return err
		}
		lc.Rules = append(lc.Rules, storage.LifecycleRule{
			Action:    storage.LifecycleAction{Type: r.Action, StorageClass: r.StorageClass},
			Condition: storage.LifecycleCondition{AgeInDays: r.AgeDays, MatchesPrefix: r.Prefixes},
		})
	}
	_, err := client.Bucket(bucket).Update(ctx, storage.BucketAttrsToUpdate{Lifecycle: &lc})
	return mapErr(err)
}

// Lifecycle reads the bucket's lifecycle rules back. A rule using any
// condition LifecycleRule cannot represent (liveness, newer versions,
// noncurrent or custom times, creation dates, storage classes, suffixes)
// is a CodeUnsupported error naming the rule by its position, never a rule
// with that condition dropped: "delete noncurrent versions after 30 days"
// written back without it would delete every object after 30 days.
func Lifecycle(ctx context.Context, client *storage.Client, bucket string) ([]LifecycleRule, error) {
	attrs, err := client.Bucket(bucket).Attrs(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	var rules []LifecycleRule
	for i, r := range attrs.Lifecycle.Rules {
		c := r.Condition
		rule := LifecycleRule{Action: r.Action.Type, StorageClass: r.Action.StorageClass, AgeDays: c.AgeInDays, Prefixes: c.MatchesPrefix}
		if !onlyAgeAndPrefix(c) || rule.validate() != nil {
			return nil, filesystem.Errorf(filesystem.CodeUnsupported, "gcs: lifecycle rule %d (%s) uses a condition or action LifecycleRule cannot represent", i, r.Action.Type)
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// validate checks a rule before it is sent.
func (r LifecycleRule) validate() error {
	switch {
	case r.Action != LifecycleDelete && r.Action != LifecycleSetStorageClass && r.Action != LifecycleAbortUploads:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: lifecycle action %q is not Delete, SetStorageClass or AbortIncompleteMultipartUpload", r.Action)
	case r.AgeDays <= 0:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: lifecycle rule %s needs a positive age", r.Action)
	case (r.Action == LifecycleSetStorageClass) != (r.StorageClass != ""):
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: a storage class goes with SetStorageClass, and only with it")
	}
	return nil
}

// onlyAgeAndPrefix reports whether c sets no condition but the age and
// the prefixes, the two LifecycleRule represents: c must equal the
// condition rebuilt from those two alone. Comparing whole values, rather
// than listing the other fields, also catches a field a later SDK adds.
func onlyAgeAndPrefix(c storage.LifecycleCondition) bool {
	return reflect.DeepEqual(c, storage.LifecycleCondition{AgeInDays: c.AgeInDays, MatchesPrefix: c.MatchesPrefix})
}
