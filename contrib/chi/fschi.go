// Package fschi mounts github.com/ubgo/filesystem's HTTP handlers on chi
// (v5).
//
// chi is net/http all the way down, so the library's handlers mount on it
// directly; this package removes the three things every chi application
// otherwise writes by hand:
//
//   - Mount serves a handler under a route or a mounted sub-router, with
//     the full matched prefix stripped (chi's own Mount does not strip it,
//     and the library's handlers route on the path below their mount
//     point), for every method.
//   - Actor and Scope are chi-style middleware that resolve the signed-in
//     user and the tenant once and put them where the library reads them:
//     records.WithActor for the Store, ScopeOf for
//     recordshttp.Config.Scope.
//   - Error answers the errors of the application's own handlers the way
//     the library's handlers do, with fshttp.ClientMessage, so a driver's
//     message (which names disks and keys) never reaches a client.
//
// Zip streams a zip download through fshttp.ServeZip; chi's Recoverer
// passes its http.ErrAbortHandler on to net/http, so a failed download
// resets the connection as it should.
package fschi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/upload"
)

// catchAll is the wildcard Mount registers below a prefix.
const catchAll = "*"

// MsgUnauthenticated is the error text of the 401 that Actor and Scope
// answer, the same words recordshttp uses for a request without a scope.
const MsgUnauthenticated = "not signed in"

// ErrNoScope is what ScopeOf returns for a request that did not pass
// through Scope: recordshttp then answers 401, so a route mounted outside
// the middleware refuses rather than running in no tenant.
var ErrNoScope = filesystem.Errorf(filesystem.CodePermission, "fschi: no scope on the request; mount the route behind fschi.Scope")

// Mount serves h for every method at every path below prefix on r, with
// the full matched prefix (every enclosing Route and Mount included)
// stripped from the request's path: the library's handlers route on the
// path below their mount point (recordshttp's "/owners/{type}/{id}/{role}",
// fshttp.Signed's "/{disk}/{key}"). The handler itself answers 405 for the
// methods it does not serve.
//
// A prefix ending in "/" is stripped with its slash ("/tus/" hands the
// handler "" and "abc"), which is what tus expects; without it the
// handler sees a leading "/" ("/media" hands it "/files/fil_…"), which is
// what every other handler expects. For an exact route (upload.Handler,
// upload.PresignHandler) use r.Method(http.MethodPost, path, h) instead.
func Mount(r chi.Router, prefix string, h http.Handler) {
	keepSlash := strings.HasSuffix(prefix, "/")
	route := prefix
	if !keepSlash {
		route += "/"
	}
	r.Handle(route+catchAll, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// The route pattern joins every enclosing router's prefix, so the
		// strip is right however deeply the route is nested.
		strip := strings.TrimSuffix(chi.RouteContext(req.Context()).RoutePattern(), catchAll)
		if !keepSlash {
			strip = strings.TrimSuffix(strip, "/")
		}
		http.StripPrefix(strip, h).ServeHTTP(w, req)
	}))
}

// Actor returns middleware that resolves the request's actor with fn (a
// user ID, "api-key:…") and records it with records.WithActor, so every
// change the request makes through a records.Store is attributed to it
// (File.UploadedBy, the audit log). fn returning "" with no error lets an
// anonymous request through without an actor; an error refuses it with
// 401. Put it before the routes that write records.
func Actor(fn func(r *http.Request) (string, error)) func(http.Handler) http.Handler {
	return Responder{}.Actor(fn)
}

// Responder answers the errors of this package's middleware and of Error
// in an application's own envelope. Its zero value is the package's
// default, the JSON upload.ErrorResponse, and the package-level Actor,
// Scope and Error are the zero Responder's.
//
// Why: an API bound to problem+json or its own {error: {...}} shape
// should not have to rewrite the 401 of Actor and Scope or the answer of
// Error by hand; the same fshttp.ErrorWriter it gives fshttp, upload and
// recordshttp renders these too.
type Responder struct {
	// Errors renders every error: the 401 of Actor and Scope (with
	// ErrUnauthenticated, or the resolver's own error wrapped as
	// CodePermission) and the errors given to Error (with the status of
	// fshttp.StatusOf). A Retry-After is set before it is called. nil
	// keeps the JSON upload.ErrorResponse.
	Errors fshttp.ErrorWriter
}

// ErrUnauthenticated is the error a Responder's Errors receives for a
// request Actor or Scope refuse without an error of the resolver's own
// (Scope's resolver returning "").
var ErrUnauthenticated = filesystem.Errorf(filesystem.CodePermission, MsgUnauthenticated)

// Actor is the package-level Actor answering its 401 through x.
func (x Responder) Actor(fn func(r *http.Request) (string, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, err := fn(r)
			if err != nil {
				x.unauthenticated(w, r, err)
				return
			}
			if actor != "" {
				r = r.WithContext(records.WithActor(r.Context(), actor))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Scope returns middleware that resolves the tenant the request may act
// in with fn and stores it on the request with recordshttp.WithScope,
// where ScopeOf and recordshttp.ScopeFromRequest find it. The
// scope must come from the authenticated session, never from the URL or
// a header the client chooses. An error or "" refuses the request with
// 401 before any handler runs.
func Scope(fn func(r *http.Request) (string, error)) func(http.Handler) http.Handler {
	return Responder{}.Scope(fn)
}

// Scope is the package-level Scope answering its 401 through x.
func (x Responder) Scope(fn func(r *http.Request) (string, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scope, err := fn(r)
			if err != nil || scope == "" {
				x.unauthenticated(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(recordshttp.WithScope(r.Context(), scope)))
		})
	}
}

// ScopeOf returns the scope Scope stored on r, or ErrNoScope. It has the
// signature of recordshttp.Config.Scope, so the records endpoints read the
// tenant the middleware resolved.
func ScopeOf(r *http.Request) (string, error) {
	if scope, ok := recordshttp.ScopeFromContext(r.Context()); ok && scope != "" {
		return scope, nil
	}
	return "", ErrNoScope
}

// Error answers err the way the library's handlers do: the status of
// fshttp.StatusOf (413 for a body over http.MaxBytesReader's cap), a JSON
// upload.ErrorResponse whose text is fshttp.ClientMessage (only the words
// of the client's own input's errors, never a driver's message with its
// disk and key) and whose code is filesystem.CodeOf, and Retry-After when
// the backend asked for a wait. Call it before anything is written.
func Error(w http.ResponseWriter, err error) {
	Responder{}.Error(w, nil, err)
}

// Error is the package-level Error answering through x.Errors when set;
// r is passed on to it (the default does not read it).
func (x Responder) Error(w http.ResponseWriter, r *http.Request, err error) {
	err = classify(err)
	fshttp.SetRetryAfter(w.Header(), err)
	if x.Errors != nil {
		x.Errors(w, r, fshttp.StatusOf(err), err)
		return
	}
	writeJSON(w, fshttp.StatusOf(err), upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}

// classify turns a tripped request-body cap into CodeTooLarge, wrapping
// the cap itself rather than err, whose text may name the disk and key.
func classify(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return filesystem.NewError(filesystem.CodeTooLarge, mbe)
	}
	return err
}

// unauthenticated answers the library's 401, through x.Errors when set,
// with cause (the resolver's error, nil when it returned none).
func (x Responder) unauthenticated(w http.ResponseWriter, r *http.Request, cause error) {
	if x.Errors != nil {
		x.Errors(w, r, http.StatusUnauthorized, refusal(cause))
		return
	}
	writeJSON(w, http.StatusUnauthorized, upload.ErrorResponse{Error: MsgUnauthenticated, Code: filesystem.CodePermission})
}

// refusal is the error an ErrorWriter gets for a 401: the resolver's own
// error as CodePermission, kept for logs, else ErrUnauthenticated.
func refusal(cause error) error {
	if cause == nil {
		return ErrUnauthenticated
	}
	return filesystem.NewError(filesystem.CodePermission, cause)
}

// writeJSON answers status with body as JSON.
func writeJSON(w http.ResponseWriter, status int, body upload.ErrorResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Zip streams the objects of d named by files to the client as one zip
// archive called filename, through fshttp.ServeZip: every name and the
// total size are checked before the first byte, and a failure after it
// aborts the response with http.ErrAbortHandler, which chi's Recoverer
// passes on to net/http. Authorise the request and choose the keys before
// calling it.
func Zip(w http.ResponseWriter, r *http.Request, d *filesystem.Disk, filename string, files []fshttp.ZipFile, opts fshttp.ZipOptions) {
	fshttp.ServeZip(w, r, d, filename, files, opts)
}
