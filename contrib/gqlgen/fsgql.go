// Package fsgql connects github.com/ubgo/filesystem to a gqlgen GraphQL
// server: files sent as the Upload scalar of the GraphQL multipart request
// specification (https://github.com/jaydenseric/graphql-multipart-request-spec)
// are stored through an upload.Policy, exactly as a form upload is.
//
// gqlgen already parses the multipart request (transport.MultipartForm)
// and hands a resolver a graphql.Upload. What it does not do is decide
// what may be stored: graphql.Upload's ContentType and Filename are the
// client's claims. This package adds the storage side:
//
//   - Receive and ReceiveAll store uploads on a Disk through upload.Receive,
//     so the type is sniffed from the bytes, the size capped, the name made
//     safe, and Process and Inspect run.
//   - ReceiveRecord stores one as a records.File through Store.Upload.
//   - ErrorPresenter turns the library's errors into GraphQL errors with
//     fshttp.ClientMessage and the code under extensions.code, so a
//     driver's message (which names disks and keys) never reaches a client.
//   - MultipartForm sizes gqlgen's transport from the policy.
//   - Schema is the Upload scalar and the StoredUpload type, ready to copy.
//
// It is a separate module so an application without GraphQL never
// downloads gqlgen.
package fsgql

import (
	"context"
	_ "embed"
	"errors"
	"net/http"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// Schema is the GraphQL schema of the Upload scalar and the StoredUpload
// type. Copy it into the application's schema directory; gqlgen generates
// code in the application's module, so the library cannot generate it.
//
//go:embed upload.graphqls
var Schema string

// CodeExtension is the key of the filesystem.Code in a GraphQL error's
// extensions, so a client can tell "too large" from "wrong type".
const CodeExtension = "code"

// FormOverhead allows for the multipart boundaries, part headers and the
// operations and map fields on top of the files, when MultipartForm caps
// a request.
const FormOverhead int64 = 64 << 10

// MaxMemory is how much of a multipart request gqlgen's transport keeps in
// memory before it spills to temporary files; MultipartForm sets it low so
// concurrent large uploads never sit in memory.
const MaxMemory int64 = 1 << 20

// MultipartForm returns gqlgen's multipart transport capped for maxFiles
// files of p.MaxSize each: the request body may not exceed
// maxFiles × p.MaxSize plus FormOverhead, and at most MaxMemory of it is
// held in memory. maxFiles below 1 counts as 1. The cap is the transport's
// first line; Receive still enforces p.MaxSize per file from the bytes.
func MultipartForm(p upload.Policy, maxFiles int) transport.MultipartForm {
	if maxFiles < 1 {
		maxFiles = 1
	}
	return transport.MultipartForm{MaxUploadSize: int64(maxFiles)*p.MaxSize + FormOverhead, MaxMemory: MaxMemory}
}

// Receive stores one GraphQL upload on disk under p with upload.Receive:
// the type is detected from the bytes (up.ContentType, the client's claim,
// is ignored), refused with CodeRejected when p does not accept it, the
// size capped at p.MaxSize (CodeTooLarge, nothing stored), the name made
// safe, and p's Process and Inspect run. An upload whose declared size is
// already over p.MaxSize is refused before the disk is touched.
func Receive(ctx context.Context, disk *filesystem.Disk, up graphql.Upload, p upload.Policy) (upload.Result, error) {
	if err := precheck(up, p); err != nil {
		return upload.Result{}, err
	}
	return upload.Receive(ctx, disk, up.File, up.Filename, p)
}

// ReceiveAll stores every upload of a list argument ([Upload!]!) with
// Receive, all or none: when one fails, the ones already stored are
// deleted and its error returned. More than maxFiles uploads (maxFiles
// below 1 counts as 1) is CodeInvalidArgument before anything is stored.
func ReceiveAll(ctx context.Context, disk *filesystem.Disk, ups []*graphql.Upload, p upload.Policy, maxFiles int) ([]upload.Result, error) {
	if maxFiles < 1 {
		maxFiles = 1
	}
	if len(ups) > maxFiles {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "at most %d files per request", maxFiles)
	}
	out := make([]upload.Result, 0, len(ups))
	for _, up := range ups {
		if up == nil {
			discard(ctx, disk, out)
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "a file is missing")
		}
		res, err := Receive(ctx, disk, *up, p)
		if err != nil {
			discard(ctx, disk, out)
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

// discard deletes what a failed ReceiveAll stored. Deletion errors are
// ignored: upload.Sweep removes anything left behind.
func discard(ctx context.Context, disk *filesystem.Disk, stored []upload.Result) {
	for _, r := range stored {
		_ = disk.Delete(context.WithoutCancel(ctx), r.Key)
	}
}

// ReceiveRecord stores one GraphQL upload as a file of scope on the named
// disk with Store.Upload under p, with the same checks as Receive: the
// object is written first and deleted again if the row cannot be
// inserted or an enclosing Tx rolls back. The actor of ctx
// (records.WithActor) becomes File.UploadedBy.
func ReceiveRecord(ctx context.Context, s *records.Store, scope, diskName string, up graphql.Upload, p upload.Policy) (records.File, error) {
	if err := precheck(up, p); err != nil {
		return records.File{}, err
	}
	return s.Upload(ctx, scope, diskName, up.File, up.Filename, p)
}

// precheck refuses an upload without a body, and one whose size, as the
// transport counted it, is already over the policy's cap.
func precheck(up graphql.Upload, p upload.Policy) error {
	if up.File == nil {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "the upload has no file")
	}
	if p.MaxSize > 0 && up.Size > p.MaxSize {
		return filesystem.Errorf(filesystem.CodeTooLarge, "the file is larger than %d bytes", p.MaxSize)
	}
	return nil
}

// ErrorPresenter returns a gqlgen error presenter that answers the
// library's errors (any error with a *filesystem.Error in its chain, and a
// tripped http.MaxBytesReader) with fshttp.ClientMessage as the message
// (only the words of the client's own input's errors, never a driver's
// message with its disk and key), the field's path, and the
// filesystem.Code under extensions.code; every other error goes to next
// (graphql.DefaultErrorPresenter when nil). Install it with
// srv.SetErrorPresenter(fsgql.ErrorPresenter(nil)).
func ErrorPresenter(next graphql.ErrorPresenterFunc) graphql.ErrorPresenterFunc {
	if next == nil {
		next = graphql.DefaultErrorPresenter
	}
	return func(ctx context.Context, err error) *gqlerror.Error {
		var fe *filesystem.Error
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			err = filesystem.NewError(filesystem.CodeTooLarge, mbe)
		case !errors.As(err, &fe):
			return next(ctx, err)
		}
		return &gqlerror.Error{
			Message:    fshttp.ClientMessage(err),
			Path:       graphql.GetPath(ctx),
			Extensions: map[string]any{CodeExtension: filesystem.CodeOf(err)},
		}
	}
}

// Code returns the filesystem code of a GraphQL error ErrorPresenter
// made, or "", for tests and Go clients.
func Code(err *gqlerror.Error) filesystem.Code {
	if err == nil {
		return ""
	}
	code, _ := err.Extensions[CodeExtension].(filesystem.Code)
	if code != "" {
		return code
	}
	s, _ := err.Extensions[CodeExtension].(string)
	return filesystem.Code(s)
}
