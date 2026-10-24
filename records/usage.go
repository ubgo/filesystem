package records

import (
	"context"

	"github.com/ubgo/sqlb"
)

// UsageQuery selects a page of the places one file is used: its
// attachments, under any owner and role.
type UsageQuery struct {
	Scope  string
	FileID string
	// Limit caps the page; 0 means DefaultPageSize, at most MaxPageSize.
	Limit int
	// Cursor continues from UsagePage.NextCursor of the same query.
	Cursor string
}

// UsagePage is one page of a file's attachments, newest first.
type UsagePage struct {
	Usages     []Attachment
	NextCursor string
}

// UsagesOf lists where a file is used (every attachment of it: owner and
// role), newest first with keyset pagination on (created_at, id), so a
// "used in 3 posts" panel or a check before purging a shared asset reads
// it a page at a time however widely the file is used. It reads the
// fs_attachments_file index. A file that does not exist, or is attached
// nowhere, has an empty page.
func (s *Store) UsagesOf(ctx context.Context, uq UsageQuery) (UsagePage, error) {
	limit, err := pageLimit(uq.Limit)
	if err != nil {
		return UsagePage{}, err
	}
	where := []sqlb.Predicate{sqlb.Eq(AttachmentScope, uq.Scope), sqlb.Eq(AttachmentFileID, uq.FileID)}
	if uq.Cursor != "" {
		at, id, err := DecodeCursor(uq.Cursor)
		if err != nil {
			return UsagePage{}, err
		}
		where = append(where, sqlb.RowLess(sqlb.Row(AttachmentCreatedAt, AttachmentID), sqlb.Row(sqlb.Param(at), sqlb.Param(id))))
	}
	var a Attachment
	q := sqlb.NewSelect(s.dialect).Columns(attachmentBindings(&a)...).From(TableAttachments).Where(where...).
		Order(sqlb.OrderBy(AttachmentCreatedAt, sqlb.SortDesc), sqlb.OrderBy(AttachmentID, sqlb.SortDesc)).Limit(limit + 1)
	var page UsagePage
	err = s.Each(ctx, q, func() error {
		a.CreatedAt = a.CreatedAt.UTC()
		page.Usages = append(page.Usages, a)
		return nil
	})
	if err != nil {
		return UsagePage{}, err
	}
	if len(page.Usages) > limit {
		page.Usages = page.Usages[:limit]
		last := page.Usages[limit-1]
		page.NextCursor = EncodeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}
