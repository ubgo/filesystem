package azure

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"

	"github.com/ubgo/filesystem"
)

// DelegateOptions describe the SAS DelegatePrefix issues. Exactly one of
// SharedKey and UserDelegation signs it.
type DelegateOptions struct {
	// ContainerURL is the container's URL without a query, for example
	// "https://account.blob.core.windows.net/media" (Azurite:
	// "http://127.0.0.1:10000/devstoreaccount1/media").
	ContainerURL string
	// SharedKey signs a service SAS with the account key.
	SharedKey *azblob.SharedKeyCredential
	// UserDelegation signs a user delegation SAS (from
	// service.Client.GetUserDelegationCredential), which Microsoft Entra
	// can audit and revoke. The SAS cannot outlive the key.
	UserDelegation *sas.UserDelegationCredential
	// Prefix limits the SAS to one directory: a directory SAS (sr=d),
	// which Azure honours on accounts with a hierarchical namespace (Data
	// Lake Storage Gen2) only. On a flat-namespace account Azure refuses
	// every request made with it (per Azure's SAS documentation; Azurite
	// has no hierarchical namespace, so this is not tested here): it fails
	// closed and never falls back to the whole container. It is a clean
	// key, never empty unless WholeContainer is set.
	Prefix string
	// WholeContainer issues a container SAS (sr=c) instead, covering every
	// blob of the container. It must be asked for by name, so an empty
	// Prefix can never widen a delegation by accident.
	WholeContainer bool
	// Write adds creating, overwriting and deleting. Off, the SAS reads
	// and lists only.
	Write bool
	// Expires is the SAS lifetime, in (0, filesystem.MaxPresignExpiry].
	// There is no default, so every caller decides.
	Expires time.Duration
	// Clock is the time the lifetime counts from; nil means time.Now.
	// Pass the disk's Config.Clock so a delegated SAS and the disk's own
	// signed URLs agree on the time.
	Clock func() time.Time
}

// ScopedSAS is a shared access signature limited to one directory (or,
// when asked, one container), for a client that talks to Azure directly.
// It never prints the signature: fmt (every verb), slog and String show
// the scope and expiry with the token redacted. Read Token, or call URL,
// where it is really needed.
type ScopedSAS struct {
	// Token is the SAS query string, without a leading "?".
	Token        string
	ContainerURL string
	Prefix       string
	Expires      time.Time
}

// sasMask stands in for the token wherever a ScopedSAS is printed.
const sasMask = "[REDACTED]"

// URL returns the container URL with the SAS, ready for
// container.NewClientWithNoCredential.
func (s ScopedSAS) URL() string { return s.ContainerURL + "?" + s.Token }

// String implements fmt.Stringer without the token.
func (s ScopedSAS) String() string {
	return "azure.ScopedSAS{Token: " + sasMask + ", ContainerURL: " + strconv.Quote(s.ContainerURL) + ", Prefix: " + strconv.Quote(s.Prefix) + ", Expires: " + s.Expires.Format(time.RFC3339) + "}"
}

// GoString implements fmt.GoStringer (%#v) like String.
func (s ScopedSAS) GoString() string { return s.String() }

// Format implements fmt.Formatter, so every verb prints String.
func (s ScopedSAS) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }

// LogValue implements slog.LogValuer without the token.
func (s ScopedSAS) LogValue() slog.Value {
	return slog.GroupValue(slog.String("token", sasMask), slog.String("container_url", s.ContainerURL), slog.String("prefix", s.Prefix), slog.Time("expires", s.Expires))
}

// DelegatePrefix signs a SAS limited to o.Prefix of the container (a
// directory SAS), or to the whole container when o.WholeContainer says
// so, for HTTPS only except on a plain-HTTP endpoint such as Azurite. No
// request is made: the signature is computed locally.
func DelegatePrefix(o DelegateOptions) (ScopedSAS, error) {
	if (o.SharedKey == nil) == (o.UserDelegation == nil) {
		return ScopedSAS{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: delegation is signed by exactly one of SharedKey and UserDelegation")
	}
	u, err := url.Parse(o.ContainerURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" {
		return ScopedSAS{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: container URL %q is not an absolute http(s) URL without a query", o.ContainerURL)
	}
	path := strings.TrimRight(u.Path, "/")
	container := path[strings.LastIndex(path, "/")+1:]
	if container == "" {
		return ScopedSAS{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: container URL %q names no container", o.ContainerURL)
	}
	if o.WholeContainer != (o.Prefix == "") {
		return ScopedSAS{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: delegation needs a prefix, or WholeContainer without one")
	}
	if o.Prefix != "" {
		if clean, err := filesystem.CleanKey(o.Prefix); err != nil || clean != o.Prefix {
			return ScopedSAS{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: delegation prefix %q is not a clean key", o.Prefix)
		}
	}
	if o.Expires <= 0 || o.Expires > filesystem.MaxPresignExpiry {
		return ScopedSAS{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: delegation lifetime %s is not in (0, %s]", o.Expires, filesystem.MaxPresignExpiry)
	}
	now := time.Now
	if o.Clock != nil {
		now = o.Clock
	}
	expires := now().UTC().Add(o.Expires).Truncate(time.Second)
	perms := sas.ContainerPermissions{Read: true, List: true}
	if o.Write {
		perms.Create, perms.Write, perms.Delete, perms.Add = true, true, true, true
	}
	v := sas.BlobSignatureValues{
		Protocol:      sas.ProtocolHTTPS,
		ExpiryTime:    expires,
		ContainerName: container,
		Directory:     o.Prefix,
		Permissions:   perms.String(),
	}
	if u.Scheme == "http" {
		v.Protocol = sas.ProtocolHTTPSandHTTP // Azurite and other local endpoints
	}
	var q sas.QueryParameters
	if o.SharedKey != nil {
		q, err = v.SignWithSharedKey(o.SharedKey)
	} else {
		q, err = v.SignWithUserDelegation(o.UserDelegation)
	}
	if err != nil {
		return ScopedSAS{}, mapErr(err)
	}
	return ScopedSAS{Token: q.Encode(), ContainerURL: strings.TrimRight(o.ContainerURL, "/"), Prefix: o.Prefix, Expires: expires}, nil
}
