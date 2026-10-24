package fshttp

import (
	"net/http"

	"github.com/ubgo/filesystem"
)

// ErrorWriter renders an error response for any HTTP handler in this
// library: fshttp's Public and Signed handlers, upload's handlers,
// recordshttp, the share handler and the framework middleware all accept
// one, so an application can answer every storage error in its own
// envelope (problem+json, its API's {error: {...}} shape, an HTML page)
// and log the cause of a 5xx.
//
// Why one type: before it, each package hard-wired its own shape (plain
// status text in fshttp and share, {error, code} JSON in upload and
// recordshttp), so an app with a house error format had to wrap every
// handler's ResponseWriter.
//
// Invariants the library keeps before calling it:
//   - status is the status the handler would have sent (StatusOf for a
//     storage error, or the handler's own 4xx for a bad request); the
//     writer may send a different one, but should keep the class (4xx/5xx).
//   - A Retry-After the backend asked for is already set on w's header.
//   - err may carry internal detail (driver messages, keys of other
//     objects): it is for logs. Text shown to clients should come from
//     ClientMessage(err) or be the writer's own; never err.Error().
//   - err is never nil: a refusal the handler makes itself (a wrong
//     method, a private object, a request that does not match its signed
//     URL) carries a *filesystem.Error naming what happened.
//
// A nil ErrorWriter in an options struct means the package's default,
// which is that package's historical shape, unchanged.
type ErrorWriter func(w http.ResponseWriter, r *http.Request, status int, err error)

// PlainErrors is fshttp's default ErrorWriter: the status line and the
// status text only, as text/plain.
func PlainErrors(w http.ResponseWriter, _ *http.Request, status int, _ error) {
	http.Error(w, http.StatusText(status), status)
}

// Refusals the fshttp handlers make themselves, passed to an ErrorWriter
// so it sees a real error, and never shown to clients by the default.
var (
	// errMethod is a method the handler does not serve (405).
	errMethod = filesystem.Errorf(filesystem.CodeInvalidArgument, "fshttp: method not allowed")
	// errNotFound is a path that names no object, or an object Public
	// will not serve (private): both answer 404 so a private key is not
	// revealed.
	errNotFound = filesystem.Errorf(filesystem.CodeNotFound, "fshttp: no such public object")
	// errMismatch is a signed request that does not match its URL: from
	// outside the allowed address range, or without the content type or
	// stored headers the URL pins (403).
	errMismatch = filesystem.Errorf(filesystem.CodePermission, "fshttp: request does not match the signed URL")
)

// fail answers err with status through ew, after passing on a Retry-After
// the backend asked for. A nil ew is PlainErrors, the handlers' historical
// answer.
func fail(ew ErrorWriter, w http.ResponseWriter, r *http.Request, status int, err error) {
	SetRetryAfter(w.Header(), err)
	if ew == nil {
		ew = PlainErrors
	}
	ew(w, r, status, err)
}

// failErr answers a storage error with StatusOf(err) through ew.
func failErr(ew ErrorWriter, w http.ResponseWriter, r *http.Request, err error) {
	fail(ew, w, r, StatusOf(err), err)
}

// notFound answers 404 through ew; a nil ew keeps http.NotFound's
// historical "404 page not found" body.
func notFound(ew ErrorWriter, w http.ResponseWriter, r *http.Request) {
	if ew == nil {
		http.NotFound(w, r)
		return
	}
	ew(w, r, http.StatusNotFound, errNotFound)
}
