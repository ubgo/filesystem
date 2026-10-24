package fshttp_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
)

func TestClientMessageNeverNamesTheDiskOrKey(t *testing.T) {
	const disk, key = "files.acme", "settings/globex/x.json"
	annotated := func(code filesystem.Code, cause error) error {
		return fmt.Errorf("handler: %w", &filesystem.Error{Code: code, Op: filesystem.OpWrite, Disk: disk, Key: key, Err: cause})
	}
	leaks := func(msg string) bool {
		return strings.Contains(msg, disk) || strings.Contains(msg, key) || strings.Contains(msg, string(filesystem.OpWrite)) || strings.Contains(msg, "filesystem:")
	}
	for _, code := range filesystem.CodeValues {
		err := annotated(code, errors.New("the file is not allowed"))
		got := fshttp.ClientMessage(err)
		if leaks(got) {
			t.Errorf("%s: %q leaks the operation, disk or key", code, got)
		}
		want := http.StatusText(fshttp.StatusOf(err))
		switch code {
		case filesystem.CodeTooLarge, filesystem.CodeRejected, filesystem.CodeQuotaExceeded, filesystem.CodeInvalidArgument, filesystem.CodeInvalidKey:
			want = "the file is not allowed"
		}
		if got != want {
			t.Errorf("%s: %q, want %q", code, got, want)
		}
		// A visible code wrapping another annotated error shows neither.
		nested := annotated(code, annotated(filesystem.CodeUnknown, errors.New("backend said no")))
		if got := fshttp.ClientMessage(nested); leaks(got) || strings.Contains(got, "backend said no") {
			t.Errorf("%s nested: %q", code, got)
		}
	}
	// The Disk's own invalid_key error, end to end.
	_, err := filesystem.CleanKey("settings/../../globex/x.json")
	if got := fshttp.ClientMessage(err); leaks(got) || got != `key contains a ".." segment` {
		t.Errorf("CleanKey: %q", got)
	}
	for err, want := range map[error]string{
		nil:                    http.StatusText(http.StatusOK),
		errors.New("internal"): http.StatusText(http.StatusInternalServerError),
		filesystem.ErrTooLarge: http.StatusText(http.StatusRequestEntityTooLarge),
		filesystem.NewError(filesystem.CodeRejected, nil): http.StatusText(http.StatusUnsupportedMediaType),
	} {
		if got := fshttp.ClientMessage(err); got != want {
			t.Errorf("ClientMessage(%v) = %q, want %q", err, got, want)
		}
	}
}
