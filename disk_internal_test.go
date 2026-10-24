package filesystem

import (
	"context"
	"io"
	"testing"
)

// overclaimingWrapper reports a capability whose interface it lacks, the
// mistake derive's verification exists to catch in a future wrapper.
type overclaimingWrapper struct{}

func (overclaimingWrapper) Stat(context.Context, string) (Entry, error) { return Entry{}, nil }
func (overclaimingWrapper) Read(context.Context, string, ByteRange) (io.ReadCloser, error) {
	return nil, ErrNotFound
}
func (overclaimingWrapper) Write(context.Context, string, io.Reader, WriteOptions) (Entry, error) {
	return Entry{}, nil
}
func (overclaimingWrapper) Delete(context.Context, string) error { return nil }
func (overclaimingWrapper) List(context.Context, string, ListOptions) (Page, error) {
	return Page{}, nil
}
func (overclaimingWrapper) Capabilities() Capabilities { return Capabilities{SweepAbandoned: true} }

func TestDeriveVerifiesTheWrapper(t *testing.T) {
	base, err := NewDisk("base", plainDriver{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.derive("wrapped", overclaimingWrapper{}, ""); CodeOf(err) != CodeInvalidArgument {
		t.Errorf("derive accepted a wrapper claiming a missing interface: %v", err)
	}
}

// plainDriver is overclaimingWrapper with no capabilities, a valid base.
type plainDriver struct{ overclaimingWrapper }

func (plainDriver) Capabilities() Capabilities { return Capabilities{} }
