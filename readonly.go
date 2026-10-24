package filesystem

import "context"

// ReadOnly is an interceptor that refuses every mutating operation (see
// Op.Mutating) with CodeReadOnly before it reaches the driver. Signed GET
// URLs and public URLs still work; signed PUT URLs do not, because they
// authorise a write.
//
// It lives in the core rather than the layer package because configuration
// (Mux's readonly parameter) needs it and the core cannot import layer.
func ReadOnly() Interceptor {
	return func(ctx context.Context, op *Op, call Call) error {
		if op.Mutating {
			return Errorf(CodeReadOnly, "disk %s is read-only", op.Disk)
		}
		return call(ctx)
	}
}
