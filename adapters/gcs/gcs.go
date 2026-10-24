// Package gcs is a filesystem.Driver for Google Cloud Storage.
//
// It is a separate module so that the Google Cloud SDK is a dependency only
// of applications that use GCS. As with the S3 driver, provider-dependent
// capabilities are configuration: per-object ACLs exist only on buckets
// without uniform bucket-level access, and signed URLs need a service
// account to sign as (with a key, a signing function, or keylessly through
// IAM signBlob), so the driver claims each only when configured for it.
//
// Multipart uploads are not offered: GCS's resumable uploads are driven by
// the SDK inside Write, and its XML multipart API is not exposed by the Go
// client. Bodies of any size stream through Write.
package gcs

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iamcredentials/v1"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
)

// VisibilityMode says how the bucket expresses object visibility.
type VisibilityMode string

const (
	// VisibilityNone: uniform bucket-level access (the GCS default and
	// recommendation); IAM on the bucket decides who reads, and the driver
	// does not claim SetVisibility.
	VisibilityNone VisibilityMode = "none"
	// VisibilityACL: fine-grained access; objects get the publicRead or
	// private predefined ACL.
	VisibilityACL VisibilityMode = "acl"
)

// VisibilityModeValues lists every VisibilityMode.
var VisibilityModeValues = []VisibilityMode{VisibilityNone, VisibilityACL}

// Config configures a Driver.
type Config struct {
	// Client is the GCS client, built by the application. Required.
	Client *storage.Client
	// Bucket is the bucket name. Required.
	Bucket string
	// Visibility is how the bucket expresses visibility; empty means
	// VisibilityNone.
	Visibility VisibilityMode
	// PrivateACL is the predefined ACL VisibilityACL gives private objects,
	// on a Write, a presigned PUT (signed as x-goog-acl) and a form (its
	// acl field), and a Copy of a private object; empty means ACLPrivate,
	// as before. Set ACLBucketOwnerFullControl when this disk writes into
	// a bucket another project owns, or ACLProjectPrivate to let the
	// project's members read by role. It must be one of PrivateACLValues
	// (public ACLs are refused: public objects get publicRead) and needs
	// VisibilityACL, since uniform bucket-level access refuses every ACL
	// (CodeInvalidArgument otherwise). SetVisibility to private still only
	// removes the public grant, leaving the rest of the object's ACL.
	PrivateACL PredefinedACL
	// Signing enables signed URLs (V4). It needs GoogleAccessID and at most
	// one of PrivateKey or SignBytes. With neither, signing is keyless:
	// each URL costs one call to the IAM Credentials signBlob API, whose
	// caller needs roles/iam.serviceAccountTokenCreator on GoogleAccessID
	// (the path for Cloud Run and GKE, where no key file exists). Set IAM
	// as well: without it the SDK makes that call with context.Background()
	// and, if Client was built from a service account key file, signs with
	// that key instead, which is only valid when GoogleAccessID is that
	// key's account. Method, Expires
	// and the other per-URL fields are set by the driver. Nil disables the
	// Presign capability.
	Signing *storage.SignedURLOptions
	// IAM, when set, is the IAM Credentials client keyless signing uses (a
	// Signing with neither PrivateKey nor SignBytes): each Presign calls
	// signBlob through it with the caller's context, so the request's
	// deadline or cancellation stops the call, and the URL is always
	// signed by Google as GoogleAccessID. Without it the SDK makes that
	// call itself with context.Background(), and signs with the client's
	// own key instead when Client was built from a key file. The opener
	// always sets it for signing_account.
	IAM *iamcredentials.Service
	// KMSKeyName is the Cloud KMS key (CMEK) this disk's objects are
	// encrypted with, "projects/P/locations/L/keyRings/R/cryptoKeys/K", so
	// one disk can use its own key instead of the bucket's default. It is
	// sent on every write and copy, and signed into presigned PUTs, so the
	// client must send it (SignedRequest.Header). GCS's service agent needs
	// roles/cloudkms.cryptoKeyEncrypterDecrypter on the key. Not a secret.
	KMSKeyName string
	// KMSKeyFor picks the Cloud KMS key per object, for one disk serving
	// many tenants that each have their own CMEK: it gets the key as the
	// driver sees it, under a scoped disk's prefix
	// ("tenants/acme/invoice.pdf"), and returns that tenant's key name, or
	// "" for KMSKeyName. It is called on every write, copy, storage-class
	// rewrite, version restore, presigned PUT and form upload (with the
	// form's key, "prefix/${filename}"), so decide by prefix and keep it
	// fast and free of I/O. Exclusive with CustomerKey. Reads need no key:
	// GCS decrypts with the key the object was written with.
	KMSKeyFor func(key string) string
	// Clock is the time source for what the driver dates from now: the
	// RetainFor retention dates and the form uploads' (PresignPost)
	// signing date and expiry. nil means time.Now. Signed URLs (Presign)
	// are signed by the SDK at the wall clock, whatever this says.
	Clock func() time.Time
	// ChunkSize is the resumable-upload chunk size, and the size up to
	// which a body is read into memory before uploading (see Write); 0
	// means DefaultChunkSize.
	ChunkSize int
	// StorageClass is the class every write gets; empty means the
	// bucket's default class. Disk.SetStorageClass changes one object's
	// class later, and Stat reports it in Entry.StorageClass.
	StorageClass StorageClass
	// RetainFor, when positive, gives every object the disk creates (a
	// Write, a Copy, a restored version, a storage-class rewrite of an
	// object without one) a retention date this long after it, in
	// RetentionMode: GCS refuses to delete or replace it before then
	// (CodePermission). The bucket must have object retention enabled.
	// Signed URLs and forms cannot pin a retention, so Presign and
	// PresignPost are not declared with it. Disk.SetHold changes one
	// object's date later.
	RetainFor time.Duration
	// RetentionMode is the mode of retention dates the driver sets; empty
	// means RetentionUnlocked.
	RetentionMode RetentionMode
	// Versioned declares the Versions and VersionRead capabilities: the
	// bucket has Object Versioning on, so overwritten and deleted objects
	// stay readable as noncurrent generations (Disk.Versions, ReadVersion,
	// RestoreVersion).
	Versioned bool
	// CustomerKey, when set, encrypts every object this disk writes with
	// a customer-supplied key (CSEK) and presents it on every read. GCS
	// keeps only its hash, so losing it loses the data. Exclusive with
	// KMSKeyName. Signed URLs and form uploads would have to hand the key
	// to the client, so Presign and PresignPost are not declared with it.
	CustomerKey CustomerKey
}

// DefaultChunkSize matches the SDK's default chunk size: 16 MiB.
const DefaultChunkSize = 16 << 20

func (d *Driver) chunkSize() int {
	if d.cfg.ChunkSize > 0 {
		return d.cfg.ChunkSize
	}
	return DefaultChunkSize
}

// Driver is the GCS backend. Create it with New; it is safe for concurrent
// use.
type Driver struct {
	cfg    Config
	bucket *storage.BucketHandle
	// now is Config.Clock, or time.Now.
	now func() time.Time
}

// New validates cfg and returns a Driver.
func New(cfg Config) (*Driver, error) {
	if cfg.Client == nil || cfg.Bucket == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: Client and Bucket are required")
	}
	if cfg.Visibility == "" {
		cfg.Visibility = VisibilityNone
	}
	if cfg.Visibility != VisibilityNone && cfg.Visibility != VisibilityACL {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: unknown visibility mode %q", cfg.Visibility)
	}
	if cfg.PrivateACL != "" && (cfg.Visibility != VisibilityACL || !slices.Contains(PrivateACLValues, cfg.PrivateACL)) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: PrivateACL %q must be one of PrivateACLValues, with VisibilityACL", cfg.PrivateACL)
	}
	if cfg.ChunkSize < 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: negative ChunkSize")
	}
	if !cfg.CustomerKey.IsZero() && (cfg.KMSKeyName != "" || cfg.KMSKeyFor != nil) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: CustomerKey is exclusive with KMSKeyName and KMSKeyFor")
	}
	if cfg.RetainFor < 0 || (cfg.RetentionMode != "" && !slices.Contains(RetentionModeValues, cfg.RetentionMode)) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: negative RetainFor or unknown RetentionMode %q", cfg.RetentionMode)
	}
	if s := cfg.Signing; s != nil && (s.GoogleAccessID == "" || (s.PrivateKey != nil && s.SignBytes != nil)) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: Signing needs GoogleAccessID and at most one of PrivateKey or SignBytes")
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Driver{cfg: cfg, bucket: cfg.Client.Bucket(cfg.Bucket), now: now}, nil
}

// Native returns the GCS client, for the filesystem.As escape hatch.
func (d *Driver) Native() *storage.Client { return d.cfg.Client }

// Capabilities reports what GCS does, adjusted by Config.
func (d *Driver) Capabilities() filesystem.Capabilities {
	// A URL for a CSEK object only works with the key, which must not
	// leave the server; and an upload through a signed URL or form would
	// skip RetainFor, which neither can pin.
	signing := d.cfg.Signing != nil && d.cfg.CustomerKey.IsZero() && d.cfg.RetainFor <= 0
	return filesystem.Capabilities{
		Copy:           true,
		SetVisibility:  d.cfg.Visibility == VisibilityACL,
		Presign:        signing,
		Metadata:       true,
		ContentType:    true,
		Headers:        true,
		PresignHeaders: signing,
		PresignPost:    signing && d.canPost(),
		// Signed as x-goog-acl on a PUT and the acl field of a form; GCS
		// refuses ACLs under uniform bucket-level access.
		PresignVisibility: d.cfg.Visibility == VisibilityACL && signing,
		ConditionalWrite:  true,
		CompareAndSwap:    true,
		Hold:              true,
		Versions:          d.cfg.Versioned,
		VersionRead:       d.cfg.Versioned,
		StorageClass:      true,
	}
}

// mapErr classifies a GCS error, keeping the native one reachable.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	code := filesystem.CodeUnknown
	var api *googleapi.Error
	switch {
	case errors.Is(err, storage.ErrObjectNotExist), errors.Is(err, storage.ErrBucketNotExist):
		code = filesystem.CodeNotFound
	case errors.As(err, &api):
		code = codeForStatus(api.Code)
	}
	e := filesystem.NewError(code, err)
	// A 429 or 503 may say how long to wait.
	if code == filesystem.CodeUnavailable && api != nil {
		if wait, ok := fshttp.ParseRetryAfter(api.Header.Get(fshttp.HeaderRetryAfter), time.Now()); ok {
			e = e.WithRetryAfter(wait)
		}
	}
	return e
}

func codeForStatus(status int) filesystem.Code {
	switch {
	case status == http.StatusNotFound:
		return filesystem.CodeNotFound
	case status == http.StatusPreconditionFailed:
		return filesystem.CodeExists
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return filesystem.CodePermission
	case status == http.StatusTooManyRequests || status >= http.StatusInternalServerError:
		return filesystem.CodeUnavailable
	}
	return filesystem.CodeUnknown
}

func statusOf(err error) int {
	var api *googleapi.Error
	if errors.As(err, &api) {
		return api.Code
	}
	return 0
}
