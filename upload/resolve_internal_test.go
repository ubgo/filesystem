package upload

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
)

// The misconfigured wrapper keeps its cause for logs and errors.Is, though
// clients never see it.
func TestMisconfiguredKeepsItsCause(t *testing.T) {
	_, err := resolve(nil, httptest.NewRequest("POST", "/", nil), Policy{})
	var m *misconfigured
	if !errors.As(err, &m) || !errors.Is(err, filesystem.ErrInvalidArgument) || !strings.Contains(err.Error(), "misconfigured policy: ") || !strings.Contains(err.Error(), "no MaxSize") {
		t.Errorf("err %v", err)
	}
}
