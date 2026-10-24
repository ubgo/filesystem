package gcs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google/downscope"

	"github.com/ubgo/filesystem"
)

// DelegateAccess is what a downscoped token may do under its prefix. The
// zero value is DelegateRead. No level includes changing ACLs or IAM
// policies (roles/storage.objectAdmin does, which on a fine-grained bucket
// would let the holder make objects public), so none is ever granted.
type DelegateAccess int

// The access levels, each a set of predefined roles kept in the boundary.
const (
	// DelegateRead keeps roles/storage.objectViewer: read objects and
	// their metadata, list them.
	DelegateRead DelegateAccess = iota
	// DelegateCreate adds roles/storage.objectCreator: upload new objects
	// (and run multipart uploads). GCS needs delete permission to replace
	// an existing object, so this level cannot overwrite or delete.
	DelegateCreate
	// DelegateReadWrite keeps roles/storage.objectUser: read, list,
	// create, overwrite, update metadata (not ACLs) and delete objects,
	// and manage multipart uploads. objectUser carries no
	// storage.objects.setIamPolicy or getIamPolicy.
	DelegateReadWrite
)

// The predefined roles the access levels are built from.
const (
	roleObjectViewer  = "inRole:roles/storage.objectViewer"
	roleObjectCreator = "inRole:roles/storage.objectCreator"
	roleObjectUser    = "inRole:roles/storage.objectUser"
)

// roles returns the boundary's roles for a, or false for a level that
// does not exist.
func (a DelegateAccess) roles() ([]string, bool) {
	switch a {
	case DelegateRead:
		return []string{roleObjectViewer}, true
	case DelegateCreate:
		return []string{roleObjectViewer, roleObjectCreator}, true
	case DelegateReadWrite:
		return []string{roleObjectUser}, true
	}
	return nil, false
}

// DelegateOptions describe the token DelegatePrefix issues.
type DelegateOptions struct {
	// Bucket and Prefix are where the token works: the objects under
	// Prefix + "/" in Bucket, and listing them. Prefix is a clean key as
	// stored in the bucket (a scoped disk's DriverKey of a directory),
	// never empty, so a delegation is never the whole bucket.
	Bucket string
	Prefix string
	// Access is what the token may do there; the zero value reads and
	// lists only.
	Access DelegateAccess
}

// ScopedToken is a short-lived OAuth access token limited to one prefix
// of one bucket, for a client (a mobile app, a batch job) that talks to
// GCS directly. It expires with the root credential's token (an hour for
// most). It never prints the token: fmt (every verb), slog and String
// show the scope and expiry with the token redacted.
type ScopedToken struct {
	AccessToken string
	Expires     time.Time
	Bucket      string
	Prefix      string
}

// tokenMask stands in for the token wherever a ScopedToken is printed.
const tokenMask = "[REDACTED]"

// String implements fmt.Stringer without the token.
func (t ScopedToken) String() string {
	return "gcs.ScopedToken{AccessToken: " + tokenMask + ", Bucket: " + strconv.Quote(t.Bucket) + ", Prefix: " + strconv.Quote(t.Prefix) + ", Expires: " + t.Expires.Format(time.RFC3339) + "}"
}

// GoString implements fmt.GoStringer (%#v) like String.
func (t ScopedToken) GoString() string { return t.String() }

// Format implements fmt.Formatter, so every verb prints String.
func (t ScopedToken) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, t.String()) }

// LogValue implements slog.LogValuer without the token.
func (t ScopedToken) LogValue() slog.Value {
	return slog.GroupValue(slog.String("token", tokenMask), slog.String("bucket", t.Bucket), slog.String("prefix", t.Prefix), slog.Time("expires", t.Expires))
}

// TokenSource returns the token as an oauth2.TokenSource, for building a
// *storage.Client that uses it (option.WithTokenSource).
func (t ScopedToken) TokenSource() oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: t.AccessToken, TokenType: "Bearer", Expiry: t.Expires})
}

// DelegatePrefix exchanges a token of root (the application's own
// credentials, for example from google.DefaultTokenSource) for a
// downscoped one whose credential access boundary allows only o.Prefix of
// o.Bucket (PrefixBoundary). The exchange goes to Google's STS endpoint
// with the HTTP client oauth2 finds in ctx (oauth2.HTTPClient), the
// default otherwise. The token can never do more than root may.
func DelegatePrefix(ctx context.Context, root oauth2.TokenSource, o DelegateOptions) (ScopedToken, error) {
	if root == nil {
		return ScopedToken{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: delegation needs a root token source")
	}
	rule, err := PrefixBoundary(o.Bucket, o.Prefix, o.Access)
	if err != nil {
		return ScopedToken{}, err
	}
	ts, err := downscope.NewTokenSource(ctx, downscope.DownscopingConfig{RootSource: root, Rules: []downscope.AccessBoundaryRule{rule}})
	if err != nil {
		return ScopedToken{}, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	tok, err := ts.Token()
	if err != nil {
		return ScopedToken{}, filesystem.NewError(filesystem.CodePermission, err)
	}
	return ScopedToken{AccessToken: tok.AccessToken, Expires: tok.Expiry, Bucket: o.Bucket, Prefix: o.Prefix}, nil
}

// celUnsafe are the characters that could end or escape the quoted
// prefix inside the CEL condition, so a prefix holding one is refused
// rather than escaped.
const celUnsafe = `'"\`

// PrefixBoundary returns the credential access boundary rule
// DelegatePrefix sends: the roles of access (see DelegateAccess) on the
// bucket, available only for objects named prefix/… and
// for listings under prefix/. The trailing slash keeps "tenant" from
// matching "tenant2". prefix must be a clean, non-empty key free of
// quotes and backslashes, so the condition allows exactly that folder.
func PrefixBoundary(bucket, prefix string, access DelegateAccess) (downscope.AccessBoundaryRule, error) {
	if bucket == "" || strings.ContainsAny(bucket, celUnsafe+"/") {
		return downscope.AccessBoundaryRule{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: delegation bucket %q is empty or not a plain name", bucket)
	}
	if clean, err := filesystem.CleanKey(prefix); err != nil || clean != prefix || prefix == "" || strings.ContainsAny(prefix, celUnsafe) {
		return downscope.AccessBoundaryRule{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: delegation prefix %q is not a clean, non-empty key without quotes", prefix)
	}
	roles, ok := access.roles()
	if !ok {
		return downscope.AccessBoundaryRule{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: delegation access %d is not DelegateRead, DelegateCreate or DelegateReadWrite", access)
	}
	folder := prefix + "/"
	return downscope.AccessBoundaryRule{
		AvailableResource:    "//storage.googleapis.com/projects/_/buckets/" + bucket,
		AvailablePermissions: roles,
		Condition: &downscope.AvailabilityCondition{
			Title: "prefix " + folder,
			Expression: "resource.name.startsWith('projects/_/buckets/" + bucket + "/objects/" + folder + "') || " +
				"api.getAttribute('storage.googleapis.com/objectListPrefix', '').startsWith('" + folder + "')",
		},
	}, nil
}
