package records_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/records"
)

// usage logs holds and releases into a shared log, fails when fail is set,
// and reports held bytes.
type usage struct {
	name string
	log  *[]string
	fail error
	held int64
}

func (u *usage) Name() string { return u.name }

func (u *usage) OnHold(_ context.Context, _ *records.Store, o records.HeldObject) error {
	*u.log = append(*u.log, u.name+" hold "+o.Key)
	return u.fail
}

func (u *usage) OnRelease(_ context.Context, _ *records.Store, o records.HeldObject) error {
	*u.log = append(*u.log, u.name+" release "+o.Key)
	return u.fail
}

func (u *usage) HeldBytes(context.Context, *records.Store, string) (int64, error) {
	return u.held, u.fail
}

func TestHeldObjects(t *testing.T) {
	ctx := t.Context()
	var log []string
	a, b := &usage{name: "a", log: &log, held: 10}, &usage{name: "b", log: &log, held: 32}
	e := newEnv(t, nil, records.WithPlugins(a, named("plain"), b))
	o := records.HeldObject{Plugin: "versions", Scope: "t", FileID: "fil_1", Disk: "media", Key: "v1", Size: 5}

	if err := e.store.Hold(ctx, o); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Release(ctx, o); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, "; "); got != "a hold v1; b hold v1; a release v1; b release v1" {
		t.Fatalf("calls: %s", got)
	}
	if n, err := e.store.HeldBytes(ctx, "t"); err != nil || n != 42 {
		t.Fatalf("HeldBytes = %d, %v", n, err)
	}

	// The first error wins and stops the rest.
	log = nil
	a.fail = errors.New("quota exceeded")
	if err := e.store.Hold(ctx, o); !errors.Is(err, a.fail) {
		t.Fatalf("Hold = %v", err)
	}
	if err := e.store.Release(ctx, o); !errors.Is(err, a.fail) {
		t.Fatalf("Release = %v", err)
	}
	if got := strings.Join(log, "; "); got != "a hold v1; a release v1" {
		t.Fatalf("calls after a failure: %s", got)
	}
	if _, err := e.store.HeldBytes(ctx, "t"); !errors.Is(err, a.fail) {
		t.Fatalf("HeldBytes = %v", err)
	}

	// Without such plugins nothing is held.
	plain := newEnv(t, nil)
	if n, err := plain.store.HeldBytes(ctx, "t"); err != nil || n != 0 {
		t.Fatalf("HeldBytes without counters = %d, %v", n, err)
	}
	if err := plain.store.Hold(ctx, o); err != nil {
		t.Fatal(err)
	}
}
