package fshttp

import (
	"errors"
	"net/http"

	"github.com/ubgo/filesystem"
)

// ClientMessage is the text an HTTP client may be shown for err. Every
// application handler built on a Disk must answer with it (and StatusOf
// for the status) instead of err.Error(), whose text names the operation,
// the disk and the storage key ("filesystem: write files.acme:<key>: ..."):
// telling a client those leaks one tenant's disk and keys to another.
//
// For the codes that describe the client's own input (too_large, rejected,
// quota_exceeded, invalid_argument, invalid_key) it is the error's own
// cause, which the library writes for clients and which never contains
// the key. For every other code it is the status text of StatusOf(err),
// so backend messages and internals never reach the client. A cause that
// is itself a *filesystem.Error is judged again by the same rule rather
// than printed, because its text would carry the disk and key. For nil it
// is the text of 200.
func ClientMessage(err error) string {
	return clientMessage(StatusOf(err), err)
}

// clientMessage applies ClientMessage's rule, falling back to the text of
// status, the status of the outermost error.
func clientMessage(status int, err error) string {
	var fe *filesystem.Error
	if !errors.As(err, &fe) || !clientVisible[fe.Code] || fe.Err == nil {
		return http.StatusText(status)
	}
	var inner *filesystem.Error
	if errors.As(fe.Err, &inner) {
		return clientMessage(status, inner)
	}
	return fe.Err.Error()
}

// clientVisible lists the codes whose messages describe the client's own
// input (its file, its parameters, its key) and so may be shown to it.
var clientVisible = map[filesystem.Code]bool{
	filesystem.CodeTooLarge:        true,
	filesystem.CodeRejected:        true,
	filesystem.CodeQuotaExceeded:   true,
	filesystem.CodeInvalidArgument: true,
	filesystem.CodeInvalidKey:      true,
}
