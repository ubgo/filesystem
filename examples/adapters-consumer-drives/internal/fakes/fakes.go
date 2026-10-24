// Package fakes holds small in-process stand-ins for the Google Drive,
// Dropbox and Microsoft Graph HTTP APIs, trimmed from the fakes the three
// drivers test against (adapters/<name>/fake_test.go). Each serves only
// the requests its driver makes, accepts only the bearer token Token, and
// counts the upload-session chunks it receives so the example can show a
// large body going through a session. They are for the example, not a
// faithful model of the services.
package fakes

import (
	"encoding/json"
	"net/http"
)

// Token is the only access token the fakes accept. A real application
// gets it from its OAuth token source.
const Token = "example-access-token"

// authorised reports whether r carries Token.
func authorised(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+Token }

// writeJSON answers v as JSON with status.
func writeJSON[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
