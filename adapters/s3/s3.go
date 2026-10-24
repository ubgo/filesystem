// Package s3 is a filesystem.Driver for Amazon S3 and every S3-compatible
// store: Cloudflare R2, Backblaze B2, Wasabi, DigitalOcean Spaces, Garage,
// SeaweedFS, MinIO and others.
//
// It is a separate module so that the AWS SDK is a dependency only of
// applications that use S3. Uploads go through the SDK's transfermanager
// (the successor of the deprecated feature/s3/manager, deprecated on
// 2026-02-05), so bodies of unknown length stream as multipart uploads
// without being buffered whole.
//
// Capabilities that differ between providers are configuration, not
// guesses (see Config): per-object ACLs exist on AWS but not on R2 or most
// self-hosted stores, and conditional writes are not honoured everywhere.
// The driver claims a capability only when its configuration says the
// provider supports it, so a disk never silently drops a guarantee.
package s3

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
)

// VisibilityMode says how the provider expresses object visibility.
type VisibilityMode string

const (
	// VisibilityNone: the bucket's policy decides who can read objects; the
	// driver sends no ACLs and does not claim the SetVisibility capability.
	// Correct for R2, B2, most self-hosted stores, and AWS buckets with
	// "bucket owner enforced" object ownership (the AWS default since 2023).
	VisibilityNone VisibilityMode = "none"
	// VisibilityACL: per-object canned ACLs (public-read / private), for AWS
	// buckets that still allow ACLs.
	VisibilityACL VisibilityMode = "acl"
)

// VisibilityModeValues lists every VisibilityMode.
var VisibilityModeValues = []VisibilityMode{VisibilityNone, VisibilityACL}

// Config configures a Driver.
type Config struct {
	// Client is the S3 client, built by the application with its own
	// region, endpoint and credentials. Required.
	Client *s3.Client
	// Bucket is the bucket name. Required.
	Bucket string
	// Visibility is how the provider expresses visibility. Empty means
	// VisibilityNone, the setting that is correct on every provider.
	Visibility VisibilityMode
	// ConditionalWrites declares that the provider honours If-None-Match on
	// PUT (AWS since 2024, R2, recent MinIO). Off by default: a provider that
	// ignores the header would turn IfNotExists into a silent overwrite.
	ConditionalWrites bool
	// ServerSideEncryption is sent on every write, copy, multipart upload
	// and presigned PUT when set: "AES256" (SSE-S3), "aws:kms" or
	// "aws:kms:dsse" (SSE-KMS). Empty leaves it to the bucket's default
	// encryption.
	ServerSideEncryption types.ServerSideEncryption
	// SSEKMSKeyID is the KMS key (ID, ARN or alias ARN) objects are
	// encrypted with, so one disk can use its own key instead of the
	// bucket's. It needs a KMS ServerSideEncryption; empty selects
	// "aws:kms". A presigned PUT signs it, so the client must send it
	// (SignedRequest.Header). Not a secret: S3 returns it on every HEAD.
	SSEKMSKeyID string
	// SSEKMSKeyFor picks the KMS key per object, for one disk (one client,
	// one bucket) serving many tenants that each have their own key: it
	// gets the key as the driver sees it, under a scoped disk's prefix
	// ("tenants/acme/invoice.pdf"), and returns that tenant's KMS key, or
	// "" for SSEKMSKeyID. It is called on every write, copy, multipart
	// upload, presigned PUT and presigned POST (with the form's key,
	// "prefix/${filename}"), so decide by prefix and keep it fast and
	// free of I/O. RequestOptions.SSEKMSKeyID overrides it for one call.
	// Like SSEKMSKeyID it needs a KMS (or empty) ServerSideEncryption, and
	// a key it returns while ServerSideEncryption is empty selects
	// "aws:kms" for that object. Not with SSECustomerKey. Reads need no
	// key: S3 decrypts with the key the object was written with, so
	// changing a tenant's key leaves its old objects readable while the
	// role may still use the old key.
	SSEKMSKeyFor func(key string) string
	// StorageClass is sent on every write when set.
	StorageClass types.StorageClass
	// StorageClasses declares the StorageClass capability: the store keeps
	// a storage class per object and reports it from HEAD (AWS, R2,
	// Wasabi), so Disk.SetStorageClass changes it and Stat reports it in
	// Entry.StorageClass. Off by default, because self-hosted stores
	// (RustFS, SeaweedFS) accept the header and keep nothing, which would
	// make a class change a silent no-op.
	StorageClasses bool
	// PartSize is the multipart part size for uploads through the transfer
	// manager; 0 means the SDK default (8 MiB).
	PartSize int64
	// Concurrency is how many parts of one upload the transfer manager
	// sends at once; 0 means the SDK default (5). Raise it for throughput
	// on a fast link; lower it to bound memory on a small machine, since
	// each part in flight holds PartSize bytes.
	Concurrency int
	// ExpectedBucketOwner is the AWS account ID that must own the bucket:
	// sent on every request (x-amz-expected-bucket-owner, which a
	// presigned URL carries in its signed query), so S3 refuses the call
	// with 403 if the bucket changed hands. Set it for a
	// bring-your-own bucket, so a deleted and re-created bucket of the
	// same name in another account never receives the tenant's data.
	// Empty sends nothing.
	ExpectedBucketOwner string
	// RequestPayer, set to "requester", accepts the charges of a
	// requester-pays bucket on every request (x-amz-request-payer, in a
	// presigned URL's signed query like ExpectedBucketOwner); S3 refuses
	// requests on such a bucket without it. Empty sends nothing.
	RequestPayer types.RequestPayer
	// PrivateACL is the canned ACL VisibilityACL gives private objects;
	// empty means CannedPrivate. Set CannedBucketOwnerFullControl when this
	// disk writes into a bucket another account owns, so the bucket owner
	// can read what it receives. Public ACLs are refused here: public
	// objects get public-read, as before.
	PrivateACL CannedACL
	// Clock is the time source for what the driver computes from now:
	// ObjectLockDays retention dates, the MaxRetention check, whether a
	// retention still holds an object, and the signing time and ExpiresAt
	// of presigned URLs, parts and POST forms. nil means time.Now (and the
	// SDK's own clock for signing). Pass one synchronised with S3 on a
	// host with a skewed clock, or a fixed one in tests. Retry-After waits
	// stay on the wall clock, since they are real waits.
	Clock func() time.Time
	// PruneEmptyPrefixes makes a non-recursive List drop "directories" that
	// no longer contain any object, at one extra request per directory
	// listed. S3 never lists an empty prefix, but filer-backed stores such
	// as SeaweedFS keep a directory after its last object is deleted; turn
	// this on for them so listings match every other backend.
	PruneEmptyPrefixes bool
	// SweepUploads declares the SweepAbandoned capability: the driver
	// lists the bucket's unfinished multipart uploads and aborts those
	// started before the cutoff. Off by default, because a bucket
	// lifecycle rule (AbortIncompleteMultipartUpload) does the same job
	// server-side, for free and without the application's credentials
	// needing s3:ListBucketMultipartUploads; turn it on for stores without
	// lifecycle rules. The sweep is bucket-wide: it also aborts uploads
	// another application started in the same bucket.
	SweepUploads bool
	// UnverifiedSignedHeaders declares that the store does not verify the
	// headers a SigV4 presigned URL signs (SeaweedFS run without
	// -s3.config checks no signature at all). The driver then does not
	// claim PresignHeaders, because such a store would accept a presigned
	// PUT without the Cache-Control or Content-Disposition it pins. Off by
	// default: AWS, R2 and every store that checks signatures enforce them.
	UnverifiedSignedHeaders bool
	// Tagging declares Tags: the store keeps object tags (AWS, MinIO,
	// SeaweedFS, RustFS; not R2). Off by default, like every provider
	// difference, so a disk never claims what its store drops.
	// RequestOptions.Tags sets tags on write.
	Tagging bool
	// ObjectLock declares Hold: the bucket was created with S3 Object Lock,
	// so objects can carry legal holds and retention dates, and Delete
	// refuses a held object with CodePermission. CheckBucket verifies the
	// bucket really has it.
	ObjectLock bool
	// ObjectLockMode is the retention mode of the retention dates this
	// disk sets (SetHold, ObjectLockDays, RequestOptions.RetainUntil).
	// Empty means GOVERNANCE, which a user with
	// s3:BypassGovernanceRetention can lift; COMPLIANCE (WORM) cannot be
	// shortened or removed by anyone until it expires.
	ObjectLockMode types.ObjectLockMode
	// ObjectLockDays, when positive, gives every object this disk writes or
	// copies a retention of this many days in ObjectLockMode. Needs
	// ObjectLock.
	ObjectLockDays int
	// MaxRetention caps every retention date this disk sets (SetHold,
	// ObjectLockDays, RequestOptions.RetainUntil): a later date is refused
	// with CodeInvalidArgument. It guards against a mistaken date, which in
	// COMPLIANCE mode nobody could ever shorten. 0 means
	// DefaultMaxRetention; raise it for longer legal requirements.
	MaxRetention time.Duration
	// Versioning declares Versions: the bucket keeps every version of an
	// object, and VersionRead: Disk.ReadVersion and Disk.RestoreVersion.
	// CheckBucket verifies versioning is enabled.
	Versioning bool
	// Restore declares Restore: the disk stores objects in archive classes
	// (GLACIER, DEEP_ARCHIVE) that must be restored before they can be
	// read. A read of an archived object fails with CodeConflict and
	// ErrArchived; Stat reports the restore state under MetaRestore.
	Restore bool
	// RestoreTier is the retrieval tier of Disk.Restore: "Standard" (the
	// default when empty), "Bulk" (cheaper, slower) or "Expedited".
	RestoreTier types.Tier
	// SSECustomerKey encrypts every object with a key the application
	// keeps (SSE-C): it is sent on every write, read, copy, multipart
	// part and presigned request, and S3 refuses to serve the object
	// without it. A presigned request carries the key in its headers, so
	// whoever receives the request holds the key. Exclusive with
	// ServerSideEncryption and SSEKMSKeyID. With it the disk declares no
	// presigned requests (Presign, PresignPart, PresignHeaders,
	// PresignPost) unless PresignCustomerKey opts in, because a presigned
	// request must carry the key: every client handed one learns the key
	// that decrypts every object of the disk.
	SSECustomerKey CustomerKey
	// PresignCustomerKey declares Presign and PresignPart under an
	// SSECustomerKey, accepting that each SignedRequest.Header holds the
	// disk-wide key. Use it only when the clients receiving signed URLs are
	// as trusted as the application (a server-side worker), never for
	// browsers. PresignPost stays off.
	PresignCustomerKey bool
	// PresignWithoutChecksum declares Presign under a ChecksumAlgorithm,
	// whose checksum a presigned PUT cannot sign (the body does not exist
	// yet): such uploads are stored without that checksum, so S3 does not
	// verify them against it and Stat reports none. Presigned parts and
	// forms stay off. Without it a disk with a ChecksumAlgorithm signs no
	// URLs at all, GETs included, because Presign covers both methods.
	PresignWithoutChecksum bool
	// ChecksumAlgorithm makes S3 store and verify a checksum of this
	// algorithm on every write and copy (CRC32, CRC32C, CRC64NVME, SHA1,
	// SHA256); Stat reports it under MetaChecksumPrefix. Empty leaves the
	// SDK default. A body that does not match is refused by S3 with
	// CodeInvalidArgument, as a SHA-256 mismatch is.
	ChecksumAlgorithm types.ChecksumAlgorithm
}

// Driver is the S3 backend. Create it with New; it is safe for concurrent
// use.
type Driver struct {
	cfg Config
	// now is Config.Clock, or time.Now.
	now       func() time.Time
	client    *s3.Client
	presigner *s3.PresignClient
	transfer  *transfermanager.Client
}

// New validates cfg and returns a Driver.
func New(cfg Config) (*Driver, error) {
	if cfg.Client == nil || cfg.Bucket == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: Client and Bucket are required")
	}
	if cfg.Visibility == "" {
		cfg.Visibility = VisibilityNone
	}
	if cfg.Visibility != VisibilityNone && cfg.Visibility != VisibilityACL {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: unknown visibility mode %q", cfg.Visibility)
	}
	if cfg.PartSize < 0 || cfg.Concurrency < 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: negative PartSize or Concurrency")
	}
	if err := validateRequestHeaders(&cfg); err != nil {
		return nil, err
	}
	if cfg.SSEKMSKeyID != "" || cfg.SSEKMSKeyFor != nil {
		if _, err := kmsEncryption(cfg.ServerSideEncryption); err != nil {
			return nil, err
		}
	}
	if cfg.SSEKMSKeyID != "" && cfg.ServerSideEncryption == "" {
		cfg.ServerSideEncryption = types.ServerSideEncryptionAwsKms
	}
	if err := validateAdvanced(&cfg); err != nil {
		return nil, err
	}
	client := withRequestHeaders(cfg.Client, cfg.ExpectedBucketOwner, cfg.RequestPayer)
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Driver{
		cfg:       cfg,
		now:       now,
		client:    client,
		presigner: s3.NewPresignClient(client, clockSigning(client, cfg.Clock)),
		transfer: transfermanager.New(client, func(o *transfermanager.Options) {
			if cfg.PartSize > 0 {
				o.PartSizeBytes = cfg.PartSize
			}
			if cfg.Concurrency > 0 {
				o.Concurrency = cfg.Concurrency
			}
		}),
	}, nil
}

// DefaultMaxRetention is Config.MaxRetention when it is 0: ten years, above
// the common regulatory periods (SEC 17a-4 six years, most others seven),
// so only a mistake exceeds it.
const DefaultMaxRetention = 10 * 365 * 24 * time.Hour

// validateAdvanced checks the object-lock, restore, checksum and SSE-C
// fields of cfg and fills their defaults.
func validateAdvanced(cfg *Config) error {
	switch {
	case cfg.MaxRetention < 0:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: negative MaxRetention")
	case cfg.MaxRetention == 0:
		cfg.MaxRetention = DefaultMaxRetention
	}
	if time.Duration(cfg.ObjectLockDays)*24*time.Hour > cfg.MaxRetention {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: ObjectLockDays %d is beyond MaxRetention %s", cfg.ObjectLockDays, cfg.MaxRetention)
	}
	switch cfg.ObjectLockMode {
	case "":
		cfg.ObjectLockMode = types.ObjectLockModeGovernance
	case types.ObjectLockModeGovernance, types.ObjectLockModeCompliance:
	default:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: unknown object lock mode %q", cfg.ObjectLockMode)
	}
	if cfg.ObjectLockDays < 0 || (cfg.ObjectLockDays > 0 && !cfg.ObjectLock) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: ObjectLockDays must be positive and needs ObjectLock")
	}
	switch cfg.RestoreTier {
	case "":
		cfg.RestoreTier = types.TierStandard
	case types.TierStandard, types.TierBulk, types.TierExpedited:
	default:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: unknown restore tier %q", cfg.RestoreTier)
	}
	if !validChecksum(cfg.ChecksumAlgorithm) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: unsupported checksum algorithm %q", cfg.ChecksumAlgorithm)
	}
	if !cfg.SSECustomerKey.IsZero() && (cfg.ServerSideEncryption != "" || cfg.SSEKMSKeyID != "" || cfg.SSEKMSKeyFor != nil) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: SSECustomerKey is exclusive with ServerSideEncryption, SSEKMSKeyID and SSEKMSKeyFor")
	}
	return nil
}

// Native returns the S3 client, for the filesystem.As escape hatch.
func (d *Driver) Native() *s3.Client { return d.client }

// Capabilities reports what S3 does, adjusted by Config.
func (d *Driver) Capabilities() filesystem.Capabilities {
	presign := (d.cfg.SSECustomerKey.IsZero() || d.cfg.PresignCustomerKey) && (d.cfg.ChecksumAlgorithm == "" || d.cfg.PresignWithoutChecksum)
	post := d.cfg.SSECustomerKey.IsZero() && d.cfg.ChecksumAlgorithm == ""
	return filesystem.Capabilities{
		Copy:          true,
		SetVisibility: d.cfg.Visibility == VisibilityACL,
		DeletePrefix:  true,
		DeleteKeys:    true,
		Presign:       presign,
		Multipart:     true,
		// Presigned parts and forms cannot carry the checksum S3 needs for
		// a configured algorithm.
		PresignPart:    presign && d.cfg.ChecksumAlgorithm == "",
		PresignPost:    post,
		SweepAbandoned: d.cfg.SweepUploads,
		FindAbandoned:  d.cfg.SweepUploads,
		Metadata:       true,
		ContentType:    true,
		Headers:        true,
		PresignHeaders: presign && !d.cfg.UnverifiedSignedHeaders,
		// The ACL is a signed header on a PUT and a policy condition on a
		// form; a store that checks no signature would let a client
		// change it, so it is claimed only where signatures are checked.
		PresignVisibility: d.cfg.Visibility == VisibilityACL && !d.cfg.UnverifiedSignedHeaders && (presign || post),
		ConditionalWrite:  d.cfg.ConditionalWrites,
		ChecksumSHA256:    d.cfg.ChecksumAlgorithm == "" || d.cfg.ChecksumAlgorithm == types.ChecksumAlgorithmSha256,
		Tags:              d.cfg.Tagging,
		Hold:              d.cfg.ObjectLock,
		Versions:          d.cfg.Versioning,
		VersionRead:       d.cfg.Versioning,
		StorageClass:      d.cfg.StorageClasses,
		Restore:           d.cfg.Restore,
	}
}

func (d *Driver) bucket() *string { return aws.String(d.cfg.Bucket) }

// kmsEncryption returns the ServerSideEncryption an object written under a
// KMS key gets on a disk configured with sse: sse itself when it is a KMS
// mode, "aws:kms" when it is empty, and CodeInvalidArgument for SSE-S3,
// which cannot take a key.
func kmsEncryption(sse types.ServerSideEncryption) (types.ServerSideEncryption, error) {
	switch sse {
	case "":
		return types.ServerSideEncryptionAwsKms, nil
	case types.ServerSideEncryptionAwsKms, types.ServerSideEncryptionAwsKmsDsse:
		return sse, nil
	}
	return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: a KMS key needs ServerSideEncryption %q or %q, not %q", types.ServerSideEncryptionAwsKms, types.ServerSideEncryptionAwsKmsDsse, sse)
}

// S3 error codes the driver classifies. They are the wire values S3 and its
// compatibles return; HeadObject has no body, so its 404 arrives as
// "NotFound" rather than "NoSuchKey".
const (
	errNoSuchKey          = "NoSuchKey"
	errNotFound           = "NotFound"
	errNoSuchUpload       = "NoSuchUpload"
	errPreconditionFailed = "PreconditionFailed"
	errInvalidRange       = "InvalidRange"
	errAccessDenied       = "AccessDenied"
	errSlowDown           = "SlowDown"
	errInvalidPart        = "InvalidPart"
	errInvalidPartOrder   = "InvalidPartOrder"
	errEntityTooSmall     = "EntityTooSmall"
	// The body did not match the digest the write sent: the checksum
	// header (BadDigest, XAmzContentChecksumMismatch) or the SigV4
	// payload hash (XAmzContentSHA256Mismatch).
	errBadDigest             = "BadDigest"
	errChecksumMismatch      = "XAmzContentChecksumMismatch"
	errContentSHA256Mismatch = "XAmzContentSHA256Mismatch"
	// A read of an object in an archive class that is not restored.
	errInvalidObjectState = "InvalidObjectState"
	// A storage class the store does not know.
	errInvalidStorageClass = "InvalidStorageClass"
)

// mapErr classifies an SDK error into a filesystem code, keeping the native
// error reachable through errors.As.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	code := filesystem.CodeUnknown
	var api smithy.APIError
	var resp *awshttp.ResponseError
	switch {
	case errors.As(err, &api) && api.ErrorCode() == errInvalidObjectState:
		return filesystem.NewError(filesystem.CodeConflict, fmt.Errorf("%w: %w", ErrArchived, err))
	case errors.As(err, &api) && isOneOf(api.ErrorCode(), errNoSuchKey, errNotFound, errNoSuchUpload):
		code = filesystem.CodeNotFound
	case errors.As(err, &api) && api.ErrorCode() == errPreconditionFailed:
		code = filesystem.CodeExists
	case errors.As(err, &api) && api.ErrorCode() == errAccessDenied:
		code = filesystem.CodePermission
	case errors.As(err, &api) && isOneOf(api.ErrorCode(), errInvalidPart, errInvalidPartOrder, errEntityTooSmall, errInvalidStorageClass, errBadDigest, errChecksumMismatch, errContentSHA256Mismatch):
		code = filesystem.CodeInvalidArgument
	case errors.As(err, &api) && api.ErrorCode() == errSlowDown:
		code = filesystem.CodeUnavailable
	case errors.As(err, &resp) && httpResponse(resp) != nil:
		code = codeForStatus(httpResponse(resp).StatusCode)
	}
	e := filesystem.NewError(code, err)
	// A throttled or unavailable answer may say how long to wait (S3 sends
	// Retry-After on 503 SlowDown from some providers, R2 on 429).
	if code == filesystem.CodeUnavailable && errors.As(err, &resp) && httpResponse(resp) != nil {
		if wait, ok := fshttp.ParseRetryAfter(httpResponse(resp).Header.Get(fshttp.HeaderRetryAfter), time.Now()); ok {
			e = e.WithRetryAfter(wait)
		}
	}
	return e
}

// httpResponse is the HTTP response behind an SDK response error, or nil
// when the SDK's wrapper carries none (an error built by middleware rather
// than from a real answer). resp and its inner smithy error are non-nil:
// errors.As found resp, and the SDK's As support needs the inner error.
func httpResponse(resp *awshttp.ResponseError) *http.Response {
	if resp.ResponseError.Response == nil {
		return nil
	}
	return resp.ResponseError.Response.Response
}

func codeForStatus(status int) filesystem.Code {
	switch {
	case status == http.StatusNotFound:
		return filesystem.CodeNotFound
	case status == http.StatusPreconditionFailed:
		return filesystem.CodeExists
	case status == http.StatusForbidden:
		return filesystem.CodePermission
	case status == http.StatusTooManyRequests || status >= http.StatusInternalServerError:
		return filesystem.CodeUnavailable
	}
	return filesystem.CodeUnknown
}

func isOneOf(code string, codes ...string) bool {
	for _, c := range codes {
		if code == c {
			return true
		}
	}
	return false
}

func errCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

// acl maps a visibility to a canned ACL in ACL mode, and to nothing
// otherwise.
func (d *Driver) acl(v filesystem.Visibility) types.ObjectCannedACL {
	if d.cfg.Visibility != VisibilityACL {
		return ""
	}
	if v == filesystem.VisibilityPublic {
		return types.ObjectCannedACLPublicRead
	}
	if d.cfg.PrivateACL != "" {
		return types.ObjectCannedACL(d.cfg.PrivateACL)
	}
	return types.ObjectCannedACLPrivate
}
