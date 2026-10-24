// Package upload is how bytes arrive from browsers and phones: streaming
// server uploads, presigned direct uploads confirmed by the server, browser
// multipart uploads in the protocol Uppy's AWS S3 plugin speaks, and the
// sweeping of uploads nobody finished.
//
// Every path enforces the same Policy, and every check runs on the server
// against the bytes actually stored: the type is detected from the content,
// never taken from the client's Content-Type or file extension, and the size
// is counted while streaming, never taken from a header.
package upload

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/sniff"
)

// DefaultDenied lists the content types refused by every policy unless
// Policy.AllowActiveContent is set: the types that run script when a
// browser renders them (sniff.ActiveTypes), which served from the
// application's origin are stored XSS, and executables.
var DefaultDenied = append(slices.Clone(sniff.ActiveTypes),
	"application/x-msdownload",
	"application/x-executable",
	"application/wasm",
)

// DefaultDeniedExtensions are file-name extensions refused by every policy
// unless Policy.AllowActiveContent is set: server-side scripts and
// executables, which a web server or a user's machine may run. Every
// dotted segment of a name is checked, not only the last, because some
// servers execute "shell.php.jpg".
var DefaultDeniedExtensions = []string{
	".php", ".php3", ".php4", ".php5", ".php7", ".php8", ".phtml", ".phar", ".pht",
	".asp", ".aspx", ".ashx", ".asmx", ".cer", ".jsp", ".jspx", ".cgi", ".pl", ".py", ".rb",
	".sh", ".bash", ".zsh", ".ps1", ".psm1", ".bat", ".cmd", ".com", ".exe", ".dll", ".msi",
	".scr", ".vbs", ".vbe", ".wsf", ".jar", ".hta", ".htaccess", ".shtml", ".lnk",
}

// KeyFunc chooses the storage key for an upload. It must not trust
// Info.Filename for anything but cosmetics: two users can upload the same
// name, and names can carry path tricks.
type KeyFunc func(Info) (string, error)

// KeyContextFunc is a KeyFunc that also receives the request's context:
// the tenant or user an authentication middleware stored on it, a trace,
// a deadline for a lookup. Set it as Policy.KeyContext. The same rule
// holds: Info.Filename is cosmetic, never part of a key's identity.
type KeyContextFunc func(ctx context.Context, info Info) (string, error)

// Policy describes what an upload endpoint accepts and where it stores it.
type Policy struct {
	// MaxSize is the largest accepted file in bytes. Required.
	MaxSize int64
	// MinSize, when positive, is the smallest accepted file in bytes;
	// MinSize: 1 refuses empty files, which are almost always a client bug
	// (a picker that lost its file, a cancelled read). 0 accepts any size.
	// Refused with CodeRejected; Receive and Confirm delete the stored
	// object, Presign refuses the declared size before signing.
	MinSize int64
	// Allowed lists accepted MIME patterns ("image/*", "application/pdf").
	// Required: there is no accept-everything default.
	Allowed []string
	// Denied lists refused patterns, checked after Allowed; DefaultDenied
	// is always added unless AllowActiveContent is set.
	Denied []string
	// AllowActiveContent lifts DefaultDenied and DefaultDeniedExtensions,
	// for endpoints that really must accept HTML, SVG or scripts (a code
	// host) and serve them from another origin.
	AllowActiveContent bool
	// AllowActive names the active types (sniff.IsActive) this policy
	// accepts while every other protection stays on: each listed type is
	// exempt from DefaultDenied and the active-content rule, and nothing
	// else is; DefaultDeniedExtensions still apply. It is the narrow
	// escape hatch AllowActiveContent is not: contrib/svg sets it to SVG,
	// which it sanitises, without letting HTML, XHTML or any other +xml
	// type through.
	AllowActive []string
	// Detector labels formats the built-in detection does not know
	// (DICOM, STL, camera RAW, Parquet), so Allowed can name them instead
	// of allowing application/octet-stream. It is consulted only when
	// sniff.Detect answers octet-stream, so it can never relabel a known
	// type, and its answer goes through Allowed, Denied and the
	// active-content rule like any other; an empty or malformed answer
	// stays octet-stream (sniff.DetectWith). Receive, Confirm (presigned
	// and Uppy uploads) and the chunked handler use it. nil means the
	// built-in detection only.
	Detector sniff.Detector
	// Key chooses storage keys. nil means RandomKey("").
	Key KeyFunc
	// KeyContext chooses storage keys like Key, with the context of the
	// upload (the request's, for the HTTP handlers), so a key can follow
	// the tenant a middleware put on it without a Resolver building a
	// Policy per request. Set Key or KeyContext, not both: Validate
	// refuses a policy with both, because which one wins would be a
	// guess. nil uses Key.
	KeyContext KeyContextFunc
	// Visibility of stored files; empty means the disk's default.
	// Receive and the Uppy handler apply it. Presign applies it only with
	// SignVisibility; otherwise a ticket signs a visibility only when its
	// TicketRequest asks for one (see AllowPublic), so a policy shared
	// with Receive never changes what existing tickets do.
	Visibility filesystem.Visibility
	// SignVisibility makes Presign sign Visibility into every ticket, so
	// the server, not the client's JSON, decides that an avatar or a
	// public gallery upload is stored public. Off, the default, keeps the
	// historical rule above.
	//
	// Invariants: it needs a Visibility (Validate refuses it without
	// one, since signing "the disk's default" signs nothing); a public
	// Visibility still needs AllowPublic (Validate refuses it otherwise),
	// because the ticket holder creates the object; and a TicketRequest
	// asking for another visibility than the policy's is refused with
	// CodePermission, never silently overridden, so a client learns its
	// request was not honoured.
	SignVisibility bool
	// AllowPublic lets a ticket request (TicketRequest.Visibility) ask for
	// filesystem.VisibilityPublic. Off, the default, such a request is
	// refused with CodePermission before anything is signed.
	//
	// Why it is a separate switch: whoever holds a ticket creates the
	// object, so a ticket that may be public lets any client publish a
	// file under the application's domain. Only the server decides that,
	// per policy (avatars yes, invoices no); the client can only ask.
	// Asking for private, or for nothing, needs no allow. The disk must
	// pin the visibility in the upload (filesystem.PresignOptions.
	// Visibility); one that cannot refuses with CodeUnsupported.
	AllowPublic bool
	// Headers, when set, chooses the HTTP headers stored with each upload
	// (Cache-Control, Content-Disposition, Content-Encoding), which the
	// backend's URLs and fshttp then send with it. StaticHeaders and
	// Attachment cover the common cases. It sees the file as it will be
	// stored (after Process). The disk must store headers
	// (Capabilities.Headers), or every upload fails with CodeUnsupported
	// rather than losing them. Receive and the Uppy handler store them;
	// Presign pins them in the signed PUT where the disk can (see
	// filesystem.PresignOptions.Headers) and refuses with CodeUnsupported
	// where it cannot, so the backend itself refuses an upload without
	// them.
	Headers HeadersFunc
	// PostForm makes Presign issue a presigned POST form (Ticket.Fields)
	// instead of a signed PUT: the backend itself then refuses an upload
	// whose size is not exactly the declared one, whose Content-Type is not
	// the declared one, or whose key differs, rather than Confirm finding
	// out afterwards. The disk needs Capabilities.PresignPost (S3, GCS);
	// without it Presign fails with CodeUnsupported. Confirm is still
	// required: the bytes are the client's.
	PostForm bool
	// Collision says what happens when the key already holds an object:
	// overwrite (the default), fail with CodeExists, or rename to a free
	// "name (1).ext". Fail and rename write with IfNotExists, so the disk
	// needs Capabilities.ConditionalWrite (CodeUnsupported otherwise).
	// Receive and the Uppy handler apply it; Presign refuses anything but
	// overwrite with CodeUnsupported, because a presigned PUT replaces
	// whatever is there.
	Collision Collision
	// Extensions, when set, lists the file-name extensions accepted
	// (".pdf", ".jpg"), compared without case. A name without an
	// extension passes; the type check still applies to it.
	Extensions []string
	// MatchExtension refuses a file whose name's extension conventionally
	// means another type than the one detected from its bytes
	// (sniff.TypeByExtension), such as a PNG named "invoice.pdf". Names
	// whose extension the table does not know pass.
	MatchExtension bool
	// Process, when set, transforms every accepted file before it is
	// stored: strip a photo's EXIF and GPS by re-encoding it
	// (imaging.Sanitize), convert a format, compress. The type it reports
	// is checked against the policy again, and MaxSize applies to what it
	// returns. Receive runs it, so it covers form uploads, imports and tus;
	// presigned and Uppy uploads go straight to the bucket and cannot be
	// processed (Confirm only checks them).
	Process Processor
	// Progress, when set, is told how many bytes of a file Receive has
	// streamed to the disk: about every ProgressStep bytes and once more,
	// with the total, when the file is fully read. It covers the form
	// handler, FromURL and tus, which all receive through Receive. It runs
	// in the request's goroutine and must return quickly (push to a
	// channel, update a counter).
	Progress func(ctx context.Context, info Info, received int64)
	// Settle, when set, runs after Inspect on every stored upload and may
	// move it, returning the Result for where it now is; ContentAddressed
	// uses it to give files content-derived keys. An error deletes the
	// upload and is returned. Receive, Confirm and the Uppy handler run it.
	Settle Settler
	// Checksums asks for more digests of every stored file beside its
	// SHA-256 (MD5 for an existing checksum column, CRC32C, BLAKE3
	// for content addressing), reported in Result.Checksums by
	// filesystem.Checksum.Name. Receive (and so the form handler, FromURL,
	// tus and the chunked handler) has Disk.Write compute them while it
	// streams; Confirm (presigned and Uppy uploads, multipart included)
	// reads the stored object once to compute them, because those bytes
	// never passed through the server. A Want, which a per-request
	// Resolver can set from what the client declared, is verified on both
	// paths: Receive fails with CodeInvalidArgument before anything is
	// committed, Confirm deletes the object and refuses it with
	// CodeRejected. They describe the bytes as stored, after Process.
	// Validate checks the list (filesystem.ValidateChecksums). nil, the
	// default, computes nothing more and leaves Result.Checksums nil.
	Checksums []filesystem.Checksum
	// Inspect, when set, examines every accepted file once it is stored
	// and before the upload reports success: a virus scan (package scan),
	// a full image decode, a moderation call. An error deletes the object
	// and is returned as the upload's error, so a file that fails
	// inspection is never reported as stored. Receive and Confirm run it,
	// which covers every upload path in this module.
	Inspect Inspector
}

// Processor transforms an upload's bytes before they are stored; see
// Policy.Process. It returns the new body and what it now is (ContentType
// and Filename); an error refuses the upload.
type Processor func(ctx context.Context, in Info, body io.Reader) (io.Reader, Info, error)

// Settler finishes a stored upload, possibly moving it; see Policy.Settle.
type Settler func(ctx context.Context, disk *filesystem.Disk, r Result) (Result, error)

// settle runs p.Settle, deleting the upload when it fails.
func (p Policy) settle(ctx context.Context, disk *filesystem.Disk, r Result) (Result, error) {
	if p.Settle == nil {
		return r, nil
	}
	out, err := p.Settle(ctx, disk, r)
	if err != nil {
		return Result{}, reject(ctx, disk, r.Key, err)
	}
	return out, nil
}

// Inspector examines a stored upload; see Policy.Inspect. It reads the
// file from disk at r.Key. Errors should carry a filesystem.Code
// (CodeRejected for a refused file, CodeUnavailable when the inspection
// itself could not run).
type Inspector func(ctx context.Context, disk *filesystem.Disk, r Result) error

// inspect runs p.Inspect on a stored result, deleting the object when it
// fails.
func (p Policy) inspect(ctx context.Context, disk *filesystem.Disk, r Result) (Result, error) {
	if p.Inspect == nil {
		return r, nil
	}
	if err := p.Inspect(ctx, disk, r); err != nil {
		return Result{}, reject(ctx, disk, r.Key, err)
	}
	return r, nil
}

// Info describes an incoming file to a KeyFunc.
type Info struct {
	// Filename is the client's file name reduced to a safe base name
	// (SafeFilename). Display only.
	Filename string
	// ContentType is the type detected from the content.
	ContentType string
	// Path is the client's relative path made safe (SafePath), ending in
	// Filename: folder uploads keep their layout through it.
	Path string
}

// Validate checks the policy itself: a MaxSize, at least one allowed type,
// a known visibility. Every upload path calls it, so a misconfigured
// endpoint fails with CodeInvalidArgument rather than accepting
// everything; integrations call it at construction to fail earlier.
func (p Policy) Validate() error {
	if p.MaxSize <= 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload policy has no MaxSize")
	}
	if p.MinSize < 0 || p.MinSize > p.MaxSize {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload policy MinSize %d is not in [0, MaxSize]", p.MinSize)
	}
	if len(p.Allowed) == 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload policy allows no content types")
	}
	for _, ext := range p.Extensions {
		if len(ext) < 2 || ext[0] != '.' || strings.ContainsAny(ext[1:], "./\\") {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload policy extension %q must look like \".pdf\"", ext)
		}
	}
	if !slices.Contains(CollisionValues, p.Collision) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload policy has unknown collision policy %q", p.Collision)
	}
	if p.Visibility != "" && !p.Visibility.Valid() {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload policy has unknown visibility %q", p.Visibility)
	}
	if p.Key != nil && p.KeyContext != nil {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload policy sets both Key and KeyContext")
	}
	if p.SignVisibility && p.Visibility == "" {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload policy sets SignVisibility without a Visibility to sign")
	}
	if p.SignVisibility && p.Visibility == filesystem.VisibilityPublic && !p.AllowPublic {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload policy signs public tickets without AllowPublic")
	}
	if err := filesystem.ValidateChecksums(p.Checksums); err != nil {
		return err
	}
	return nil
}

// AcceptsName applies the name rules (DefaultDeniedExtensions, Extensions, MatchExtension) to a
// file name and the content type detected or declared for it;
// CodeRejected when they refuse it.
func (p Policy) AcceptsName(name, contentType string) error {
	base := strings.ToLower(SafeFilename(name))
	if !p.AllowActiveContent {
		// Every segment after the first dot, and the whole name for
		// dot-files such as ".htaccess".
		segments := strings.Split(base, ".")[1:]
		for _, seg := range segments {
			if slices.Contains(DefaultDeniedExtensions, "."+seg) {
				return filesystem.Errorf(filesystem.CodeRejected, "files named %q are refused: %q can be executed", base, "."+seg)
			}
		}
	}
	ext := path.Ext(base)
	if ext == "" {
		return nil
	}
	if len(p.Extensions) > 0 && !slices.ContainsFunc(p.Extensions, func(e string) bool { return strings.EqualFold(e, ext) }) {
		return filesystem.Errorf(filesystem.CodeRejected, "the extension %q is not allowed", ext)
	}
	if p.MatchExtension {
		if want := sniff.TypeByExtension(ext); want != "" && !sniff.Matches(contentType, want) {
			return filesystem.Errorf(filesystem.CodeRejected, "the name says %s but the content is %s", want, contentType)
		}
	}
	return nil
}

// Inspectors runs several inspectors in order, stopping at the first
// error: a virus scan, then a decode check, then a moderation call. nil
// entries are skipped.
func Inspectors(list ...Inspector) Inspector {
	return func(ctx context.Context, disk *filesystem.Disk, r Result) error {
		for _, in := range list {
			if in == nil {
				continue
			}
			if err := in(ctx, disk, r); err != nil {
				return err
			}
		}
		return nil
	}
}

// detect is the content type of an upload whose first bytes are head and
// whose (already sanitised) name is name, with p.Detector consulted for
// formats the built-in detection does not know.
func (p Policy) detect(head []byte, name string) string {
	return sniff.DetectWith(p.Detector, head, name)
}

// Accepts reports whether the policy accepts contentType, and if not, why.
func (p Policy) Accepts(contentType string) error {
	if !sniff.MatchesAny(contentType, p.Allowed) {
		return filesystem.Errorf(filesystem.CodeRejected, "content type %q is not allowed", contentType)
	}
	// sniff.IsActive also covers every +xml type (XHTML script runs in any
	// of them), which no fixed list in DefaultDenied can enumerate.
	if sniff.MatchesAny(contentType, p.deniedTypes()) || (!p.AllowActiveContent && sniff.IsActive(contentType) && !sniff.MatchesAny(contentType, p.AllowActive)) {
		return filesystem.Errorf(filesystem.CodeRejected, "content type %q is refused", contentType)
	}
	return nil
}

func (p Policy) key(ctx context.Context, info Info) (string, error) {
	fn := p.KeyContext
	if fn == nil {
		byInfo := p.Key
		if byInfo == nil {
			byInfo = RandomKey("")
		}
		fn = func(_ context.Context, info Info) (string, error) { return byInfo(info) }
	}
	k, err := fn(ctx, info)
	if err != nil {
		return "", err
	}
	return filesystem.CleanKey(k)
}

// Resolver chooses the policy for one request from the handler's base
// policy, before any byte of the request body is read: limits that follow
// the user's plan or role (a 5 MB free tier, a 100 MB paid one), a key
// directory per user, a stricter type list per collection. It is also the
// hook for refusing early: authentication, a quota that is already used
// up, or a rate limit (return CodeUnavailable with WithRetryAfter and the
// client gets 503 and Retry-After). The error is answered with its code's
// status and fshttp.ClientMessage: its own words reach the client only for
// the codes about the client's input (invalid_argument, rejected, …), the
// status text otherwise. The returned policy is validated like any other;
// one that fails is the server's misconfiguration, not the client's
// mistake, so it is answered 500 with the status text only.
type Resolver func(r *http.Request, base Policy) (Policy, error)

// resolve runs fn, or returns base when fn is nil, and validates the
// result. A resolved policy that fails Validate comes back as a
// *misconfigured error, which writeResolveError answers as a server fault.
func resolve(fn Resolver, r *http.Request, base Policy) (Policy, error) {
	p := base
	if fn != nil {
		var err error
		if p, err = fn(r, base); err != nil {
			return Policy{}, err
		}
	}
	if err := p.Validate(); err != nil {
		return Policy{}, &misconfigured{err: err}
	}
	return p, nil
}

// misconfigured marks a policy the server built wrong. It keeps the cause
// for errors.As and logs, but is never shown to the client.
type misconfigured struct{ err error }

func (m *misconfigured) Error() string { return "upload: misconfigured policy: " + m.err.Error() }
func (m *misconfigured) Unwrap() error { return m.err }

// writeResolveError answers a failed resolve: a misconfigured policy is
// 500 with the status text and code unknown, so no configuration detail
// reaches the client; the Resolver's own error keeps its code and status.
func writeResolveError(ew fshttp.ErrorWriter, w http.ResponseWriter, r *http.Request, err error) {
	var m *misconfigured
	if errors.As(err, &m) {
		if ew != nil {
			// A custom writer gets the cause for its logs; like every
			// ErrorWriter it must not show err's text to the client.
			ew(w, r, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: http.StatusText(http.StatusInternalServerError), Code: filesystem.CodeUnknown})
		return
	}
	writeJSONError(ew, w, r, statusOf(err), err)
}

// HeadersFunc chooses the HTTP headers stored with an upload; see
// Policy.Headers. Info.Filename is the client's name made safe, fit for a
// Content-Disposition filename and nothing else. An error refuses the
// upload and should carry a filesystem.Code.
type HeadersFunc func(Info) (filesystem.HTTPHeaders, error)

// StaticHeaders returns a HeadersFunc that stores h with every upload, such
// as a long Cache-Control for random, never-reused keys:
//
//	p.Headers = upload.StaticHeaders(filesystem.HTTPHeaders{CacheControl: "public, max-age=31536000, immutable"})
func StaticHeaders(h filesystem.HTTPHeaders) HeadersFunc {
	return func(Info) (filesystem.HTTPHeaders, error) { return h, nil }
}

// Attachment returns a HeadersFunc that makes every upload download under
// its original name rather than display in the browser:
// `attachment; filename=report.pdf` (mime.FormatMediaType quotes a name
// only when it is not a plain token, `filename="my report.pdf"`, and
// encodes one a quoted string cannot hold per RFC 2231,
// `filename*=utf-8”f%C3%AAte.png`), and cacheControl as
// the Cache-Control ("" stores none). Random keys lose the client's name;
// this is how a download gets it back.
func Attachment(cacheControl string) HeadersFunc {
	return func(info Info) (filesystem.HTTPHeaders, error) {
		return filesystem.HTTPHeaders{CacheControl: cacheControl, ContentDisposition: attachmentDisposition(info.Filename)}, nil
	}
}

// dispositionAttachment is the Content-Disposition type that makes a
// browser download instead of display.
const dispositionAttachment = "attachment"

// attachmentDisposition formats an attachment disposition for name.
// mime.FormatMediaType returns "" only when the type or a parameter name
// is not a token; both are constants here, and any value, even invalid
// UTF-8 or a control character, is quoted or RFC 2231 encoded, so the
// result is never empty.
func attachmentDisposition(name string) string {
	return mime.FormatMediaType(dispositionAttachment, map[string]string{"filename": name})
}

// checkMinSize refuses a file smaller than MinSize.
func (p Policy) checkMinSize(size int64) error {
	if size < p.MinSize {
		return filesystem.Errorf(filesystem.CodeRejected, "the file is %d bytes, under the minimum of %d", size, p.MinSize)
	}
	return nil
}

// headers runs p.Headers, or stores none when it is nil.
func (p Policy) headers(info Info) (filesystem.HTTPHeaders, error) {
	if p.Headers == nil {
		return filesystem.HTTPHeaders{}, nil
	}
	return p.Headers(info)
}

// RandomKey returns a KeyFunc that stores files under dir with a random
// name and the extension of the detected type, e.g. "avatars/3QX…7A.png".
// The client's name never reaches the key, so it cannot collide, overwrite
// or smuggle a path.
func RandomKey(dir string) KeyFunc {
	return func(info Info) (string, error) {
		return filesystem.JoinKey(dir, randomName()+sniff.Extension(info.ContentType)), nil
	}
}

// MaxShardDepth bounds ShardedKey's depth: four levels of 1,024
// directories each already spread far more files than any disk holds.
const MaxShardDepth = 4

// shardWidth is the characters of the random name each shard level takes:
// two base32 characters, 1,024 directories per level.
const shardWidth = 2

// ShardedKey returns a KeyFunc like RandomKey that also splits the random
// name into depth levels of directories, two characters each:
// "photos/q3/7a/q37a…x2.png" for depth 2. A local disk then never holds a
// huge flat directory, which slows listing and some filesystems. depth
// must be in [1, MaxShardDepth]; any other value makes every key fail
// with CodeInvalidArgument, so the mistake shows at the first upload.
func ShardedKey(dir string, depth int) KeyFunc {
	return func(info Info) (string, error) {
		if depth < 1 || depth > MaxShardDepth {
			return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "shard depth %d is not in [1, %d]", depth, MaxShardDepth)
		}
		name := randomName()
		parts := []string{dir}
		for i := range depth {
			parts = append(parts, name[i*shardWidth:(i+1)*shardWidth])
		}
		return filesystem.JoinKey(append(parts, name+sniff.Extension(info.ContentType))...), nil
	}
}

// NameKey returns a KeyFunc that stores files under dir by the client's
// file name made safe (SafeFilename), for products where users see and
// pick names (a shared drive). Two uploads of one name then meet at one
// key: choose what happens with Policy.Collision. The policy's name rules
// (DefaultDeniedExtensions, Extensions, MatchExtension) apply to the name.
func NameKey(dir string) KeyFunc {
	return func(info Info) (string, error) {
		return filesystem.JoinKey(dir, SafeFilename(info.Filename)), nil
	}
}

// Collision says what an upload does when its key already holds an
// object. It matters only for keys the client's name chooses (NameKey);
// random keys do not collide.
type Collision string

// The closed set of collision policies.
const (
	// CollisionOverwrite (the zero value) replaces the existing object.
	CollisionOverwrite Collision = ""
	// CollisionFail refuses the upload with CodeExists, atomically.
	CollisionFail Collision = "fail"
	// CollisionRename stores the upload under the first free name of
	// "photo (1).png", "photo (2).png", … (see FreeKey), as desktop
	// systems do, and still refuses with CodeExists, never overwriting,
	// if a racing upload takes that name first.
	CollisionRename Collision = "rename"
)

// CollisionValues lists every Collision.
var CollisionValues = []Collision{CollisionOverwrite, CollisionFail, CollisionRename}

// MaxRenameAttempts bounds how many suffixed names FreeKey tries.
const MaxRenameAttempts = 100

// FreeKey returns key when nothing is stored there, else the first of
// "dir/name (1).ext" … "dir/name (MaxRenameAttempts).ext" that is free,
// or CodeExists when all are taken. It only looks: write with
// WriteOptions.IfNotExists so a racing writer cannot be overwritten
// between this check and the write.
func FreeKey(ctx context.Context, disk *filesystem.Disk, key string) (string, error) {
	ext := path.Ext(key)
	stem := strings.TrimSuffix(key, ext)
	candidate := key
	for n := 1; ; n++ {
		taken, err := disk.Exists(ctx, candidate)
		if err != nil || !taken {
			return candidate, err
		}
		if n > MaxRenameAttempts {
			return "", filesystem.Errorf(filesystem.CodeExists, "no free name after %d attempts", MaxRenameAttempts)
		}
		candidate = stem + " (" + strconv.Itoa(n) + ")" + ext
	}
}

// claim applies the collision policy to key: the key to write and whether
// the write must be conditional.
func (p Policy) claim(ctx context.Context, disk *filesystem.Disk, key string) (string, bool, error) {
	switch p.Collision {
	case CollisionFail:
		return key, true, nil
	case CollisionRename:
		free, err := FreeKey(ctx, disk, key)
		return free, true, err
	}
	return key, false, nil
}

// maxFilenameBytes caps a display file name; longer names are truncated.
const maxFilenameBytes = 255

// SafeFilename reduces a client-supplied file name to something safe to
// store and display: the base name only (no directories, either separator),
// no control characters, at most 255 bytes, and "file" when nothing is left.
func SafeFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "." || name == "/" || name == ".." || name == "" {
		return "file"
	}
	for len(name) > maxFilenameBytes {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// deniedTypes is every type the policy refuses: Denied, plus DefaultDenied
// unless AllowActiveContent, less the types AllowActive exempts.
func (p Policy) deniedTypes() []string {
	if p.AllowActiveContent {
		return p.Denied
	}
	out := slices.Clone(p.Denied)
	for _, t := range DefaultDenied {
		if !sniff.MatchesAny(t, p.AllowActive) {
			out = append(out, t)
		}
	}
	return out
}
