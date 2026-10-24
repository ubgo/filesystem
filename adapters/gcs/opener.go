package gcs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"strconv"

	"cloud.google.com/go/compute/metadata"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"

	"github.com/ubgo/filesystem"
)

// Scheme is the DSN scheme of the GCS driver.
const Scheme = "gs"

// DSN parameters understood by Opener, besides the Mux's own.
const (
	// ParamEndpoint overrides the API endpoint (an emulator such as
	// fake-gcs-server); the SDK also honours STORAGE_EMULATOR_HOST.
	ParamEndpoint = "endpoint"
	// ParamACL ("true") selects VisibilityACL.
	ParamACL = "acl"
	// ParamSigningKey is the path of a service account JSON key whose
	// client_email and private_key sign URLs; without it Presign is off.
	ParamSigningKey = "signing_key"
	// ParamNoAuth ("true") sends no credentials, for emulators.
	ParamNoAuth = "no_auth"
	// ParamKMSKey sets Config.KMSKeyName: the Cloud KMS key (CMEK) this
	// disk's objects are encrypted with.
	ParamKMSKey = "kms_key"
	// ParamSigningAccount signs URLs without a key file: the service
	// account email whose key Google holds, or SigningAccountDefault for
	// the runtime's own account (Cloud Run, GKE, GCE). Each URL is signed
	// by the IAM Credentials signBlob API, called with the client's
	// credentials, which need roles/iam.serviceAccountTokenCreator on that
	// account. Exclusive with ParamSigningKey.
	ParamSigningAccount = "signing_account"
	// ParamStorageClass sets Config.StorageClass.
	ParamStorageClass = "storage_class"
	// ParamVersioned ("true") sets Config.Versioned.
	ParamVersioned = "versioned"
	// ParamCustomerKeyEnv names an environment variable holding the
	// base64 of a 32-byte customer-supplied key (Config.CustomerKey). The
	// key itself never appears in the DSN.
	ParamCustomerKeyEnv = "customer_key_env"
	// ParamCredentialsFile is the path of a service account JSON key the
	// disk's client (and its signBlob calls under ParamSigningAccount)
	// authenticates with, instead of Application Default Credentials, so
	// two gs:// disks in one process can act as two service accounts.
	// Only service account keys are accepted. Exclusive with
	// ParamCredentialsEnv and ParamNoAuth.
	ParamCredentialsFile = "credentials_file"
	// ParamCredentialsEnv names an environment variable holding a service
	// account JSON key, used like ParamCredentialsFile; the key itself
	// never appears in the DSN.
	ParamCredentialsEnv = "credentials_env"
)

// SigningAccountDefault, as ParamSigningAccount, reads the runtime's service
// account from the metadata server once, when the disk opens.
const SigningAccountDefault = "default"

// serviceAccountKey is the part of a service account JSON key file signing
// needs.
type serviceAccountKey struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
}

// Opener opens "gs://bucket" DSNs. Credentials come from Application
// Default Credentials unless credentials_file or credentials_env names a
// service account key; the DSN never holds a secret. URLs are signed with a key file
// (signing_key) or keylessly through IAM (signing_account); with neither,
// Presign is off. signing_account=default asks the metadata server for the
// runtime's account, so it fails at open, not at the first signed URL,
// when there is none.
func Opener(ctx context.Context, u *url.URL) (filesystem.Driver, error) {
	if err := filesystem.RejectParams(u, ParamEndpoint, ParamACL, ParamSigningKey, ParamNoAuth, ParamSigningAccount, ParamKMSKey, ParamStorageClass, ParamVersioned, ParamCustomerKeyEnv, ParamCredentialsFile, ParamCredentialsEnv); err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gs DSN has no bucket")
	}
	q := u.Query()
	var copts []option.ClientOption
	if ep := q.Get(ParamEndpoint); ep != "" {
		copts = append(copts, option.WithEndpoint(ep))
	}
	flags := map[string]bool{}
	for _, p := range []string{ParamACL, ParamNoAuth, ParamVersioned} {
		if raw := q.Get(p); raw != "" {
			on, err := strconv.ParseBool(raw)
			if err != nil {
				return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "%s=%q is not a boolean", p, raw)
			}
			flags[p] = on
		}
	}
	auth, err := authOption(q, flags[ParamNoAuth])
	if err != nil {
		return nil, err
	}
	if auth != nil {
		copts = append(copts, auth)
	}
	cfg := Config{Bucket: u.Host, KMSKeyName: q.Get(ParamKMSKey), StorageClass: StorageClass(q.Get(ParamStorageClass)), Versioned: flags[ParamVersioned]}
	if flags[ParamACL] {
		cfg.Visibility = VisibilityACL
	}
	if env := q.Get(ParamCustomerKeyEnv); env != "" {
		raw, err := base64.StdEncoding.DecodeString(os.Getenv(env))
		if err != nil {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gs: %s=%s does not hold a base64 key", ParamCustomerKeyEnv, env)
		}
		if cfg.CustomerKey, err = NewCustomerKey(raw); err != nil {
			return nil, err
		}
	}
	if path := q.Get(ParamSigningKey); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
		}
		var key serviceAccountKey
		if err := json.Unmarshal(raw, &key); err != nil || key.ClientEmail == "" || key.PrivateKey == "" {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gs: %s is not a service account key", ParamSigningKey)
		}
		cfg.Signing = &storage.SignedURLOptions{GoogleAccessID: key.ClientEmail, PrivateKey: []byte(key.PrivateKey)}
	}
	if account := q.Get(ParamSigningAccount); account != "" {
		if cfg.Signing != nil {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gs: %s and %s are exclusive", ParamSigningKey, ParamSigningAccount)
		}
		if account == SigningAccountDefault {
			var err error
			if account, err = runtimeAccount(ctx); err != nil {
				return nil, err
			}
		}
		// No key: Google signs through IAM signBlob as this account, with
		// the same credentials as the storage client (not its endpoint).
		cfg.Signing = &storage.SignedURLOptions{GoogleAccessID: account}
		var iamOpts []option.ClientOption
		if auth != nil {
			iamOpts = append(iamOpts, auth)
		}
		iam, err := iamcredentials.NewService(ctx, iamOpts...)
		if err != nil {
			return nil, filesystem.NewError(filesystem.CodePermission, err)
		}
		cfg.IAM = iam
	}
	client, err := storage.NewClient(ctx, copts...)
	if err != nil {
		// The DSN parsed; what failed is finding or reading credentials
		// (no Application Default Credentials, an unusable key file), so
		// the caller is not authorised rather than wrong.
		return nil, filesystem.NewError(filesystem.CodePermission, err)
	}
	cfg.Client = client
	return New(cfg)
}

// authOption is the client option the DSN's credentials select: none
// (Application Default Credentials), no authentication (ParamNoAuth), or
// the service account key of ParamCredentialsFile or ParamCredentialsEnv.
// Errors name the parameter or the variable, never the key.
func authOption(q url.Values, noAuth bool) (option.ClientOption, error) {
	file, env := q.Get(ParamCredentialsFile), q.Get(ParamCredentialsEnv)
	set := 0
	for _, on := range []bool{file != "", env != "", noAuth} {
		if on {
			set++
		}
	}
	switch {
	case set > 1:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gs: %s, %s and %s are exclusive", ParamCredentialsFile, ParamCredentialsEnv, ParamNoAuth)
	case noAuth:
		return option.WithoutAuthentication(), nil
	case file != "":
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
		}
		return serviceAccount(raw, ParamCredentialsFile)
	case env != "":
		raw := os.Getenv(env)
		if raw == "" {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gs: %s=%s names an unset or empty variable", ParamCredentialsEnv, env)
		}
		return serviceAccount([]byte(raw), ParamCredentialsEnv+"="+env)
	}
	return nil, nil
}

// serviceAccount checks raw is a service account key and returns the
// option authenticating with it; what names the source in errors.
func serviceAccount(raw []byte, what string) (option.ClientOption, error) {
	var key struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &key); err != nil || key.Type != serviceAccountType {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gs: %s is not a service account key", what)
	}
	return option.WithAuthCredentialsJSON(option.ServiceAccount, raw), nil
}

// serviceAccountType is the "type" of a service account JSON key.
const serviceAccountType = "service_account"

// runtimeAccount asks the metadata server for the email of the service
// account the process runs as. Off Google Cloud there is no metadata server
// (GCE_METADATA_HOST points the client at another one), which is a
// configuration error: name the account instead.
func runtimeAccount(ctx context.Context) (string, error) {
	// A client of its own: the package-level OnGCE memoizes its first
	// answer for the life of the process.
	md := metadata.NewClient(nil)
	if !md.OnGCEWithContext(ctx) {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "gs: %s=%s needs the metadata server (Cloud Run, GKE, GCE); name the service account instead", ParamSigningAccount, SigningAccountDefault)
	}
	email, err := md.EmailWithContext(ctx, "default")
	if err != nil {
		return "", filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	if email == "" {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "gs: the metadata server reports no service account")
	}
	return email, nil
}
