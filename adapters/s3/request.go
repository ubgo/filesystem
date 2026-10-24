package s3

import (
	"context"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
)

// RequestOptions are S3 settings for the operations on one key, for what
// filesystem.WriteOptions has no portable field for. Attach them with
// WithRequestOptions and call the Disk as usual: the options travel
// through the Disk (key cleaning, a scoped disk's prefix, interceptors) to
// the driver, which is why they ride on the context rather than on a
// driver method. They apply only to that exact key, so a context reused
// for another write (a staging file, a thumbnail, the same name in another
// directory) never inherits a legal hold or a retention. Every field is optional and overrides the disk's Config
// for those calls only; a field whose capability the disk lacks is refused
// with CodeUnsupported rather than ignored.
//
// It is this package's one escape hatch for per-call S3 features; the
// native client (filesystem.As[*s3.Client]) is the one for everything else.
type RequestOptions struct {
	// Tags are stored with a written or copied object, replacing a copy's
	// source tags. Needs Config.Tagging.
	Tags map[string]string
	// StorageClass of a written or copied object, overriding
	// Config.StorageClass ("STANDARD_IA", "GLACIER_IR", "DEEP_ARCHIVE"…).
	StorageClass types.StorageClass
	// LegalHold places a legal hold on a written or copied object. Needs
	// Config.ObjectLock.
	LegalHold bool
	// RetainUntil protects a written or copied object until this time in
	// Config.ObjectLockMode, overriding Config.ObjectLockDays. Needs
	// Config.ObjectLock; a date beyond Config.MaxRetention is refused.
	RetainUntil time.Time
	// ChecksumAlgorithm for a written or copied object, overriding
	// Config.ChecksumAlgorithm. On Stat it asks S3 for the stored
	// checksum, reported under MetaChecksumPrefix.
	ChecksumAlgorithm types.ChecksumAlgorithm
	// RestoreTier for Disk.Restore, overriding Config.RestoreTier.
	RestoreTier types.Tier
	// SSEKMSKeyID is the KMS key a written or copied object (or a
	// presigned PUT or POST for it) is encrypted with, overriding
	// Config.SSEKMSKeyFor and Config.SSEKMSKeyID. Same rules as those: a
	// KMS or empty Config.ServerSideEncryption, never SSE-C
	// (CodeInvalidArgument otherwise).
	SSEKMSKeyID string
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
// not merged. They are not consumed, so a retried call (layer.Retry) gets
// them again.
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

// checksumAlgorithms are the algorithms this driver lets a write choose:
// the ones every SDK path (single PUT, transfer manager, multipart, copy)
// computes and S3 verifies.
var checksumAlgorithms = []types.ChecksumAlgorithm{
	types.ChecksumAlgorithmCrc32,
	types.ChecksumAlgorithmCrc32c,
	types.ChecksumAlgorithmCrc64nvme,
	types.ChecksumAlgorithmSha1,
	types.ChecksumAlgorithmSha256,
}

// validChecksum reports whether a is empty or one of checksumAlgorithms.
func validChecksum(a types.ChecksumAlgorithm) bool {
	return a == "" || slices.Contains(checksumAlgorithms, a)
}

// writeSettings are the S3-only fields of one write, copy or multipart
// upload: the disk's Config overridden by the context's RequestOptions.
// Pointer and enum fields are nil or empty when unset, so S3 applies its
// own defaults (the bucket's default retention, for instance).
type writeSettings struct {
	tagging     *string
	class       types.StorageClass
	lockMode    types.ObjectLockMode
	retainUntil *time.Time
	legalHold   types.ObjectLockLegalHoldStatus
	checksum    types.ChecksumAlgorithm
	ssec        customerHeaders
	// sse and kmsKey are the object's encryption: Config's, or "aws:kms"
	// (or the disk's KMS mode) with the key RequestOptions.SSEKMSKeyID,
	// Config.SSEKMSKeyFor or Config.SSEKMSKeyID chose, in that order.
	sse    types.ServerSideEncryption
	kmsKey *string
}

// writeSettings resolves the settings of a write of key made with ctx,
// refusing options the disk's capabilities do not cover and retention
// beyond Config.MaxRetention.
func (d *Driver) writeSettings(ctx context.Context, key string) (writeSettings, error) {
	o := requestOptions(ctx, key)
	ws := writeSettings{class: d.cfg.StorageClass, checksum: d.cfg.ChecksumAlgorithm, ssec: d.cfg.SSECustomerKey.headers(), sse: d.cfg.ServerSideEncryption}
	if err := d.encryption(&ws, key, o.SSEKMSKeyID); err != nil {
		return writeSettings{}, err
	}
	if o.StorageClass != "" {
		ws.class = o.StorageClass
	}
	if o.ChecksumAlgorithm != "" {
		if !validChecksum(o.ChecksumAlgorithm) {
			return writeSettings{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: unsupported checksum algorithm %q", o.ChecksumAlgorithm)
		}
		ws.checksum = o.ChecksumAlgorithm
	}
	if len(o.Tags) > 0 {
		if !d.cfg.Tagging {
			return writeSettings{}, filesystem.Errorf(filesystem.CodeUnsupported, "s3: tags need Config.Tagging")
		}
		tagging, err := encodeTags(o.Tags)
		if err != nil {
			return writeSettings{}, err
		}
		ws.tagging = aws.String(tagging)
	}
	if (o.LegalHold || !o.RetainUntil.IsZero()) && !d.cfg.ObjectLock {
		return writeSettings{}, filesystem.Errorf(filesystem.CodeUnsupported, "s3: legal holds and retention need Config.ObjectLock")
	}
	until := o.RetainUntil
	if until.IsZero() && d.cfg.ObjectLockDays > 0 {
		until = d.now().AddDate(0, 0, d.cfg.ObjectLockDays)
	}
	if !until.IsZero() {
		if err := d.checkRetention(until); err != nil {
			return writeSettings{}, err
		}
		ws.lockMode, ws.retainUntil = d.cfg.ObjectLockMode, aws.Time(until.UTC())
	}
	if o.LegalHold {
		ws.legalHold = types.ObjectLockLegalHoldStatusOn
	}
	return ws, nil
}

// encryption sets ws.sse and ws.kmsKey for key: the call's KMS key, else
// Config.SSEKMSKeyFor's, else Config.SSEKMSKeyID, refusing a key on an
// SSE-C or SSE-S3 disk. No key leaves Config's ServerSideEncryption.
func (d *Driver) encryption(ws *writeSettings, key, callKey string) error {
	kms := callKey
	if kms == "" && d.cfg.SSEKMSKeyFor != nil {
		kms = d.cfg.SSEKMSKeyFor(key)
	}
	if kms == "" {
		kms = d.cfg.SSEKMSKeyID
	}
	if kms == "" {
		return nil
	}
	if !d.cfg.SSECustomerKey.IsZero() {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: a KMS key is exclusive with Config.SSECustomerKey")
	}
	sse, err := kmsEncryption(d.cfg.ServerSideEncryption)
	if err != nil {
		return err
	}
	ws.sse, ws.kmsKey = sse, aws.String(kms)
	return nil
}

// encodeTags renders tags as the URL query S3 takes in x-amz-tagging,
// refusing an empty key the way Disk.SetTags does.
func encodeTags(tags map[string]string) (string, error) {
	q := make(url.Values, len(tags))
	for k, v := range tags {
		if k == "" {
			return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: tag keys must be non-empty")
		}
		q.Set(k, v)
	}
	return q.Encode(), nil
}

// MetaPrefix starts every key the driver adds to Entry.Metadata from Stat
// to report S3 state that Entry has no field for. ':' cannot appear in an
// HTTP header name, so no user metadata key (x-amz-meta-*) can collide with
// one, and Write drops such keys from WriteOptions.Metadata, so an Entry
// copied into a write (a mirror, a cross-disk copy) does not fail on them.
const MetaPrefix = "s3:"

// Entry.Metadata keys the driver reports from Stat, each present only when
// S3 reported a value.
const (
	// MetaVersionID is the ID of the current version, on a versioned
	// bucket.
	MetaVersionID = MetaPrefix + "version-id"
	// MetaRestore is the restore state of an archived object:
	// RestoreOngoing or RestoreDone.
	MetaRestore = MetaPrefix + "restore"
	// MetaRestoreExpiry is when a restored copy returns to the archive,
	// RFC 3339.
	MetaRestoreExpiry = MetaPrefix + "restore-expiry"
	// MetaChecksumPrefix starts the key of a stored checksum, followed by
	// the lower-case algorithm ("s3:checksum-crc32c"); the value is
	// base64, as S3 returns it. Reported when the disk or the call's
	// RequestOptions name a ChecksumAlgorithm.
	MetaChecksumPrefix = MetaPrefix + "checksum-"
)

// The values of MetaRestore.
const (
	// RestoreOngoing: Restore was called and the copy is not readable yet.
	RestoreOngoing = "ongoing"
	// RestoreDone: a restored copy is readable until MetaRestoreExpiry.
	RestoreDone = "restored"
)

// userMetadata returns m without the driver's own MetaPrefix keys, nil
// when nothing is left.
func userMetadata(m map[string]string) map[string]string {
	var out map[string]string
	for k, v := range m {
		if strings.HasPrefix(k, MetaPrefix) {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(m))
		}
		out[k] = v
	}
	return out
}

// stateMetadata is out's user metadata plus the S3 state Stat reports
// under MetaPrefix keys, each only when S3 reported a value.
func (d *Driver) stateMetadata(ctx context.Context, key string, out *s3.HeadObjectOutput) map[string]string {
	m := maps.Clone(out.Metadata)
	set := func(k, v string) {
		if v == "" {
			return
		}
		if m == nil {
			m = make(map[string]string, 1)
		}
		m[k] = v
	}
	if d.cfg.Versioning {
		set(MetaVersionID, aws.ToString(out.VersionId))
	}
	state, expiry := restoreState(aws.ToString(out.Restore))
	set(MetaRestore, state)
	set(MetaRestoreExpiry, expiry)
	if alg := d.checksumAlgorithm(ctx, key); alg != "" {
		set(MetaChecksumPrefix+strings.ToLower(string(alg)), storedChecksum(out, alg))
	}
	return m
}
