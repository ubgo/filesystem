package ftp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/url"
	"os"
	"strconv"

	"github.com/ubgo/filesystem"
)

// DSN schemes: ftp://user@host[:port]/absolute/root is plain FTP;
// ftps://… is explicit FTPS (AUTH TLS on port 21), and ftps-implicit://…
// implicit FTPS (TLS from the first byte, port 990 unless given). Both
// FTPS schemes verify the server's certificate against the system roots,
// or the ParamCAFile bundle, and keep a TLS session cache so data
// connections resume the control connection's session (servers such as
// vsftpd, whose require_ssl_reuse defaults to on, refuse data connections
// that do not).
const (
	Scheme         = "ftp"
	SchemeSecure   = "ftps"
	SchemeImplicit = "ftps-implicit"
)

// implicitPort is the port implicit FTPS listens on by convention.
const implicitPort = "990"

// tlsSessionCacheSize is the opener's TLS session cache capacity. One
// disk talks to one server, so a handful of sessions is plenty; zero would
// mean the crypto/tls default (64).
const tlsSessionCacheSize = 8

// DSN parameters understood by Opener, besides the Mux's own. Secrets
// never appear in the DSN: a password in the URL is refused.
const (
	// ParamPasswordEnv names the environment variable holding the
	// password.
	ParamPasswordEnv = "password_env"
	// ParamMaxConns is Config.MaxConns.
	ParamMaxConns = "max_conns"
	// ParamCAFile is a PEM bundle of the certificate authorities that
	// verify the server, instead of the system roots: an internal CA or a
	// self-signed server certificate. FTPS schemes only.
	ParamCAFile = "ca_file"
)

// Opener logs in to the server a DSN names and opens the root directory in
// its path (New runs within ctx). Register it under each scheme it should
// serve:
//
//	mux.Register(ftp.Scheme, ftp.Opener)
//	mux.Register(ftp.SchemeSecure, ftp.Opener)
//	mux.Register(ftp.SchemeImplicit, ftp.Opener)
func Opener(ctx context.Context, u *url.URL) (filesystem.Driver, error) {
	if err := filesystem.RejectParams(u, ParamPasswordEnv, ParamMaxConns, ParamCAFile); err != nil {
		return nil, err
	}
	q := u.Query()
	cfg := Config{Addr: u.Host, Root: u.Path}
	switch u.Scheme {
	case Scheme:
		if q.Has(ParamCAFile) {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "ftp: %s needs an FTPS scheme", ParamCAFile)
		}
	case SchemeSecure, SchemeImplicit:
		tc, err := dsnTLS(u.Hostname(), q.Get(ParamCAFile))
		if err != nil {
			return nil, err
		}
		cfg.TLS = tc
		if u.Scheme == SchemeImplicit {
			cfg.ImplicitTLS = true
			if u.Port() == "" {
				cfg.Addr = net.JoinHostPort(u.Hostname(), implicitPort)
			}
		}
	default:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "ftp: scheme %q is not %s, %s or %s", u.Scheme, Scheme, SchemeSecure, SchemeImplicit)
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "ftp DSN must not contain a password; use %s", ParamPasswordEnv)
		}
		cfg.Username = u.User.Username()
	}
	if env := q.Get(ParamPasswordEnv); env != "" {
		if cfg.Username == "" {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "ftp DSN has %s but no user", ParamPasswordEnv)
		}
		cfg.Password = os.Getenv(env)
	}
	if raw := q.Get(ParamMaxConns); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "%s=%q is not a positive integer", ParamMaxConns, raw)
		}
		cfg.MaxConns = n
	}
	return New(ctx, cfg)
}

// dsnTLS is the TLS configuration of an FTPS DSN: the server's name to
// verify, TLS 1.2 or later, a session cache shared by the control and data
// connections (see the schemes), and the roots in caFile when one is named.
func dsnTLS(host, caFile string) (*tls.Config, error) {
	tc := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, ClientSessionCache: tls.NewLRUClientSessionCache(tlsSessionCacheSize)}
	if caFile == "" {
		return tc, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "ftp: %s %q holds no PEM certificate", ParamCAFile, caFile)
	}
	tc.RootCAs = roots
	return tc, nil
}
