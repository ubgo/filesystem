package gcs

import (
	"context"
	"slices"
	"time"

	"cloud.google.com/go/storage"

	"github.com/ubgo/filesystem"
)

// CORSRule is one cross-origin rule of a bucket: which web origins may
// call it from a browser, with which methods, which headers, and how long a
// browser may cache the preflight answer. GCS has a single header list
// (responseHeader) that both allows request headers and exposes response
// headers, so AllowedHeaders and ExposeHeaders are merged into it.
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
// download directly with this driver's signed URLs and presigned POSTs,
// from the given origins (for example "https://app.example.com"; avoid
// "*", which lets any site use links your users hold). It allows GET,
// HEAD, PUT and POST; the headers a signed request may carry
// (Content-Type, the pinned Cache-Control, Content-Disposition and
// Content-Encoding, and the KMS key header); and exposes ETag.
func DirectUploadCORS(origins ...string) CORSRule {
	return CORSRule{
		AllowedOrigins: origins,
		AllowedMethods: []string{"GET", "HEAD", "PUT", "POST"},
		AllowedHeaders: []string{"Content-Type", filesystem.HeaderCacheControl, filesystem.HeaderContentDisposition, filesystem.HeaderContentEncoding, headerKMSKeyName},
		ExposeHeaders:  []string{"ETag"},
		MaxAge:         directUploadMaxAge,
	}
}

// SetCORS replaces the bucket's CORS configuration with rules. It is a
// setup step, not part of the Disk: run it once from a deploy script or at
// start-up, with credentials allowed storage.buckets.update (the
// application's own usually are not, and should not be). A rule without
// origins or methods is refused before any request. Get the client from a
// disk with filesystem.As[*storage.Client].
func SetCORS(ctx context.Context, client *storage.Client, bucket string, rules ...CORSRule) error {
	if len(rules) == 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: SetCORS needs at least one rule")
	}
	cors := make([]storage.CORS, 0, len(rules))
	for _, r := range rules {
		if len(r.AllowedOrigins) == 0 || len(r.AllowedMethods) == 0 {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: a CORS rule needs origins and methods")
		}
		cors = append(cors, storage.CORS{
			Origins:         r.AllowedOrigins,
			Methods:         r.AllowedMethods,
			ResponseHeaders: slices.Concat(r.AllowedHeaders, r.ExposeHeaders),
			MaxAge:          r.MaxAge,
		})
	}
	_, err := client.Bucket(bucket).Update(ctx, storage.BucketAttrsToUpdate{CORS: cors})
	return mapErr(err)
}
