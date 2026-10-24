package main

import (
	"fmt"
	"net/http"

	"github.com/ubgo/filesystem/fshttp"
)

const (
	// avatarField is the form field the avatar arrives in.
	avatarField = "avatar"
	// headerUser names the signed-in user. It stands in for the app's
	// session lookup, which this example does not have.
	headerUser = "X-User"
	// formOverhead is the room left above maxAvatarSize for the multipart
	// framing, so a file just over the limit reaches the policy, which
	// answers 413, rather than failing to parse.
	formOverhead = 64 << 10
)

// ServeHTTP accepts a multipart form with the avatar in the "avatar"
// field and answers with the stored key. Errors are answered with the
// status and the client-safe message fshttp derives from their code.
// ds:def id=testing-servehttp-57hnvr2r
func (a *Avatars) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get(headerUser)
	if user == "" {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarSize+formOverhead)
	file, _, err := r.FormFile(avatarField)
	if err != nil {
		http.Error(w, "send the image in the "+avatarField+" field", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()
	key, err := a.Save(r.Context(), user, file)
	if err != nil {
		http.Error(w, fshttp.ClientMessage(err), fshttp.StatusOf(err))
		return
	}
	_, _ = fmt.Fprintln(w, key)
}
