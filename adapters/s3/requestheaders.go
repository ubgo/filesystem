package s3

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/ubgo/filesystem"
)

// The headers Config.ExpectedBucketOwner and Config.RequestPayer send.
const (
	headerExpectedBucketOwner = "X-Amz-Expected-Bucket-Owner"
	headerRequestPayer        = "X-Amz-Request-Payer"
)

// accountID is the shape of an AWS account ID, which
// Config.ExpectedBucketOwner must be: a typo would otherwise fail every
// request with a 403 that looks like a permissions problem.
var accountID = regexp.MustCompile(`^[0-9]{12}$`)

// validateRequestHeaders checks Config.ExpectedBucketOwner,
// Config.RequestPayer and Config.PrivateACL.
func validateRequestHeaders(cfg *Config) error {
	if cfg.ExpectedBucketOwner != "" && !accountID.MatchString(cfg.ExpectedBucketOwner) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: ExpectedBucketOwner %q is not a 12-digit AWS account ID", cfg.ExpectedBucketOwner)
	}
	if cfg.RequestPayer != "" && cfg.RequestPayer != types.RequestPayerRequester {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: RequestPayer %q is not %q", cfg.RequestPayer, types.RequestPayerRequester)
	}
	if cfg.PrivateACL != "" {
		public, known := cfg.PrivateACL.public()
		if !known || public {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: PrivateACL %q is not a non-public canned ACL", cfg.PrivateACL)
		}
	}
	return nil
}

// withRequestHeaders returns client, or a copy of it that adds the
// expected-owner and request-payer headers to every request when either
// is set. The headers are added before signing, so a presigned URL
// signs them too (SigV4 presigning moves x-amz-* headers into the URL's
// query). The caller's client is never changed.
func withRequestHeaders(client *s3.Client, owner string, payer types.RequestPayer) *s3.Client {
	if owner == "" && payer == "" {
		return client
	}
	return s3.New(client.Options(), func(o *s3.Options) {
		if owner != "" {
			o.APIOptions = append(o.APIOptions, smithyhttp.SetHeaderValue(headerExpectedBucketOwner, owner))
		}
		if payer != "" {
			o.APIOptions = append(o.APIOptions, smithyhttp.SetHeaderValue(headerRequestPayer, string(payer)))
		}
	})
}

// clockSigning returns the presign client option that signs URLs at
// clock's time (Config.Clock), so a URL's X-Amz-Date and the ExpiresAt
// the driver reports agree. With no clock the SDK keeps its own signer and
// time.
func clockSigning(client *s3.Client, clock func() time.Time) func(*s3.PresignOptions) {
	return func(o *s3.PresignOptions) {
		if clock == nil {
			return
		}
		opts := client.Options()
		// The signer the SDK builds by default (newDefaultV4Signer):
		// S3 keys are signed unescaped.
		next := v4.NewSigner(func(so *v4.SignerOptions) {
			so.Logger = opts.Logger
			so.LogSigning = opts.ClientLogMode.IsSigning()
			so.DisableURIPathEscaping = true
		})
		o.Presigner = clockPresigner{next: next, now: clock}
	}
}

// clockPresigner signs with now() in place of the SDK's signing time.
type clockPresigner struct {
	next s3.HTTPPresignerV4
	now  func() time.Time
}

// PresignHTTP implements s3.HTTPPresignerV4.
func (c clockPresigner) PresignHTTP(ctx context.Context, credentials aws.Credentials, r *http.Request, payloadHash, service, region string, _ time.Time, optFns ...func(*v4.SignerOptions)) (string, http.Header, error) {
	return c.next.PresignHTTP(ctx, credentials, r, payloadHash, service, region, c.now(), optFns...)
}

// clockPost signs a POST policy at now(), keeping the lifetime the SDK
// computed (its expiration minus its signing time).
type clockPost struct {
	next s3.PresignPost
	now  func() time.Time
}

// PresignPost implements s3.PresignPost.
func (c clockPost) PresignPost(credentials aws.Credentials, bucket, key, region, service string, signingTime time.Time, conditions []any, expirationTime time.Time, optFns ...func(*v4.SignerOptions)) (map[string]string, error) {
	at := c.now().UTC()
	return c.next.PresignPost(credentials, bucket, key, region, service, at, conditions, at.Add(expirationTime.Sub(signingTime)), optFns...)
}
