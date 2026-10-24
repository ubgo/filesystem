package webdav

import (
	"context"
	"net/url"
	"os"
	"strconv"

	"github.com/ubgo/filesystem"
)

// DSN schemes: webdavs://user@host/path is https, webdav://user@host/path
// is plain http (a server on localhost, or a private network named with
// ParamInsecureHTTP).
const (
	Scheme       = "webdav"
	SchemeSecure = "webdavs"
)

// DSN parameters understood by Opener, besides the Mux's own.
const (
	// ParamPasswordEnv names the environment variable holding the
	// password. Secrets never appear in the DSN: a password in the URL is
	// refused.
	ParamPasswordEnv = "password_env"
	// ParamInsecureHTTP ("true") is Config.AllowInsecureHTTP, needed for a
	// webdav:// (plain http) DSN to a host other than loopback.
	ParamInsecureHTTP = "insecure_http"
)

// Opener opens the collection a webdav or webdavs DSN names (New's probe
// runs within ctx). Register it under both schemes:
//
//	mux.Register(webdav.Scheme, webdav.Opener)
//	mux.Register(webdav.SchemeSecure, webdav.Opener)
func Opener(ctx context.Context, u *url.URL) (filesystem.Driver, error) {
	if err := filesystem.RejectParams(u, ParamPasswordEnv, ParamInsecureHTTP); err != nil {
		return nil, err
	}
	scheme := "https"
	switch u.Scheme {
	case SchemeSecure:
	case Scheme:
		scheme = "http"
	default:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "webdav: scheme %q is not %s or %s", u.Scheme, Scheme, SchemeSecure)
	}
	cfg := Config{Endpoint: (&url.URL{Scheme: scheme, Host: u.Host, Path: u.Path}).String()}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "webdav DSN must not contain a password; use %s", ParamPasswordEnv)
		}
		cfg.Username = u.User.Username()
	}
	if env := u.Query().Get(ParamPasswordEnv); env != "" {
		if cfg.Username == "" {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "webdav DSN has %s but no user", ParamPasswordEnv)
		}
		cfg.Password = os.Getenv(env)
	}
	if raw := u.Query().Get(ParamInsecureHTTP); raw != "" {
		on, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "%s=%q is not a boolean", ParamInsecureHTTP, raw)
		}
		cfg.AllowInsecureHTTP = on
	}
	return New(ctx, cfg)
}
