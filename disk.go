package filesystem

import (
	"context"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/ubgo/filesystem/sniff"
)

// Disk is the handle applications use for one storage location. It is safe
// for concurrent use. Construct it with NewDisk; the zero value is not usable.
//
// Every method cleans its key with CleanKey before the driver sees it, runs
// the driver call through the interceptor chain as one Op, and returns
// errors as *Error with the operation, disk name and key filled in.
type Disk struct {
	name              string
	driver            Driver
	caps              Capabilities
	interceptors      []Interceptor
	run               Interceptor
	publicBase        string
	signer            URLSigner
	signerMethods     []HTTPMethod // nil: the default rule in signerSigns
	detector          sniff.Detector
	defaultVisibility Visibility
	detectContentType bool
	normalizeKey      func(string) (string, error)
}

// URLSigner signs URLs for disks whose driver cannot (local disk, memory),
// so that a signed download or upload link works the same in development as
// on S3. The signedurl sub-package provides an HMAC implementation and the
// fshttp sub-package serves its URLs.
type URLSigner interface {
	SignURL(ctx context.Context, disk, key string, opts PresignOptions) (SignedRequest, error)
}

// HeaderPinner is implemented by a URLSigner whose PUT URLs pin
// PresignOptions.Headers: the URL carries them under its signature, and the
// server that accepts the upload refuses a request without them and
// stores them (signedurl.Signer with fshttp.Signed does). The Disk gives
// headers only to a signer whose PinsHeaders reports true, so a signer
// that does not know about them can never drop them silently.
type HeaderPinner interface {
	PinsHeaders() bool
}

// IPRestricter is implemented by a URLSigner whose URLs carry
// PresignOptions.AllowedIP under their signature, and whose server refuses
// a request from an address outside it (signedurl.Signer with
// fshttp.Signed does). The Disk gives AllowedIP only to a signer whose
// RestrictsIP reports true, so a signer that does not know about it can
// never issue a URL that works from anywhere.
type IPRestricter interface {
	RestrictsIP() bool
}

// Option configures a Disk in NewDisk.
type Option func(*Disk) error

// WithInterceptors appends interceptors; the first given runs outermost.
func WithInterceptors(interceptors ...Interceptor) Option {
	return func(d *Disk) error {
		for _, i := range interceptors {
			if i == nil {
				return Errorf(CodeInvalidArgument, "nil interceptor")
			}
		}
		d.interceptors = append(d.interceptors, interceptors...)
		return nil
	}
}

// WithPublicURL makes PublicURL return base + "/" + key, typically a CDN
// origin. It takes precedence over the driver's own public URL. base must be
// an absolute http or https URL; a trailing slash is ignored.
func WithPublicURL(base string) Option {
	return func(d *Disk) error {
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Errorf(CodeInvalidArgument, "public URL %q is not an absolute http(s) URL", base)
		}
		d.publicBase = strings.TrimRight(base, "/")
		return nil
	}
}

// WithURLSigner sets the signer SignedURL uses, typically a CDN's
// (contrib/cloudfront, contrib/cloudflare) or the HMAC signer of package
// signedurl.
//
// Which methods it signs: with no methods given, the signer signs every
// download (MethodGet), even on a disk whose driver presigns (S3, GCS,
// Azure), so a CDN in front of a bucket hands out CDN links rather than raw
// bucket links; uploads (MethodPut) stay with the driver's own presigning,
// which sends the bytes straight to the backend, and go to the signer only
// when the driver cannot presign. Naming methods replaces that rule: the
// signer signs exactly those, and the driver (when it presigns) the rest,
// so WithURLSigner(s, MethodGet, MethodPut) routes uploads through the
// signer too, and WithURLSigner(s, MethodPut) keeps downloads on the
// driver.
//
// Invariant: a method routed to the signer never falls back to the driver
// or to an unsigned link. A signer error, or a signer that cannot honour
// PresignOptions.Headers or AllowedIP, fails the call. An unknown method
// is CodeInvalidArgument.
func WithURLSigner(s URLSigner, methods ...HTTPMethod) Option {
	return func(d *Disk) error {
		if s == nil {
			return Errorf(CodeInvalidArgument, "nil URL signer")
		}
		for _, m := range methods {
			if !slices.Contains(HTTPMethodValues, m) {
				return Errorf(CodeInvalidArgument, "URL signer method %q is not one of %v", m, HTTPMethodValues)
			}
		}
		d.signer = s
		d.signerMethods = nil
		if len(methods) > 0 {
			d.signerMethods = slices.Clone(methods)
		}
		return nil
	}
}

// signerSigns reports whether SignedURL routes method m to the URLSigner;
// see WithURLSigner for the rule.
func (d *Disk) signerSigns(m HTTPMethod) bool {
	switch {
	case d.signer == nil:
		return false
	case d.signerMethods != nil:
		return slices.Contains(d.signerMethods, m)
	}
	return m == MethodGet || !d.caps.Presign
}

// WithContentDetector adds detect to the content-type detection of writes
// that do not declare a type, for formats the built-in tables do not know
// (DICOM, STL, camera RAW, Parquet). It is consulted only when the built-in
// answer is application/octet-stream, so it can never relabel a type
// sniff.Detect already recognises (HTML stays HTML); see sniff.DetectWith.
// It has no effect under WithoutContentTypeDetection. Uploads detect with
// upload.Policy.Detector, so set it there too.
func WithContentDetector(detect sniff.Detector) Option {
	return func(d *Disk) error {
		if detect == nil {
			return Errorf(CodeInvalidArgument, "nil content detector")
		}
		d.detector = detect
		return nil
	}
}

// WithDefaultVisibility sets the visibility of writes that do not choose
// one. The default is VisibilityPrivate.
func WithDefaultVisibility(v Visibility) Option {
	return func(d *Disk) error {
		if !v.Valid() {
			return Errorf(CodeInvalidArgument, "unknown visibility %q", v)
		}
		d.defaultVisibility = v
		return nil
	}
}

// WithoutContentTypeDetection stops the Disk from detecting the content type
// of writes that do not declare one; they are stored without a type.
func WithoutContentTypeDetection() Option {
	return func(d *Disk) error {
		d.detectContentType = false
		return nil
	}
}

// WithKeyNormalizer makes the Disk pass every key and directory through fn
// before cleaning it (CleanKey), so spellings fn treats as equal address
// one object: contrib/keynorm.NFC makes the NFC and NFD forms of "café.txt"
// one key, as macOS and Linux clients produce them. fn runs before the
// traversal check, so a normaliser that turns other characters into "."
// or "/" cannot smuggle a ".." past it. An error from fn is CodeInvalidKey.
// Every key this disk is given is normalised, so objects already stored
// under another spelling cannot be reached through it, not even to move
// them: set it on an empty disk, or first rename the old keys through a
// Disk over the same driver without the normaliser.
func WithKeyNormalizer(fn func(string) (string, error)) Option {
	return func(d *Disk) error {
		if fn == nil {
			return Errorf(CodeInvalidArgument, "nil key normalizer")
		}
		d.normalizeKey = fn
		return nil
	}
}

// CleanKey returns key as this disk addresses it: passed through the key
// normalizer (WithKeyNormalizer), if any, then cleaned and checked like the
// package-level CleanKey. Code that compares keys it built with keys the
// disk returns (List, Stat) should clean them with this method.
func (d *Disk) CleanKey(key string) (string, error) {
	n, err := d.normalized(key)
	if err != nil {
		return "", err
	}
	return CleanKey(n)
}

// cleanDir is CleanDir after the key normalizer.
func (d *Disk) cleanDir(dir string) (string, error) {
	n, err := d.normalized(dir)
	if err != nil {
		return "", err
	}
	return CleanDir(n)
}

// normalized runs the key normalizer, classifying its error as the key's.
func (d *Disk) normalized(key string) (string, error) {
	if d.normalizeKey == nil {
		return key, nil
	}
	n, err := d.normalizeKey(key)
	if err != nil {
		return "", NewError(CodeInvalidKey, err)
	}
	return n, nil
}

// diskNamePattern constrains disk names. Names appear in signed URL paths,
// log lines and configuration keys, so they are restricted to characters
// that need no escaping anywhere.
var diskNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// NewDisk builds a Disk over driver. name identifies the disk in errors,
// logs and signed URLs, and must match [A-Za-z0-9][A-Za-z0-9._-]{0,63}.
//
// It fails with CodeInvalidArgument when the driver declares a capability
// it does not implement (see Capabilities), so a misdeclared driver is
// caught at start-up rather than at the first call.
func NewDisk(name string, driver Driver, opts ...Option) (*Disk, error) {
	if !diskNamePattern.MatchString(name) {
		return nil, Errorf(CodeInvalidArgument, "invalid disk name %q", name)
	}
	if driver == nil {
		return nil, Errorf(CodeInvalidArgument, "nil driver for disk %q", name)
	}
	if err := verifyCapabilities(driver); err != nil {
		return nil, err
	}
	d := &Disk{
		name:              name,
		driver:            driver,
		caps:              driver.Capabilities(),
		defaultVisibility: VisibilityPrivate,
		detectContentType: true,
	}
	for _, o := range opts {
		if err := o(d); err != nil {
			return nil, err
		}
	}
	d.run = chain(d.interceptors)
	return d, nil
}

// derive builds a Disk that shares d's configuration over another driver.
// Scoped and mirrored disks use it.
func (d *Disk) derive(name string, driver Driver, publicBase string) (*Disk, error) {
	if !diskNamePattern.MatchString(name) {
		return nil, Errorf(CodeInvalidArgument, "invalid disk name %q", name)
	}
	// The wrapper drivers passed here report the inner driver's verified
	// capabilities minus the ones they do not forward (SweepAbandoned and
	// FindAbandoned, which are backend-wide). Verifying turns a wrapper that forgets to
	// clear one into an error here rather than a panic on first use.
	if err := verifyCapabilities(driver); err != nil {
		return nil, err
	}
	nd := *d
	nd.name = name
	nd.driver = driver
	nd.caps = driver.Capabilities()
	nd.publicBase = publicBase
	nd.interceptors = append([]Interceptor(nil), d.interceptors...)
	nd.run = chain(nd.interceptors)
	return &nd, nil
}

// Name returns the disk's name.
func (d *Disk) Name() string { return d.name }

// DefaultVisibility returns the visibility writes get when they do not
// choose one. For a driver without per-object visibility it is the
// visibility of every object on the disk.
func (d *Disk) DefaultVisibility() Visibility { return d.defaultVisibility }

// Capabilities returns what the disk's driver can do. Methods backed by a
// missing capability either fall back (Copy, Move, DeletePrefix) or return
// CodeUnsupported; this lets callers check up front.
func (d *Disk) Capabilities() Capabilities { return d.caps }

// Close releases the driver's resources when it holds any (it implements
// io.Closer); otherwise it does nothing.
func (d *Disk) Close() error {
	if c, ok := d.driver.(io.Closer); ok {
		return annotate(c.Close(), "", d.name, "")
	}
	return nil
}

// As is the typed escape hatch to a backend's native client: it returns the
// value of the first driver in the wrapper chain (following Unwrapper) that
// has a method Native() T. For example, As[*s3.Client](disk) on an S3 disk.
// It returns false when no driver offers a T.
func As[T any](d *Disk) (T, bool) {
	drv := d.driver
	for {
		if n, ok := drv.(interface{ Native() T }); ok {
			return n.Native(), true
		}
		u, ok := drv.(Unwrapper)
		if !ok {
			var zero T
			return zero, false
		}
		drv = u.Unwrap()
	}
}

// DriverKey returns the key the backend driver receives for key: the key
// cleaned as the Disk cleans it, then rewritten by every key-changing
// wrapper on the way down (KeyMapper, following Unwrapper). On a disk scoped
// to "tenant", DriverKey("a.png") is "tenant/a.png". It exists so options a
// backend binds to one exact key can name that key without guessing at
// prefixes. It does not follow wrappers that send a key to one of several
// disks (Route, Overlay), which implement neither interface; there it
// returns the key at that level.
func (d *Disk) DriverKey(key string) (string, error) {
	k, err := d.CleanKey(key)
	if err != nil {
		return "", err
	}
	drv := d.driver
	for {
		if m, ok := drv.(KeyMapper); ok {
			k = m.InnerKey(k)
		}
		u, ok := drv.(Unwrapper)
		if !ok {
			return k, nil
		}
		drv = u.Unwrap()
	}
}

// do runs one driver call through the interceptor chain. It counts attempts
// and annotates the error, so every method has one exit path for errors.
func (d *Disk) do(ctx context.Context, op *Op, call Call) error {
	op.Disk = d.name
	// A hedging interceptor may start attempts concurrently (Op.Hedgeable).
	var mu sync.Mutex
	err := d.run(ctx, op, func(ctx context.Context) error {
		mu.Lock()
		op.Attempt++
		mu.Unlock()
		return call(ctx)
	})
	return annotate(err, op.Kind, d.name, op.Key)
}

// Stat returns the entry for key, or an error with CodeNotFound.
func (d *Disk) Stat(ctx context.Context, key string) (Entry, error) {
	k, err := d.CleanKey(key)
	if err != nil {
		return Entry{}, annotate(err, OpStat, d.name, key)
	}
	var (
		e    Entry
		mu   sync.Mutex
		have bool
	)
	op := &Op{Kind: OpStat, Key: k, Replayable: true, Hedgeable: true}
	err = d.do(ctx, op, func(ctx context.Context) error {
		got, err := d.driver.Stat(ctx, k)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		if have && op.Concurrent {
			return ErrSuperseded
		}
		e, have = got, true
		return nil
	})
	if err != nil {
		return Entry{}, err
	}
	e.Key = k
	return e, nil
}

// Exists reports whether key exists. A CodeNotFound error is false, nil;
// any other error is returned.
func (d *Disk) Exists(ctx context.Context, key string) (bool, error) {
	_, err := d.Stat(ctx, key)
	switch CodeOf(err) {
	case "":
		return true, nil
	case CodeNotFound:
		return false, nil
	}
	return false, err
}

// Read opens key for reading. The caller must close the reader.
func (d *Disk) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	return d.ReadRange(ctx, key, ByteRange{})
}

// ReadRange opens part of key for reading (see ByteRange). The caller must
// close the reader. Offset and Length must not be negative.
func (d *Disk) ReadRange(ctx context.Context, key string, rng ByteRange) (io.ReadCloser, error) {
	k, err := d.CleanKey(key)
	if err != nil {
		return nil, annotate(err, OpRead, d.name, key)
	}
	if rng.Offset < 0 || rng.Length < 0 {
		return nil, annotate(Errorf(CodeInvalidArgument, "negative byte range %+v", rng), OpRead, d.name, k)
	}
	var (
		rc io.ReadCloser
		mu sync.Mutex
	)
	op := &Op{Kind: OpRead, Key: k, Replayable: true, Streaming: true, Hedgeable: true}
	err = d.do(ctx, op, func(ctx context.Context) error {
		r, err := d.driver.Read(ctx, k, rng)
		if err != nil {
			// A driver may return a reader together with an error; close
			// it so a failed (or retried) open never leaks a handle.
			if r != nil {
				_ = r.Close()
			}
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		if rc != nil {
			if op.Concurrent {
				// A concurrent (hedged) attempt opened the object
				// first, and its reader is the one returned: this one,
				// and the context it lives on, are not used.
				_ = r.Close()
				return ErrSuperseded
			}
			// An earlier attempt, run before this one, succeeded and
			// was called again: the latest result replaces it.
			_ = rc.Close()
		}
		rc = r
		return nil
	})
	if err != nil {
		// A hedged attempt may have opened the object although the
		// attempt the chain answered with failed.
		if rc != nil {
			_ = rc.Close()
		}
		op.closed()
		return nil, err
	}
	return op.wrapReadCloser(rc), nil
}

// ReadAll reads the whole of key into memory. Use Read for large objects.
func (d *Disk) ReadAll(ctx context.Context, key string) ([]byte, error) {
	rc, err := d.Read(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, annotate(err, OpRead, d.name, key)
	}
	return b, nil
}

// Delete removes key. Deleting a missing key succeeds, so deletes are
// idempotent and safe to retry.
func (d *Disk) Delete(ctx context.Context, key string) error {
	k, err := d.CleanKey(key)
	if err != nil {
		return annotate(err, OpDelete, d.name, key)
	}
	op := &Op{Kind: OpDelete, Key: k, Mutating: true, Replayable: true}
	return d.do(ctx, op, func(ctx context.Context) error {
		return d.driver.Delete(ctx, k)
	})
}

// SetVisibility changes the visibility of an existing key. It needs the
// SetVisibility capability.
func (d *Disk) SetVisibility(ctx context.Context, key string, v Visibility) error {
	k, err := d.CleanKey(key)
	if err != nil {
		return annotate(err, OpSetVisibility, d.name, key)
	}
	if !v.Valid() {
		return annotate(Errorf(CodeInvalidArgument, "unknown visibility %q", v), OpSetVisibility, d.name, k)
	}
	if !d.caps.SetVisibility {
		return d.unsupported(OpSetVisibility, k)
	}
	setter := d.driver.(VisibilitySetter)
	op := &Op{Kind: OpSetVisibility, Key: k, Mutating: true, Replayable: true}
	return d.do(ctx, op, func(ctx context.Context) error {
		return setter.SetVisibility(ctx, k, v)
	})
}

// unsupported builds the error for a call that needs a missing capability.
func (d *Disk) unsupported(op OpKind, key string) error {
	return annotate(Errorf(CodeUnsupported, "driver %T has no %s capability", d.driver, op), op, d.name, key)
}
