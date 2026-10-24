package filesystem

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strconv"
	"sync"
)

// Opener builds a driver from a parsed DSN. It receives the URL with the
// Mux's own query parameters already removed, and must reject any
// parameter it does not understand, so a typo in configuration fails at
// start-up instead of being ignored.
type Opener func(ctx context.Context, u *url.URL) (Driver, error)

// Query parameters every DSN accepts, handled by the Mux itself.
const (
	// ParamPrefix scopes the disk to a key prefix (see Disk.Scope).
	ParamPrefix = "prefix"
	// ParamPublicURL sets WithPublicURL.
	ParamPublicURL = "public_url"
	// ParamVisibility sets WithDefaultVisibility ("public" or "private").
	ParamVisibility = "visibility"
	// ParamReadOnly ("true" or "false") adds the ReadOnly interceptor.
	ParamReadOnly = "readonly"
)

// Mux opens disks from DSN strings such as
// "s3://bucket?region=auto&prefix=tenant-a&visibility=public". Each driver
// package exports an Opener; the application registers the ones it imports.
//
// Unlike a global registry filled by init functions, a Mux is a value: two
// parts of a program (or two tests) can hold different sets of drivers, and
// nothing is registered by importing a package.
type Mux struct {
	mu      sync.RWMutex
	openers map[string]Opener
}

// NewMux returns an empty Mux.
func NewMux() *Mux { return &Mux{openers: make(map[string]Opener)} }

// Register adds an opener for a URL scheme. Registering a scheme twice is
// CodeExists.
func (m *Mux) Register(scheme string, o Opener) error {
	if scheme == "" || o == nil {
		return Errorf(CodeInvalidArgument, "empty scheme or nil opener")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.openers[scheme]; dup {
		return Errorf(CodeExists, "scheme %q is already registered", scheme)
	}
	m.openers[scheme] = o
	return nil
}

// Open parses dsn, builds its driver with the registered opener and returns
// a Disk named name. opts are applied after the DSN's own settings, so code
// can add interceptors or a signer to a configured disk.
//
// Every returned error names the disk ("disk media: …") and never contains
// the DSN itself, because DSNs often carry credentials. When the driver was
// built but the Disk could not be (an invalid Mux parameter value), the
// driver is closed so no connection leaks.
func (m *Mux) Open(ctx context.Context, name, dsn string, opts ...Option) (*Disk, error) {
	d, err := m.open(ctx, name, dsn, opts)
	if err != nil {
		return nil, prefixed(err, "disk "+name)
	}
	return d, nil
}

func (m *Mux) open(ctx context.Context, name, dsn string, opts []Option) (*Disk, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, Errorf(CodeInvalidArgument, "DSN does not parse as a URL")
	}
	m.mu.RLock()
	opener, ok := m.openers[u.Scheme]
	m.mu.RUnlock()
	if !ok {
		return nil, Errorf(CodeInvalidArgument, "no opener registered for scheme %q", u.Scheme)
	}
	q := u.Query()
	diskOpts, prefix, err := muxOptions(q, opts)
	if err != nil {
		return nil, err
	}
	for _, p := range []string{ParamPrefix, ParamPublicURL, ParamVisibility, ParamReadOnly} {
		q.Del(p)
	}
	u.RawQuery = q.Encode()
	drv, err := opener(ctx, u)
	if err != nil {
		return nil, err
	}
	if prefix != "" {
		drv = ownedScopedDriver{newScopedDriver(drv, prefix)}
	}
	d, err := NewDisk(name, drv, append(diskOpts, opts...)...)
	if err != nil {
		if c, ok := drv.(io.Closer); ok {
			err = errors.Join(err, c.Close())
		}
		return nil, err
	}
	return d, nil
}

// ownedScopedDriver is the scoped driver Open builds for the prefix
// parameter. Unlike Disk.Scope, which shares its parent's driver, the Disk
// it backs is the only owner of the driver underneath, so closing that Disk
// must close it (an SFTP connection would leak otherwise).
type ownedScopedDriver struct{ *scopedDriver }

// Close closes the driver underneath when it holds resources.
func (o ownedScopedDriver) Close() error {
	if c, ok := o.inner.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// muxOptions turns the Mux's query parameters into Disk options. The
// prefix is cleaned the way the Disk will clean keys, through the key
// normalizer the code's options set (WithKeyNormalizer), so a prefix and the
// keys under it can never be two spellings of one name.
func muxOptions(q url.Values, codeOpts []Option) ([]Option, string, error) {
	var opts []Option
	probe := &Disk{}
	for _, o := range codeOpts {
		if err := o(probe); err != nil {
			return nil, "", err
		}
	}
	prefix, err := probe.cleanDir(q.Get(ParamPrefix))
	if err != nil {
		return nil, "", err
	}
	if base := q.Get(ParamPublicURL); base != "" {
		if prefix != "" {
			base = base + "/" + EscapeKey(prefix)
		}
		opts = append(opts, WithPublicURL(base))
	}
	if v := q.Get(ParamVisibility); v != "" {
		opts = append(opts, WithDefaultVisibility(Visibility(v)))
	}
	if ro := q.Get(ParamReadOnly); ro != "" {
		on, err := strconv.ParseBool(ro)
		if err != nil {
			return nil, "", Errorf(CodeInvalidArgument, "%s=%q is not a boolean", ParamReadOnly, ro)
		}
		if on {
			opts = append(opts, WithInterceptors(ReadOnly()))
		}
	}
	return opts, prefix, nil
}

// RejectParams returns CodeInvalidArgument naming the first query parameter
// of u that is not in allowed. Openers call it to refuse typos.
func RejectParams(u *url.URL, allowed ...string) error {
	ok := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		ok[a] = true
	}
	for p := range u.Query() {
		if !ok[p] {
			return Errorf(CodeInvalidArgument, "unknown DSN parameter %q for scheme %q", p, u.Scheme)
		}
	}
	return nil
}
