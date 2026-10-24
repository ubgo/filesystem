package app

import (
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
)

// PathInfo is where each framework program mounts a handler of its own
// (not one the library ships): GET PathInfo/<key> answers the metadata of
// one object of the media disk. It shows the shape every application
// handler that calls the library directly should have.
const PathInfo = "/info"

// ErrSignedOutMessage is the body text of the info route's 401.
const ErrSignedOutMessage = "sign in first"

// Info is the info route's 200 body.
type Info struct {
	Key         string    `json:"key"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	Modified    time.Time `json:"modified"`
	ETag        string    `json:"etag"`
}

// InfoOf is the body for one entry.
func InfoOf(e filesystem.Entry) Info {
	return Info{Key: e.Key, Size: e.Size, ContentType: e.ContentType, Modified: e.ModTime.UTC(), ETag: e.ETag}
}

// Problem is an error body: the client-safe message and the error's code,
// so clients branch on Code and show Error.
type Problem struct {
	Error string          `json:"error"`
	Code  filesystem.Code `json:"code"`
}

// ProblemOf is the body for err: fshttp.ClientMessage, never err.Error(),
// which names the disk and the key (and for some backends the bucket).
// ds:def id=problemof-kjsuu6nd
func ProblemOf(err error) Problem {
	return Problem{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)}
}
