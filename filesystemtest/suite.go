// Package filesystemtest holds the conformance suite every driver must pass
// and a Fake disk with assertions for application tests.
//
// The suite runs against a *filesystem.Disk rather than a bare driver, so it
// checks the behaviour applications actually see: the driver's contract and
// the Disk's portable logic together. Cases that need a capability are
// skipped when the disk does not declare it, and fail when it declares one
// it cannot deliver.
//
// Each case is a function returning an error that describes the violation,
// which keeps the suite testable itself: its own tests feed it deliberately
// broken drivers and require every case to notice.
package filesystemtest

import (
	"context"
	"testing"

	"github.com/ubgo/filesystem"
)

// Case is one conformance check.
type Case struct {
	// Name is the sub-test name.
	Name string
	// Needs reports whether the case applies to a disk with these
	// capabilities. Nil means it always applies.
	Needs func(filesystem.Capabilities) bool
	// Check runs the case against an empty disk and returns a description
	// of the first violation, or nil.
	Check func(ctx context.Context, d *filesystem.Disk) error
}

// Factory returns a new, empty disk for one case. It is called once per case
// so cases cannot see each other's objects; use t.Cleanup for teardown.
type Factory func(t *testing.T) *filesystem.Disk

// Run runs every case in Cases as a sub-test of t.
//
// Point it at a backend nothing else uses while it runs (a dedicated
// bucket, container or directory): the SweepAbandoned cases sweep the
// whole backend, as the capability does, so they abort other clients'
// multipart uploads in progress and remove their staging files.
func Run(t *testing.T, newDisk Factory) {
	t.Helper()
	RunWith(t, newDisk, Options{})
}

// DefaultStorageClass is the class the StorageClass case moves an object
// to when Options.StorageClass is empty: S3's infrequent-access class,
// which S3-compatible servers and the memory driver accept. Backends with
// other class names (GCS "NEARLINE", Azure "Cool") pass theirs.
const DefaultStorageClass = "STANDARD_IA"

// Options carry the backend-specific values a few cases need. The zero
// value suits S3 and the memory driver.
type Options struct {
	// StorageClass is a class the backend accepts for SetStorageClass and
	// reports back from Stat; "" means DefaultStorageClass.
	StorageClass string
}

// optionsKey carries Options to the cases through their context.
type optionsKey struct{}

// optionsFrom returns the Options RunWith was given.
func optionsFrom(ctx context.Context) Options {
	o, _ := ctx.Value(optionsKey{}).(Options)
	if o.StorageClass == "" {
		o.StorageClass = DefaultStorageClass
	}
	return o
}

// RunWith is Run with backend-specific Options.
func RunWith(t *testing.T, newDisk Factory, opts Options) {
	t.Helper()
	for _, c := range Cases() {
		t.Run(c.Name, func(t *testing.T) {
			d := newDisk(t)
			if c.Needs != nil && !c.Needs(d.Capabilities()) {
				t.Skipf("disk %s lacks the capability this case needs", d.Name())
			}
			if err := c.Check(context.WithValue(t.Context(), optionsKey{}, opts), d); err != nil {
				t.Error(err)
			}
		})
	}
}
