package s3

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"

	"github.com/ubgo/filesystem"
)

// DefaultRoleDuration is RoleOptions.Duration when it is 0: one hour, the
// lifetime STS itself gives a session when none is asked for. The
// credentials are refreshed before they expire, so the duration bounds
// how long a leaked set stays usable, not how long the disk works.
const DefaultRoleDuration = time.Hour

// roleExpiryWindow is how long before expiry assumed credentials are
// renewed, so a request is never signed with credentials that expire
// while it is in flight (a large upload, a slow network).
const roleExpiryWindow = time.Minute

// RoleOptions describe the IAM role in a customer's AWS account that a
// bring-your-own-bucket disk works through: the customer creates the role,
// trusts the application's account in its trust policy on condition of
// ExternalID, and allows it on their bucket; the application assumes it
// with its own credentials and never holds the customer's keys.
type RoleOptions struct {
	// RoleARN is the role to assume, "arn:aws:iam::<customer account>:role/<name>".
	// Required.
	RoleARN string
	// ExternalID is the value the role's trust policy requires
	// (sts:ExternalId). Required, because without it the role trusts the
	// application's whole account: any other customer of the application
	// could enter this customer's role ARN as their own and have the
	// application read the bucket for them (the confused-deputy problem).
	// Generate it per tenant on the application's side and never let a
	// tenant choose it. It is not a password, but it is never printed.
	ExternalID string
	// SessionName names the role session in the customer's CloudTrail, for
	// example the tenant ID. Required.
	SessionName string
	// Duration is how long one set of credentials lives, in
	// [MinDelegateDuration, MaxDelegateDuration] and within the role's own
	// maximum; 0 means DefaultRoleDuration.
	Duration time.Duration
	// Client is the STS client the role is assumed with, holding the
	// application's own credentials. nil means sts.NewFromConfig of the
	// aws.Config the caller passes (NewRoleClient) or the DSN's
	// configuration (Opener).
	Client AssumeRoler
	// Policy is an inline session policy (IAM policy JSON). The session
	// gets only what both the role and this policy allow, so one role can
	// be narrowed per tenant, for example to arn:aws:s3:::bucket/tenant/*.
	// Empty sends none. STS refuses a malformed policy (CodePermission or
	// CodeInvalidArgument at the first Retrieve).
	Policy string
	// PolicyARNs are managed policies that narrow the session the same
	// way Policy does (at most 10).
	PolicyARNs []string
	// Tags are session tags, for attribute-based access control: the
	// role's and bucket's policies can match aws:PrincipalTag/<key>, so
	// one role serves every tenant with "tenant" set per session. The
	// role's trust policy must allow sts:TagSession. Sent sorted by key.
	Tags map[string]string
	// TransitiveTagKeys are the Tags keys that persist through a chain
	// of role assumptions.
	TransitiveTagKeys []string
	// SourceIdentity is recorded in the customer's CloudTrail for the
	// session and every role it chains to, naming the person or system
	// behind it (the role's trust policy must allow
	// sts:SetSourceIdentity). Unlike SessionName it cannot be changed
	// further down a chain.
	SourceIdentity string
}

// apply copies the optional session settings into the assume-role
// options; unset fields send nothing.
func (o RoleOptions) apply(a *stscreds.AssumeRoleOptions) {
	if o.Policy != "" {
		a.Policy = aws.String(o.Policy)
	}
	for _, arn := range o.PolicyARNs {
		a.PolicyARNs = append(a.PolicyARNs, ststypes.PolicyDescriptorType{Arn: aws.String(arn)})
	}
	for _, k := range slices.Sorted(maps.Keys(o.Tags)) {
		a.Tags = append(a.Tags, ststypes.Tag{Key: aws.String(k), Value: aws.String(o.Tags[k])})
	}
	a.TransitiveTagKeys = o.TransitiveTagKeys
	if o.SourceIdentity != "" {
		a.SourceIdentity = aws.String(o.SourceIdentity)
	}
}

// roleDuration validates o and returns its effective duration. Errors
// name the missing field, never the external ID.
func (o RoleOptions) roleDuration() (time.Duration, error) {
	switch {
	case o.RoleARN == "":
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: assuming a role needs its ARN")
	case o.ExternalID == "":
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: assuming role %s needs an external ID (the confused-deputy guard)", o.RoleARN)
	case o.SessionName == "":
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: assuming role %s needs a session name", o.RoleARN)
	case o.Duration == 0:
		return DefaultRoleDuration, nil
	case o.Duration < MinDelegateDuration || o.Duration > MaxDelegateDuration:
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: role duration %s is not in [%s, %s]", o.Duration, MinDelegateDuration, MaxDelegateDuration)
	}
	return o.Duration, nil
}

// RoleCredentials returns an aws.CredentialsProvider that assumes o's role
// through client (o.Client wins when set) and keeps the credentials,
// assuming the role again shortly before they expire, so a client built on
// it works for as long as the process runs. It makes no call until the
// first Retrieve. Neither it nor anything it holds prints the credentials
// or the external ID, under any fmt verb.
func RoleCredentials(client AssumeRoler, o RoleOptions) (aws.CredentialsProvider, error) {
	dur, err := o.roleDuration()
	if err != nil {
		return nil, err
	}
	if o.Client != nil {
		client = o.Client
	}
	if client == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: assuming role %s needs an STS client", o.RoleARN)
	}
	assume := stscreds.NewAssumeRoleProvider(client, o.RoleARN, func(a *stscreds.AssumeRoleOptions) {
		a.ExternalID = aws.String(o.ExternalID)
		a.RoleSessionName = o.SessionName
		a.Duration = dur
		o.apply(a)
	})
	cache := aws.NewCredentialsCache(assume, func(c *aws.CredentialsCacheOptions) { c.ExpiryWindow = roleExpiryWindow })
	return &roleCredentials{cache: cache, role: o.RoleARN, session: o.SessionName}, nil
}

// roleCredentials is the provider RoleCredentials returns. The cache (which
// keeps the retrieved keys in fields %+v would print) and the assume-role
// provider (which keeps the external ID) sit behind a pointer, which fmt
// prints as an address below the top level, and the provider's own methods
// print only the role and the session name.
type roleCredentials struct {
	cache         *aws.CredentialsCache
	role, session string
}

// Retrieve implements aws.CredentialsProvider: the cached credentials, or
// a fresh AssumeRole when they are missing or about to expire. An STS
// refusal (a wrong external ID, a trust policy that does not name the
// application) is CodePermission.
func (r *roleCredentials) Retrieve(ctx context.Context) (aws.Credentials, error) {
	c, err := r.cache.Retrieve(ctx)
	if err != nil {
		return aws.Credentials{}, mapErr(err)
	}
	return c, nil
}

// String implements fmt.Stringer with the role and session only.
func (r *roleCredentials) String() string {
	return "s3.RoleCredentials{RoleARN: " + strconv.Quote(r.role) + ", SessionName: " + strconv.Quote(r.session) + ", " + secretMask + "}"
}

// GoString implements fmt.GoStringer like String.
func (r *roleCredentials) GoString() string { return r.String() }

// Format implements fmt.Formatter, so every verb prints String.
func (r *roleCredentials) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, r.String()) }

// NewRoleClient returns an S3 client that works through o's role, for a
// bring-your-own-bucket disk: STS calls use base (the application's own
// configuration and credentials, unless o.Client is set), S3 calls use
// the assumed role's credentials, renewed automatically. It assumes the
// role once before returning, so a wrong role ARN, external ID or trust
// policy fails here, at opening, rather than on the first write. optFns
// apply to the S3 client as in s3.NewFromConfig (an endpoint, path-style
// addressing). Pass the client to New with the customer's bucket:
//
//	client, err := s3.NewRoleClient(ctx, appCfg, s3.RoleOptions{RoleARN: t.RoleARN, ExternalID: t.ExternalID, SessionName: t.ID})
//	drv, err := s3.New(s3.Config{Client: client, Bucket: t.Bucket})
//
// or open the same disk from a DSN with role_arn, external_id and
// role_session_name.
func NewRoleClient(ctx context.Context, base aws.Config, o RoleOptions, optFns ...func(*s3.Options)) (*s3.Client, error) {
	creds, err := roleCredentialsFor(ctx, base, o)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(base, append(optFns, func(so *s3.Options) { so.Credentials = creds })...), nil
}

// roleCredentialsFor builds o's provider with an STS client from base and
// retrieves once, so a refusal surfaces at opening.
func roleCredentialsFor(ctx context.Context, base aws.Config, o RoleOptions) (aws.CredentialsProvider, error) {
	var client AssumeRoler
	if o.Client == nil {
		client = sts.NewFromConfig(base)
	}
	creds, err := RoleCredentials(client, o)
	if err != nil {
		return nil, err
	}
	if _, err := creds.Retrieve(ctx); err != nil {
		return nil, err
	}
	return creds, nil
}
