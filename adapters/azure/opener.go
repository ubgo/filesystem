package azure

import (
	"context"
	"encoding/base64"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/ubgo/filesystem"
)

// Scheme is the DSN scheme of the Azure driver: "azblob://container".
const Scheme = "azblob"

// ParamConnectionEnv names the environment variable holding the storage
// connection string; DefaultConnectionEnv when absent. The connection
// string (which carries the account key) is never part of the DSN, so a
// DSN can be logged.
const ParamConnectionEnv = "connection_env"

// DSN parameters understood by the opener TokenOpener returns.
const (
	// ParamAccount is the storage account name; the endpoint is then
	// https://<account>.blob.core.windows.net/.
	ParamAccount = "account"
	// ParamEndpoint is the blob service endpoint, for sovereign clouds:
	// https://<account>.blob.<suffix>. A user delegation SAS names the
	// account taken from the host's first label, so any other host (a
	// custom domain, an IP, Azurite's path-style URL) is refused, unless
	// ParamAccount is given too: then the endpoint is any https URL (a
	// custom domain, Front Door, a private-endpoint name) and the account
	// is Config.AccountName, which signs URLs for it.
	ParamEndpoint = "endpoint"
)

// DSN parameters understood by both openers.
const (
	// ParamEncryptionScope sets Config.EncryptionScope.
	ParamEncryptionScope = "encryption_scope"
	// ParamAccessTier sets Config.AccessTier (Hot, Cool, Cold, Archive).
	ParamAccessTier = "access_tier"
	// ParamVersioned ("true") sets Config.Versioned.
	ParamVersioned = "versioned"
	// ParamCustomerKeyEnv names an environment variable holding the
	// base64 of a 32-byte customer-provided key (Config.CustomerKey). The
	// key itself never appears in the DSN.
	ParamCustomerKeyEnv = "customer_key_env"
	// ParamBlockSize sets Config.BlockSize in bytes.
	ParamBlockSize = "block_size"
	// ParamConcurrency sets Config.Concurrency.
	ParamConcurrency = "concurrency"
)

// objectParams reads the parameters both openers share into cfg.
func objectParams(q url.Values, cfg *Config) error {
	cfg.EncryptionScope = q.Get(ParamEncryptionScope)
	cfg.AccessTier = blob.AccessTier(q.Get(ParamAccessTier))
	if cfg.AccessTier != "" && !slices.Contains(blob.PossibleAccessTierValues(), cfg.AccessTier) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: unknown %s %q", ParamAccessTier, cfg.AccessTier)
	}
	if raw := q.Get(ParamVersioned); raw != "" {
		on, err := strconv.ParseBool(raw)
		if err != nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "%s=%q is not a boolean", ParamVersioned, raw)
		}
		cfg.Versioned = on
	}
	if raw := q.Get(ParamBlockSize); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: %s=%q is not a number of bytes", ParamBlockSize, raw)
		}
		cfg.BlockSize = n
	}
	if raw := q.Get(ParamConcurrency); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: %s=%q is not a number", ParamConcurrency, raw)
		}
		cfg.Concurrency = n
	}
	if env := q.Get(ParamCustomerKeyEnv); env != "" {
		raw, err := base64.StdEncoding.DecodeString(os.Getenv(env))
		if err != nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: %s=%s does not hold a base64 key", ParamCustomerKeyEnv, env)
		}
		if cfg.CustomerKey, err = NewCustomerKey(raw); err != nil {
			return err
		}
	}
	return nil
}

// DefaultConnectionEnv is the variable Azure tools conventionally use.
const DefaultConnectionEnv = "AZURE_STORAGE_CONNECTION_STRING"

// Connection-string fields the opener reads.
const (
	csAccountName = "AccountName"
	csAccountKey  = "AccountKey"
	csSAS         = "SharedAccessSignature"
	csEndpoint    = "BlobEndpoint"
	csProtocol    = "DefaultEndpointsProtocol"
	csSuffix      = "EndpointSuffix"
	defaultSuffix = "core.windows.net"
	defaultScheme = "https"
)

// Opener opens "azblob://container" DSNs with a connection string from the
// environment. A shared-key one (AccountName and AccountKey) also enables
// signed URLs. A SAS one (SharedAccessSignature, with BlobEndpoint or
// AccountName), the least-privilege setup where the application holds a
// token limited in permissions and lifetime rather than the account key,
// authenticates every request with that token and declares no Presign:
// the driver cannot sign URLs without a key. Such a disk stops working
// when the token expires; issue a new one and reopen it.
func Opener(_ context.Context, u *url.URL) (filesystem.Driver, error) {
	if err := filesystem.RejectParams(u, ParamConnectionEnv, ParamEncryptionScope, ParamAccessTier, ParamVersioned, ParamCustomerKeyEnv, ParamBlockSize, ParamConcurrency); err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azblob DSN has no container")
	}
	env := u.Query().Get(ParamConnectionEnv)
	if env == "" {
		env = DefaultConnectionEnv
	}
	fields := parseConnectionString(os.Getenv(env))
	name, key, token := fields[csAccountName], fields[csAccountKey], strings.TrimPrefix(fields[csSAS], "?")
	if key == "" && token != "" {
		return sasOpener(u, env, fields, token)
	}
	if name == "" || key == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: %s has no AccountName and AccountKey, nor a SharedAccessSignature", env)
	}
	cred, err := azblob.NewSharedKeyCredential(name, key)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	endpoint := blobEndpoint(fields)
	client, err := azblob.NewClientWithSharedKeyCredential(strings.TrimRight(endpoint, "/")+"/", cred, nil)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	cfg := Config{Client: client, Container: u.Host, SharedKey: cred}
	if err := objectParams(u.Query(), &cfg); err != nil {
		return nil, err
	}
	return New(cfg)
}

// sasOpener opens the DSN's container with a SAS connection string: the
// token rides on the client's URL, so every request carries it, and the
// driver gets no key to sign URLs with.
func sasOpener(u *url.URL, env string, fields map[string]string, token string) (filesystem.Driver, error) {
	if fields[csEndpoint] == "" && fields[csAccountName] == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: %s has a SharedAccessSignature but no BlobEndpoint or AccountName", env)
	}
	if _, err := url.ParseQuery(token); err != nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: %s has a malformed SharedAccessSignature", env)
	}
	client, err := azblob.NewClientWithNoCredential(strings.TrimRight(blobEndpoint(fields), "/")+"/?"+token, nil)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	cfg := Config{Client: client, Container: u.Host}
	if err := objectParams(u.Query(), &cfg); err != nil {
		return nil, err
	}
	return New(cfg)
}

// blobEndpoint is the connection string's BlobEndpoint, or the endpoint
// built from its protocol, account name and endpoint suffix.
func blobEndpoint(fields map[string]string) string {
	endpoint := fields[csEndpoint]
	if endpoint == "" {
		scheme, suffix := fields[csProtocol], fields[csSuffix]
		if scheme == "" {
			scheme = defaultScheme
		}
		if suffix == "" {
			suffix = defaultSuffix
		}
		endpoint = scheme + "://" + fields[csAccountName] + ".blob." + suffix
	}
	return endpoint
}

// parseConnectionString splits "Key=Value;Key=Value". Values may contain
// "=" (account keys are base64), so only the first one splits.
func parseConnectionString(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		if k, v, ok := strings.Cut(part, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// TokenOpener returns an Opener for "azblob://container?account=<name>"
// (or "?endpoint=<url>", or both for an endpoint behind a custom domain)
// DSNs that authenticates with cred, an Entra ID
// token credential the application builds (azidentity's
// DefaultAzureCredential, a managed identity, a workload identity). The
// credential is code, not configuration, so it is bound here rather than
// named in the DSN, and this module needs no identity library. Signed URLs
// are user delegation SAS (Config.UserDelegation), so the identity needs a
// role that may request a user delegation key. Register it under its own
// scheme, or under Scheme in place of Opener.
func TokenOpener(cred azcore.TokenCredential) filesystem.Opener {
	return func(_ context.Context, u *url.URL) (filesystem.Driver, error) {
		if err := filesystem.RejectParams(u, ParamAccount, ParamEndpoint, ParamEncryptionScope, ParamAccessTier, ParamVersioned, ParamCustomerKeyEnv, ParamBlockSize, ParamConcurrency); err != nil {
			return nil, err
		}
		if u.Host == "" {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azblob DSN has no container")
		}
		if cred == nil {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: TokenOpener needs a credential")
		}
		q := u.Query()
		account, endpoint := q.Get(ParamAccount), q.Get(ParamEndpoint)
		cfg := Config{Container: u.Host, UserDelegation: true}
		switch {
		case account == "" && endpoint == "":
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azblob DSN needs %s, %s or both", ParamAccount, ParamEndpoint)
		case endpoint == "":
			endpoint = defaultScheme + "://" + account + ".blob." + defaultSuffix
		case account == "":
			if err := checkAccountEndpoint(endpoint); err != nil {
				return nil, err
			}
		default:
			// A front end: Config.AccountName signs for the account.
			if e, err := url.Parse(endpoint); err != nil || e.Scheme != defaultScheme || e.Host == "" {
				return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "azblob %s must be an https URL", ParamEndpoint)
			}
			cfg.AccountName, cfg.Credential = account, cred
		}
		client, err := azblob.NewClient(strings.TrimRight(endpoint, "/")+"/", cred, nil)
		if err != nil {
			return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
		}
		cfg.Client = client
		if err := objectParams(q, &cfg); err != nil {
			return nil, err
		}
		return New(cfg)
	}
}

// checkAccountEndpoint refuses an endpoint whose host is not
// <account>.blob.<suffix>: the SDK takes the account name for a user
// delegation SAS from the host's first label, so on any other host every
// signed URL would name the wrong account and fail.
func checkAccountEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != defaultScheme {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "azblob %s must be an https URL", ParamEndpoint)
	}
	labels := strings.Split(u.Hostname(), ".")
	if len(labels) < 3 || labels[0] == "" || labels[1] != "blob" || u.Port() != "" || (u.Path != "" && u.Path != "/") {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "azblob %s host must be <account>.blob.<suffix>, with no port or path", ParamEndpoint)
	}
	return nil
}
