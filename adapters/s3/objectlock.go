package s3

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/ubgo/filesystem"
)

// Hold implements filesystem.Holder (declared with Config.ObjectLock). It
// reads the legal hold and retention date from one HeadObject, which needs
// s3:GetObjectLegalHold and s3:GetObjectRetention for S3 to include them.
func (d *Driver) Hold(ctx context.Context, key string) (filesystem.Hold, error) {
	out, err := d.head(ctx, key, nil)
	if err != nil {
		return filesystem.Hold{}, err
	}
	return holdOf(out), nil
}

// holdOf is the hold HeadObject reported.
func holdOf(out *s3.HeadObjectOutput) filesystem.Hold {
	return filesystem.Hold{
		Legal:       out.ObjectLockLegalHoldStatus == types.ObjectLockLegalHoldStatusOn,
		RetainUntil: aws.ToTime(out.ObjectLockRetainUntilDate),
	}
}

// SetHold implements filesystem.Holder: it turns the legal hold on or off,
// then sets the retention date in Config.ObjectLockMode, or removes a
// retention the object has when h.RetainUntil is zero. Removing or
// shortening a GOVERNANCE retention sends the bypass header, so it needs
// s3:BypassGovernanceRetention; S3 refuses it for COMPLIANCE with
// CodePermission. A date beyond Config.MaxRetention is refused with
// CodeInvalidArgument before anything is sent. The two are separate S3 calls: if the second fails, the
// legal hold is already set.
func (d *Driver) SetHold(ctx context.Context, key string, h filesystem.Hold) error {
	if !h.RetainUntil.IsZero() {
		if err := d.checkRetention(h.RetainUntil); err != nil {
			return err
		}
	}
	status := types.ObjectLockLegalHoldStatusOff
	if h.Legal {
		status = types.ObjectLockLegalHoldStatusOn
	}
	if _, err := d.client.PutObjectLegalHold(ctx, &s3.PutObjectLegalHoldInput{Bucket: d.bucket(), Key: aws.String(key), LegalHold: &types.ObjectLockLegalHold{Status: status}}); err != nil {
		return mapErr(err)
	}
	in := &s3.PutObjectRetentionInput{Bucket: d.bucket(), Key: aws.String(key), BypassGovernanceRetention: aws.Bool(d.cfg.ObjectLockMode == types.ObjectLockModeGovernance)}
	if h.RetainUntil.IsZero() {
		out, err := d.head(ctx, key, nil)
		if err != nil {
			return err
		}
		if out.ObjectLockRetainUntilDate == nil {
			return nil
		}
		in.Retention = &types.ObjectLockRetention{}
	} else {
		in.Retention = &types.ObjectLockRetention{Mode: types.ObjectLockRetentionMode(d.cfg.ObjectLockMode), RetainUntilDate: aws.Time(h.RetainUntil.UTC())}
	}
	_, err := d.client.PutObjectRetention(ctx, in)
	return mapErr(err)
}

// refuseHeld returns a CodePermission error when key has a legal hold or a
// retention date in the future, nil when it has neither or does not exist.
// In a versioned bucket, which Object Lock requires, S3 itself would
// answer a plain delete of a held object by hiding it behind a delete
// marker; the driver refuses instead, so a held object stays readable as
// the Hold contract promises. The check and the delete are two requests:
// a hold set in between is still enforced by S3 on the stored version.
func (d *Driver) refuseHeld(ctx context.Context, key string) error {
	out, err := d.head(ctx, key, nil)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	// The message names no key: the driver's key carries a scoped disk's
	// prefix, and the Disk adds the caller's own key to the error.
	if h := holdOf(out); h.Legal || h.RetainUntil.After(d.now()) {
		return filesystem.Errorf(filesystem.CodePermission, "s3: the object is held (legal hold %t, retained until %s)", h.Legal, h.RetainUntil.Format(time.RFC3339))
	}
	return nil
}

// holdChecks is how many HEADs heldKeys runs at once: enough to hide the
// round trip on a 1,000-key batch, few enough not to trip S3 throttling.
const holdChecks = 16

// heldKeys checks keys with refuseHeld, holdChecks at a time, and returns
// the held ones with their CodePermission errors. Any other failure is
// returned as err, since nothing about those keys is known.
func (d *Driver) heldKeys(ctx context.Context, keys []string) (map[string]error, error) {
	err := filesystem.Batch(ctx, keys, holdChecks, d.refuseHeld)
	var be *filesystem.BatchError
	if err == nil || !errors.As(err, &be) {
		return nil, err
	}
	held := make(map[string]error, len(be.Failures))
	for _, f := range be.Failures {
		if filesystem.CodeOf(f.Err) != filesystem.CodePermission {
			return nil, f.Err
		}
		held[f.Key] = f.Err
	}
	return held, nil
}

// checkRetention refuses a retention date further away than
// Config.MaxRetention, so a mistaken date (or a COMPLIANCE lock nobody
// can lift) cannot outlive what the disk allows.
func (d *Driver) checkRetention(until time.Time) error {
	if limit := d.now().Add(d.cfg.MaxRetention); until.After(limit) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: retention until %s is beyond the disk's MaxRetention of %s", until.UTC().Format(time.RFC3339), d.cfg.MaxRetention)
	}
	return nil
}

// emptyLockDateMiddlewareID names dropEmptyLockDate in the SDK stack.
const emptyLockDateMiddlewareID = "ubgoDropEmptyLockDate"

// headerRetainUntil is the HeadObject header of a retention date.
const headerRetainUntil = "X-Amz-Object-Lock-Retain-Until-Date"

// dropEmptyLockDate removes an empty x-amz-object-lock-retain-until-date
// from a HeadObject answer before the SDK parses it. RustFS sends the
// header empty once a retention has been removed, and the SDK fails the
// whole call on an empty date; an empty date means no retention.
type dropEmptyLockDate struct{}

// ID implements middleware.DeserializeMiddleware.
func (dropEmptyLockDate) ID() string { return emptyLockDateMiddlewareID }

// HandleDeserialize implements middleware.DeserializeMiddleware: it runs
// inside the operation's deserializer, so it sees the raw response first.
func (dropEmptyLockDate) HandleDeserialize(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (middleware.DeserializeOutput, middleware.Metadata, error) {
	out, md, err := next.HandleDeserialize(ctx, in)
	if resp, ok := out.RawResponse.(*smithyhttp.Response); ok && resp.Header.Get(headerRetainUntil) == "" {
		resp.Header.Del(headerRetainUntil)
	}
	return out, md, err
}

// withEmptyLockDateDropped installs dropEmptyLockDate for one call.
func withEmptyLockDateDropped(o *s3.Options) {
	o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
		return stack.Deserialize.Add(dropEmptyLockDate{}, middleware.After)
	})
}
