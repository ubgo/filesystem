package records

import (
	"context"
	"database/sql"
	"maps"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// Reserved Metadata keys of a file's describing text (Text). They live in
// the metadata column rather than in columns of their own because nothing
// filters or sorts by them: they are read with the file and shown beside
// it. Writers other than SetText (CreateFile's Metadata, a BeforeCreate
// hook) are checked against the same limits.
const (
	MetaAlt     = "alt"
	MetaCaption = "caption"
	MetaTitle   = "title"
)

// Limits of the describing text, in characters. Alt text is read aloud by
// screen readers and belongs short; a caption may be a paragraph.
const (
	MaxAltChars     = 1000
	MaxCaptionChars = 2000
	MaxTitleChars   = 255
)

// Text is a file's describing text: Alt for screen readers and broken
// images, Caption shown under it, Title as its display heading. "" means
// none. Caption may hold line breaks; Alt and Title are one line.
type Text struct {
	Alt     string
	Caption string
	Title   string
}

// Text returns the file's describing text from its Metadata.
func (f File) Text() Text {
	return Text{Alt: f.Metadata[MetaAlt], Caption: f.Metadata[MetaCaption], Title: f.Metadata[MetaTitle]}
}

// textFields pairs each reserved key with its limit and whether it may
// hold line breaks.
var textFields = []struct {
	key       string
	max       int
	multiline bool
}{
	{MetaAlt, MaxAltChars, false},
	{MetaCaption, MaxCaptionChars, true},
	{MetaTitle, MaxTitleChars, false},
}

// checkText refuses reserved text in metadata that is invalid UTF-8,
// over its limit, or holds control characters (line breaks are allowed in
// a caption only).
func checkText(m map[string]string) error {
	for _, fld := range textFields {
		v := m[fld.key]
		bad := func(r rune) bool { return unicode.IsControl(r) && !(fld.multiline && r == '\n') }
		if !utf8.ValidString(v) || utf8.RuneCountInString(v) > fld.max || strings.IndexFunc(v, bad) >= 0 {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: %s must be valid UTF-8 of at most %d characters without control characters", fld.key, fld.max)
		}
	}
	return nil
}

// SetText replaces a file's describing text (all three fields; "" removes
// one) and returns the file as stored. The rest of its Metadata is kept.
// It works on a trashed file too. The row is written only if its metadata
// is still what was read, so a concurrent metadata writer is never
// silently overwritten: that race is CodeUnavailable, to retry. Setting
// the current text changes nothing. It emits no event.
func (s *Store) SetText(ctx context.Context, scope, id string, t Text) (File, error) {
	next := map[string]string{MetaAlt: t.Alt, MetaCaption: t.Caption, MetaTitle: t.Title}
	if err := checkText(next); err != nil {
		return File{}, err
	}
	var out File
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		// The row with its metadata as stored: the write below matches
		// it as stored, since re-encoding could differ from a row another
		// writer formatted.
		var r fileRow
		q := sqlb.NewSelect(s.dialect).Columns(fileBindings(&r)...).From(TableFiles).Where(sqlb.Eq(FileScope, scope), sqlb.Eq(FileID, id))
		if err := s.One(ctx, q); err != nil {
			return notFound(err, "file", id)
		}
		f, raw := r.decode(), r.metadata
		meta := maps.Clone(decodeMetadata(raw))
		if meta == nil {
			meta = map[string]string{}
		}
		for k, v := range next {
			if v == "" {
				delete(meta, k)
			} else {
				meta[k] = v
			}
		}
		out = f
		if maps.Equal(meta, f.Metadata) {
			return nil
		}
		upd := sqlb.NewUpdate(s.dialect).Table(TableFiles).Set(sqlb.Set(FileMetadata, encodeMetadata(meta))).
			Where(sqlb.Eq(FileScope, scope), sqlb.Eq(FileID, id), sqlb.Eq(FileMetadata, raw))
		n, err := s.Exec(ctx, upd)
		if err != nil {
			return err
		}
		if n != 1 {
			return filesystem.Errorf(filesystem.CodeUnavailable, "records: file %q changed while its text was set; retry", id)
		}
		if len(meta) == 0 {
			meta = nil
		}
		out.Metadata = meta
		return nil
	})
	if err != nil {
		return File{}, err
	}
	return out, nil
}
