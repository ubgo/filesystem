package videoplatform_test

import (
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/videoplatform"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// newFaultyEnv is newEnv over a database whose steps the returned Faults
// can make fail one at a time.
func newFaultyEnv(t *testing.T) (*env, *recordstest.Faults) {
	t.Helper()
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{fake: newFake(), recorder: &recorder{}}
	e.plugin = videoplatform.New(e.fake, videoplatform.WithSourceURL(sourceURL))
	db, faults := recordstest.OpenFaulty(t)
	e.db = db
	e.store, err = records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(e.plugin, e.recorder))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.hook = e.plugin.Webhook(e.store, nil)
	return e, faults
}

// TestEveryDatabaseStepFailsCleanly measures the database steps of each
// plugin operation on a published video, then reruns it on a fresh
// database once per step with that step failing: every run must fail,
// never report success over a half-written row.
func TestEveryDatabaseStepFailsCleanly(t *testing.T) {
	// Each op gets the published video's ID and a second, unpublished one.
	ops := map[string]func(t *testing.T, e *env, id, fresh string) error{
		"publish": func(t *testing.T, e *env, _, fresh string) error {
			_, err := e.plugin.Publish(t.Context(), e.store, scope, fresh)
			return err
		},
		"unpublish": func(t *testing.T, e *env, id, _ string) error {
			return e.plugin.Unpublish(t.Context(), e.store, scope, id)
		},
		"sync": func(t *testing.T, e *env, id, _ string) error {
			e.fake.set(videoplatform.Asset{ID: "asset-1", Status: videoplatform.StatusReady, HLS: "https://h/x.m3u8"})
			_, err := e.plugin.Sync(t.Context(), e.store, scope, id)
			return err
		},
		"purge scope": func(t *testing.T, e *env, _, _ string) error {
			return e.plugin.PurgeScope(t.Context(), e.store, scope)
		},
	}
	// prepared returns an env holding one published video and one not
	// yet published, and their IDs.
	prepared := func(t *testing.T) (*env, *recordstest.Faults, string, string) {
		e, faults := newFaultyEnv(t)
		f, fresh := e.video(t), e.video(t)
		if _, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID); err != nil {
			t.Fatal(err)
		}
		return e, faults, f.ID, fresh.ID
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			e, faults, id, fresh := prepared(t)
			faults.Arm(0)
			if err := op(t, e, id, fresh); err != nil {
				t.Fatalf("unfaulted run: %v", err)
			}
			steps := faults.Steps()
			if steps == 0 {
				t.Fatal("the operation took no database step")
			}
			for n := 1; n <= steps; n++ {
				e, faults, id, fresh := prepared(t)
				faults.Arm(n)
				if err := op(t, e, id, fresh); err == nil {
					t.Errorf("step %d of %d failing: the operation succeeded", n, steps)
				}
			}
		})
	}
}

// TestSecretMethodsNeverReveal pins that String and GoString, called
// directly rather than through fmt, are redacted too.
func TestSecretMethodsNeverReveal(t *testing.T) {
	s := videoplatform.NewSecret("tok-SECRET")
	if strings.Contains(s.String(), "SECRET") || strings.Contains(s.GoString(), "SECRET") {
		t.Fatalf("String %q, GoString %q", s.String(), s.GoString())
	}
}
