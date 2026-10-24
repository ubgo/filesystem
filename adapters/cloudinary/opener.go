package cloudinary

import (
	"context"
	"net/url"
	"os"

	"github.com/ubgo/filesystem"
)

// Scheme is the DSN scheme: cloudinary://<api_key>@<cloud_name>, the shape
// of Cloudinary's own CLOUDINARY_URL without the secret.
const Scheme = "cloudinary"

// DSN parameters understood by Opener, besides the Mux's own. The API
// secret never appears in the DSN: a password in the URL is refused, and
// ParamSecretEnv names the variable holding it.
const (
	// ParamSecretEnv names the environment variable holding the API
	// secret. Required.
	ParamSecretEnv = "api_secret_env"
	// ParamType sets Config.Type ("authenticated", "upload", "private").
	ParamType = "type"
	// ParamResourceType sets Config.ResourceType ("raw", "image",
	// "video").
	ParamResourceType = "resource_type"
	// ParamAlgorithm sets Config.Algorithm ("sha1", "sha256").
	ParamAlgorithm = "algorithm"
)

// Opener opens "cloudinary://api_key@cloud_name?api_secret_env=NAME" DSNs.
// It makes no request (New does not), so wrong credentials surface at the
// first call.
func Opener(_ context.Context, u *url.URL) (filesystem.Driver, error) {
	if err := filesystem.RejectParams(u, ParamSecretEnv, ParamType, ParamResourceType, ParamAlgorithm); err != nil {
		return nil, err
	}
	if u.User == nil || u.User.Username() == "" || u.Host == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "cloudinary DSN needs api_key@cloud_name")
	}
	if _, has := u.User.Password(); has {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "cloudinary DSN must not contain the API secret; use %s", ParamSecretEnv)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "cloudinary DSN takes no path; scope the disk instead")
	}
	q := u.Query()
	env := q.Get(ParamSecretEnv)
	if env == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "cloudinary DSN needs %s", ParamSecretEnv)
	}
	secret := os.Getenv(env)
	if secret == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "cloudinary DSN: environment variable %s is unset or empty", env)
	}
	return New(Config{
		CloudName:    u.Host,
		APIKey:       u.User.Username(),
		APISecret:    secret,
		Type:         Type(q.Get(ParamType)),
		ResourceType: ResourceType(q.Get(ParamResourceType)),
		Algorithm:    Algorithm(q.Get(ParamAlgorithm)),
	})
}
