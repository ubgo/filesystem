package fwtest

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
)

// MediaProblem is the content type Problem answers with.
const MediaProblem = "application/problem+json"

// ProblemBody is what Problem writes: an RFC 9457 style envelope, the
// model an application plugs in through a Responder's Errors.
type ProblemBody struct {
	Status int             `json:"status"`
	Title  string          `json:"title"`
	Code   filesystem.Code `json:"code"`
}

// Problem is a custom fshttp.ErrorWriter, the house envelope the adapter
// tests plug into each framework's Responder. A nil err (which the
// contract forbids) is answered 500 with code "nil-error", so the check
// fails loudly.
func Problem(w http.ResponseWriter, _ *http.Request, status int, err error) {
	body := ProblemBody{Status: status, Title: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)}
	if err == nil {
		body = ProblemBody{Status: http.StatusInternalServerError, Code: "nil-error"}
	}
	w.Header().Set("Content-Type", MediaProblem)
	w.WriteHeader(body.Status)
	_ = json.NewEncoder(w).Encode(body)
}

// WantProblem checks that resp was answered by Problem with status and
// code, and returns its body for further checks.
func WantProblem(t *testing.T, resp *http.Response, status int, code filesystem.Code) ProblemBody {
	t.Helper()
	raw, _ := io.ReadAll(resp.Body)
	var got ProblemBody
	if ct := resp.Header.Get("Content-Type"); ct != MediaProblem || json.Unmarshal(raw, &got) != nil {
		t.Fatalf("answer was not the custom writer's: %d %q %s", resp.StatusCode, ct, raw)
	}
	if resp.StatusCode != status || got.Status != status || got.Code != code {
		t.Errorf("custom writer answer = %d %+v, want %d %s", resp.StatusCode, got, status, code)
	}
	return got
}

// RetryHeader is the Retry-After CaseRetry must carry, also through a
// custom writer.
const RetryHeader = retryHeader
