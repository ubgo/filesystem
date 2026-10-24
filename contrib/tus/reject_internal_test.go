package tus

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
)

// TestRejectNeverNamesTheDiskOrKey pins that a refusal shows the client the
// cause of a client-visible code and the status text otherwise, never the
// operation, disk or storage key err.Error() carries.
func TestRejectNeverNamesTheDiskOrKey(t *testing.T) {
	_, keyErr := filesystem.CleanKey("uploads/../../tenants/42/x.bin")
	refused := &filesystem.Error{Op: filesystem.OpWrite, Disk: "files.acme", Key: "tenants/42/x.bin", Code: filesystem.CodeRejected, Err: keyErr}
	failed := &filesystem.Error{Op: filesystem.OpWrite, Disk: "files.acme", Key: "tenants/42/x.bin", Code: filesystem.CodeUnavailable, Err: http.ErrHandlerTimeout}
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{keyErr, http.StatusBadRequest, "ERR_INVALID_KEY"},
		{refused, http.StatusUnsupportedMediaType, "ERR_REJECTED"},
		{failed, http.StatusServiceUnavailable, "ERR_UNAVAILABLE"},
	} {
		e := reject(c.err, c.status)
		if e.ErrorCode != c.code || e.HTTPResponse.StatusCode != c.status {
			t.Errorf("reject(%v) = %s %d, want %s %d", c.err, e.ErrorCode, e.HTTPResponse.StatusCode, c.code, c.status)
		}
		for _, leak := range []string{"files.acme", "tenants/42", "write"} {
			if strings.Contains(e.Message, leak) {
				t.Errorf("reject(%v) message %q names %q", c.err, e.Message, leak)
			}
		}
	}
	if got := reject(failed, http.StatusServiceUnavailable).Message; got != http.StatusText(http.StatusServiceUnavailable) {
		t.Errorf("server-side failure message = %q, want the status text", got)
	}
}
