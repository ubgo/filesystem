package filesystem

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
)

// Code classifies an error independently of the backend that produced it.
// Callers branch on the Code (with errors.Is against the Err* sentinels, or
// with CodeOf) and never on a vendor error type, which is what lets an
// application swap S3 for local disk without touching its error handling.
type Code string

// The closed set of error codes. Every driver maps its native errors into
// these; an error it cannot classify becomes CodeUnknown, never a new code.
const (
	// CodeNotFound: the key does not exist (or the directory has no keys).
	CodeNotFound Code = "not_found"
	// CodeExists: a conditional write (WriteOptions.IfNotExists) found the
	// key already present, or a registry already holds the name.
	CodeExists Code = "exists"
	// CodeInvalidKey: the key is empty, too long, not valid UTF-8, contains a
	// control character, or contains a ".." segment.
	CodeInvalidKey Code = "invalid_key"
	// CodeInvalidArgument: an option or argument other than the key is
	// invalid (an unknown visibility, a non-positive expiry, …).
	CodeInvalidArgument Code = "invalid_argument"
	// CodeUnsupported: the driver does not have the capability the call
	// needs, and there is no portable fallback.
	CodeUnsupported Code = "unsupported"
	// CodeReadOnly: a mutating operation reached a read-only disk.
	CodeReadOnly Code = "read_only"
	// CodeTooLarge: a write exceeded WriteOptions.MaxSize. Nothing is stored.
	CodeTooLarge Code = "too_large"
	// CodeRejected: the content was refused by a policy, for example an
	// upload whose detected type is not allowed. Nothing is stored.
	CodeRejected Code = "rejected"
	// CodeQuotaExceeded: the owner of the scope has no storage left (see
	// records/quota). Distinct from CodeTooLarge, which is about one file.
	CodeQuotaExceeded Code = "quota_exceeded"
	// CodePermission: the backend refused the operation (credentials, ACL,
	// file mode).
	CodePermission Code = "permission"
	// CodeUnavailable: a transient failure (timeout, throttling, connection
	// reset). The only code the retry interceptor retries. A context that
	// ended (context.DeadlineExceeded or context.Canceled anywhere in the
	// chain) is classified here too, so a per-attempt deadline from
	// layer.Timeout is retried; layer.Retry still stops once the caller's
	// own context has ended.
	CodeUnavailable Code = "unavailable"
	// CodeConflict: a compare-and-swap write (WriteOptions.IfMatch) found
	// the object changed, or gone, since the caller read it. Nothing is
	// written; read it again and retry.
	CodeConflict Code = "conflict"
	// CodeNotModified: a conditional read (Disk.ReadIf) found the object
	// unchanged, so no body is returned. Not a failure: the caller's copy
	// is current.
	CodeNotModified Code = "not_modified"
	// CodeUnknown: an error the driver could not classify.
	CodeUnknown Code = "unknown"
)

// CodeValues lists every Code, for exhaustive tests and documentation.
var CodeValues = []Code{
	CodeNotFound, CodeExists, CodeInvalidKey, CodeInvalidArgument, CodeUnsupported,
	CodeReadOnly, CodeTooLarge, CodeRejected, CodeQuotaExceeded, CodePermission, CodeUnavailable,
	CodeConflict, CodeNotModified, CodeUnknown,
}

// codeSentinel is the type of the Err* values. It exists so that
// errors.Is(err, ErrNotFound) matches any *Error carrying CodeNotFound,
// whichever operation, disk or key produced it.
type codeSentinel struct{ code Code }

func (s *codeSentinel) Error() string { return "filesystem: " + string(s.code) }

// Sentinels for errors.Is. Each matches every *Error with the same Code.
var (
	ErrNotFound        error = &codeSentinel{CodeNotFound}
	ErrExists          error = &codeSentinel{CodeExists}
	ErrInvalidKey      error = &codeSentinel{CodeInvalidKey}
	ErrInvalidArgument error = &codeSentinel{CodeInvalidArgument}
	ErrUnsupported     error = &codeSentinel{CodeUnsupported}
	ErrReadOnly        error = &codeSentinel{CodeReadOnly}
	ErrTooLarge        error = &codeSentinel{CodeTooLarge}
	ErrRejected        error = &codeSentinel{CodeRejected}
	ErrQuotaExceeded   error = &codeSentinel{CodeQuotaExceeded}
	ErrPermission      error = &codeSentinel{CodePermission}
	ErrUnavailable     error = &codeSentinel{CodeUnavailable}
	ErrConflict        error = &codeSentinel{CodeConflict}
	ErrNotModified     error = &codeSentinel{CodeNotModified}
	ErrUnknown         error = &codeSentinel{CodeUnknown}
)

// Error is the one error type the library returns. Drivers create it with
// NewError; the Disk fills in Op, Disk and Key before returning it, so a log
// line always says which operation on which disk and key failed.
//
// Err is the underlying cause and may be a native SDK error: errors.As
// reaches it through Unwrap, which is the escape hatch for vendor-specific
// error handling.
type Error struct {
	Code Code
	Op   OpKind
	Disk string
	Key  string
	Err  error
	// RetryAfter is how long the backend asked the caller to wait before
	// trying again (an HTTP Retry-After header on a 429 or 503, or the
	// vendor's equivalent); 0 means it did not say. A driver sets it on a
	// CodeUnavailable error with WithRetryAfter. layer.Retry waits at least
	// this long before the next attempt, and fshttp and upload pass it on to
	// their clients as a Retry-After header.
	RetryAfter time.Duration
}

// WithRetryAfter records the backend's requested wait on e and returns e,
// so a driver can write
//
//	return filesystem.NewError(filesystem.CodeUnavailable, err).WithRetryAfter(d)
//
// A non-positive d leaves e unchanged.
func (e *Error) WithRetryAfter(d time.Duration) *Error {
	if d > 0 {
		e.RetryAfter = d
	}
	return e
}

// RetryAfterOf returns the wait the backend asked for, from the outermost
// *Error in err's chain that carries one, and false when none does.
func RetryAfterOf(err error) (time.Duration, bool) {
	for err != nil {
		var e *Error
		if !errors.As(err, &e) {
			return 0, false
		}
		if e.RetryAfter > 0 {
			return e.RetryAfter, true
		}
		err = e.Err
	}
	return 0, false
}

// NewError is how a driver reports a classified failure, keeping the native
// cause (which may be nil) reachable through errors.As.
func NewError(code Code, cause error) *Error {
	return &Error{Code: code, Err: cause}
}

// Errorf builds a classified error from a format string, for failures that
// have no underlying cause (validation in the Disk or a driver).
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// Error renders "filesystem: <op> <disk>:<key>: <code>: <cause>", omitting
// the parts that are empty.
func (e *Error) Error() string {
	var where string
	switch {
	case e.Disk != "" && e.Key != "":
		where = e.Disk + ":" + e.Key
	case e.Disk != "":
		where = e.Disk
	default:
		where = e.Key
	}
	var b strings.Builder
	b.WriteString("filesystem")
	if context := strings.TrimSpace(string(e.Op) + " " + where); context != "" {
		b.WriteString(": ")
		b.WriteString(context)
	}
	b.WriteString(": ")
	b.WriteString(string(e.Code))
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }

// Is makes errors.Is(err, ErrNotFound) true for every *Error whose Code is
// CodeNotFound, and likewise for each sentinel.
func (e *Error) Is(target error) bool {
	s, ok := target.(*codeSentinel)
	return ok && s.code == e.Code
}

// CodeOf returns the Code of err, classified exactly as the Disk classifies
// a driver's error: the Code of the outermost *Error in its chain (an
// unknown one becomes CodeUnavailable when the chain holds a context
// error), the code of a bare sentinel, CodeNotFound, CodeExists or
// CodePermission for the io/fs sentinels, CodeUnavailable for a context
// that ended, or CodeUnknown for any other non-nil error. It returns "" for
// a nil error.
//
// Interceptors see a driver's error before the Disk annotates it, so this
// is what lets layer.Retry recognise a bare context.DeadlineExceeded from
// an attempt that layer.Timeout cut short.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	return classify(err).Code
}

// classify turns any error a driver returned into an *Error. Errors that are
// already classified keep their Code, except that CodeUnknown over a context
// error becomes CodeUnavailable (an SDK that wraps the context's error is
// still reporting a deadline, not an unclassifiable failure); the io/fs
// sentinels that local-disk style drivers return map to their obvious codes;
// a context that ended is CodeUnavailable; anything else is CodeUnknown with
// the original error kept as the cause.
func classify(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		cp := *e
		if cp.Code == CodeUnknown && contextEnded(err) {
			cp.Code = CodeUnavailable
		}
		return &cp
	}
	var s *codeSentinel
	if errors.As(err, &s) {
		return &Error{Code: s.code, Err: err}
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &Error{Code: CodeNotFound, Err: err}
	case errors.Is(err, fs.ErrExist):
		return &Error{Code: CodeExists, Err: err}
	case errors.Is(err, fs.ErrPermission):
		return &Error{Code: CodePermission, Err: err}
	case contextEnded(err):
		return &Error{Code: CodeUnavailable, Err: err}
	}
	return &Error{Code: CodeUnknown, Err: err}
}

// contextEnded reports whether err comes from a context that was cancelled
// or passed its deadline.
func contextEnded(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// prefixed classifies err and puts prefix in front of its cause, keeping
// the Code and the cause reachable through errors.Is and errors.As. Mux and
// OpenManager use it to say which disk and variable an error is about.
func prefixed(err error, prefix string) *Error {
	e := classify(err)
	if e.Err == nil {
		e.Err = errors.New(prefix)
	} else {
		e.Err = fmt.Errorf("%s: %w", prefix, e.Err)
	}
	return e
}

// annotate classifies err and stamps the operation, disk and key on it,
// keeping any of those a driver already set. It returns nil for nil.
func annotate(err error, op OpKind, disk, key string) error {
	if err == nil {
		return nil
	}
	// A batch keeps its shape: classify would find the first per-key
	// *Error inside it and return that alone. Each failure is annotated
	// with its own key instead.
	var batch *BatchError
	if errors.As(err, &batch) {
		out := &BatchError{Failures: make([]KeyError, len(batch.Failures))}
		for i, f := range batch.Failures {
			out.Failures[i] = KeyError{Key: f.Key, Err: annotate(f.Err, op, disk, f.Key)}
		}
		return out
	}
	e := classify(err)
	if e.Op == "" {
		e.Op = op
	}
	if e.Disk == "" {
		e.Disk = disk
	}
	if e.Key == "" {
		e.Key = key
	}
	return e
}

// KeyError is one key a batch operation failed on.
type KeyError struct {
	Key string
	Err error
}

// BatchError reports the keys a batch operation (Disk.DeleteKeys) could
// not process; every other key succeeded. errors.Is and CodeOf see the
// per-key errors, so errors.Is(err, ErrPermission) asks whether any key
// failed for lack of permission.
type BatchError struct {
	Failures []KeyError
}

// Error names the failure count and the first failure. A BatchError with
// no failures is a caller's bug (a driver built one by hand), but Error
// must not panic inside a log line, so it says so instead.
func (e *BatchError) Error() string {
	if len(e.Failures) == 0 {
		return "filesystem: batch error with no failures"
	}
	first := e.Failures[0]
	return fmt.Sprintf("filesystem: %d keys failed; %s: %v", len(e.Failures), first.Key, first.Err)
}

// Unwrap returns the per-key errors.
func (e *BatchError) Unwrap() []error {
	out := make([]error, len(e.Failures))
	for i, f := range e.Failures {
		out[i] = f.Err
	}
	return out
}
