package filesystem

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/ubgo/filesystem/sniff"
)

// PublicURL returns a URL anyone can use to read key. With WithPublicURL it
// is the configured base plus the escaped key (no backend call); otherwise
// it needs the driver's PublicURL capability. It does not check that the
// object exists or is public: a URL for a private object simply fails when
// fetched.
func (d *Disk) PublicURL(ctx context.Context, key string) (string, error) {
	k, err := d.CleanKey(key)
	if err != nil {
		return "", annotate(err, OpPublicURL, d.name, key)
	}
	if d.publicBase != "" {
		return d.publicBase + "/" + EscapeKey(k), nil
	}
	if !d.caps.PublicURL {
		return "", d.unsupported(OpPublicURL, k)
	}
	urler := d.driver.(PublicURLer)
	var u string
	op := &Op{Kind: OpPublicURL, Key: k, Replayable: true}
	err = d.do(ctx, op, func(ctx context.Context) error {
		var err error
		u, err = urler.PublicURL(ctx, k)
		return err
	})
	if err != nil {
		return "", err
	}
	return u, nil
}

// SignedURL returns a pre-authorised URL for downloading (MethodGet) or
// uploading (MethodPut) key. It uses the disk's URLSigner for the methods
// WithURLSigner routes to it (by default every download, and uploads when
// the driver cannot presign), else the driver's own signing when it has the
// Presign capability, else fails with CodeUnsupported. A method routed to
// the signer never falls back to the driver. A PUT URL counts as a
// mutation, so a read-only disk refuses to issue one.
func (d *Disk) SignedURL(ctx context.Context, key string, opts PresignOptions) (SignedRequest, error) {
	k, err := d.CleanKey(key)
	if err != nil {
		return SignedRequest{}, annotate(err, OpPresign, d.name, key)
	}
	if err := ValidatePresign(opts); err != nil {
		return SignedRequest{}, annotate(err, OpPresign, d.name, k)
	}
	// A download of active content (HTML, SVG, XML, script) is forced to
	// an attachment, so a signed link can never render user content with
	// script on the backend's origin. It needs opts.ContentType, which
	// records.Store.URL always passes.
	if opts.Method == MethodGet && opts.ContentDisposition == "" && sniff.IsActive(opts.ContentType) {
		opts.ContentDisposition = dispositionAttachment
	}
	if opts.Visibility, err = d.presignVisibility(opts.Visibility); err != nil {
		return SignedRequest{}, annotate(err, OpPresign, d.name, k)
	}
	pinHeaders := !opts.Headers.IsZero()
	restrictIP := opts.AllowedIP != ""
	pinVisibility := opts.Visibility != ""
	var sign func(ctx context.Context) (SignedRequest, error)
	switch {
	case d.signerSigns(opts.Method):
		// No URLSigner pins a visibility: the upload it signs is stored
		// by a Write at the disk's default.
		if pinVisibility {
			return SignedRequest{}, annotate(Errorf(CodeUnsupported, "disk %s cannot pin visibility in a signed PUT: its URLSigner signs uploads", d.name), OpPresign, d.name, k)
		}
		if pinHeaders && (!d.caps.Headers || !pinsHeaders(d.signer)) {
			return SignedRequest{}, annotate(Errorf(CodeUnsupported, "disk %s cannot store HTTP headers from a signed PUT: the driver must store headers and the URLSigner must pin them", d.name), OpPresign, d.name, k)
		}
		if restrictIP && !restrictsIP(d.signer) {
			return SignedRequest{}, annotate(Errorf(CodeUnsupported, "disk %s cannot restrict a signed URL to an IP range: its URLSigner does not enforce AllowedIP", d.name), OpPresign, d.name, k)
		}
		sign = func(ctx context.Context) (SignedRequest, error) { return d.signer.SignURL(ctx, d.name, k, opts) }
	case d.caps.Presign:
		if pinHeaders && !d.caps.PresignHeaders {
			return SignedRequest{}, annotate(Errorf(CodeUnsupported, "driver %T cannot pin HTTP headers in a presigned PUT", d.driver), OpPresign, d.name, k)
		}
		if restrictIP && !d.caps.PresignIP {
			return SignedRequest{}, annotate(Errorf(CodeUnsupported, "driver %T cannot restrict a presigned URL to an IP range", d.driver), OpPresign, d.name, k)
		}
		if pinVisibility && !d.caps.PresignVisibility {
			return SignedRequest{}, annotate(Errorf(CodeUnsupported, "driver %T cannot pin visibility in a presigned PUT (an ACL-mode bucket is needed)", d.driver), OpPresign, d.name, k)
		}
		p := d.driver.(Presigner)
		sign = func(ctx context.Context) (SignedRequest, error) { return p.Presign(ctx, k, opts) }
	default:
		return SignedRequest{}, d.unsupported(OpPresign, k)
	}
	var req SignedRequest
	op := &Op{Kind: OpPresign, Key: k, Mutating: opts.Method == MethodPut, Replayable: true}
	err = d.do(ctx, op, func(ctx context.Context) error {
		var err error
		req, err = sign(ctx)
		return err
	})
	if err != nil {
		return SignedRequest{}, err
	}
	return req, nil
}

// signing reports what SignedURL on d can actually issue, method by
// method, under the routing rule of WithURLSigner: each method goes to the
// URLSigner when signerSigns says so, otherwise to the driver.
//
// Why it exists: a wrapper that reports a disk's signing as its own
// capabilities (Route) must not claim more than SignedURL delivers. The
// driver's Capabilities alone no longer say it, because a signer may take
// some methods and leave others, and a signer taking a method decides
// alone whether AllowedIP and pinned Headers can be honoured for it.
//
// presign: every method (HTTPMethodValues) can be signed. ip: every
// method can be restricted to an IP range. headers: a PUT can pin stored
// headers. ip and headers imply presign.
func (d *Disk) signing() (presign, ip, headers bool) {
	presign, ip = true, true
	for _, m := range HTTPMethodValues {
		if d.signerSigns(m) {
			ip = ip && restrictsIP(d.signer)
			continue
		}
		presign = presign && driverPresigns(d.driver, m)
		ip = ip && driverPresigns(d.driver, m) && d.caps.PresignIP
	}
	if d.signerSigns(MethodPut) {
		headers = d.caps.Headers && pinsHeaders(d.signer)
	} else {
		headers = d.caps.PresignHeaders
	}
	return presign, presign && ip, presign && headers
}

// presignsVisibility reports whether SignedURL on d can pin
// PresignOptions.Visibility on a PUT: the PUT goes to the driver (a
// URLSigner never pins it) and the driver declares PresignVisibility.
// Route reports its mounts' signing with it.
func (d *Disk) presignsVisibility() bool {
	return !d.signerSigns(MethodPut) && driverPresigns(d.driver, MethodPut) && d.caps.PresignVisibility
}

// presignVisibility resolves the visibility a presigned PUT or POST form
// asks for into the one the driver must pin, "" for none, under the rules
// of PresignOptions.Visibility: empty pins nothing; an unknown value is
// CodeInvalidArgument; on a disk without SetVisibility the default needs
// no pin (the backend stores every object at it) and any other value is
// CodeUnsupported, as for a Write. The caller then refuses a non-empty
// result its signing path cannot pin.
func (d *Disk) presignVisibility(v Visibility) (Visibility, error) {
	switch {
	case v == "":
		return "", nil
	case !v.Valid():
		return "", Errorf(CodeInvalidArgument, "unknown visibility %q", v)
	case d.caps.SetVisibility:
		return v, nil
	case v == d.defaultVisibility:
		return "", nil
	default:
		return "", Errorf(CodeUnsupported, "driver %T stores no per-object visibility; disk default is %s", d.driver, d.defaultVisibility)
	}
}

// methodPresigner is implemented by wrapper drivers whose Presigner
// refuses some methods although they declare Presign: a mirrored or cached
// disk signs downloads only, since an upload straight to the backend would
// bypass the mirrors or leave the cache stale. Capabilities has one Presign
// bit for both methods, so this is how signing learns the difference.
// Wrappers that forward to another driver (scoped, failover) forward it.
type methodPresigner interface {
	presignsMethod(m HTTPMethod) bool
}

// driverPresigns reports whether d's own Presign signs method m: d
// declares Presign and, when it implements methodPresigner, takes m.
func driverPresigns(d Driver, m HTTPMethod) bool {
	if !d.Capabilities().Presign {
		return false
	}
	if p, ok := d.(methodPresigner); ok {
		return p.presignsMethod(m)
	}
	return true
}

// pinsHeaders reports whether s promises to pin PresignOptions.Headers.
func pinsHeaders(s URLSigner) bool {
	p, ok := s.(HeaderPinner)
	return ok && p.PinsHeaders()
}

// restrictsIP reports whether s promises to enforce PresignOptions.AllowedIP.
func restrictsIP(s URLSigner) bool {
	r, ok := s.(IPRestricter)
	return ok && r.RestrictsIP()
}

// dispositionAttachment makes a browser download instead of render.
const dispositionAttachment = "attachment"

// ValidatePresign checks PresignOptions: a known method, an expiry in
// (0, MaxPresignExpiry], Headers only on a PUT and free of CR and LF,
// AllowedIP empty or a canonical CIDR prefix (ParseAllowedIP), and
// Visibility empty or a known value on a PUT.
// Signers call it too, so the rule is enforced the same way on every path.
func ValidatePresign(opts PresignOptions) error {
	if opts.Method != MethodGet && opts.Method != MethodPut {
		return Errorf(CodeInvalidArgument, "presign method %q is not GET or PUT", opts.Method)
	}
	if !opts.Headers.IsZero() {
		if opts.Method != MethodPut {
			return Errorf(CodeInvalidArgument, "stored headers apply to a PUT; a GET overrides the response with ContentDisposition")
		}
		if err := opts.Headers.validate(); err != nil {
			return err
		}
	}
	if opts.AllowedIP != "" {
		if _, err := ParseAllowedIP(opts.AllowedIP); err != nil {
			return err
		}
	}
	if opts.Visibility != "" {
		if opts.Method != MethodPut {
			return Errorf(CodeInvalidArgument, "a visibility applies to a PUT; a GET reads the object as it is")
		}
		if !opts.Visibility.Valid() {
			return Errorf(CodeInvalidArgument, "unknown visibility %q", opts.Visibility)
		}
	}
	return validateExpiry(opts.Expires)
}

func validateExpiry(d time.Duration) error {
	if d <= 0 || d > MaxPresignExpiry {
		return Errorf(CodeInvalidArgument, "expiry %s is not in (0, %s]", d, MaxPresignExpiry)
	}
	return nil
}

// EscapeKey percent-escapes each segment of a clean key for use in a URL
// path, keeping the slashes between segments.
func EscapeKey(key string) string {
	parts := strings.Split(key, separator)
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, separator)
}
