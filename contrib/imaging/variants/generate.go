package variants

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// Events the plugin emits after a generation run (Generate, and the eager
// generation of WithEager), in the transaction that records the file's
// status, so a webhook or an index hears exactly once per run.
const (
	// EventReady: every requested variant of the file exists.
	EventReady records.EventKind = PluginName + ".ready"
	// EventFailed: at least one could not be made; the file's status is
	// failed and its detail names the first variant and the error code.
	EventFailed records.EventKind = PluginName + ".failed"
)

// Keys of the plugin's Event.Detail.
const (
	// DetailVariants lists the variants the run made or found, comma
	// separated, in the order requested.
	DetailVariants = "variants"
	// DetailFailed names the first variant that failed (EventFailed only).
	DetailFailed = "failed"
	// DetailCode is that failure's filesystem.Code (EventFailed only).
	DetailCode = "code"
)

// statusPrefix starts every status detail the plugin writes, so it only
// ever clears a processing status it set itself.
const statusPrefix = "variants"

func progressDetail(n int) string { return fmt.Sprintf("%s 0/%d", statusPrefix, n) }

func isImage(f records.File) bool { return strings.HasPrefix(f.ContentType, "image/") }

// Generate makes the named variants of a file now (every declared variant
// when no name is given): the call for a job runner that generates
// derivatives after an upload, instead of on a page's first request.
//
// The run is visible through the file's processing status (records.Status):
// processing with the detail "variants 0/N" while it works, then ready, or
// failed with the detail "<variant>: <code>" (codes only, since the detail
// is served to clients). In the same transaction as that final status it
// emits EventReady or EventFailed with the file and DetailVariants,
// DetailFailed and DetailCode. Variants that already exist are found, not
// made again, so a retry finishes what a failed run left.
//
// The error is the first variant's failure (so a job runner retries the
// run), CodeNotFound for an unknown or trashed file or an undeclared name,
// or a failure to record the status. A file that is not an image has no
// variants: Generate returns nil and changes nothing.
func (p *Plugin) Generate(ctx context.Context, s *records.Store, scope, fileID string, names ...string) error {
	if len(names) == 0 {
		names = p.declared()
	}
	for _, n := range names {
		if _, ok := p.transforms[n]; !ok {
			return filesystem.Errorf(filesystem.CodeNotFound, "variants: no variant named %q", n)
		}
	}
	return p.run(ctx, s, scope, fileID, names, nil)
}

// declared lists the declared names in a stable order.
func (p *Plugin) declared() []string {
	names := make([]string, 0, len(p.transforms))
	for n := range p.transforms {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// run is Generate for checked names; onFail, when set, hears every
// variant that failed.
func (p *Plugin) run(ctx context.Context, s *records.Store, scope, fileID string, names []string, onFail func(context.Context, records.File, string, error)) error {
	f, err := s.File(ctx, scope, fileID)
	if err != nil {
		return err
	}
	if f.DeletedAt != nil || !isImage(f) {
		// Nothing to make: clear a processing status this plugin set (the
		// file was trashed, or replaced by a non-image, since).
		if f.Status == records.StatusProcessing && strings.HasPrefix(f.StatusDetail, statusPrefix) {
			_, err = s.SetStatus(ctx, scope, fileID, records.StatusReady, "")
		}
		if f.DeletedAt != nil && err == nil {
			err = filesystem.Errorf(filesystem.CodeNotFound, "variants: file %q is in the trash", fileID)
		}
		return err
	}
	if _, err := s.SetStatus(ctx, scope, fileID, records.StatusProcessing, progressDetail(len(names))); err != nil {
		return err
	}
	var made []string
	var failed string
	var firstErr error
	for _, name := range names {
		if _, err := p.variant(ctx, s, f, name); err != nil {
			if onFail != nil {
				onFail(ctx, f, name, err)
			}
			if firstErr == nil {
				failed, firstErr = name, err
			}
			continue
		}
		made = append(made, name)
	}
	st, detail, kind := records.StatusReady, "", EventReady
	info := map[string]string{DetailVariants: strings.Join(made, ",")}
	if firstErr != nil {
		code := filesystem.CodeOf(firstErr)
		st, detail, kind = records.StatusFailed, failed+": "+string(code), EventFailed
		info[DetailFailed], info[DetailCode] = failed, string(code)
	}
	err = s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		updated, err := s.SetStatus(ctx, scope, fileID, st, detail)
		if err != nil {
			return err
		}
		return s.Emit(ctx, p, records.Event{Kind: kind, Scope: scope, FileID: fileID, File: &updated, Detail: info})
	})
	return errors.Join(firstErr, err)
}

// MissingOptions select the files GenerateMissing works on.
type MissingOptions struct {
	// Scope is the records scope, one at a time as Store.Files works.
	Scope string
	// Names are the variants to make; empty means every declared one.
	Names []string
	// Cursor resumes after a previous result's NextCursor; empty starts
	// with the newest file.
	Cursor string
	// MaxFiles bounds how many image files one call examines, newest
	// first, so a scheduled job can cover recent uploads in small runs; 0
	// means no bound.
	MaxFiles int
	// PageSize is how many rows each database read fetches; 0 means
	// records.DefaultPageSize.
	PageSize int
}

// MissingResult says what one GenerateMissing call did.
type MissingResult struct {
	// Generated counts files that lacked a variant and were run through
	// Generate; Complete counts files that had them all.
	Generated, Complete int
	// Failed lists files whose run failed, with its error; the call
	// carries on past them, and their status says failed.
	Failed []MissingFailure
	// NextCursor resumes after the last file examined; empty when the
	// scope is done.
	NextCursor string
}

// MissingFailure is one file GenerateMissing could not complete.
type MissingFailure struct {
	FileID string
	Err    error
}

// GenerateMissing runs Generate for every live image of a scope that
// lacks one of the named variants, newest first: the scheduled companion
// of Generate, which catches files whose per-upload job was lost (a
// crash, a full queue) and makes the variants of a newly declared name
// for files recorded before it. Files that have every variant are only
// looked up, not read. It is resumable through NextCursor, and safe to run
// again. An error is returned only when listing fails or ctx ends; the
// result then holds the progress so far.
func (p *Plugin) GenerateMissing(ctx context.Context, s *records.Store, opts MissingOptions) (MissingResult, error) {
	names := opts.Names
	if len(names) == 0 {
		names = p.declared()
	}
	for _, n := range names {
		if _, ok := p.transforms[n]; !ok {
			return MissingResult{}, filesystem.Errorf(filesystem.CodeNotFound, "variants: no variant named %q", n)
		}
	}
	var res MissingResult
	q := records.FileQuery{Scope: opts.Scope, Cursor: opts.Cursor, Limit: opts.PageSize,
		// A quarantined file is not generated for (its bytes are unscanned);
		// its release generates its eager variants, and a later run finds it.
		Filters: []sqlb.Predicate{sqlb.Like(records.FileContentType, "image/%"), sqlb.NotEq(records.FileStatus, records.StatusQuarantined)}}
	examined := 0
	for {
		page, err := s.Files(ctx, q)
		if err != nil {
			return res, err
		}
		for _, f := range page.Files {
			if opts.MaxFiles > 0 && examined == opts.MaxFiles {
				return res, nil
			}
			if err := ctx.Err(); err != nil {
				return res, err
			}
			missing, err := p.missing(ctx, s, f, names)
			if err != nil {
				return res, err
			}
			switch {
			case !missing:
				res.Complete++
			default:
				if err := p.run(ctx, s, f.Scope, f.ID, names, nil); err != nil {
					res.Failed = append(res.Failed, MissingFailure{FileID: f.ID, Err: err})
				} else {
					res.Generated++
				}
			}
			examined++
			res.NextCursor = records.EncodeCursor(f.CreatedAt, f.ID)
		}
		if page.NextCursor == "" {
			res.NextCursor = ""
			return res, nil
		}
		q.Cursor = page.NextCursor
	}
}

// missing reports whether f lacks one of the named variants.
func (p *Plugin) missing(ctx context.Context, s *records.Store, f records.File, names []string) (bool, error) {
	for _, n := range names {
		_, err := p.find(ctx, s, f.Scope, f.ID, n, p.transforms[n].Digest())
		switch {
		case filesystem.CodeOf(err) == filesystem.CodeNotFound:
			return true, nil
		case err != nil:
			return false, err
		}
	}
	return false, nil
}
