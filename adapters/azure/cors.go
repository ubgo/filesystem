package azure

import (
	"context"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/ubgo/filesystem"
)

// CORSRule is one cross-origin rule of the account's blob service: which
// web origins may call it from a browser, with which methods and request
// headers, which response headers their scripts may read, and how long a
// browser may cache the preflight answer. Azure accepts a trailing "*" in
// header names ("x-ms-meta-*").
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
// download directly with this driver's SAS URLs, from the given origins
// (for example "https://app.example.com"; avoid "*", which lets any site
// use links your users hold). It allows GET, HEAD and PUT (Azure has no
// POST upload), the request headers a SAS upload carries (Content-Type
// and Azure's x-ms-*, x-ms-blob-type among them), and exposes ETag.
func DirectUploadCORS(origins ...string) CORSRule {
	return CORSRule{
		AllowedOrigins: origins,
		AllowedMethods: []string{"GET", "HEAD", "PUT"},
		AllowedHeaders: []string{"content-type", "x-ms-*"},
		ExposeHeaders:  []string{"etag"},
		MaxAge:         directUploadMaxAge,
	}
}

// SetCORS replaces the CORS rules of the account's blob service with
// rules. CORS is an account-wide setting on Azure, not a container one,
// so this affects every container of the account; Azure keeps at most
// five rules. The other service properties are left as they are. It is a
// setup step, not part of the Disk: run it once from a deploy script with
// credentials allowed to set service properties (an account key, or the
// Storage Account Contributor role). A rule without origins or methods is
// refused before any request. Get the client from a disk with
// filesystem.As[*azblob.Client].
func SetCORS(ctx context.Context, client *azblob.Client, rules ...CORSRule) error {
	if len(rules) == 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: SetCORS needs at least one rule")
	}
	cors := make([]*service.CORSRule, 0, len(rules))
	for _, r := range rules {
		if len(r.AllowedOrigins) == 0 || len(r.AllowedMethods) == 0 {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: a CORS rule needs origins and methods")
		}
		cors = append(cors, &service.CORSRule{
			AllowedOrigins:  ptr(strings.Join(r.AllowedOrigins, ",")),
			AllowedMethods:  ptr(strings.Join(r.AllowedMethods, ",")),
			AllowedHeaders:  ptr(strings.Join(r.AllowedHeaders, ",")),
			ExposedHeaders:  ptr(strings.Join(r.ExposeHeaders, ",")),
			MaxAgeInSeconds: ptr(int32(r.MaxAge / time.Second)),
		})
	}
	_, err := client.ServiceClient().SetProperties(ctx, &service.SetPropertiesOptions{CORS: cors})
	return mapErr(err)
}
