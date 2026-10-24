package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// recordedPrefix mounts the Uppy routes that record a file in the
// complete request itself, with no confirm call afterwards.
const recordedPrefix = "/uploads/s3r"

// uppyRecorded serves Uppy's multipart protocol with OnStored: once the
// completed file has passed Confirm (run by the complete route), OnStored
// records and attaches it, so the complete request is the last one. The
// key is under files/<user>/ from the start, since nothing moves it. If
// recording fails, the file is deleted and the complete request fails
// with that error, so no stored file lacks its row. A custom
// completeMultipartUpload may send "filename" beside the parts; it is
// checked by the policy's name rules and recorded as the file's name.
// ds:def id=app-uppyrecorded-d3k7v9qw
func (a *App) uppyRecorded(w http.ResponseWriter, r *http.Request) {
	user, err := userOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	mine := path.Join(filesDir, user)
	p := policyFor(user)
	p.Key = upload.RandomKey(mine)
	opts := upload.UppyOptions{
		Authorize: func(_ *http.Request, key string) error {
			if !filesystem.KeyInDir(key, mine) {
				return fmt.Errorf("key %q is not this user's upload", key)
			}
			return nil
		},
		OnStored: func(ctx context.Context, _ *http.Request, res upload.Result) error {
			return a.record(ctx, user, res)
		},
	}
	upload.Uppy(a.Disk, p, opts).ServeHTTP(w, r)
}

// record creates the file's row and attaches it to user in one
// transaction.
func (a *App) record(ctx context.Context, user string, res upload.Result) error {
	return a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err := a.Store.CreateFile(ctx, records.NewFile{
			Disk: diskName, Key: res.Key, Filename: res.Filename,
			ContentType: res.ContentType, Size: res.Size, SHA256: res.SHA256, UploadedBy: user,
		})
		if err != nil {
			return err
		}
		_, err = a.Store.Attach(ctx, "", f.ID, records.Owner{Type: ownerUser, ID: user}, roleUploads)
		return err
	})
}
