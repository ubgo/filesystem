package dimensions

import (
	"context"
	"encoding/json"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// BackfillOptions selects the files Backfill measures.
type BackfillOptions struct {
	// Scope is the records scope to backfill; Backfill works one scope at
	// a time, as Store.Files does.
	Scope string
	// Trashed backfills the scope's trashed files instead of its live
	// ones, so a file restored later has its dimensions too.
	Trashed bool
	// Cursor resumes after a previous call's BackfillResult.NextCursor.
	// Empty starts from the newest file.
	Cursor string
	// MaxFiles bounds how many raster files one call examines, so a large
	// scope can be done in batches (a job per batch, a pause between
	// them). 0 means no bound: the call runs to the end of the scope.
	MaxFiles int
	// PageSize is how many rows each database read fetches; 0 means
	// records.DefaultPageSize. At most records.MaxPageSize.
	PageSize int
	// Plugin is the installed plugin, so its processor and content types
	// (WithProcessor) apply to the backfill too; nil means New().
	Plugin *Plugin
}

// BackfillResult says what one Backfill call did.
type BackfillResult struct {
	// Measured counts files whose dimensions were written.
	Measured int
	// Present counts files that already had dimensions and were left
	// alone.
	Present int
	// Unreadable counts raster files whose header could not be parsed;
	// they stay without dimensions, as the plugin records them without
	// WithStrict.
	Unreadable int
	// Changed counts files whose row changed between the listing and the
	// write (replaced, or their metadata rewritten); they were not
	// written, and the change that won measured them if this plugin is
	// installed.
	Changed int
	// Failed lists files whose object could not be read; the call carries
	// on past them, and a later call with the same options retries them.
	Failed []BackfillFailure
	// NextCursor resumes after the last file this call examined; empty
	// when the scope is done.
	NextCursor string
}

// BackfillFailure is one file Backfill could not measure.
type BackfillFailure struct {
	FileID string
	Err    error
}

// Backfill writes width and height into the metadata of raster files
// recorded before the plugin was installed. It walks one scope newest
// first with Store.Files' keyset pagination, so it is resumable: run it
// once with MaxFiles 0, or in batches, passing each result's NextCursor to
// the next call until it is empty. Running it twice is harmless, because a
// file that already has dimensions is skipped without being read.
//
// Each file costs one header read (imaging.ProbeBytes through
// Disk.ReadRange), never the whole object. Each write is its own
// statement, guarded on the object key and the metadata the listing saw,
// so it never overwrites a concurrent Replace or a newer metadata value:
// such a file is counted as Changed instead. The write goes straight to
// the row: no hooks run and no event is emitted, because the file itself
// did not change.
//
// An error is returned only when listing or writing fails, or ctx ends;
// the result then holds the progress so far, and its NextCursor (when set)
// resumes after the last file fully handled.
func Backfill(ctx context.Context, s *records.Store, opts BackfillOptions) (BackfillResult, error) {
	var res BackfillResult
	plugin := opts.Plugin
	if plugin == nil {
		plugin = New()
	}
	q := records.FileQuery{Scope: opts.Scope, Trashed: opts.Trashed, Cursor: opts.Cursor, Limit: opts.PageSize,
		Filters: []sqlb.Predicate{sqlb.In(records.FileContentType, plugin.measured()...)}}
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
			if err := plugin.backfillOne(ctx, s, f, &res); err != nil {
				return res, err
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

// backfillOne measures one listed file and records the outcome in res. It
// returns an error only for a failed database write.
func (p *Plugin) backfillOne(ctx context.Context, s *records.Store, f records.File, res *BackfillResult) error {
	if _, _, ok := Of(f); ok {
		_, hashed := ThumbHashOf(f)
		_, coloured := ColourOf(f)
		if (hashed || !p.thumbHash) && (coloured || !p.colour) {
			res.Present++
			return nil
		}
	}
	next := f
	// Strictness does not apply: the file is already recorded.
	lenient := *p
	lenient.strict = false
	if err := lenient.measure(ctx, s, &next); err != nil {
		res.Failed = append(res.Failed, BackfillFailure{FileID: f.ID, Err: err})
		return nil
	}
	if _, _, ok := Of(next); !ok {
		res.Unreadable++
		return nil
	}
	// The metadata compare is exact on MySQL too because records creates
	// its tables with COLLATE=utf8mb4_bin (records.TypesFor).
	n, err := s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(records.TableFiles).
		Set(sqlb.Set(records.FileMetadata, storedMetadata(next.Metadata))).
		Where(sqlb.Eq(records.FileScope, f.Scope), sqlb.Eq(records.FileID, f.ID), sqlb.Eq(records.FileKey, f.Key),
			sqlb.Eq(records.FileMetadata, storedMetadata(f.Metadata))))
	if err != nil {
		return err
	}
	if n == 0 {
		res.Changed++
		return nil
	}
	res.Measured++
	return nil
}

// emptyMetadata is how records stores a file without metadata.
const emptyMetadata = "{}"

// storedMetadata is the column text records writes for m: "{}" when empty,
// else JSON, whose object keys encoding/json sorts, so the same map always
// renders the same text. The compare-and-set in backfillOne relies on that;
// a row in any other form (a hand edit) never matches and is counted as
// Changed rather than overwritten. TestBackfillMatchesStoredMetadata pins
// the format against rows records itself wrote.
func storedMetadata(m map[string]string) string {
	if len(m) == 0 {
		return emptyMetadata
	}
	b, _ := json.Marshal(m) // a map of strings cannot fail to marshal
	return string(b)
}
