package filesystem_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
)

func TestErrorFormatting(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		err  *filesystem.Error
		want string
	}{
		{&filesystem.Error{Code: filesystem.CodeNotFound, Op: filesystem.OpRead, Disk: "d", Key: "k", Err: cause}, "filesystem: read d:k: not_found: boom"},
		{&filesystem.Error{Code: filesystem.CodeUnknown, Disk: "d"}, "filesystem: d: unknown"},
		{&filesystem.Error{Code: filesystem.CodeInvalidKey, Key: "k"}, "filesystem: k: invalid_key"},
		{filesystem.NewError(filesystem.CodePermission, nil), "filesystem: permission"},
	}
	for _, c := range cases {
		if got := c.err.Error(); got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
}

// nativeErr stands in for an SDK error type reached through errors.As.
type nativeErr struct{ status int }

func (n *nativeErr) Error() string { return fmt.Sprintf("status %d", n.status) }

func TestErrorMatching(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &filesystem.Error{Code: filesystem.CodeNotFound, Err: &nativeErr{404}})
	if !errors.Is(err, filesystem.ErrNotFound) || errors.Is(err, filesystem.ErrExists) {
		t.Fatal("errors.Is does not match by code")
	}
	var n *nativeErr
	if !errors.As(err, &n) || n.status != 404 {
		t.Fatal("native cause not reachable through errors.As")
	}
	for _, c := range []struct {
		err  error
		want filesystem.Code
	}{
		{nil, ""},
		{err, filesystem.CodeNotFound},
		{filesystem.ErrReadOnly, filesystem.CodeReadOnly},
		{fmt.Errorf("x: %w", filesystem.ErrUnavailable), filesystem.CodeUnavailable},
		{errors.New("plain"), filesystem.CodeUnknown},
		{fs.ErrNotExist, filesystem.CodeNotFound},
		{fmt.Errorf("stat: %w", context.DeadlineExceeded), filesystem.CodeUnavailable},
		{context.Canceled, filesystem.CodeUnavailable},
		{filesystem.NewError(filesystem.CodeUnknown, fmt.Errorf("sdk: %w", context.DeadlineExceeded)), filesystem.CodeUnavailable},
		{filesystem.NewError(filesystem.CodeNotFound, context.Canceled), filesystem.CodeNotFound},
	} {
		if got := filesystem.CodeOf(c.err); got != c.want {
			t.Errorf("CodeOf(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	sentinels := []error{filesystem.ErrNotFound, filesystem.ErrExists, filesystem.ErrInvalidKey, filesystem.ErrInvalidArgument, filesystem.ErrUnsupported,
		filesystem.ErrReadOnly, filesystem.ErrTooLarge, filesystem.ErrRejected, filesystem.ErrQuotaExceeded, filesystem.ErrPermission, filesystem.ErrUnavailable,
		filesystem.ErrConflict, filesystem.ErrNotModified, filesystem.ErrUnknown}
	if len(sentinels) != len(filesystem.CodeValues) {
		t.Fatalf("%d sentinels for %d codes", len(sentinels), len(filesystem.CodeValues))
	}
	for i, s := range sentinels {
		if filesystem.CodeOf(s) != filesystem.CodeValues[i] || s.Error() != "filesystem: "+string(filesystem.CodeValues[i]) {
			t.Errorf("sentinel %d (%v) does not match code %q", i, s, filesystem.CodeValues[i])
		}
	}
}

func TestErrorsFromDriversAreClassified(t *testing.T) {
	cases := []struct {
		driverErr error
		want      filesystem.Code
	}{
		{fs.ErrNotExist, filesystem.CodeNotFound},
		{fs.ErrExist, filesystem.CodeExists},
		{fs.ErrPermission, filesystem.CodePermission},
		{filesystem.ErrUnavailable, filesystem.CodeUnavailable},
		{context.DeadlineExceeded, filesystem.CodeUnavailable},
		{filesystem.NewError(filesystem.CodeUnknown, context.Canceled), filesystem.CodeUnavailable},
		{errors.New("mystery"), filesystem.CodeUnknown},
	}
	for _, c := range cases {
		d := mustDisk(t, "d", &erroringDriver{basicDriver: newBasic(), err: c.driverErr})
		_, err := d.Stat(t.Context(), "some/key")
		var fe *filesystem.Error
		if !errors.As(err, &fe) || fe.Code != c.want || fe.Op != filesystem.OpStat || fe.Disk != "d" || fe.Key != "some/key" {
			t.Errorf("driver error %v surfaced as %#v, want code %q with op, disk and key", c.driverErr, fe, c.want)
		}
	}
	// A driver that already annotated keeps its own fields.
	pre := &filesystem.Error{Code: filesystem.CodeNotFound, Op: filesystem.OpRead, Disk: "inner", Key: "inner/key"}
	d := mustDisk(t, "outer", &erroringDriver{basicDriver: newBasic(), err: pre})
	_, err := d.Stat(t.Context(), "k")
	var fe *filesystem.Error
	if !errors.As(err, &fe) || fe.Disk != "inner" || fe.Key != "inner/key" || fe.Op != filesystem.OpRead {
		t.Errorf("pre-annotated error rewritten: %#v", fe)
	}
	if pre.Disk != "inner" || pre == fe {
		t.Error("annotate mutated or reused the driver's error value")
	}
}

// erroringDriver fails Stat with a fixed error.
type erroringDriver struct {
	*basicDriver
	err error
}

func (e *erroringDriver) Stat(context.Context, string) (filesystem.Entry, error) {
	return filesystem.Entry{}, e.err
}

func TestRetryAfter(t *testing.T) {
	if _, ok := filesystem.RetryAfterOf(nil); ok {
		t.Error("nil carries a Retry-After")
	}
	if _, ok := filesystem.RetryAfterOf(errors.New("x")); ok {
		t.Error("a plain error carries a Retry-After")
	}
	e := filesystem.NewError(filesystem.CodeUnavailable, nil).WithRetryAfter(-time.Second)
	if e.RetryAfter != 0 {
		t.Errorf("negative wait recorded: %v", e.RetryAfter)
	}
	// Found under an outer *Error that has none, and kept by the Disk's
	// annotation of a driver error.
	inner := filesystem.NewError(filesystem.CodeUnavailable, nil).WithRetryAfter(2 * time.Second)
	outer := fmt.Errorf("wrapped: %w", filesystem.NewError(filesystem.CodeUnavailable, inner))
	if d, ok := filesystem.RetryAfterOf(outer); !ok || d != 2*time.Second {
		t.Errorf("RetryAfterOf = %v %v", d, ok)
	}
	disk := mustDisk(t, "d", &erroringDriver{basicDriver: newBasic(), err: inner})
	_, err := disk.Stat(t.Context(), "k")
	if d, ok := filesystem.RetryAfterOf(err); !ok || d != 2*time.Second {
		t.Errorf("after the Disk: %v %v (%v)", d, ok, err)
	}
}

// A hand-built BatchError with no failures must still format: Error runs
// inside log lines, where a panic would take down the caller.
func TestBatchErrorWithNoFailuresFormats(t *testing.T) {
	empty := &filesystem.BatchError{}
	if got := empty.Error(); got != "filesystem: batch error with no failures" {
		t.Fatalf("Error() = %q", got)
	}
	one := &filesystem.BatchError{Failures: []filesystem.KeyError{{Key: "a", Err: errors.New("boom")}}}
	if got := one.Error(); got != "filesystem: 1 keys failed; a: boom" {
		t.Fatalf("Error() = %q", got)
	}
}
