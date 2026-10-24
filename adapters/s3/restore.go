package s3

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
)

// ErrArchived is in the chain of the CodeConflict error a read of an
// archived object (GLACIER, DEEP_ARCHIVE) returns before Restore has
// finished: errors.Is(err, s3.ErrArchived). CodeConflict, as the Azure
// driver answers for its archive tier, because layer.Retry never retries
// it: a restore takes minutes to hours and retrying the read cannot help.
var ErrArchived = errors.New("s3: object is archived; restore it first")

// errRestoreInProgress is S3's answer to a restore already running.
const errRestoreInProgress = "RestoreAlreadyInProgress"

// Restore implements filesystem.Restorer (declared with Config.Restore):
// it asks S3 to make a readable copy of archived key for opts.Days, at
// RequestOptions.RestoreTier or Config.RestoreTier. A restore already in
// progress is not an error, so calling it twice is safe; Stat reports the
// progress under MetaRestore.
func (d *Driver) Restore(ctx context.Context, key string, opts filesystem.RestoreOptions) error {
	tier := d.cfg.RestoreTier
	if t := requestOptions(ctx, key).RestoreTier; t != "" {
		tier = t
	}
	_, err := d.client.RestoreObject(ctx, &s3.RestoreObjectInput{
		Bucket: d.bucket(),
		Key:    aws.String(key),
		RestoreRequest: &types.RestoreRequest{
			Days:                 aws.Int32(int32(opts.Days)),
			GlacierJobParameters: &types.GlacierJobParameters{Tier: tier},
		},
	})
	if errCode(err) == errRestoreInProgress {
		return nil
	}
	return mapErr(err)
}

// restoreState parses S3's x-amz-restore header (`ongoing-request="true"`,
// or `ongoing-request="false", expiry-date="Fri, 21 Dec 2012 00:00:00
// GMT"`) into a MetaRestore value and an RFC 3339 expiry, both empty when
// the header is.
func restoreState(header string) (state, expiry string) {
	switch {
	case header == "":
		return "", ""
	case strings.Contains(header, `ongoing-request="true"`):
		return RestoreOngoing, ""
	}
	const expiryField = `expiry-date="`
	if i := strings.Index(header, expiryField); i >= 0 {
		rest := header[i+len(expiryField):]
		if j := strings.IndexByte(rest, '"'); j >= 0 {
			if t, err := time.Parse(http.TimeFormat, rest[:j]); err == nil {
				expiry = t.UTC().Format(time.RFC3339)
			}
		}
	}
	return RestoreDone, expiry
}
