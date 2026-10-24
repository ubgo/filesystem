package fshttp

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/sniff"
)

// attachment is the Content-Disposition that makes a browser download.
const attachment = "attachment"

// serving is what one handler decided about serving an object: the
// signed overrides, the Cache-Control and the opt-ins of its options.
type serving struct {
	// contentType and disposition, from a signed URL, override the
	// stored ones; empty keeps them.
	contentType, disposition string
	// cacheControl replaces the object's stored Cache-Control (chosen by
	// the caller, see cache.go).
	cacheControl string
	// sendfile, when it maps the key, leaves the body to the front proxy.
	sendfile *Sendfile
	// inlinePDF sends InlinePDFCSP instead of the sandbox CSP for a PDF
	// shown inline.
	inlinePDF bool
	// errors answers a read that fails before any header is sent (see
	// serveObject); nil is PlainErrors.
	errors ErrorWriter
}

// conditionalHeaders are the request headers that let http.ServeContent
// answer something other than the full body (304, 412) when no Range is
// asked for. If-Range is not among them: without Range it is ignored.
var conditionalHeaders = [...]string{"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"}

// sendsFullBody reports whether http.ServeContent will answer r with the
// whole object and a 200: a GET (anything but HEAD, as ServeContent sees
// it) with no Range and no conditional header. Only then does serveObject
// open the read before the status is sent. The rule is deliberately
// coarse: a conditional request that turns out to need the body still
// takes the lazy path, which is correct, only without the eager status.
func sendsFullBody(r *http.Request) bool {
	if r.Method == http.MethodHead || r.Header.Get("Range") != "" {
		return false
	}
	for _, h := range conditionalHeaders {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

// serveObject streams key from d for a GET or HEAD, honouring Range,
// If-None-Match and If-Modified-Since through http.ServeContent. Explicit
// contentType and disposition (from a signed URL) override the stored ones;
// the object's stored Content-Disposition and Content-Encoding are sent as
// they are, and cacheControl replaces its stored Cache-Control. With
// sendfile mapping the key, the conditional request is answered and the
// body left to the front proxy (see Sendfile) once every header is set.
//
// http.ServeContent sends the status before its first Read, so a read that
// failed there (the object deleted since Stat, a driver failure, a
// throttled backend) would reach the client as a 200 with an empty or
// short body. So when the whole body will be sent (sendsFullBody) and no
// proxy takes it, the read is opened first: a failure is answered with
// StatusOf(err) and Retry-After through s.errors, and a success is the
// reader ServeContent streams, so there is still one backend request.
// Range, HEAD and conditional requests keep the lazy read, so a range
// reads only its bytes and a 304 reads nothing.
func serveObject(w http.ResponseWriter, r *http.Request, d *filesystem.Disk, e filesystem.Entry, s serving) {
	secure(w)
	proxied, handoff := s.sendfile.target(d, e)
	rs := &rangeSeeker{ctx: r.Context(), disk: d, key: e.Key, size: e.Size}
	defer func() { _ = rs.Close() }()
	if !handoff && sendsFullBody(r) {
		if err := rs.open(); err != nil {
			failErr(s.errors, w, r, err)
			return
		}
	}
	h := w.Header()
	contentType, disposition := s.contentType, s.disposition
	if contentType == "" {
		contentType = e.ContentType
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	h.Set("Content-Type", contentType)
	if disposition == "" {
		disposition = e.Headers.ContentDisposition
	}
	// Active content is never rendered inline, whatever was stored or
	// signed: as a download it cannot run script on this origin.
	if sniff.IsActive(contentType) && !isAttachment(disposition) {
		disposition = attachment
	}
	if s.inlinePDF && !isAttachment(disposition) && sniff.Matches(contentType, pdfType) {
		h.Set(HeaderCSP, InlinePDFCSP)
	}
	stored := e.Headers
	stored.ContentDisposition = disposition
	stored.CacheControl = s.cacheControl
	stored.SetOn(h)
	if e.ETag != "" {
		h.Set("ETag", `"`+strings.Trim(e.ETag, `"`)+`"`)
	}
	if handoff {
		s.sendfile.serve(w, r, e, proxied)
		return
	}
	http.ServeContent(w, r, "", e.ModTime, rs)
}

// rangeSeeker adapts ranged Disk reads to the io.ReadSeeker http.ServeContent
// needs. It opens a ranged read lazily at the current offset, so serving a
// byte range reads only that range from the backend.
//
// Seek only moves offset; a reader already open is kept until a Read finds
// it at another position (rcAt != offset). That is what lets a reader
// opened before ServeContent (see serveObject) survive ServeContent's size
// probe, Seek(0, SeekEnd) then Seek(0, SeekStart), and be streamed rather
// than opened a second time.
type rangeSeeker struct {
	ctx    context.Context
	disk   *filesystem.Disk
	key    string
	size   int64
	offset int64
	// rc is the open read, nil before the first Read; rcAt is the offset
	// its next byte is at.
	rc   io.ReadCloser
	rcAt int64
}

// open opens the read at the current offset, closing one at another
// position.
func (s *rangeSeeker) open() error {
	_ = s.Close()
	rc, err := s.disk.ReadRange(s.ctx, s.key, filesystem.ByteRange{Offset: s.offset})
	if err != nil {
		return err
	}
	s.rc, s.rcAt = rc, s.offset
	return nil
}

func (s *rangeSeeker) Read(p []byte) (int, error) {
	if s.offset >= s.size {
		return 0, io.EOF
	}
	if s.rc == nil || s.rcAt != s.offset {
		if err := s.open(); err != nil {
			return 0, err
		}
	}
	n, err := s.rc.Read(p)
	s.offset += int64(n)
	s.rcAt = s.offset
	return n, err
}

func (s *rangeSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = s.offset + offset
	case io.SeekEnd:
		abs = s.size + offset
	default:
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "seek to negative offset %d", abs)
	}
	s.offset = abs
	return abs, nil
}

func (s *rangeSeeker) Close() error {
	if s.rc == nil {
		return nil
	}
	err := s.rc.Close()
	s.rc = nil
	return err
}

// isAttachment reports whether a Content-Disposition makes a browser
// download rather than render.
func isAttachment(disposition string) bool {
	return strings.HasPrefix(strings.ToLower(disposition), attachment)
}

// splitPath splits "/disk/key/with/slashes" into its disk and key.
func splitPath(p string) (string, string, bool) {
	disk, key, ok := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	return disk, key, ok && disk != "" && key != ""
}
