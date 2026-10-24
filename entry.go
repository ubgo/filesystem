package filesystem

import (
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// Visibility is the portable access level of a stored object. Each driver
// maps it to its own mechanism: file modes on local disk, ACLs or separate
// buckets on object stores.
type Visibility string

// The closed set of visibilities.
const (
	// VisibilityPublic: anyone with the object's public URL may read it.
	VisibilityPublic Visibility = "public"
	// VisibilityPrivate: readable only through the backend's credentials or
	// a signed URL. The default for a Disk unless configured otherwise,
	// because exposing a file by accident is worse than having to opt in.
	VisibilityPrivate Visibility = "private"
)

// VisibilityValues lists every Visibility.
var VisibilityValues = []Visibility{VisibilityPublic, VisibilityPrivate}

// Valid reports whether v is one of VisibilityValues.
func (v Visibility) Valid() bool {
	return v == VisibilityPublic || v == VisibilityPrivate
}

// Entry describes one stored object, or one directory in a non-recursive
// listing.
//
// Field presence depends on the source:
//   - From Stat and Write: every field the backend can report. ContentType,
//     ETag, Visibility and Metadata are empty when the driver does not store
//     them (see Capabilities).
//   - From a non-recursive List: a directory appears once with IsDir set,
//     Key set to the directory's key, and every other field zero.
//   - SHA256 is set by the Disk on a Write it streamed, and by drivers that
//     store a SHA-256 (none of the built-in ones do on Stat).
//   - Checksums is set only by the Disk on a Write that asked for more
//     digests (WriteOptions.Checksums), never by Stat or List.
type Entry struct {
	Key         string
	IsDir       bool
	Size        int64
	ModTime     time.Time
	ContentType string
	ETag        string
	SHA256      string
	// Checksums holds the extra digests WriteOptions.Checksums asked for,
	// by Checksum.Name, in lowercase hex; nil when none were asked for.
	Checksums  map[string]string
	Visibility Visibility
	Metadata   map[string]string
	// Headers are empty when the driver does not store them.
	Headers HTTPHeaders
	// StorageClass is the backend's name for the object's storage class
	// (S3 "STANDARD_IA", GCS "NEARLINE", Azure "Cool"), empty when the
	// driver does not report one or the backend leaves its default unnamed.
	// SetStorageClass changes it.
	StorageClass string
}

// HTTPHeaders are stored with an object and sent with it when it is
// served (by the backend through its own URLs, or by fshttp), so a CDN
// caches it and a browser handles it as intended. Needs
// Capabilities.Headers. Values may not contain CR or LF, which would let
// a value inject another header when it is served.
type HTTPHeaders struct {
	// CacheControl, such as "public, max-age=31536000, immutable" for
	// content-addressed keys.
	CacheControl string
	// ContentDisposition, such as `attachment; filename="report.pdf"`.
	ContentDisposition string
	// ContentEncoding, such as "gzip" for a pre-compressed object, which is
	// stored and read back exactly as written.
	ContentEncoding string
}

// The HTTP names of the HTTPHeaders fields.
const (
	HeaderCacheControl       = "Cache-Control"
	HeaderContentDisposition = "Content-Disposition"
	HeaderContentEncoding    = "Content-Encoding"
)

// IsZero reports whether no header is set.
func (h HTTPHeaders) IsZero() bool { return h == HTTPHeaders{} }

// SetOn writes every non-empty field into dst under its HTTP name, leaving
// the others as they are. Signers use it to list the headers a client must
// send with a presigned PUT (SignedRequest.Header); servers use it to send
// stored headers with an object.
func (h HTTPHeaders) SetOn(dst http.Header) {
	for name, v := range h.fields() {
		if v != "" {
			dst.Set(name, v)
		}
	}
}

// HTTPHeadersFrom reads the HTTPHeaders fields from an HTTP header set,
// such as the request of a signed PUT.
func HTTPHeadersFrom(src http.Header) HTTPHeaders {
	return HTTPHeaders{
		CacheControl:       src.Get(HeaderCacheControl),
		ContentDisposition: src.Get(HeaderContentDisposition),
		ContentEncoding:    src.Get(HeaderContentEncoding),
	}
}

// fields maps each HTTP name to its value.
func (h HTTPHeaders) fields() map[string]string {
	return map[string]string{
		HeaderCacheControl:       h.CacheControl,
		HeaderContentDisposition: h.ContentDisposition,
		HeaderContentEncoding:    h.ContentEncoding,
	}
}

// validate refuses values that would split a header line when served.
func (h HTTPHeaders) validate() error {
	for _, v := range []string{h.CacheControl, h.ContentDisposition, h.ContentEncoding} {
		if strings.ContainsAny(v, "\r\n") {
			return Errorf(CodeInvalidArgument, "HTTP header values may not contain CR or LF")
		}
	}
	return nil
}

// ByteRange selects part of an object for Read. Offset is the first byte;
// Length is the number of bytes, where 0 means "to the end of the object"
// (reading zero bytes is never useful, so the zero value reads everything
// from Offset). A range past the end of the object yields an empty read, not
// an error.
type ByteRange struct {
	Offset int64
	Length int64
}

// WriteOptions control a write. The zero value writes with the Disk's
// default visibility and a detected content type.
type WriteOptions struct {
	// ContentType is stored by drivers with Capabilities.ContentType. Empty
	// means the Disk detects it from the first bytes and the key's extension.
	ContentType string
	// Visibility overrides the Disk's default visibility. Empty means the
	// default.
	Visibility Visibility
	// Metadata is stored by drivers with Capabilities.Metadata; on any other
	// driver a non-empty map is CodeUnsupported rather than silently lost.
	Metadata map[string]string
	// Headers are stored with the object by drivers with
	// Capabilities.Headers; on any other driver non-empty headers are
	// CodeUnsupported rather than silently lost.
	Headers HTTPHeaders
	// IfNotExists makes the write fail with CodeExists when the key already
	// exists, atomically. Needs Capabilities.ConditionalWrite; without it
	// the write is refused with CodeUnsupported before any byte is read.
	//
	// The condition is checked when the object is committed, not before the
	// body is sent: the local, memory and S3 drivers (and GCS, Azure and
	// SFTP) read or upload the whole body first, so a losing writer streams
	// all of it before getting CodeExists. Stat first when bodies are large
	// and collisions likely; that is a hint, and IfNotExists stays the
	// guarantee.
	IfNotExists bool
	// IfMatch makes the write a compare-and-swap: it replaces the object
	// only while its ETag is still IfMatch (the one the caller read), and
	// fails with CodeConflict, writing nothing, when the object changed or
	// was deleted in between. Two writers that read the same version can
	// then never both win. Needs Capabilities.CompareAndSwap
	// (CodeUnsupported otherwise); not with IfNotExists, and not for
	// multipart uploads.
	IfMatch string
	// SizeHint is the body's length when known in advance. 0 means unknown
	// (an empty body is still written correctly). A positive hint is also a
	// declaration the Disk holds the body to: a body that ends early or
	// runs past it fails the write with CodeInvalidArgument and nothing is
	// stored, so a truncated upload (a dropped connection, a short
	// Content-Length) is never kept as if it were whole. Drivers may use it
	// to choose a single-request upload.
	SizeHint int64
	// SHA256, when set, is the hex SHA-256 the body must have (the client's
	// declared digest, or one computed upstream). The Disk hashes every
	// body anyway and refuses a mismatch with CodeInvalidArgument before
	// the driver commits, on every driver, so a corrupted body never
	// replaces what was stored. Drivers with Capabilities.ChecksumSHA256
	// also send it to the backend, which verifies it on its side of the
	// wire. Not for multipart uploads (CodeUnsupported).
	SHA256 string
	// Checksums are more digests the Disk computes while streaming, beside
	// SHA-256, each optionally verified (Checksum.Want), and reports in
	// Entry.Checksums: MD5, CRC32C, BLAKE3, whatever hash.Hash the caller
	// brings. A malformed list is CodeInvalidArgument. Not for multipart
	// uploads (CodeUnsupported). nil computes SHA-256 only.
	Checksums []Checksum
	// MaxSize, when positive, aborts the write with CodeTooLarge once the
	// body exceeds this many bytes. Enforced by the Disk while streaming, so
	// nothing larger is ever stored.
	MaxSize int64
}

// ListOptions control one page of a listing.
type ListOptions struct {
	// Recursive lists every key under the directory. When false, keys in
	// sub-directories are folded into one IsDir entry per sub-directory.
	Recursive bool
	// Cursor resumes after a previous page: pass Page.NextCursor. Empty
	// starts from the beginning.
	Cursor string
	// Limit caps the entries per page. 0 means DefaultPageSize.
	Limit int
}

// DefaultPageSize is the page size used when ListOptions.Limit is 0. It is
// S3's maximum, so one page is one request on the most common backend.
const DefaultPageSize = 1000

// Page is one page of a listing, in ascending key order. NextCursor is empty
// on the last page.
type Page struct {
	Entries    []Entry
	NextCursor string
}

// HTTPMethod is the method a signed request authorises.
type HTTPMethod string

// The closed set of methods a signed URL can authorise.
const (
	MethodGet HTTPMethod = http.MethodGet
	MethodPut HTTPMethod = http.MethodPut
)

// HTTPMethodValues lists every HTTPMethod.
var HTTPMethodValues = []HTTPMethod{MethodGet, MethodPut}

// MaxPresignExpiry is the longest lifetime a signed URL may have. It is the
// S3 SigV4 limit; a longer-lived URL is effectively a public link and should
// be one, through VisibilityPublic.
const MaxPresignExpiry = 7 * 24 * time.Hour

// PresignOptions describe a signed URL.
type PresignOptions struct {
	// Method is MethodGet (download) or MethodPut (direct upload).
	Method HTTPMethod
	// Expires is the URL's lifetime: required, positive, at most
	// MaxPresignExpiry. There is deliberately no default, so every caller
	// decides how long a link may live.
	Expires time.Duration
	// ContentType: for a PUT, the type the upload must declare; for a GET,
	// an override for the response's Content-Type.
	ContentType string
	// ContentDisposition overrides the response's Content-Disposition on a
	// GET, e.g. `attachment; filename="report.pdf"`.
	ContentDisposition string
	// Headers, for a PUT only, are stored with the uploaded object, exactly
	// as WriteOptions.Headers are for a Write. The signed request pins
	// them: SignedRequest.Header lists them, the client must send them
	// unchanged, and the backend refuses an upload without them. The Disk
	// issues such a URL only when that is guaranteed: through the driver
	// when it declares Capabilities.PresignHeaders, through its URLSigner
	// when the driver stores headers (Capabilities.Headers) and the signer
	// implements HeaderPinner reporting true. Otherwise it is
	// CodeUnsupported, never a URL that silently loses them. Non-empty on
	// a GET is CodeInvalidArgument: a download overrides the response with
	// ContentDisposition instead.
	Headers HTTPHeaders
	// AllowedIP, when set, restricts the URL to clients whose address is
	// inside this CIDR prefix: "203.0.113.0/24", or "203.0.113.7/32" for
	// one address. It must be in canonical form (no bits set past the
	// prefix length), so what is signed is exactly what is meant. The Disk
	// issues such a URL only when the restriction is enforced: through the
	// driver when it declares Capabilities.PresignIP, through its URLSigner
	// when the signer implements IPRestricter reporting true. Otherwise it
	// is CodeUnsupported, never a URL that silently works from anywhere.
	AllowedIP string
	// Visibility, for a PUT only, is the visibility the uploaded object is
	// stored with, signed into the request (S3's x-amz-acl, GCS's
	// x-goog-acl), so the client cannot change it. Empty, the default,
	// signs nothing and the object gets whatever the backend gives an
	// upload that names none, exactly as before this field existed.
	//
	// It is checked like WriteOptions.Visibility: a value other than the
	// disk's default needs Capabilities.SetVisibility, and on a disk
	// without SetVisibility the default itself is accepted and signs
	// nothing (that backend stores every object at the default). On a
	// disk with SetVisibility any value needs a driver that pins it,
	// Capabilities.PresignVisibility, and a URLSigner never does:
	// otherwise it is CodeUnsupported, never a URL that silently stores
	// the object some other way. Non-empty on a GET is
	// CodeInvalidArgument.
	//
	// Who may ask for VisibilityPublic is the caller's decision: an
	// upload ticket handed to a browser lets its holder create a public
	// object, so upload.Presign issues one only under
	// upload.Policy.AllowPublic.
	Visibility Visibility
}

// ParseAllowedIP parses PresignOptions.AllowedIP: a CIDR prefix in
// canonical form. Drivers and signers that enforce the restriction parse
// it with this one function, so every backend accepts and refuses the same
// values. An IPv4-mapped IPv6 prefix (::ffff:203.0.113.0/120) is returned
// as the IPv4 prefix it covers (203.0.113.0/24), because client addresses
// are compared unmapped and the mapped form would match none of them; one
// shorter than /96 also covers non-IPv4 addresses and is refused.
func ParseAllowedIP(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil || p != p.Masked() {
		return netip.Prefix{}, Errorf(CodeInvalidArgument, "allowed IP %q is not a canonical CIDR prefix such as 203.0.113.0/24", s)
	}
	// A canonical prefix whose address is IPv4-mapped is at least /96: the
	// ::ffff marker fills bits 80 to 95, and canonical form clears every
	// bit past the prefix length, so a shorter one (which would also cover
	// non-IPv4 addresses) already failed the check above.
	if p.Addr().Is4In6() {
		return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-mappedPrefixBits), nil
	}
	return p, nil
}

// mappedPrefixBits is the length of the ::ffff:0:0/96 prefix that holds
// IPv4-mapped IPv6 addresses.
const mappedPrefixBits = 96

// SignedRequest is a pre-authorised HTTP request a client can make without
// credentials. Header holds headers the client must send unchanged (some
// backends sign the content type of a PUT).
type SignedRequest struct {
	Method    HTTPMethod
	URL       string
	Header    http.Header
	ExpiresAt time.Time
}
