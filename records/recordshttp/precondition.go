package recordshttp

import (
	"net/http"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// A file's ETag over HTTP is its content digest, records.File.SHA256, in
// quotes. Clients send it back in If-Match to change only the content they
// read (RFC 9110 §13.1.1): PUT /files/{id}, PUT /files/{id}/visibility and
// DELETE /files/{id}, restore and purge answer 412 Precondition Failed
// when the file's content changed since, checked by the route and again
// by the Store under the row lock (records.IfUnchanged).

// headerIfMatch is the request header carrying the ETags a change needs.
const headerIfMatch = "If-Match"

// etag is f's ETag: its SHA-256 in quotes.
func etag(f records.File) string { return `"` + f.SHA256 + `"` }

// errPrecondition is a failed If-Match; fail answers it with 412.
var errPrecondition = filesystem.Errorf(filesystem.CodeConflict, "the file changed since it was read (If-Match)")

// ifMatch checks r's If-Match header against f, the file as loaded. No
// header, or "*" (any current content), is no condition. Otherwise the
// header lists ETags (strong or weak, compared by value) and f's must be
// one of them, else errPrecondition. It returns the change options that
// hold the condition until the write: IfUnchanged with f's digest, which
// the Store checks again under the row lock, so a replacement landing
// between this check and the write is refused too.
func ifMatch(r *http.Request, f records.File) ([]records.ChangeOption, error) {
	header := strings.TrimSpace(r.Header.Get(headerIfMatch))
	if header == "" || header == "*" {
		return nil, nil
	}
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimPrefix(strings.TrimSpace(tag), "W/")
		if strings.EqualFold(strings.Trim(tag, `"`), f.SHA256) && f.SHA256 != "" {
			return []records.ChangeOption{records.IfUnchanged(f.SHA256)}, nil
		}
	}
	return nil, errPrecondition
}
