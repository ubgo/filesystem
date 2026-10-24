package webdav

import (
	"errors"
	"testing"
)

// TestBodyErrorKeepsItsCause pins that the marker put on a failing body's
// error still unwraps to it, so a caller inspecting the transport's error
// chain finds the body's own error.
func TestBodyErrorKeepsItsCause(t *testing.T) {
	cause := errors.New("disk read failed")
	if err := (bodyError{cause}); !errors.Is(err, cause) || err.Error() != cause.Error() {
		t.Fatalf("bodyError %v does not carry its cause", err)
	}
}
