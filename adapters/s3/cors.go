package s3

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
)

// CORSRule is one cross-origin rule of a bucket: which web origins may
// call it from a browser, with which methods and request headers, which
// response headers their scripts may read, and how long a browser may
// cache the preflight answer.
type CORSRule struct {
	AllowedOrigins []string
	AllowedMethods []string
	AllowedHeaders []string
	ExposeHeaders  []string
	MaxAge         time.Duration
}

// directUploadMaxAge is how long browsers may cache the preflight of the
// recommended rule: an hour saves a preflight per request without keeping
// a removed origin allowed for long.
const directUploadMaxAge = time.Hour

// DirectUploadCORS is the recommended rule for browsers that upload and
// download directly with this driver's signed URLs, presigned parts and
// presigned POSTs, from the given origins (for example
// "https://app.example.com"; avoid "*", which lets any site use links
// your users hold). It allows GET, HEAD, PUT and POST; the request headers
// a signed request may carry (Content-Type, the pinned Cache-Control,
// Content-Disposition and Content-Encoding, and S3's own x-amz-*, which
// covers encryption and checksums); and exposes ETag, which browser
// multipart uploaders (Uppy) read to complete an upload.
func DirectUploadCORS(origins ...string) CORSRule {
	return CORSRule{
		AllowedOrigins: origins,
		AllowedMethods: []string{"GET", "HEAD", "PUT", "POST"},
		AllowedHeaders: []string{"Content-Type", filesystem.HeaderCacheControl, filesystem.HeaderContentDisposition, filesystem.HeaderContentEncoding, "x-amz-*"},
		ExposeHeaders:  []string{"ETag"},
		MaxAge:         directUploadMaxAge,
	}
}

// SetCORS replaces the bucket's CORS configuration with rules
// (PutBucketCors). It is a setup step, not part of the Disk: run it once
// from a deploy script or at start-up, with credentials allowed
// s3:PutBucketCORS (the application's own usually are not, and should
// not be). A rule without origins or methods is refused before any
// request. Get the client from a disk with filesystem.As[*s3.Client].
func SetCORS(ctx context.Context, client *s3.Client, bucket string, rules ...CORSRule) error {
	if len(rules) == 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: SetCORS needs at least one rule")
	}
	cfg := &types.CORSConfiguration{}
	for _, r := range rules {
		if len(r.AllowedOrigins) == 0 || len(r.AllowedMethods) == 0 {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: a CORS rule needs origins and methods")
		}
		rule := types.CORSRule{
			AllowedOrigins: r.AllowedOrigins,
			AllowedMethods: r.AllowedMethods,
			AllowedHeaders: r.AllowedHeaders,
			ExposeHeaders:  r.ExposeHeaders,
		}
		if r.MaxAge > 0 {
			rule.MaxAgeSeconds = aws.Int32(int32(r.MaxAge / time.Second))
		}
		cfg.CORSRules = append(cfg.CORSRules, rule)
	}
	_, err := client.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: aws.String(bucket), CORSConfiguration: cfg})
	return mapErr(err)
}
