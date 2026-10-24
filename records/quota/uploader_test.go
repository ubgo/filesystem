package quota_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/upload"
)

const (
	alice = "user:alice"
	bob   = "user:bob"
)

// seats gives alice 50 bytes and 2 files, bob no limit, and everyone
// else 10 bytes.
func seats(_ context.Context, _, uploader string) (quota.Limit, error) {
	switch uploader {
	case alice:
		return quota.Limit{Bytes: 50, Files: 2}, nil
	case bob:
		return quota.Limit{}, nil
	}
	return quota.Limit{Bytes: 10}, nil
}

// addBy stores size bytes under key and records them in scope as
// uploader's.
func (e *env) addBy(t *testing.T, scope, uploader, key string, size int) (records.File, error) {
	t.Helper()
	if _, err := e.disk.WriteBytes(t.Context(), key, []byte(strings.Repeat("x", size)), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	return e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: key, UploadedBy: uploader})
}

func uploaderUsage(t *testing.T, e *env, scope, uploader string) quota.Usage {
	t.Helper()
	u, err := e.plugin.UploaderUsage(t.Context(), e.store, scope, uploader)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

var uploaderPolicy = upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}

// TestUploaderLimits: each uploader is limited and counted on its own,
// beside the scope; a refusal by either counter changes neither.
func TestUploaderLimits(t *testing.T) {
	e, _ := newEnv(t, fixed(quota.Limit{Bytes: 200}), quota.WithUploaderLimits(seats))
	if _, err := e.addBy(t, "t", alice, "a1", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := e.addBy(t, "t", alice, "a2", 20); err != nil {
		t.Fatal(err)
	}
	_, err := e.addBy(t, "t", alice, "a3", 1)
	if filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded || !strings.Contains(err.Error(), `uploader "user:alice" in scope "t"`) ||
		!strings.Contains(err.Error(), "limits of 50 bytes and 2 files") {
		t.Fatalf("alice past her limits = %v", err)
	}
	if _, err := e.addBy(t, "t", bob, "b1", 140); err != nil {
		t.Fatalf("bob has no limit of his own: %v", err)
	}
	if _, err := e.addBy(t, "t", bob, "b2", 20); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded || !strings.Contains(err.Error(), `scope "t"`) {
		t.Fatalf("bob past the scope's limit = %v", err)
	}
	if _, err := e.addBy(t, "t", "", "anon", 11); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("an upload with no actor is limited as uploader \"\": %v", err)
	}
	if _, err := e.addBy(t, "t", "", "anon", 10); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		uploader string
		want     quota.Usage
	}{{alice, quota.Usage{Bytes: 50, Files: 2}}, {bob, quota.Usage{Bytes: 140, Files: 1}}, {"", quota.Usage{Bytes: 10, Files: 1}}, {"user:none", quota.Usage{}}} {
		if got := uploaderUsage(t, e, "t", c.uploader); got != c.want {
			t.Errorf("usage of %q = %+v, want %+v", c.uploader, got, c.want)
		}
	}
	if got := usage(t, e, "t"); got != (quota.Usage{Bytes: 200, Files: 4}) {
		t.Fatalf("scope usage = %+v", got)
	}
}

// TestUploaderReplaceAndPurge: a replacement is charged to the file's
// uploader whoever makes it, a shrink and a purge credit that uploader,
// and Recount and RecountUploader rebuild the rows from fs_files.
func TestUploaderReplaceAndPurge(t *testing.T) {
	ctx := t.Context()
	e, exec := newEnv(t, fixed(quota.Limit{}), quota.WithUploaderLimits(seats))
	f, err := e.addBy(t, "t", alice, "a1", 40)
	if err != nil {
		t.Fatal(err)
	}
	asBob := records.WithActor(ctx, bob)
	replace := func(size int) error {
		_, err := e.store.Replace(asBob, "t", f.ID, strings.NewReader(strings.Repeat("y", size)), "", uploaderPolicy)
		return err
	}
	if err := replace(51); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded || !strings.Contains(err.Error(), alice) {
		t.Fatalf("bob growing alice's file past her limit = %v", err)
	}
	if err := replace(45); err != nil {
		t.Fatal(err)
	}
	if got := uploaderUsage(t, e, "t", alice); got != (quota.Usage{Bytes: 45, Files: 1}) {
		t.Fatalf("alice after the growth = %+v", got)
	}
	if got := uploaderUsage(t, e, "t", bob); got != (quota.Usage{}) {
		t.Fatalf("the replacer was charged: %+v", got)
	}
	if err := replace(5); err != nil {
		t.Fatal(err)
	}
	if got := uploaderUsage(t, e, "t", alice); got != (quota.Usage{Bytes: 5, Files: 1}) {
		t.Fatalf("alice after the shrink = %+v", got)
	}
	if _, err := e.addBy(t, "t", bob, "b1", 7); err != nil {
		t.Fatal(err)
	}

	exec("DELETE FROM " + quota.TableNameUploaderUsage)
	if u, err := e.plugin.Recount(ctx, e.store, "t"); err != nil || u != (quota.Usage{Bytes: 12, Files: 2}) {
		t.Fatalf("Recount = %+v, %v", u, err)
	}
	if got := uploaderUsage(t, e, "t", alice); got != (quota.Usage{Bytes: 5, Files: 1}) {
		t.Fatalf("alice after Recount = %+v", got)
	}
	exec("UPDATE " + quota.TableNameUploaderUsage + " SET bytes = 999")
	if u, err := e.plugin.RecountUploader(ctx, e.store, "t", bob); err != nil || u != (quota.Usage{Bytes: 7, Files: 1}) {
		t.Fatalf("RecountUploader = %+v, %v", u, err)
	}
	if got := uploaderUsage(t, e, "t", alice); got.Bytes != 999 {
		t.Fatalf("RecountUploader touched another uploader: %+v", got)
	}

	if err := e.store.Purge(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if got := uploaderUsage(t, e, "t", alice); got != (quota.Usage{Bytes: 994, Files: 0}) {
		t.Fatalf("alice after the purge = %+v", got)
	}
	if _, err := e.store.PurgeScope(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if got := uploaderUsage(t, e, "t", bob); got != (quota.Usage{}) {
		t.Fatalf("an uploader row outlived PurgeScope: %+v", got)
	}
}

// TestUploaderLimitsOffByDefault: without the option no uploader row is
// kept, and Recount leaves the uploader table alone.
func TestUploaderLimitsOffByDefault(t *testing.T) {
	e, _ := newEnv(t, fixed(quota.Limit{}))
	if _, err := e.addBy(t, "t", alice, "a1", 40); err != nil {
		t.Fatal(err)
	}
	if _, err := e.plugin.Recount(t.Context(), e.store, "t"); err != nil {
		t.Fatal(err)
	}
	if got := uploaderUsage(t, e, "t", alice); got != (quota.Usage{}) {
		t.Fatalf("an uploader row without the option: %+v", got)
	}
	if _, err := quota.New(fixed(quota.Limit{}), quota.WithUploaderLimits(nil)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a nil UploaderLimitFunc = %v", err)
	}
}

// TestUploaderLimitErrors: an UploaderLimitFunc error fails the upload or
// the growing replacement unchanged.
func TestUploaderLimitErrors(t *testing.T) {
	broken := errors.New("plan service down")
	fail := false
	e, _ := newEnv(t, fixed(quota.Limit{}), quota.WithUploaderLimits(func(context.Context, string, string) (quota.Limit, error) {
		if fail {
			return quota.Limit{}, broken
		}
		return quota.Limit{}, nil
	}))
	f, err := e.addBy(t, "t", alice, "a1", 4)
	if err != nil {
		t.Fatal(err)
	}
	fail = true
	if _, err := e.addBy(t, "t", alice, "a2", 1); !errors.Is(err, broken) {
		t.Fatalf("upload = %v", err)
	}
	if _, err := e.store.Replace(t.Context(), "t", f.ID, strings.NewReader("longer"), "", uploaderPolicy); !errors.Is(err, broken) {
		t.Fatalf("replace = %v", err)
	}
}

// TestUploaderFaultSweep fails every database step of each uploader path
// in turn: a failed run leaves both the scope's and the uploader's usage
// as they were.
func TestUploaderFaultSweep(t *testing.T) {
	ctx := t.Context()
	e, _ := newEnv(t, fixed(quota.Limit{}), quota.WithUploaderLimits(seats))
	grow, _ := e.addBy(t, "t", bob, "grow", 10)
	shrink, _ := e.addBy(t, "t", bob, "shrink", 10)
	purge, _ := e.addBy(t, "t", bob, "purge", 10)
	key := 0
	ops := []struct {
		name string
		run  func() error
		// resumable paths commit per batch: a failure may keep what
		// earlier batches did, and the rerun finishes the work.
		resumable bool
	}{
		{"charge", func() error { key++; _, err := e.addBy(t, "t", bob, "k"+strings.Repeat("x", key), 1); return err }, false},
		{"first charge", func() error { key++; _, err := e.addBy(t, "t", alice, "k"+strings.Repeat("x", key), 1); return err }, false},
		{"grow", func() error {
			_, err := e.store.Replace(ctx, "t", grow.ID, strings.NewReader(strings.Repeat("g", 20)), "", uploaderPolicy)
			return err
		}, false},
		{"shrink", func() error {
			_, err := e.store.Replace(ctx, "t", shrink.ID, strings.NewReader("s"), "", uploaderPolicy)
			return err
		}, false},
		{"purge", func() error { return e.store.Purge(ctx, "t", purge.ID) }, false},
		{"recount", func() error { _, err := e.plugin.Recount(ctx, e.store, "t"); return err }, false},
		{"recount uploader", func() error { _, err := e.plugin.RecountUploader(ctx, e.store, "t", bob); return err }, false},
		{"uploader usage", func() error { _, err := e.plugin.UploaderUsage(ctx, e.store, "t", bob); return err }, false},
		{"purge scope", func() error { _, err := e.store.PurgeScope(ctx, "t"); return err }, true},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				before := [3]quota.Usage{usage(t, e, "t"), uploaderUsage(t, e, "t", bob), uploaderUsage(t, e, "t", alice)}
				e.faults.Arm(n)
				err := op.run()
				e.faults.Arm(0)
				if !errors.Is(err, testdb.ErrInjected) {
					if err != nil {
						t.Fatalf("outcome = %v", err)
					}
					if after := [3]quota.Usage{usage(t, e, "t"), uploaderUsage(t, e, "t", bob), uploaderUsage(t, e, "t", alice)}; op.resumable && after != ([3]quota.Usage{}) {
						t.Fatalf("usage left after the scope was purged: %+v", after)
					}
					return
				}
				if after := [3]quota.Usage{usage(t, e, "t"), uploaderUsage(t, e, "t", bob), uploaderUsage(t, e, "t", alice)}; after != before && !op.resumable {
					t.Fatalf("step %d failed but changed the usage: %+v -> %+v", n, before, after)
				}
			}
		})
	}
}

// TestPurgeScopeUsageFailure: a usage row that cannot be deleted fails
// Store.PurgeScope instead of leaving a counter behind silently.
func TestPurgeScopeUsageFailure(t *testing.T) {
	testdb.OnlySQLite(t)
	e, exec := newEnv(t, fixed(quota.Limit{}), quota.WithUploaderLimits(seats))
	if _, err := e.addBy(t, "t", bob, "b1", 1); err != nil {
		t.Fatal(err)
	}
	exec(failOn("DELETE", quota.TableNameUsage))
	if _, err := e.store.PurgeScope(t.Context(), "t"); err == nil {
		t.Fatal("a usage row that could not be deleted was ignored")
	}
}
