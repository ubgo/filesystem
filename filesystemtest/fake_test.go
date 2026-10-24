package filesystemtest_test

import (
	"fmt"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
)

// recorder captures assertion failures instead of failing the real test, so
// the Fake's failure paths can be checked. Embedding testing.TB satisfies
// its unexported method; only the reporting methods are overridden.
type recorder struct {
	testing.TB
	errors []string
	fatal  string
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}
func (r *recorder) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
}

func TestFakeAssertionsPass(t *testing.T) {
	f := filesystemtest.NewFake(t, "uploads")
	f.AssertEmpty()
	if _, err := f.WriteBytes(t.Context(), "avatars/1.png", []byte("img"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.AssertExists("avatars/1.png")
	f.AssertMissing("avatars/2.png")
	f.AssertContent("avatars/1.png", "img")
	f.AssertCount("avatars", 1)
	f.AssertCount("", 1)
}

func TestFakeAssertionsFail(t *testing.T) {
	r := &recorder{TB: t}
	f := filesystemtest.NewFake(r, "uploads")
	_, _ = f.WriteBytes(t.Context(), "a.txt", []byte("A"), filesystem.WriteOptions{})
	f.AssertExists("b.txt")
	f.AssertMissing("a.txt")
	f.AssertContent("a.txt", "B")
	f.AssertContent("missing.txt", "x")
	f.AssertCount("", 5)
	f.AssertEmpty()
	f.AssertExists("../bad")
	f.AssertMissing("../bad")
	f.AssertCount("../bad", 0)
	if len(r.errors) != 9 {
		t.Errorf("recorded %d failures, want 9: %q", len(r.errors), r.errors)
	}
	bad := &recorder{TB: t}
	filesystemtest.NewFake(bad, "bad name!")
	if bad.fatal == "" {
		t.Error("invalid fake name did not fail the test")
	}
}
