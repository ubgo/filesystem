package versions_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/versions"
)

// policyEnv is newEnv with any plugin options and a clock the test moves
// by hand (newEnv's advances on every read).
func policyEnv(t *testing.T, now *time.Time, opts ...versions.Option) *env {
	t.Helper()
	e := &env{}
	e.db, e.faults = testdb.OpenFaulty(t)
	e.disk, _ = filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(e.disk)
	var err error
	e.p, err = versions.New(append(opts, versions.WithClock(func() time.Time { return *now }))...)
	if err != nil {
		t.Fatal(err)
	}
	if e.store, err = records.New(e.db, testdb.Dialect(), mgr, records.WithPlugins(e.p)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

var errPlan = errors.New("plan service down")

// TestKeepFuncCount: a KeepFunc's count is used per file, overriding
// WithKeep, and the request names the file.
func TestKeepFuncCount(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var asked []string
	e := policyEnv(t, &now, versions.WithKeep(5), versions.WithKeepFunc(func(_ context.Context, req versions.KeepRequest) (versions.KeepPolicy, error) {
		asked = append(asked, req.File.ID)
		return versions.KeepPolicy{Count: 1}, nil
	}))
	f := e.upload(t, "one")
	e.replace(t, f.ID, "two")
	e.replace(t, f.ID, "three")
	if got := e.history(t, f.ID); got != "2:two" {
		t.Fatalf("history = %s, want only the newest (count 1)", got)
	}
	if len(asked) != 2 || asked[0] != f.ID {
		t.Fatalf("KeepFunc asked for %v", asked)
	}
}

// TestKeepFuncMaxAge: versions older than MaxAge are dropped at the next
// replacement, and by Trim without one.
func TestKeepFuncMaxAge(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	e := policyEnv(t, &now, versions.WithKeepFunc(func(context.Context, versions.KeepRequest) (versions.KeepPolicy, error) {
		return versions.KeepPolicy{Count: 10, MaxAge: time.Hour}, nil
	}))
	f := e.upload(t, "one")
	e.replace(t, f.ID, "two") // version 1 at 12:00
	now = now.Add(30 * time.Minute)
	e.replace(t, f.ID, "three") // version 2 at 12:30
	now = now.Add(45 * time.Minute)
	e.replace(t, f.ID, "four") // 13:15: version 1 is 75 minutes old
	if got := e.history(t, f.ID); got != "3:three,2:two" {
		t.Fatalf("history = %s, want version 1 aged out", got)
	}
	now = now.Add(50 * time.Minute) // 14:05: version 2 (12:30) is past an hour, 3 (13:15) is not
	n, err := e.p.Trim(ctx, e.store, scope, f.ID)
	if err != nil || n != 1 {
		t.Fatalf("Trim = %d, %v", n, err)
	}
	if got := e.history(t, f.ID); got != "3:three" {
		t.Fatalf("history after Trim = %s", got)
	}
	if n := e.objects(t); n != 2 {
		t.Fatalf("%d objects, want the current one and version 3", n)
	}
}

// TestKeepFuncFailsClosed: an error from the KeepFunc, or a policy that
// keeps nothing, refuses the replacement and changes nothing.
func TestKeepFuncFailsClosed(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	answer := func(context.Context, versions.KeepRequest) (versions.KeepPolicy, error) {
		return versions.KeepPolicy{Count: 3}, nil
	}
	e := policyEnv(t, &now, versions.WithKeepFunc(func(ctx context.Context, req versions.KeepRequest) (versions.KeepPolicy, error) {
		return answer(ctx, req)
	}))
	f := e.upload(t, "one")
	e.replace(t, f.ID, "two")

	for name, fn := range map[string]versions.KeepFunc{
		"error": func(context.Context, versions.KeepRequest) (versions.KeepPolicy, error) {
			return versions.KeepPolicy{Count: 3}, errPlan
		},
		"count 0": func(context.Context, versions.KeepRequest) (versions.KeepPolicy, error) {
			return versions.KeepPolicy{}, nil
		},
		"negative": func(context.Context, versions.KeepRequest) (versions.KeepPolicy, error) {
			return versions.KeepPolicy{Count: 1, MaxAge: -1}, nil
		},
	} {
		answer = fn
		if _, err := e.store.Replace(ctx, scope, f.ID, strings.NewReader("three"), "", anything); err == nil {
			t.Fatalf("%s: the replacement went through", name)
		}
		if _, err := e.p.Trim(ctx, e.store, scope, f.ID); err == nil {
			t.Fatalf("%s: Trim went through", name)
		}
		if got := e.content(t, f.ID) + "|" + e.history(t, f.ID); got != "two|1:one" {
			t.Fatalf("%s: content|history = %s", name, got)
		}
	}
}

// TestDefaultKeep: without WithKeep or WithKeepFunc a file keeps
// DefaultKeep versions, and Trim under that policy drops nothing new.
func TestDefaultKeep(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	e := policyEnv(t, &now)
	f := e.upload(t, "v0")
	for i := range versions.DefaultKeep + 2 {
		now = now.Add(24 * time.Hour)
		e.replace(t, f.ID, strings.Repeat("x", i+1))
	}
	list, err := e.p.Versions(t.Context(), e.store, scope, f.ID)
	if err != nil || len(list) != versions.DefaultKeep || list[len(list)-1].Number != 3 {
		t.Fatalf("%d versions, oldest %+v, %v", len(list), list[len(list)-1], err)
	}
	if n, err := e.p.Trim(t.Context(), e.store, scope, f.ID); err != nil || n != 0 {
		t.Fatalf("Trim = %d, %v", n, err)
	}
}

// TestRestoreKey: a custom key is where the restored copy goes, and a key
// that is unusable refuses the restore before anything is written.
func TestRestoreKey(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	name := func(context.Context, versions.RestoreKeyRequest) (string, error) { return "tenants/t/restored-1", nil }
	var got versions.RestoreKeyRequest
	e := policyEnv(t, &now, versions.WithRestoreKey(func(ctx context.Context, req versions.RestoreKeyRequest) (string, error) {
		got = req
		return name(ctx, req)
	}))
	f := e.upload(t, "one")
	e.replace(t, f.ID, "two")
	list, _ := e.p.Versions(ctx, e.store, scope, f.ID)
	v := list[0]

	restored, err := e.p.Restore(ctx, e.store, scope, v.ID)
	if err != nil || restored.Key != "tenants/t/restored-1" || got.File.ID != f.ID || got.Version.ID != v.ID {
		t.Fatalf("Restore = %+v, %v; asked %+v", restored, err, got)
	}
	if e.content(t, f.ID) != "one" {
		t.Fatalf("content = %q", e.content(t, f.ID))
	}

	list, _ = e.p.Versions(ctx, e.store, scope, f.ID)
	v = list[0] // "two"
	cur, _ := e.store.File(ctx, scope, f.ID)
	objects := e.objects(t)
	for label, c := range map[string]struct {
		fn   versions.RestoreKeyFunc
		code filesystem.Code
	}{
		"error":      {func(context.Context, versions.RestoreKeyRequest) (string, error) { return "x", errPlan }, filesystem.CodeUnknown},
		"empty":      {func(context.Context, versions.RestoreKeyRequest) (string, error) { return "", nil }, filesystem.CodeInvalidArgument},
		"file's own": {func(context.Context, versions.RestoreKeyRequest) (string, error) { return cur.Key, nil }, filesystem.CodeInvalidArgument},
		"version's":  {func(context.Context, versions.RestoreKeyRequest) (string, error) { return v.Key, nil }, filesystem.CodeInvalidArgument},
		"taken":      {func(context.Context, versions.RestoreKeyRequest) (string, error) { return list[1].Key, nil }, filesystem.CodeExists},
		"traversal":  {func(context.Context, versions.RestoreKeyRequest) (string, error) { return "../escape", nil }, filesystem.CodeInvalidKey},
	} {
		name = c.fn
		_, err := e.p.Restore(ctx, e.store, scope, v.ID)
		if err == nil || (c.code != filesystem.CodeUnknown && filesystem.CodeOf(err) != c.code) || (c.code == filesystem.CodeUnknown && !errors.Is(err, errPlan)) {
			t.Fatalf("%s: Restore = %v, want %s", label, err, c.code)
		}
		if e.content(t, f.ID) != "one" || e.objects(t) != objects {
			t.Fatalf("%s: a refused restore changed something", label)
		}
	}
}

// TestRestoreKeyDefault: without WithRestoreKey a restored copy goes
// under "versions/" with a random name, as before.
func TestRestoreKeyDefault(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	e := policyEnv(t, &now)
	f := e.upload(t, "one")
	e.replace(t, f.ID, "two")
	list, _ := e.p.Versions(ctx, e.store, scope, f.ID)
	restored, err := e.p.Restore(ctx, e.store, scope, list[0].ID)
	if err != nil || !strings.HasPrefix(restored.Key, "versions/") || len(restored.Key) < len("versions/")+20 {
		t.Fatalf("Restore = %q, %v", restored.Key, err)
	}
}

// TestRestoreBesideFile: the ready RestoreKeyFunc keeps the copy in the
// file's own directory.
func TestRestoreBesideFile(t *testing.T) {
	for key, prefix := range map[string]string{
		"tenants/t1/docs/a.pdf": "tenants/t1/docs/versions/",
		"a.pdf":                 "versions/",
	} {
		got, err := versions.RestoreBesideFile(t.Context(), versions.RestoreKeyRequest{File: records.File{Key: key}})
		if err != nil || !strings.HasPrefix(got, prefix) || len(got) < len(prefix)+20 {
			t.Fatalf("RestoreBesideFile(%q) = %q, %v", key, got, err)
		}
		again, _ := versions.RestoreBesideFile(t.Context(), versions.RestoreKeyRequest{File: records.File{Key: key}})
		if again == got {
			t.Fatal("two restores got the same key")
		}
	}
}
