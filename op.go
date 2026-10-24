package filesystem

import (
	"context"
	"io"
	"sync"
)

// OpKind names a Disk operation. Interceptors switch on it, errors carry it,
// and metrics label by it.
type OpKind string

// The closed set of operations. Every Disk method that reaches a driver runs
// as exactly one of these (a portable fallback such as Copy without server
// side copy runs as the operations it is made of).
const (
	OpStat               OpKind = "stat"
	OpRead               OpKind = "read"
	OpWrite              OpKind = "write"
	OpDelete             OpKind = "delete"
	OpDeletePrefix       OpKind = "delete_prefix"
	OpDeleteKeys         OpKind = "delete_keys"
	OpList               OpKind = "list"
	OpCopy               OpKind = "copy"
	OpMove               OpKind = "move"
	OpSetVisibility      OpKind = "set_visibility"
	OpPresign            OpKind = "presign"
	OpPublicURL          OpKind = "public_url"
	OpMultipartCreate    OpKind = "multipart_create"
	OpMultipartUpload    OpKind = "multipart_upload_part"
	OpMultipartPresign   OpKind = "multipart_presign_part"
	OpMultipartListParts OpKind = "multipart_list_parts"
	OpMultipartComplete  OpKind = "multipart_complete"
	OpMultipartAbort     OpKind = "multipart_abort"
	OpSweepAbandoned     OpKind = "sweep_abandoned"
	OpFindAbandoned      OpKind = "find_abandoned"
	OpMakeDir            OpKind = "make_dir"
	OpDirExists          OpKind = "dir_exists"
	OpPresignPost        OpKind = "presign_post"
	OpAppend             OpKind = "append"
	OpSetModTime         OpKind = "set_mod_time"
	OpTags               OpKind = "tags"
	OpSetTags            OpKind = "set_tags"
	OpHold               OpKind = "hold"
	OpSetHold            OpKind = "set_hold"
	OpVersions           OpKind = "versions"
	OpRestore            OpKind = "restore"
	OpSetStorageClass    OpKind = "set_storage_class"
	OpReadVersion        OpKind = "read_version"
	OpRestoreVersion     OpKind = "restore_version"
)

// OpKindValues lists every OpKind.
var OpKindValues = []OpKind{
	OpStat, OpRead, OpWrite, OpDelete, OpDeletePrefix, OpDeleteKeys, OpList, OpCopy, OpMove,
	OpSetVisibility, OpPresign, OpPublicURL, OpMultipartCreate, OpMultipartUpload,
	OpMultipartPresign, OpMultipartListParts, OpMultipartComplete, OpMultipartAbort,
	OpSweepAbandoned, OpPresignPost, OpAppend, OpSetModTime, OpTags, OpSetTags,
	OpHold, OpSetHold, OpVersions, OpRestore, OpSetStorageClass, OpReadVersion, OpRestoreVersion,
	OpFindAbandoned, OpMakeDir, OpDirExists,
}

// Mutates reports whether the operation changes stored data or grants a
// write. A presign is a mutation only when it authorises a PUT, so the Disk
// marks that on the Op (see Op.Mutating) rather than here; this method gives
// the answer for the kind alone.
func (k OpKind) Mutates() bool {
	switch k {
	case OpWrite, OpDelete, OpDeletePrefix, OpDeleteKeys, OpCopy, OpMove, OpSetVisibility,
		OpMultipartCreate, OpMultipartUpload, OpMultipartPresign, OpMultipartComplete, OpMultipartAbort,
		OpSweepAbandoned, OpPresignPost, OpAppend, OpSetModTime, OpSetTags, OpSetHold, OpRestore,
		OpSetStorageClass, OpRestoreVersion, OpMakeDir:
		return true
	}
	return false
}

// Op describes one operation as it passes through the interceptor chain.
// Interceptors read it; the Disk owns it and updates the result fields.
type Op struct {
	// Kind is the operation.
	Kind OpKind
	// Disk is the name of the Disk running it.
	Disk string
	// Key is the object key, the directory for List, DeletePrefix,
	// MakeDir and DirExists, or empty for DeleteKeys and the abandoned
	// sweeps.
	Key string
	// Keys are the keys of a DeleteKeys, else nil.
	Keys []string
	// Target is the destination key of a Copy or Move, else empty.
	Target string
	// Mutating is true when the operation changes data or authorises a
	// change: Kind.Mutates(), plus a presign for MethodPut. Read-only
	// enforcement keys off this.
	Mutating bool
	// Replayable is true when the call may safely run more than once. False
	// for a write whose body cannot be rewound, so a retry interceptor must
	// not retry it.
	Replayable bool
	// Streaming is true when the call returns a stream that outlives it (a
	// Read): cancelling the context after the call returns would break the
	// stream, so timeout interceptors leave such calls alone.
	Streaming bool
	// Attempt counts how many times the call has started, from 1.
	Attempt int
	// Bytes is set after a successful Write to the number of bytes stored.
	Bytes int64
	// Hedgeable is true when the call may run concurrently with itself
	// (Stat, Read). An interceptor may call call again before an earlier
	// call returned only on a hedgeable operation, after setting
	// Concurrent, and must wait for every call it started before it
	// returns.
	Hedgeable bool
	// Concurrent is set by an interceptor (layer.Hedge) before it starts
	// calls of a hedgeable operation that may overlap. The Disk then keeps
	// the result of the first call to succeed, and a later call that also
	// succeeds discards its own result (closing its reader) and returns
	// ErrSuperseded, so the call that returned nil is always the one whose
	// result the Disk returns. Without it, calls are taken to run one
	// after another, and each success replaces the previous result,
	// closing its reader.
	Concurrent bool

	// bodies are the wrappers WrapBody registered, applied in order.
	bodies []func(io.Reader) io.Reader
	// closers run when the reader the operation returns is closed.
	closers []func()
}

// ErrSuperseded is what a concurrent call of a hedgeable operation (see
// Op.Concurrent) returns when another call already delivered the result:
// the operation succeeded, through the other call. A hedging interceptor
// waits for the other call's answer instead of taking this one. Should it
// reach a caller anyway, its code is CodeConflict.
var ErrSuperseded = Errorf(CodeConflict, "a concurrent attempt delivered the result")

// OnClose registers fn to run once when the caller closes the reader the
// operation returns (a Read), so an interceptor can release what the
// stream needed until then, such as an attempt's context. If the
// operation fails, or returns no reader, fn runs when it returns. Call it
// before returning from the interceptor, never from a concurrent attempt.
func (op *Op) OnClose(fn func()) {
	op.closers = append(op.closers, fn)
}

// closed runs the OnClose functions.
func (op *Op) closed() {
	for _, fn := range op.closers {
		fn()
	}
}

// WrapBody asks the Disk to pass the operation's byte stream through fn:
// the body of a Write, Append or UploadPart before the driver reads it, and
// the reader a Read returns before the caller reads it (its Close still
// closes the driver's reader). It is how an interceptor reaches bytes it
// otherwise never sees, to throttle or count them. Operations without a
// byte stream ignore it. fn must return a reader that yields exactly the
// bytes it was given; call WrapBody before calling call, because a body is
// opened when an attempt starts. An interceptor placed inside Retry runs,
// and registers fn, once per attempt, so fn should return a reader it
// already wrapped unchanged; one placed after a hedging interceptor must
// not call WrapBody at all, as hedged attempts run concurrently.
func (op *Op) WrapBody(fn func(io.Reader) io.Reader) {
	op.bodies = append(op.bodies, fn)
}

// wrapBody applies the registered wrappers to r.
func (op *Op) wrapBody(r io.Reader) io.Reader {
	for _, fn := range op.bodies {
		r = fn(r)
	}
	return r
}

// wrapReadCloser applies the registered wrappers to a reader the caller
// will close, keeping rc's Close.
func (op *Op) wrapReadCloser(rc io.ReadCloser) io.ReadCloser {
	if len(op.bodies) == 0 && len(op.closers) == 0 {
		return rc
	}
	return &wrappedReadCloser{Reader: op.wrapBody(rc), rc: rc, onClose: op.closed}
}

// wrappedReadCloser reads through a wrapper, closes the original, and
// then runs the operation's OnClose functions, once.
type wrappedReadCloser struct {
	io.Reader
	rc      io.ReadCloser
	onClose func()
	once    sync.Once
}

func (w *wrappedReadCloser) Close() error {
	err := w.rc.Close()
	w.once.Do(w.onClose)
	return err
}

// Call runs the operation (or the rest of the chain). Each invocation is one
// attempt.
type Call func(ctx context.Context) error

// Interceptor runs around an operation. It must call call to proceed (zero,
// one or several times) and return its error or its own. It may inspect op
// before and after the call, but must not change Kind, Disk, Key or Target.
type Interceptor func(ctx context.Context, op *Op, call Call) error

// chain composes interceptors so that the first one given is the outermost.
func chain(interceptors []Interceptor) Interceptor {
	if len(interceptors) == 0 {
		return func(ctx context.Context, _ *Op, call Call) error { return call(ctx) }
	}
	return func(ctx context.Context, op *Op, call Call) error {
		return runFrom(ctx, op, interceptors, 0, call)
	}
}

func runFrom(ctx context.Context, op *Op, interceptors []Interceptor, i int, last Call) error {
	if i == len(interceptors) {
		return last(ctx)
	}
	return interceptors[i](ctx, op, func(ctx context.Context) error {
		return runFrom(ctx, op, interceptors, i+1, last)
	})
}
