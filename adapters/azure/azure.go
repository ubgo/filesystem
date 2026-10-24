// Package azure is a filesystem.Driver for Azure Blob Storage (block
// blobs), and for Azurite, its emulator.
//
// Multipart uploads are server-side only (UploadPart): Azure's Put Block
// returns no ETag, which browser uploaders such as Uppy's S3 plugin depend
// on, so PresignPart is not claimed rather than offered half-working. An
// upload's key and options are kept in a small marker blob under the
// reserved prefix MultipartDir until it completes or is aborted.
//
// It is a separate module so that the Azure SDK is a dependency only of
// applications that use Azure.
//
// Writes stage blocks and commit the block list at the end, so a failing
// body never produces a blob. Multipart uploads map to the same mechanism:
// parts are staged blocks, completion commits them, and the parts of an
// upload that is never completed expire on their own after seven days
// (Azure's rule), which is why AbortMultipart has nothing to delete.
//
// Azure has no per-object visibility (public access is a container
// setting), so SetVisibility is not claimed. Signed URLs are SAS tokens,
// signed with the account's shared key (Config.SharedKey) or, for a client
// authenticated with Entra ID, with a user delegation key the driver fetches
// and caches (Config.UserDelegation).
package azure

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
)

// Config configures a Driver.
type Config struct {
	// Client is the account client, built by the application. Required.
	Client *azblob.Client
	// Container is the container name. Required.
	Container string
	// SharedKey enables signed URLs (SAS) for downloads and uploads.
	// Exclusive with UserDelegation; with neither, Presign is off.
	SharedKey *azblob.SharedKeyCredential
	// UserDelegation enables signed URLs for a Client authenticated with
	// Entra ID (azblob.NewClient with an azcore.TokenCredential), which has
	// no account key: URLs are user delegation SAS, signed with a key the
	// driver requests through Client (Get User Delegation Key) and reuses
	// until it would expire before the URL. The identity needs the
	// generateUserDelegationKey action on the account (Storage Blob
	// Delegator, or any Storage Blob Data role) and a data role granting
	// what the URL allows; a missing role is CodePermission at Presign.
	// The account name is taken from the endpoint's host, so the endpoint
	// must be <account>.blob.<suffix> (Azurite's path-style URLs are not),
	// unless AccountName names it.
	UserDelegation bool
	// AccountName is the storage account a user delegation SAS is signed
	// for, when Client's endpoint is not <account>.blob.<suffix>: a custom
	// domain, Azure Front Door, a private-endpoint DNS name. Azure checks
	// the account in the signature, and the SDK takes it from the host's
	// first label ("files" for files.example.com), so without this every
	// URL signed behind such a front end is refused. With it, the driver
	// fetches the user delegation key through a second client, built from
	// Credential and DelegationClientOptions, whose requests go to
	// Client's host exactly as Client's would; only the account the key
	// is bound to changes. Signed URLs keep Client's host, so they work
	// only if that front end forwards SAS requests to the account
	// unchanged. Empty (the default) keeps the host's first label. It
	// needs UserDelegation and Credential, and must be a valid account
	// name (3 to 24 lower-case letters and digits).
	AccountName string
	// Credential is the Entra ID credential Client was built with, which
	// the driver cannot read back from Client; it is required with
	// AccountName and used for nothing else.
	Credential azcore.TokenCredential
	// DelegationClientOptions are the options of the client AccountName
	// builds (a custom transport, retries, a sovereign cloud's audience),
	// usually the ones Client was built with; nil means the SDK defaults.
	// They are copied, never modified. Only valid with AccountName.
	DelegationClientOptions *azblob.ClientOptions
	// EncryptionScope is the encryption scope (a named, account-level key,
	// Microsoft- or customer-managed) this disk's blobs are encrypted
	// with, so one disk can use its own key instead of the container's
	// default scope. It is sent on every write, staged block and commit,
	// and a presigned PUT's SAS carries it (ses), so a direct upload is
	// encrypted the same way. The SDK's server-side copy cannot name a
	// scope, so a disk with one does not declare Copy: the Disk copies by
	// streaming through Write, which applies the scope. The scope must exist on the account, and
	// a container that enforces its default scope refuses any other. Not
	// a secret.
	EncryptionScope string
	// EncryptionScopeFor picks the encryption scope per blob, for one disk
	// serving many tenants that each have their own (customer-managed)
	// scope: it gets the key as the driver sees it, under a scoped disk's
	// prefix ("tenants/acme/invoice.pdf"), and returns that tenant's
	// scope, or "" for EncryptionScope. It is called on every write,
	// staged block, commit, append and presigned PUT, so decide by prefix
	// and keep it fast and free of I/O. Like EncryptionScope it rules out
	// the server-side copy (Copy and VersionRead are not declared), and
	// it is exclusive with CustomerKey. Reads need no scope.
	EncryptionScopeFor func(key string) string
	// Clock is the time source for what the driver dates from now: SAS
	// start and expiry times and ExpiresAt, user delegation key validity,
	// delegated SAS expiry and RetainFor immutability policies. nil means
	// time.Now. Pass one synchronised with Azure on a host with a skewed
	// clock, or a fixed one in tests. Retry-After waits stay on the wall
	// clock.
	Clock func() time.Time
	// CopyPollInterval is how often Copy checks a pending server-side copy
	// (one that Azure completes asynchronously, typically a large blob or
	// a cross-account source); 0 means DefaultCopyPollInterval. Raise it
	// to spend fewer requests on long copies.
	CopyPollInterval time.Duration
	// CustomerKey, when set, encrypts every blob this disk writes with a
	// customer-provided key (CPK) and presents it on every read, Stat and
	// staged block. Azure keeps only its hash, so losing it loses the
	// data, and it accepts the key only over HTTPS. Exclusive with
	// EncryptionScope. A SAS URL would need the key on the client and a
	// server-side copy cannot carry it, so Presign and Copy are not
	// declared with it (the Disk copies by streaming).
	CustomerKey CustomerKey
	// AccessTier is the tier every write gets (Hot, Cool, Cold or
	// Archive); empty means the account's default. Disk.SetStorageClass
	// changes one blob's tier later, and Stat reports it in
	// Entry.StorageClass.
	AccessTier blob.AccessTier
	// RehydrateTier is the tier Restore moves an archived blob to; empty
	// means Hot. RehydratePriority is how fast (Standard, the default, or
	// High, which costs more).
	RehydrateTier     blob.AccessTier
	RehydratePriority blob.RehydratePriority
	// RetainFor, when positive, gives every blob the disk creates (a
	// Write, a completed multipart upload, a Copy, a restored version) an
	// immutability policy expiring this long after it, in RetentionMode:
	// Azure refuses to delete or replace it before then (CodePermission).
	// The container needs version-level immutability support. The policy
	// is set right after the blob is stored, a second request, so a crash
	// between the two leaves the blob stored but unprotected, and a failed
	// policy request returns an error saying so; re-apply it with
	// Disk.SetHold, or use a container-level policy when that matters. A
	// SAS upload cannot pin the policy and a protected append blob refuses
	// appends, so Presign and Append are not declared with it.
	RetainFor time.Duration
	// RetentionMode is the mode of policies the driver sets; empty means
	// blob.ImmutabilityPolicySettingUnlocked.
	RetentionMode blob.ImmutabilityPolicySetting
	// Versioned declares the Versions capability, and VersionRead unless a
	// CustomerKey or EncryptionScope rules out the server-side copy
	// RestoreVersion needs: the account has blob versioning on, so
	// overwritten and deleted blobs stay readable as versions.
	Versioned bool
	// ValidateCRC64 sends a CRC64 of every staged block, which Azure checks
	// before accepting it, so bytes corrupted between this process and
	// Azure are refused rather than stored. Off by default: it costs a
	// pass over each block, and TLS already protects the wire.
	ValidateCRC64 bool
	// BlockSize is the size of each block Write stages, in bytes; 0 means
	// DefaultBlockSize. A block blob holds at most MaxBlocks blocks, so it
	// caps how large one Write can be: MaxBlocks times BlockSize (about
	// 390.6 GiB at the default, about 190.7 TiB at MaxBlockSize). It must
	// be in [MinBlockSize, MaxBlockSize]. Raise it for backups or video
	// archives; each in-flight block is held in memory.
	BlockSize int64
	// Concurrency is how many blocks one Write stages at once; 0 means
	// DefaultConcurrency. A Write holds up to Concurrency times BlockSize
	// bytes in memory (40 MiB at the defaults), so the two together bound
	// an upload's memory whatever the machine's core count.
	Concurrency int
}

// Block-size bounds of Config.BlockSize. Azure refuses a staged block over
// MaxBlockSize (4000 MiB), and the SDK silently raises anything below
// MinBlockSize (1 MiB) to it, so a smaller value is refused rather than
// ignored.
const (
	MinBlockSize int64 = 1 << 20
	MaxBlockSize int64 = blockblob.MaxStageBlockBytes
	// MaxBlocks is how many blocks a block blob may have, which is why
	// BlockSize caps a blob's size.
	MaxBlocks = blockblob.MaxBlocks
)

// Write defaults. DefaultBlockSize (8 MiB, the s3 adapter's default part
// size) lifts the largest blob from the SDK default's 48.8 GiB (50,000
// blocks of 1 MiB) to about 390.6 GiB. DefaultConcurrency is fixed rather
// than the SDK's core-count default (8 to 96), so an upload's memory,
// DefaultBlockSize times DefaultConcurrency, does not grow with the host.
const (
	DefaultBlockSize   int64 = 8 << 20
	DefaultConcurrency       = 5
)

// blockSize is Config.BlockSize, or DefaultBlockSize when unset.
func (d *Driver) blockSize() int64 {
	if d.cfg.BlockSize == 0 {
		return DefaultBlockSize
	}
	return d.cfg.BlockSize
}

// concurrency is Config.Concurrency, or DefaultConcurrency when unset.
func (d *Driver) concurrency() int {
	if d.cfg.Concurrency == 0 {
		return DefaultConcurrency
	}
	return d.cfg.Concurrency
}

// scopeName is the encryption scope for key: Config.EncryptionScopeFor's
// choice, else Config.EncryptionScope (empty for the container default).
func (d *Driver) scopeName(key string) string {
	if d.cfg.EncryptionScopeFor != nil {
		if s := d.cfg.EncryptionScopeFor(key); s != "" {
			return s
		}
	}
	return d.cfg.EncryptionScope
}

// MaxUserDelegationKeyLifetime is Azure's limit on a user delegation key:
// seven days from the request. Keys are requested for this long, so one
// key serves every URL until the newest would outlive it.
const MaxUserDelegationKeyLifetime = 7 * 24 * time.Hour

// delegationKeySkew back-dates a key's start, so a server whose clock runs
// behind Azure's does not sign with a key that is not valid yet, and
// shortens its requested expiry by as much, so a server whose clock runs
// ahead does not ask for more than Azure's seven days.
const delegationKeySkew = 5 * time.Minute

// delegationKeyReuse is how much shorter than asked a URL may live so the
// cached key can sign it. A fresh key cannot reach past now plus its seven
// days less skew, so without this slack every URL asking for close to
// seven days would fetch a key; with it, such URLs end up to this long
// earlier (SignedRequest.ExpiresAt reports when), and one key serves them
// for this long.
const delegationKeyReuse = time.Hour

// Driver is the Azure Blob backend. Create it with New; it is safe for
// concurrent use.
type Driver struct {
	cfg       Config
	container *container.Client
	// now is Config.Clock, or time.Now.
	now func() time.Time

	// delegation caches the user delegation key; mu serialises fetches
	// so concurrent Presign calls share one request.
	mu         sync.Mutex
	delegation *sas.UserDelegationCredential
	delegExp   time.Time
	// delegationClient fetches user delegation keys: Client's service
	// client, or the one Config.AccountName builds.
	delegationClient *service.Client
}

// New validates cfg and returns a Driver.
func New(cfg Config) (*Driver, error) {
	if cfg.Client == nil || cfg.Container == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: Client and Container are required")
	}
	if cfg.SharedKey != nil && cfg.UserDelegation {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: SharedKey and UserDelegation are exclusive")
	}
	if !cfg.CustomerKey.IsZero() && (cfg.EncryptionScope != "" || cfg.EncryptionScopeFor != nil) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: CustomerKey is exclusive with EncryptionScope and EncryptionScopeFor")
	}
	if cfg.CopyPollInterval < 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: negative CopyPollInterval")
	}
	if cfg.RetainFor < 0 || (cfg.RetentionMode != "" && !slices.Contains(blob.PossibleImmutabilityPolicySettingValues(), cfg.RetentionMode)) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: negative RetainFor or unknown RetentionMode %q", cfg.RetentionMode)
	}
	if cfg.BlockSize != 0 && (cfg.BlockSize < MinBlockSize || cfg.BlockSize > MaxBlockSize) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: BlockSize %d is not in [%d, %d]", cfg.BlockSize, MinBlockSize, MaxBlockSize)
	}
	if cfg.Concurrency < 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: negative Concurrency")
	}
	delegationClient, err := newDelegationClient(cfg)
	if err != nil {
		return nil, err
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Driver{cfg: cfg, container: cfg.Client.ServiceClient().NewContainerClient(cfg.Container), now: now, delegationClient: delegationClient}, nil
}

// Native returns the account client, for the filesystem.As escape hatch.
func (d *Driver) Native() *azblob.Client { return d.cfg.Client }

// Capabilities reports what Azure Blob does, adjusted by Config.
func (d *Driver) Capabilities() filesystem.Capabilities {
	cpk := !d.cfg.CustomerKey.IsZero()
	retained := d.cfg.RetainFor > 0
	// A SAS URL cannot carry a customer-provided key, nor pin RetainFor's
	// policy on an upload through it.
	signing := (d.cfg.SharedKey != nil || d.cfg.UserDelegation) && !cpk && !retained
	// A server-side copy would use the container's default scope, and
	// cannot present a customer-provided key.
	serverCopy := d.cfg.EncryptionScope == "" && d.cfg.EncryptionScopeFor == nil && !cpk
	return filesystem.Capabilities{
		Copy:             serverCopy,
		Presign:          signing,
		PresignIP:        signing,
		Multipart:        true,
		Metadata:         true,
		ContentType:      true,
		Headers:          true,
		ConditionalWrite: true,
		CompareAndSwap:   true,
		SweepAbandoned:   true,
		FindAbandoned:    true,
		Append:           !retained, // a blob policy refuses later appends
		Tags:             true,
		Hold:             true,
		Restore:          true,
		Versions:         d.cfg.Versioned,
		VersionRead:      d.cfg.Versioned && serverCopy,
		StorageClass:     true,
	}
}

// mapErr classifies an Azure error, keeping the native one reachable.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	code := filesystem.CodeUnknown
	var resp *azcore.ResponseError
	switch {
	case bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound):
		code = filesystem.CodeNotFound
	case bloberror.HasCode(err, bloberror.BlobImmutableDueToPolicy, bloberror.ImmutabilityPolicyDeleteOnLockedPolicy):
		// A legal hold or an unexpired immutability policy protects it.
		code = filesystem.CodePermission
	case bloberror.HasCode(err, bloberror.BlobArchived, bloberror.BlobBeingRehydrated):
		return filesystem.NewError(filesystem.CodeConflict, errors.Join(ErrArchived, err))
	case bloberror.HasCode(err, bloberror.InvalidBlobType):
		// An append to a block blob, or a block write to an append blob.
		code = filesystem.CodeConflict
	case bloberror.HasCode(err, bloberror.ConditionNotMet, bloberror.BlobAlreadyExists):
		code = filesystem.CodeExists
	case bloberror.HasCode(err, bloberror.InvalidBlockList, bloberror.InvalidBlockID):
		code = filesystem.CodeInvalidArgument
	case errors.As(err, &resp):
		code = codeForStatus(resp.StatusCode)
	}
	if code == filesystem.CodeExists && errors.As(err, &resp) && holdRefusal(resp.ErrorCode) {
		code = filesystem.CodePermission
	}
	e := filesystem.NewError(code, err)
	// A 429 or 503 (ServerBusy) may say how long to wait.
	if code == filesystem.CodeUnavailable && errors.As(err, &resp) && resp.RawResponse != nil {
		if wait, ok := fshttp.ParseRetryAfter(resp.RawResponse.Header.Get(fshttp.HeaderRetryAfter), time.Now()); ok {
			e = e.WithRetryAfter(wait)
		}
	}
	return e
}

// holdRefusal reports whether an Azure error code is a refusal caused by a
// legal hold or an immutability policy. Azure names these codes after the
// protection (BlobImmutableDueToPolicy, BlobImmutableDueToLegalHold,
// ImmutabilityPolicy...), and lists more over service versions than the
// SDK's constants, so they are matched by those words: a protected blob is
// CodePermission whatever the operation, not the CodeExists other 409s get.
func holdRefusal(code string) bool {
	return strings.Contains(code, "Immutab") || strings.Contains(code, "LegalHold")
}

func codeForStatus(status int) filesystem.Code {
	switch {
	case status == http.StatusNotFound:
		return filesystem.CodeNotFound
	case status == http.StatusPreconditionFailed || status == http.StatusConflict:
		return filesystem.CodeExists
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return filesystem.CodePermission
	case status == http.StatusTooManyRequests || status >= http.StatusInternalServerError:
		return filesystem.CodeUnavailable
	}
	return filesystem.CodeUnknown
}
