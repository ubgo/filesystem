package records

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// AttachedTo is the condition on TableFiles that keeps the files of scope
// attached to owner, under role or, with role "", under any role. Put it
// in FileQuery.Filters to list, page and filter an owner's files like any
// listing ("this post's PDFs over 1 MB, newest first"), or combine it with
// a plugin's filter (a folder, a tag). The owner type must be registered
// (WithOwnerTypes); a bad owner or role is CodeInvalidArgument.
func (s *Store) AttachedTo(scope string, owner Owner, role string) (sqlb.Predicate, error) {
	check := role
	if check == "" {
		check = "any" // any valid role name: only the owner is checked
	}
	if err := s.checkOwner(owner, check); err != nil {
		return nil, err
	}
	where := []sqlb.Predicate{sqlb.Eq(AttachmentScope, scope), sqlb.Eq(AttachmentOwnerType, owner.Type), sqlb.Eq(AttachmentOwnerID, owner.ID)}
	if role != "" {
		where = append(where, sqlb.Eq(AttachmentRole, role))
	}
	attached := sqlb.NewSelect(s.dialect).Exprs(AttachmentFileID).From(TableAttachments).Where(where...)
	return sqlb.InSubquery(FileID, attached), nil
}

// AttachmentQuery selects a page of an owner's attachments under a role.
type AttachmentQuery struct {
	Scope string
	Owner Owner
	Role  string
	// Limit caps the page; 0 means DefaultPageSize, at most MaxPageSize.
	Limit int
	// Cursor continues from AttachmentPage.NextCursor.
	Cursor string
}

// AttachmentPage is one page of attached live files in position order.
type AttachmentPage struct {
	Files      []AttachedFile
	NextCursor string
}

// AttachmentsPage is Attachments a page at a time, for owners with many
// files (a gallery of thousands): the same live files in the same
// (position, id) order, with keyset pagination on it. Reorder changes
// positions, so a cursor taken before a reorder may skip or repeat items.
func (s *Store) AttachmentsPage(ctx context.Context, aq AttachmentQuery) (AttachmentPage, error) {
	if err := s.checkOwner(aq.Owner, aq.Role); err != nil {
		return AttachmentPage{}, err
	}
	limit, err := pageLimit(aq.Limit)
	if err != nil {
		return AttachmentPage{}, err
	}
	where := append(ownerWhere(aq.Scope, aq.Owner, aq.Role), sqlb.IsNull(FileDeletedAt))
	if aq.Cursor != "" {
		pos, id, err := decodePositionCursor(aq.Cursor)
		if err != nil {
			return AttachmentPage{}, err
		}
		where = append(where, sqlb.RowGreater(sqlb.Row(AttachmentPosition, AttachmentID), sqlb.Row(sqlb.Param(pos), sqlb.Param(id))))
	}
	var a Attachment
	var r fileRow
	q := sqlb.NewSelect(s.dialect).Columns(append(attachmentBindings(&a), fileBindings(&r)...)...).
		From(TableAttachments).
		Join(sqlb.Join{Type: sqlb.JoinInner, Table: TableFiles, On: []sqlb.Predicate{sqlb.EqExpr(FileID, AttachmentFileID), sqlb.EqExpr(FileScope, AttachmentScope)}}).
		Where(where...).
		Order(sqlb.OrderBy(AttachmentPosition, sqlb.SortAsc), sqlb.OrderBy(AttachmentID, sqlb.SortAsc)).Limit(limit + 1)
	var page AttachmentPage
	err = s.Each(ctx, q, func() error {
		page.Files = append(page.Files, AttachedFile{Attachment: a, File: r.decode()})
		return nil
	})
	if err != nil {
		return AttachmentPage{}, err
	}
	if len(page.Files) > limit {
		page.Files = page.Files[:limit]
		last := page.Files[limit-1].Attachment
		page.NextCursor = encodePositionCursor(last.Position, last.ID)
	}
	return page, nil
}

// pageLimit applies DefaultPageSize and MaxPageSize to a requested limit.
func pageLimit(limit int) (int, error) {
	switch {
	case limit < 0 || limit > MaxPageSize:
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: limit %d is not in [0, %d]", limit, MaxPageSize)
	case limit == 0:
		return DefaultPageSize, nil
	}
	return limit, nil
}

// encodePositionCursor is the keyset cursor after the attachment
// (position, id) of a listing in position order.
func encodePositionCursor(pos int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(pos, 10) + cursorSeparator + id))
}

func decodePositionCursor(c string) (int64, string, error) {
	invalid := filesystem.Errorf(filesystem.CodeInvalidArgument, "records: invalid cursor")
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", invalid
	}
	p, id, ok := strings.Cut(string(raw), cursorSeparator)
	n, err := strconv.ParseInt(p, 10, 64)
	if !ok || err != nil || id == "" {
		return 0, "", invalid
	}
	return n, id, nil
}

// Facet is a column files can be counted by.
type Facet string

// The closed set of facets: the columns that hold a small set of values,
// so a count by them is a short list. A folder facet is the folders
// plugin's (FileCounts), since folders are its rows.
const (
	FacetContentType Facet = "content_type"
	FacetVisibility  Facet = "visibility"
)

// FacetValues lists every Facet.
var FacetValues = []Facet{FacetContentType, FacetVisibility}

// FacetCount is one value of a facet with how many files have it and
// their bytes.
type FacetCount struct {
	Value string
	Files int64
	Bytes int64
}

// Facets counts the files FileQuery selects (its Scope, Trashed and
// Filters; Limit and Cursor must be unset, as a count has no pages) by
// facet, most files first, then by value: the sidebar of a media library
// ("images 120, PDFs 14"). Content types are counted as stored, with any
// parameters ("text/plain; charset=utf-8"). An unknown facet, or a query
// with Limit or Cursor, is CodeInvalidArgument.
func (s *Store) Facets(ctx context.Context, fq FileQuery, facet Facet) ([]FacetCount, error) {
	var col sqlb.Expr[string]
	switch facet {
	case FacetContentType:
		col = FileContentType
	case FacetVisibility:
		col = sqlb.Col[string](TableFiles, ColFileVisibility)
	default:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: unknown facet %q", facet)
	}
	if fq.Limit != 0 || fq.Cursor != "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: Facets counts every file the query selects; Limit and Cursor do not apply")
	}
	var c FacetCount
	var out []FacetCount
	n := sqlb.Count()
	bytes := sqlb.Coalesce(sqlb.Sum(FileSize), sqlb.Param[int64](0))
	q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(col, &c.Value), sqlb.Bind(n, &c.Files), sqlb.Bind(bytes, &c.Bytes)).
		From(TableFiles).Where(fileWhere(fq)...).
		GroupBy(col).Order(sqlb.OrderBy(n, sqlb.SortDesc), sqlb.OrderBy(col, sqlb.SortAsc))
	if err := s.Each(ctx, q, func() error { out = append(out, c); return nil }); err != nil {
		return nil, err
	}
	return out, nil
}

// fileWhere is the scope, trash and Filters conditions of fq, shared by
// Files and Facets.
func fileWhere(fq FileQuery) []sqlb.Predicate {
	where := []sqlb.Predicate{sqlb.Eq(FileScope, fq.Scope)}
	if fq.Trashed {
		where = append(where, sqlb.IsNotNull(FileDeletedAt))
	} else {
		where = append(where, sqlb.IsNull(FileDeletedAt))
	}
	return append(where, fq.Filters...)
}
