package azure

import (
	"net/http"
	"net/url"
	"regexp"
	"slices"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/ubgo/filesystem"
)

// accountNamePattern is Azure's rule for storage account names. It also
// guarantees the name has no '.', which matters below: the SDK takes the
// account from the host's first label.
var accountNamePattern = regexp.MustCompile(`^[a-z0-9]{3,24}$`)

// aliasDomain ends the host of the key client's URL: a reserved name that
// never resolves (see newDelegationClient).
const aliasDomain = ".invalid"

// newDelegationClient returns the client that fetches user delegation
// keys: Client's own service client, or, with Config.AccountName, a second
// one whose URL's host is "<AccountName>.invalid" so that the SDK
// (which takes the account from the host's first label, and has no public
// constructor taking it as an argument in azblob v1.8.2) binds the key to
// AccountName, while hostRewrite sends every request to Client's endpoint
// host. That fake host fails closed: .invalid is reserved (RFC 2606) and
// never resolves, so were the rewrite ever skipped the request would fail
// rather than go elsewhere.
func newDelegationClient(cfg Config) (*service.Client, error) {
	if cfg.AccountName == "" {
		if cfg.Credential != nil || cfg.DelegationClientOptions != nil {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: Credential and DelegationClientOptions are only used with AccountName")
		}
		return cfg.Client.ServiceClient(), nil
	}
	if !cfg.UserDelegation || cfg.Credential == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: AccountName needs UserDelegation and Credential")
	}
	if !accountNamePattern.MatchString(cfg.AccountName) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: AccountName %q is not 3 to 24 lower-case letters and digits", cfg.AccountName)
	}
	endpoint, err := url.Parse(cfg.Client.URL())
	if err != nil || endpoint.Host == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: Client has no endpoint host")
	}
	alias := *endpoint
	alias.Host = cfg.AccountName + aliasDomain
	var o service.ClientOptions
	if cfg.DelegationClientOptions != nil {
		o = service.ClientOptions(*cfg.DelegationClientOptions)
	}
	// Last of the per-call policies, so the caller's own see the request
	// as built; per-call policies run once, before retries and auth.
	o.PerCallPolicies = append(slices.Clone(o.PerCallPolicies), hostRewrite{host: endpoint.Host})
	c, err := service.NewClient(alias.String(), cfg.Credential, &o)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	return c, nil
}

// hostRewrite sends a request to host, whatever host its URL was built
// with.
type hostRewrite struct{ host string }

// Do implements policy.Policy.
func (h hostRewrite) Do(req *policy.Request) (*http.Response, error) {
	raw := req.Raw()
	raw.URL.Host, raw.Host = h.host, h.host
	return req.Next()
}
