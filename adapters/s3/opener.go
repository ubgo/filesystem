package s3

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
)

// Scheme is the DSN scheme of the S3 driver.
const Scheme = "s3"

// DSN parameters understood by Opener, besides the Mux's own.
const (
	// ParamRegion is the AWS region ("auto" for R2).
	ParamRegion = "region"
	// ParamEndpoint is the S3 endpoint for non-AWS providers, e.g.
	// "https://<account>.r2.cloudflarestorage.com".
	ParamEndpoint = "endpoint"
	// ParamPathStyle ("true") addresses the bucket in the path instead of
	// the host name, which most self-hosted stores need.
	ParamPathStyle = "path_style"
	// ParamACL ("true") selects VisibilityACL.
	ParamACL = "acl"
	// ParamConditionalWrite ("true") sets Config.ConditionalWrites.
	ParamConditionalWrite = "conditional_write"
	// ParamSSE sets Config.ServerSideEncryption ("AES256", "aws:kms").
	ParamSSE = "sse"
	// ParamSSEKMSKeyID sets Config.SSEKMSKeyID: the KMS key (ID, ARN or
	// alias ARN) this disk's objects are encrypted with; implies
	// sse=aws:kms when sse is absent.
	ParamSSEKMSKeyID = "sse_kms_key_id"
	// ParamStorageClass sets Config.StorageClass.
	ParamStorageClass = "storage_class"
	// ParamStorageClasses ("true") sets Config.StorageClasses, declaring
	// StorageClass.
	ParamStorageClasses = "storage_classes"
	// ParamPartSize sets Config.PartSize in bytes.
	ParamPartSize = "part_size"
	// ParamConcurrency sets Config.Concurrency, the parts of one upload
	// in flight at once.
	ParamConcurrency = "concurrency"
	// ParamExpectedBucketOwner sets Config.ExpectedBucketOwner, the
	// 12-digit account ID that must own the bucket.
	ParamExpectedBucketOwner = "expected_bucket_owner"
	// ParamRequestPayer sets Config.RequestPayer ("requester"), for
	// requester-pays buckets.
	ParamRequestPayer = "request_payer"
	// ParamPruneEmptyPrefixes ("true") sets Config.PruneEmptyPrefixes.
	ParamPruneEmptyPrefixes = "prune_prefixes"
	// ParamPresignHeaders ("false") sets Config.UnverifiedSignedHeaders,
	// for stores that do not verify SigV4 signatures. Default true.
	ParamPresignHeaders = "presign_headers"
	// ParamSweepUploads ("true") sets Config.SweepUploads, declaring
	// SweepAbandoned. Prefer a bucket lifecycle rule where the store has
	// one.
	ParamSweepUploads = "sweep_uploads"
	// ParamProfile selects a profile of the shared AWS config and
	// credentials files for this disk alone (an assume-role or SSO profile
	// works too). Exclusive with the *_env parameters.
	ParamProfile = "profile"
	// ParamAccessKeyEnv names the environment variable holding this disk's
	// access key ID. Requires ParamSecretKeyEnv.
	ParamAccessKeyEnv = "access_key_env"
	// ParamSecretKeyEnv names the environment variable holding this disk's
	// secret access key. Requires ParamAccessKeyEnv.
	ParamSecretKeyEnv = "secret_key_env"
	// ParamSessionTokenEnv names the environment variable holding a session
	// token for temporary credentials. Optional, with the two above.
	ParamSessionTokenEnv = "session_token_env"
	// ParamTagging ("true") sets Config.Tagging, declaring Tags.
	ParamTagging = "tagging"
	// ParamObjectLock ("true") sets Config.ObjectLock, declaring Hold; the
	// Opener checks the bucket has Object Lock enabled.
	ParamObjectLock = "object_lock"
	// ParamObjectLockMode sets Config.ObjectLockMode ("GOVERNANCE",
	// "COMPLIANCE"); implies object_lock.
	ParamObjectLockMode = "object_lock_mode"
	// ParamObjectLockDays sets Config.ObjectLockDays, a retention every
	// write gets; implies object_lock.
	ParamObjectLockDays = "object_lock_days"
	// ParamVersioning ("true") sets Config.Versioning, declaring Versions;
	// the Opener checks the bucket has versioning enabled.
	ParamVersioning = "versioning"
	// ParamRestore ("true") sets Config.Restore, declaring Restore.
	ParamRestore = "restore"
	// ParamRestoreTier sets Config.RestoreTier ("Standard", "Bulk",
	// "Expedited").
	ParamRestoreTier = "restore_tier"
	// ParamSSECKeyEnv names the environment variable holding this disk's
	// SSE-C key, base64-encoded (32 bytes decoded), for
	// Config.SSECustomerKey. Like the credentials, the DSN only names it.
	ParamSSECKeyEnv = "sse_c_key_env"
	// ParamMaxRetentionDays sets Config.MaxRetention in days; default
	// DefaultMaxRetention.
	ParamMaxRetentionDays = "max_retention_days"
	// ParamPresignCustomerKey ("true") sets Config.PresignCustomerKey:
	// signed URLs under sse_c_key_env, which carry the disk-wide key.
	ParamPresignCustomerKey = "presign_customer_key"
	// ParamPresignWithoutChecksum ("true") sets
	// Config.PresignWithoutChecksum: signed URLs under checksum=, whose
	// uploads carry no checksum.
	ParamPresignWithoutChecksum = "presign_without_checksum"
	// ParamChecksum sets Config.ChecksumAlgorithm ("CRC32", "CRC32C",
	// "CRC64NVME", "SHA1", "SHA256").
	ParamChecksum = "checksum"
	// ParamRoleARN makes the disk work through an IAM role in a
	// customer's account (RoleOptions.RoleARN), for a bring-your-own
	// bucket: the disk's other credential parameters (or the default
	// chain) are the application's own, used only to assume the role.
	// Requires ParamExternalID and ParamRoleSessionName.
	ParamRoleARN = "role_arn"
	// ParamExternalID is RoleOptions.ExternalID, the confused-deputy guard
	// the role's trust policy requires. AWS does not treat it as a secret,
	// so the DSN may hold it; it is still never printed in errors.
	ParamExternalID = "external_id"
	// ParamRoleSessionName is RoleOptions.SessionName.
	ParamRoleSessionName = "role_session_name"
	// ParamRoleDuration is RoleOptions.Duration, a Go duration ("1h");
	// default DefaultRoleDuration.
	ParamRoleDuration = "role_duration"
)

// Opener opens "s3://bucket?region=…&endpoint=…" DSNs. Without credential
// parameters the client uses the AWS default chain (environment, shared
// files, instance roles). With them, the disk has credentials of its own,
// so two disks in one process can reach two accounts or providers: profile
// picks a shared-config profile, and access_key_env / secret_key_env /
// session_token_env name environment variables holding static keys. With
// role_arn (plus external_id and role_session_name) those credentials only
// assume a role in a customer's account and the disk works through the
// role, renewed automatically: one DSN per tenant opens that tenant's own
// bucket (see NewRoleClient). The
// DSN only ever names where a secret lives, never holds one, so it can be
// logged; errors name the variable, never its value.
func Opener(ctx context.Context, u *url.URL) (filesystem.Driver, error) {
	if err := filesystem.RejectParams(u, ParamRegion, ParamEndpoint, ParamPathStyle, ParamACL, ParamConditionalWrite, ParamSSE, ParamSSEKMSKeyID, ParamStorageClass, ParamPartSize, ParamPruneEmptyPrefixes,
		ParamPresignHeaders, ParamSweepUploads, ParamProfile, ParamAccessKeyEnv, ParamSecretKeyEnv, ParamSessionTokenEnv,
		ParamStorageClasses, ParamTagging, ParamObjectLock, ParamObjectLockMode, ParamObjectLockDays, ParamVersioning, ParamRestore, ParamRestoreTier, ParamSSECKeyEnv, ParamChecksum, ParamMaxRetentionDays, ParamPresignCustomerKey, ParamPresignWithoutChecksum,
		ParamRoleARN, ParamExternalID, ParamRoleSessionName, ParamRoleDuration, ParamConcurrency, ParamExpectedBucketOwner, ParamRequestPayer); err != nil {
		return nil, err
	}
	if u.User != nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3 DSN must not contain credentials; use %s and %s", ParamAccessKeyEnv, ParamSecretKeyEnv)
	}
	if u.Host == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3 DSN has no bucket")
	}
	q := u.Query()
	flags := make(map[string]bool, 4)
	// presign_headers defaults to true; the others to false.
	flags[ParamPresignHeaders] = true
	for _, p := range []string{ParamPathStyle, ParamACL, ParamConditionalWrite, ParamPruneEmptyPrefixes, ParamPresignHeaders, ParamSweepUploads, ParamStorageClasses, ParamTagging, ParamObjectLock, ParamVersioning, ParamRestore, ParamPresignCustomerKey, ParamPresignWithoutChecksum} {
		if raw := q.Get(p); raw != "" {
			on, err := strconv.ParseBool(raw)
			if err != nil {
				return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "%s=%q is not a boolean", p, raw)
			}
			flags[p] = on
		}
	}
	numbers := make(map[string]int64, 2)
	for _, p := range []string{ParamPartSize, ParamObjectLockDays, ParamMaxRetentionDays, ParamConcurrency} {
		if raw := q.Get(p); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "%s=%q is not a number", p, raw)
			}
			numbers[p] = n
		}
	}
	customerKey, err := customerKeyFromEnv(q.Get(ParamSSECKeyEnv))
	if err != nil {
		return nil, err
	}
	loadOpts, static, err := credentialOptions(q)
	if err != nil {
		return nil, err
	}
	if region := q.Get(ParamRegion); region != "" {
		loadOpts = append(loadOpts, config.WithRegion(region))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	role, err := roleFromQuery(ctx, awsCfg, q)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if ep := q.Get(ParamEndpoint); ep != "" {
			o.BaseEndpoint = aws.String(ep)
		}
		o.UsePathStyle = flags[ParamPathStyle]
		if static != nil {
			// Unwrapped: config.LoadDefaultConfig puts every provider in an
			// aws.CredentialsCache, which keeps the retrieved keys in a
			// field %+v prints. Static keys need no cache.
			o.Credentials = static
		}
		if role != nil {
			// The application's credentials above only assume the role.
			o.Credentials = role
		}
	})
	cfg := Config{
		Client:                  client,
		Bucket:                  u.Host,
		ConditionalWrites:       flags[ParamConditionalWrite],
		ServerSideEncryption:    types.ServerSideEncryption(q.Get(ParamSSE)),
		SSEKMSKeyID:             q.Get(ParamSSEKMSKeyID),
		StorageClass:            types.StorageClass(q.Get(ParamStorageClass)),
		PartSize:                numbers[ParamPartSize],
		Concurrency:             int(numbers[ParamConcurrency]),
		ExpectedBucketOwner:     q.Get(ParamExpectedBucketOwner),
		RequestPayer:            types.RequestPayer(q.Get(ParamRequestPayer)),
		PruneEmptyPrefixes:      flags[ParamPruneEmptyPrefixes],
		UnverifiedSignedHeaders: !flags[ParamPresignHeaders],
		SweepUploads:            flags[ParamSweepUploads],
		StorageClasses:          flags[ParamStorageClasses],
		Tagging:                 flags[ParamTagging],
		ObjectLock:              flags[ParamObjectLock] || q.Get(ParamObjectLockMode) != "" || numbers[ParamObjectLockDays] != 0,
		ObjectLockMode:          types.ObjectLockMode(q.Get(ParamObjectLockMode)),
		ObjectLockDays:          int(numbers[ParamObjectLockDays]),
		MaxRetention:            time.Duration(numbers[ParamMaxRetentionDays]) * 24 * time.Hour,
		PresignCustomerKey:      flags[ParamPresignCustomerKey],
		PresignWithoutChecksum:  flags[ParamPresignWithoutChecksum],
		Versioning:              flags[ParamVersioning],
		Restore:                 flags[ParamRestore],
		RestoreTier:             types.Tier(q.Get(ParamRestoreTier)),
		SSECustomerKey:          customerKey,
		ChecksumAlgorithm:       types.ChecksumAlgorithm(q.Get(ParamChecksum)),
	}
	if flags[ParamACL] {
		cfg.Visibility = VisibilityACL
	}
	drv, err := New(cfg)
	if err != nil {
		return nil, err
	}
	if err := drv.CheckBucket(ctx); err != nil {
		return nil, err
	}
	return drv, nil
}

// customerKeyFromEnv reads a base64 SSE-C key from the environment
// variable env; no variable named means no key. Errors name the variable,
// never its value.
func customerKeyFromEnv(env string) (CustomerKey, error) {
	if env == "" {
		return CustomerKey{}, nil
	}
	val, ok := os.LookupEnv(env)
	if !ok || val == "" {
		return CustomerKey{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3 DSN: environment variable %s is unset or empty", env)
	}
	raw, err := base64.StdEncoding.DecodeString(val)
	if err != nil {
		return CustomerKey{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3 DSN: environment variable %s is not base64", env)
	}
	k, err := NewCustomerKey(raw)
	if err != nil {
		return CustomerKey{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3 DSN: environment variable %s does not hold a 32-byte key", env)
	}
	return k, nil
}

// roleFromQuery returns the role credentials the DSN's role_* parameters
// ask for, assumed once with awsCfg's credentials, or nil without
// role_arn. The external ID never appears in an error.
func roleFromQuery(ctx context.Context, awsCfg aws.Config, q url.Values) (aws.CredentialsProvider, error) {
	arn := q.Get(ParamRoleARN)
	if arn == "" {
		if q.Get(ParamExternalID) != "" || q.Get(ParamRoleSessionName) != "" || q.Get(ParamRoleDuration) != "" {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3 DSN: %s, %s and %s need %s", ParamExternalID, ParamRoleSessionName, ParamRoleDuration, ParamRoleARN)
		}
		return nil, nil
	}
	o := RoleOptions{RoleARN: arn, ExternalID: q.Get(ParamExternalID), SessionName: q.Get(ParamRoleSessionName)}
	if raw := q.Get(ParamRoleDuration); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "%s=%q is not a duration", ParamRoleDuration, raw)
		}
		o.Duration = d
	}
	return roleCredentialsFor(ctx, awsCfg, o)
}

// credentialOptions turns the DSN's credential parameters into config load
// options: none (the default chain), a shared-config profile, or static
// keys read from the named environment variables, which it also returns so
// the client can use them directly. Errors name parameters and variables,
// never values.
func credentialOptions(q url.Values) ([]func(*config.LoadOptions) error, aws.CredentialsProvider, error) {
	profile := q.Get(ParamProfile)
	akEnv, skEnv, tokEnv := q.Get(ParamAccessKeyEnv), q.Get(ParamSecretKeyEnv), q.Get(ParamSessionTokenEnv)
	static := akEnv != "" || skEnv != "" || tokEnv != ""
	switch {
	case profile != "" && static:
		return nil, nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3 DSN: %s and the *_env credential parameters are exclusive", ParamProfile)
	case profile != "":
		return []func(*config.LoadOptions) error{config.WithSharedConfigProfile(profile)}, nil, nil
	case !static:
		return nil, nil, nil
	case akEnv == "" || skEnv == "":
		return nil, nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3 DSN: %s and %s go together", ParamAccessKeyEnv, ParamSecretKeyEnv)
	}
	creds := aws.Credentials{Source: credentialSource}
	for _, v := range []struct {
		env string
		dst *string
	}{{akEnv, &creds.AccessKeyID}, {skEnv, &creds.SecretAccessKey}, {tokEnv, &creds.SessionToken}} {
		if v.env == "" {
			continue
		}
		val, ok := os.LookupEnv(v.env)
		if !ok || val == "" {
			return nil, nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3 DSN: environment variable %s is unset or empty", v.env)
		}
		*v.dst = val
	}
	p := staticCredentials{&creds}
	return []func(*config.LoadOptions) error{config.WithCredentialsProvider(p)}, p, nil
}

// credentialSource is aws.Credentials.Source for keys read through the
// DSN's *_env parameters.
const credentialSource = "ubgo/filesystem s3 DSN environment"

// redacted is what staticCredentials prints in place of its keys.
const redacted = "s3.staticCredentials{[redacted]}"

// staticCredentials is a fixed aws.CredentialsProvider that never formats
// its keys. Formatted directly, its methods print a placeholder; formatted
// inside something else (an *s3.Client keeps it in unexported fields, where
// fmt does not call methods) the keys sit behind a pointer, which fmt
// prints as an address below the top level. Either way a logged client or
// config does not leak them.
type staticCredentials struct{ v *aws.Credentials }

// Retrieve implements aws.CredentialsProvider.
func (s staticCredentials) Retrieve(context.Context) (aws.Credentials, error) { return *s.v, nil }

// String implements fmt.Stringer without the keys.
func (s staticCredentials) String() string { return redacted }

// GoString implements fmt.GoStringer without the keys.
func (s staticCredentials) GoString() string { return redacted }

// Format implements fmt.Formatter, so no verb or flag reaches the fields.
func (s staticCredentials) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }
