package s3

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/ubgo/filesystem"
)

// AssumeRoler is the one STS call DelegatePrefix makes. *sts.Client
// implements it; a test or another token service can stand in for it.
type AssumeRoler interface {
	AssumeRole(ctx context.Context, in *sts.AssumeRoleInput, optFns ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

// STS bounds on a role session's lifetime: AWS refuses shorter than 15
// minutes, and longer than 12 hours whatever the role allows.
const (
	MinDelegateDuration = 15 * time.Minute
	MaxDelegateDuration = 12 * time.Hour
)

// DelegateOptions describe the credentials DelegatePrefix issues.
type DelegateOptions struct {
	// Role is the ARN of the IAM role to assume. The session can never do
	// more than the role may; the session policy narrows it to Prefix.
	Role string
	// SessionName identifies the session in CloudTrail, for example the
	// user it was issued to. Required.
	SessionName string
	// Bucket and Prefix are where the credentials work: the objects under
	// Prefix + "/" in Bucket, and listing them. Prefix is a clean key as
	// stored in the bucket (a scoped disk's DriverKey of a directory),
	// never empty, so a delegation is never the whole bucket.
	Bucket string
	Prefix string
	// Write adds uploading, overwriting and deleting under the prefix
	// (and aborting multipart uploads there). Off, the credentials read
	// and list only.
	Write bool
	// Duration is how long the credentials live, in [MinDelegateDuration,
	// MaxDelegateDuration]. There is no default, so every caller decides.
	Duration time.Duration
	// Partition is the AWS partition of the bucket, which every ARN of the
	// session policy names ("arn:<partition>:s3:::bucket"). A policy that
	// names the wrong partition matches nothing, so every delegated call
	// would be denied: GovCloud is PartitionGovCloud, China PartitionChina.
	// Empty means the partition of Role when Role is an ARN (STS assumes
	// only roles of its own partition), else PartitionAWS. A Partition
	// that disagrees with Role's is refused. Any other partition (the ISO
	// regions) is accepted as a name of lowercase letters, digits and
	// hyphens.
	Partition string
	// KMSKeyARNs, when set, adds one statement allowing the KMS calls
	// SSE-KMS needs, on exactly these keys: kms:Decrypt, to read objects
	// encrypted with them, and with Write also kms:GenerateDataKey, to
	// upload. A session policy intersects with the role's, so without it
	// the credentials cannot read or write a bucket encrypted with a
	// customer-managed key even when the role may. Each entry must be a
	// key ARN (arn:<partition>:kms:<region>:<account>:key/<id>) in the
	// delegation's partition, free of IAM wildcards, so the statement
	// never reaches another key; an alias ARN is refused, because IAM
	// authorizes these calls on the key, not on its alias. Empty (the
	// default) adds no statement.
	KMSKeyARNs []string
}

// AWS partitions DelegateOptions.Partition commonly names. The set is not
// closed: any valid partition name is accepted.
const (
	PartitionAWS      = "aws"
	PartitionChina    = "aws-cn"
	PartitionGovCloud = "aws-us-gov"
)

// ScopedCredentials are temporary AWS credentials limited to one prefix
// of one bucket, for handing to a client (a mobile app, a batch job) that
// talks to S3 directly. They never print their secret or session token:
// fmt (every verb), slog and String show the key ID, scope and expiry
// with the rest redacted. Read the fields directly where they are really
// needed.
type ScopedCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expires         time.Time
	Bucket          string
	Prefix          string
}

// secretMask stands in for a secret wherever credentials are printed.
const secretMask = "[REDACTED]"

// String implements fmt.Stringer without the secret or the token.
func (c ScopedCredentials) String() string {
	return "s3.ScopedCredentials{AccessKeyID: " + strconv.Quote(c.AccessKeyID) + ", SecretAccessKey: " + secretMask + ", SessionToken: " + secretMask +
		", Bucket: " + strconv.Quote(c.Bucket) + ", Prefix: " + strconv.Quote(c.Prefix) + ", Expires: " + c.Expires.Format(time.RFC3339) + "}"
}

// GoString implements fmt.GoStringer (%#v) like String.
func (c ScopedCredentials) GoString() string { return c.String() }

// Format implements fmt.Formatter, so every verb prints String.
func (c ScopedCredentials) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// LogValue implements slog.LogValuer without the secret or the token.
func (c ScopedCredentials) LogValue() slog.Value {
	return slog.GroupValue(slog.String("access_key_id", c.AccessKeyID), slog.String("secret", secretMask), slog.String("bucket", c.Bucket), slog.String("prefix", c.Prefix), slog.Time("expires", c.Expires))
}

// Provider returns the credentials as an aws.CredentialsProvider that
// reports their expiry, for building an *s3.Client that uses them.
func (c ScopedCredentials) Provider() aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, CanExpire: true, Expires: c.Expires, Source: "ubgo/filesystem DelegatePrefix"}, nil
	})
}

// IAM actions a delegated session may be allowed.
var (
	readActions  = []string{"s3:GetObject"}
	writeActions = []string{"s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"}
	// kmsReadActions and kmsWriteActions are what S3 calls on the
	// caller's behalf for an SSE-KMS object: Decrypt to read it (and to
	// complete a multipart upload), GenerateDataKey to write it.
	kmsReadActions  = []string{"kms:Decrypt"}
	kmsWriteActions = []string{"kms:GenerateDataKey"}
)

// DelegatePrefix assumes o.Role with a session policy that allows only
// o.Prefix of o.Bucket (SessionPolicy), and returns the session's
// credentials. Use it to let a client upload to or read its own folder
// without a round trip through the application, and without credentials
// that reach anything else. The caller's own credentials must be allowed
// sts:AssumeRole on the role.
func DelegatePrefix(ctx context.Context, client AssumeRoler, o DelegateOptions) (ScopedCredentials, error) {
	if o.Role == "" || o.SessionName == "" {
		return ScopedCredentials{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: delegation needs a role and a session name")
	}
	if o.Duration < MinDelegateDuration || o.Duration > MaxDelegateDuration {
		return ScopedCredentials{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: delegation duration %s is not in [%s, %s]", o.Duration, MinDelegateDuration, MaxDelegateDuration)
	}
	policy, err := SessionPolicy(o)
	if err != nil {
		return ScopedCredentials{}, err
	}
	out, err := client.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(o.Role),
		RoleSessionName: aws.String(o.SessionName),
		Policy:          aws.String(policy),
		DurationSeconds: aws.Int32(int32(o.Duration / time.Second)),
	})
	if err != nil {
		return ScopedCredentials{}, mapErr(err)
	}
	c := out.Credentials
	if c == nil || c.AccessKeyId == nil || c.SecretAccessKey == nil || c.Expiration == nil {
		return ScopedCredentials{}, filesystem.Errorf(filesystem.CodeUnknown, "s3: AssumeRole returned no credentials")
	}
	return ScopedCredentials{
		AccessKeyID: *c.AccessKeyId, SecretAccessKey: *c.SecretAccessKey, SessionToken: aws.ToString(c.SessionToken),
		Expires: *c.Expiration, Bucket: o.Bucket, Prefix: o.Prefix,
	}, nil
}

// policyWildcards are the characters IAM expands inside a resource or a
// condition value ("*", "?", and "${…}" policy variables). A prefix
// containing one would allow more than itself, so it is refused.
const policyWildcards = "*?$"

// iamPolicyVersion is the policy language version every current IAM
// feature needs.
const iamPolicyVersion = "2012-10-17"

// PrefixPolicy returns the session policy DelegatePrefix sends for bucket
// and prefix in the aws partition, with no KMS keys: SessionPolicy of a
// DelegateOptions with only Bucket, Prefix and Write set.
func PrefixPolicy(bucket, prefix string, write bool) (string, error) {
	return SessionPolicy(DelegateOptions{Bucket: bucket, Prefix: prefix, Write: write})
}

// SessionPolicy returns the IAM session policy DelegatePrefix sends for o:
// get (and, with o.Write, put, delete and multipart cleanup) on
// arn:<partition>:s3:::bucket/prefix/*, ListBucket on the bucket only for
// listings under prefix/, and, when o.KMSKeyARNs is set, the KMS calls
// SSE-KMS needs on exactly those keys. The trailing slash keeps "tenant"
// from matching "tenant2". The prefix must be a clean, non-empty key free
// of IAM wildcards, so the policy allows exactly that folder and nothing
// wider. Role, SessionName and Duration are not checked here; only the
// policy's inputs are.
func SessionPolicy(o DelegateOptions) (string, error) {
	bucket, prefix := o.Bucket, o.Prefix
	if bucket == "" || strings.ContainsAny(bucket, policyWildcards+"/") {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: delegation bucket %q is empty or not a plain name", bucket)
	}
	if clean, err := filesystem.CleanKey(prefix); err != nil || clean != prefix || prefix == "" || strings.ContainsAny(prefix, policyWildcards) {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: delegation prefix %q is not a clean, non-empty key without IAM wildcards", prefix)
	}
	partition, err := o.partition()
	if err != nil {
		return "", err
	}
	for _, key := range o.KMSKeyARNs {
		if err := checkKMSKeyARN(key, partition); err != nil {
			return "", err
		}
	}
	actions := readActions
	if o.Write {
		actions = append(append([]string(nil), readActions...), writeActions...)
	}
	type statement struct {
		Effect    string
		Action    []string
		Resource  []string
		Condition map[string]map[string][]string `json:",omitempty"`
	}
	bucketARN := "arn:" + partition + ":s3:::" + bucket
	statements := []statement{
		{Effect: "Allow", Action: actions, Resource: []string{bucketARN + "/" + prefix + "/*"}},
		{Effect: "Allow", Action: []string{"s3:ListBucket"}, Resource: []string{bucketARN}, Condition: map[string]map[string][]string{"StringLike": {"s3:prefix": {prefix + "/*"}}}},
	}
	if len(o.KMSKeyARNs) > 0 {
		kms := kmsReadActions
		if o.Write {
			kms = append(append([]string(nil), kmsReadActions...), kmsWriteActions...)
		}
		statements = append(statements, statement{Effect: "Allow", Action: kms, Resource: append([]string(nil), o.KMSKeyARNs...)})
	}
	doc := struct {
		Version   string
		Statement []statement
	}{iamPolicyVersion, statements}
	b, err := json.Marshal(doc)
	return string(b), err
}

// partition resolves DelegateOptions.Partition (see its doc): the explicit
// value, else Role's, else PartitionAWS, refusing a malformed name and an
// explicit value that disagrees with Role's.
func (o DelegateOptions) partition() (string, error) {
	fromRole := arnPartition(o.Role)
	switch {
	case o.Partition == "" && fromRole != "":
		return fromRole, nil
	case o.Partition == "":
		return PartitionAWS, nil
	case !validPartition(o.Partition):
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: delegation partition %q is not a partition name", o.Partition)
	case fromRole != "" && fromRole != o.Partition:
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: delegation partition %q differs from the role's %q", o.Partition, fromRole)
	}
	return o.Partition, nil
}

// arnPartition returns the partition field of an ARN
// ("arn:<partition>:service:..."), or "" when s is not one.
func arnPartition(s string) string {
	fields := strings.SplitN(s, ":", 3)
	if len(fields) < 3 || fields[0] != "arn" || !validPartition(fields[1]) {
		return ""
	}
	return fields[1]
}

// validPartition reports whether p is shaped like an AWS partition name:
// lowercase letters, digits and hyphens, so it cannot smuggle a wildcard
// or a field separator into an ARN.
func validPartition(p string) bool {
	if p == "" {
		return false
	}
	for _, r := range p {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// kmsKeyResource is the resource part every key ARN starts with.
const kmsKeyResource = "key/"

// checkKMSKeyARN refuses anything but one key's ARN in partition:
// arn:<partition>:kms:<region>:<account>:key/<id>, with no IAM wildcard.
func checkKMSKeyARN(arn, partition string) error {
	rest, ok := strings.CutPrefix(arn, "arn:"+partition+":kms:")
	fields := strings.Split(rest, ":")
	if !ok || strings.ContainsAny(arn, policyWildcards) || len(fields) != 3 || fields[0] == "" || fields[1] == "" ||
		!strings.HasPrefix(fields[2], kmsKeyResource) || len(fields[2]) == len(kmsKeyResource) || strings.Contains(fields[2][len(kmsKeyResource):], "/") {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: delegation KMS key %q is not a key ARN in partition %q without IAM wildcards", arn, partition)
	}
	return nil
}
