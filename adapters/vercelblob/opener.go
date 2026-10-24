package vercelblob

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/ubgo/filesystem"
)

// Scheme is the DSN scheme: vercelblob://<access>, where the host is the
// store's access mode ("public" or "private"), which must match the store.
const Scheme = "vercelblob"

// DSN parameters understood by Opener, besides the Mux's own. The token
// never appears in the DSN: ParamTokenEnv names the variable holding it.
const (
	// ParamTokenEnv names the environment variable holding the store's
	// read-write token; empty means DefaultTokenEnv.
	ParamTokenEnv = "token_env"
	// ParamCacheMaxAge sets Config.CacheMaxAge in seconds.
	ParamCacheMaxAge = "cache_max_age"
)

// DefaultTokenEnv is the variable Vercel sets for a project's Blob store.
const DefaultTokenEnv = "BLOB_READ_WRITE_TOKEN"

// Opener opens "vercelblob://public" or "vercelblob://private" DSNs with
// the token from the environment. It makes no request (New does not), so
// a wrong token surfaces at the first call.
func Opener(_ context.Context, u *url.URL) (filesystem.Driver, error) {
	if err := filesystem.RejectParams(u, ParamTokenEnv, ParamCacheMaxAge); err != nil {
		return nil, err
	}
	if u.User != nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "vercelblob DSN must not contain credentials; use %s", ParamTokenEnv)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "vercelblob DSN takes no path; scope the disk instead")
	}
	q := u.Query()
	env := q.Get(ParamTokenEnv)
	if env == "" {
		env = DefaultTokenEnv
	}
	token := os.Getenv(env)
	if token == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "vercelblob DSN: environment variable %s is unset or empty", env)
	}
	cfg := Config{Token: token, Access: Access(u.Host)}
	if raw := q.Get(ParamCacheMaxAge); raw != "" {
		secs, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || secs <= 0 {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "%s=%q is not a positive number of seconds", ParamCacheMaxAge, raw)
		}
		cfg.CacheMaxAge = time.Duration(secs) * time.Second
	}
	return New(cfg)
}
