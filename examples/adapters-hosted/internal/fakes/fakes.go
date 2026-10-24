// Package fakes holds small in-process stand-ins for the Vercel Blob and
// Cloudinary HTTP APIs, cut down from the fakes in the drivers' own tests
// (adapters/vercelblob/fake_test.go, adapters/cloudinary/fake_test.go) so
// the example runs with no account and no network. They model only what
// the example shows; the drivers' tests remain the full check.
package fakes

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// Failure is an error answer a fake sends instead of serving the next
// request, to show how the driver maps it to a filesystem code.
type Failure struct {
	// Status is the HTTP status, Code the API's error code (Vercel) or
	// message (Cloudinary).
	Status int
	Code   string
	// RetryAfter, when positive, is sent as Retry-After in seconds.
	RetryAfter int
}

// writeFailure writes f as the JSON error body both APIs use:
// {"error":{"code":...,"message":...}}.
func writeFailure(w http.ResponseWriter, f Failure) {
	w.Header().Set("Content-Type", "application/json")
	if f.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(f.RetryAfter))
	}
	w.WriteHeader(f.Status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorObject{Code: f.Code, Message: f.Code}})
}

// errorBody is the error envelope both APIs answer with.
type errorBody struct {
	Error errorObject `json:"error"`
}

// errorObject is the error inside errorBody.
type errorObject struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// bearer is the Authorization header value that carries token.
func bearer(token string) string { return "Bearer " + token }
