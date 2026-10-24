package azure

import (
	"context"
	"slices"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/ubgo/filesystem"
)

// RequestOptions are Azure settings for the operations on one key, for
// what filesystem.WriteOptions has no portable field for: one tenant's
// encryption scope, an archive tier for one upload, index tags, a legal
// hold. Attach them with WithRequestOptions and call the Disk as usual:
// the options travel through the Disk (key cleaning, a scoped disk's
// prefix, interceptors) to the driver, which is why they ride on the
// context rather than on a driver method. They apply only to that exact
// key, so a context reused for another write (a staging blob, a thumbnail,
// the same name in another directory) never inherits a hold or a scope.
// Every field is optional and overrides the disk's Config for those calls
// only; a field a call cannot honour is refused with CodeUnsupported
// rather than ignored.
//
// Where they apply: Write, CompleteMultipart, Copy and RestoreVersion
// honour every write field (Copy and RestoreVersion refuse
// EncryptionScope, which Azure's server-side copy cannot set); a
// multipart upload takes them from CreateMultipart's context and keeps
// them for its parts and completion; Append honours EncryptionScope and
// Tags (when it creates the blob) and refuses the rest; a presigned PUT
// honours EncryptionScope (a SAS cannot pin the others) and refuses the
// rest; Restore and SetStorageClass honour the rehydration fields.
//
// It is this package's one escape hatch for per-call Azure features; the
// native client (filesystem.As[*azblob.Client]) is the one for everything
// else.
type RequestOptions struct {
	// EncryptionScope encrypts a written blob with this scope, overriding
	// Config.EncryptionScopeFor and Config.EncryptionScope. Exclusive with
	// Config.CustomerKey (CodeInvalidArgument).
	EncryptionScope string `json:"encryption_scope,omitempty"`
	// AccessTier of a written or copied blob, overriding
	// Config.AccessTier (Hot, Cool, Cold or Archive).
	AccessTier blob.AccessTier `json:"access_tier,omitempty"`
	// Tags are the blob index tags stored with a written or copied blob,
	// in the same request, so the blob is never visible untagged (a
	// lifecycle rule keyed on a tag cannot miss it). Azure allows ten.
	Tags map[string]string `json:"tags,omitempty"`
	// LegalHold places a legal hold on a written or copied blob. Like
	// Config.RetainFor it is a second request after the blob is stored;
	// a failure says the blob is stored but unprotected. The container
	// needs version-level immutability support.
	LegalHold bool `json:"legal_hold,omitempty"`
	// RetainUntil gives a written or copied blob an immutability policy
	// expiring then, in Config.RetentionMode, instead of Config.RetainFor's
	// (also a second request, as LegalHold).
	RetainUntil time.Time `json:"retain_until,omitzero"`
	// RehydrateTier and RehydratePriority override Config's for Restore
	// and for SetStorageClass moving an archived blob online.
	RehydrateTier     blob.AccessTier        `json:"rehydrate_tier,omitempty"`
	RehydratePriority blob.RehydratePriority `json:"rehydrate_priority,omitempty"`
}

type requestOptionsKey struct{}

// keyedOptions are RequestOptions bound to the key they are for.
type keyedOptions struct {
	key  string
	opts RequestOptions
}

// WithRequestOptions returns a context carrying o for the calls on disk's
// key made with it (the destination of a Copy); a call on any other key,
// however similar, ignores them. It binds the options to the key the
// driver will see (disk.DriverKey: cleaned, under a scoped disk's prefix),
// so a caller never joins prefixes and cannot bind them to the wrong key;
// an invalid key is returned as the error, with ctx unchanged. Use the
// returned context with the same disk. The latest call wins: options are
// not merged. They are not consumed, so a retried call gets them again.
func WithRequestOptions(ctx context.Context, disk *filesystem.Disk, key string, o RequestOptions) (context.Context, error) {
	k, err := disk.DriverKey(key)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, requestOptionsKey{}, keyedOptions{key: k, opts: o}), nil
}

// requestOptions returns the options ctx carries for driver key key, or
// the zero value.
func requestOptions(ctx context.Context, key string) RequestOptions {
	k, ok := ctx.Value(requestOptionsKey{}).(keyedOptions)
	if !ok || k.key == "" || key != k.key {
		return RequestOptions{}
	}
	return k.opts
}

// writeSettings are the Azure-only fields of one write, copy or multipart
// upload: the disk's Config overridden by the call's RequestOptions.
type writeSettings struct {
	// scope is the encryption scope, "" for the container default.
	scope       string
	tier        *blob.AccessTier
	tags        map[string]string
	legalHold   bool
	retainUntil time.Time
}

// cpkScope is ws.scope for an SDK request, nil when there is none.
func (ws writeSettings) cpkScope() *blob.CPKScopeInfo {
	if ws.scope == "" {
		return nil
	}
	return &blob.CPKScopeInfo{EncryptionScope: &ws.scope}
}

// settings resolves the write settings of key under o, refusing values
// Azure does not know and a scope on a customer-key disk.
func (d *Driver) settings(key string, o RequestOptions) (writeSettings, error) {
	if err := checkRequestOptions(o); err != nil {
		return writeSettings{}, err
	}
	ws := writeSettings{scope: d.scopeName(key), tier: d.tier(), tags: o.Tags, legalHold: o.LegalHold, retainUntil: o.RetainUntil}
	if o.EncryptionScope != "" {
		if !d.cfg.CustomerKey.IsZero() {
			return writeSettings{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: an encryption scope is exclusive with Config.CustomerKey")
		}
		ws.scope = o.EncryptionScope
	}
	if o.AccessTier != "" {
		ws.tier = &o.AccessTier
	}
	return ws, nil
}

// checkRequestOptions refuses tiers and priorities Azure does not know,
// before any request is made.
func checkRequestOptions(o RequestOptions) error {
	for _, t := range []blob.AccessTier{o.AccessTier, o.RehydrateTier} {
		if t != "" && !slices.Contains(blob.PossibleAccessTierValues(), t) {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: unknown access tier %q", t)
		}
	}
	if p := o.RehydratePriority; p != "" && !slices.Contains(blob.PossibleRehydratePriorityValues(), p) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: unknown rehydrate priority %q", p)
	}
	return nil
}

// onlyScopeAndTags refuses the fields a call cannot honour when it can
// set only an encryption scope and (with tags) index tags: what refuses
// says which call it was.
func onlyScopeAndTags(o RequestOptions, call string, tags bool) error {
	if o.AccessTier != "" || o.LegalHold || !o.RetainUntil.IsZero() || (!tags && len(o.Tags) > 0) {
		return filesystem.Errorf(filesystem.CodeUnsupported, "azure: %s cannot set an access tier, a legal hold, a retention or (unless it creates the blob) tags", call)
	}
	return nil
}

// rehydration is the tier and priority of a rehydration of key: the
// call's RequestOptions, else Config's; empty when neither sets them.
func (d *Driver) rehydration(ctx context.Context, key string) (blob.AccessTier, blob.RehydratePriority, error) {
	o := requestOptions(ctx, key)
	if err := checkRequestOptions(o); err != nil {
		return "", "", err
	}
	tier, priority := d.cfg.RehydrateTier, d.cfg.RehydratePriority
	if o.RehydrateTier != "" {
		tier = o.RehydrateTier
	}
	if o.RehydratePriority != "" {
		priority = o.RehydratePriority
	}
	return tier, priority, nil
}
