package records

import (
	"context"
	"database/sql"
	"regexp"
	"slices"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// rolePattern constrains attachment roles ("avatar", "gallery", "cover"):
// application-chosen names compared as plain strings.
var rolePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// ownerIDMaxBytes is the indexed width of owner_id on MySQL.
const ownerIDMaxBytes = 191

func (s *Store) checkOwner(o Owner, role string) error {
	if !s.ownerTypes[o.Type] {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: owner type %q is not registered (WithOwnerTypes)", o.Type)
	}
	if o.ID == "" || len(o.ID) > ownerIDMaxBytes {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: owner ID must be 1 to %d bytes", ownerIDMaxBytes)
	}
	if !rolePattern.MatchString(role) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: role %q must match %s", role, rolePattern)
	}
	return nil
}

// Attach links a live file of scope to owner under role, as the last
// position. Attaching the same file twice to the same owner and role is
// CodeExists; a trashed or unknown file is CodeNotFound.
func (s *Store) Attach(ctx context.Context, scope, fileID string, owner Owner, role string) (Attachment, error) {
	if err := s.checkOwner(owner, role); err != nil {
		return Attachment{}, err
	}
	var a Attachment
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		// A shared lock on the file until the commit: a purge racing this
		// attach waits for it, then sees the attachment and keeps the file.
		f, err := s.lockedFile(ctx, scope, fileID, lockShare)
		if err != nil {
			return err
		}
		if f.DeletedAt != nil {
			return filesystem.Errorf(filesystem.CodeNotFound, "records: file %q is in the trash", fileID)
		}
		var last int64
		lastQ := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(sqlb.Coalesce(sqlb.Max(AttachmentPosition), sqlb.Param[int64](0)), &last)).
			From(TableAttachments).Where(ownerWhere(scope, owner, role)...)
		if err := s.One(ctx, lastQ); err != nil {
			return err
		}
		id := s.ids(IDPrefixAttachment)
		if s.singleRoles[role] {
			id = singleRoleID(scope, owner, role)
		}
		limited := s.roleLimits[role] > 0
		if limited {
			if id, err = s.freeSlot(ctx, scope, fileID, owner, role); err != nil {
				return err
			}
		}
		a = Attachment{ID: id, Scope: scope, FileID: fileID, Owner: owner, Role: role, Position: last + 1, CreatedAt: s.clock()}
		ins := sqlb.NewInsert(s.dialect).Into(TableAttachments).Row(
			sqlb.Set(AttachmentID, a.ID), sqlb.Set(AttachmentScope, a.Scope), sqlb.Set(AttachmentFileID, a.FileID),
			sqlb.Set(AttachmentOwnerType, owner.Type), sqlb.Set(AttachmentOwnerID, owner.ID), sqlb.Set(AttachmentRole, role),
			sqlb.Set(AttachmentPosition, a.Position), sqlb.Set(AttachmentCreatedAt, a.CreatedAt),
		)
		if _, err := s.Exec(ctx, ins); err != nil {
			if IsUniqueViolation(err) {
				if s.singleRoles[role] {
					return filesystem.Errorf(filesystem.CodeExists, "records: %s %q already has a file as %q, which holds one (use SetAttachment)", owner.Type, owner.ID, role)
				}
				if limited {
					// freeSlot ruled out the file being attached already, so
					// the clash is another Attach taking the same slot.
					return filesystem.Errorf(filesystem.CodeUnavailable, "records: another request took the free place of %s %q under %q; retry", owner.Type, owner.ID, role)
				}
				return filesystem.Errorf(filesystem.CodeExists, "records: file %q is already attached to %s %q as %q", fileID, owner.Type, owner.ID, role)
			}
			return err
		}
		return s.emit(ctx, Event{Kind: EventAttachmentCreated, Scope: scope, FileID: fileID, File: fileRef(f), AttachmentID: a.ID, Owner: owner, Role: role})
	})
	if err != nil {
		return Attachment{}, err
	}
	return a, nil
}

func ownerWhere(scope string, o Owner, role string) []sqlb.Predicate {
	return []sqlb.Predicate{
		sqlb.Eq(AttachmentScope, scope), sqlb.Eq(AttachmentOwnerType, o.Type),
		sqlb.Eq(AttachmentOwnerID, o.ID), sqlb.Eq(AttachmentRole, role),
	}
}

// Attachment returns one attachment of scope, or CodeNotFound.
func (s *Store) Attachment(ctx context.Context, scope, id string) (Attachment, error) {
	var a Attachment
	q := sqlb.NewSelect(s.dialect).Columns(attachmentBindings(&a)...).From(TableAttachments).
		Where(sqlb.Eq(AttachmentScope, scope), sqlb.Eq(AttachmentID, id))
	if err := s.One(ctx, q); err != nil {
		return Attachment{}, notFound(err, "attachment", id)
	}
	return a, nil
}

// Detach removes an attachment; the file itself stays. Detaching an
// unknown attachment succeeds, so cleanup code can call it unconditionally.
func (s *Store) Detach(ctx context.Context, scope, attachmentID string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		a, err := s.Attachment(ctx, scope, attachmentID)
		if filesystem.CodeOf(err) == filesystem.CodeNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		q := sqlb.NewDelete(s.dialect).From(TableAttachments).Where(sqlb.Eq(AttachmentScope, scope), sqlb.Eq(AttachmentID, attachmentID))
		if _, err := s.Exec(ctx, q); err != nil {
			return err
		}
		f, err := s.File(ctx, scope, a.FileID)
		if err != nil {
			return err
		}
		return s.emit(ctx, Event{Kind: EventAttachmentDeleted, Scope: scope, FileID: a.FileID, File: fileRef(f), AttachmentID: a.ID, Owner: a.Owner, Role: a.Role})
	})
}

// AttachedFile is an attachment together with its file.
type AttachedFile struct {
	Attachment Attachment
	File       File
}

// Attachments returns the live files attached to owner under role, in
// position order. Trashed files are left out.
func (s *Store) Attachments(ctx context.Context, scope string, owner Owner, role string) ([]AttachedFile, error) {
	if err := s.checkOwner(owner, role); err != nil {
		return nil, err
	}
	var a Attachment
	var r fileRow
	q := sqlb.NewSelect(s.dialect).Columns(append(attachmentBindings(&a), fileBindings(&r)...)...).
		From(TableAttachments).
		Join(sqlb.Join{Type: sqlb.JoinInner, Table: TableFiles, On: []sqlb.Predicate{sqlb.EqExpr(FileID, AttachmentFileID), sqlb.EqExpr(FileScope, AttachmentScope)}}).
		Where(append(ownerWhere(scope, owner, role), sqlb.IsNull(FileDeletedAt))...).
		Order(sqlb.OrderBy(AttachmentPosition, sqlb.SortAsc), sqlb.OrderBy(AttachmentID, sqlb.SortAsc))
	var out []AttachedFile
	err := s.Each(ctx, q, func() error {
		out = append(out, AttachedFile{Attachment: a, File: r.decode()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Reorder sets the order of owner's attachments under role: ids must list
// every attachment of that owner and role that Attachments returns (the
// live files) exactly once, in the new order. Anything else is
// CodeInvalidArgument and changes nothing, so a client working from a
// stale list cannot silently drop an item. Attachments of trashed files,
// which clients do not see, keep their relative order after the listed
// ones, so a restored file comes back at the end.
func (s *Store) Reorder(ctx context.Context, scope string, owner Owner, role string, ids []string) error {
	if err := s.checkOwner(owner, role); err != nil {
		return err
	}
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var id string
		var deletedAt *time.Time
		var live, trashed []string
		q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(AttachmentID, &id), sqlb.Bind(FileDeletedAt, &deletedAt)).
			From(TableAttachments).
			Join(sqlb.Join{Type: sqlb.JoinInner, Table: TableFiles, On: []sqlb.Predicate{sqlb.EqExpr(FileID, AttachmentFileID), sqlb.EqExpr(FileScope, AttachmentScope)}}).
			Where(ownerWhere(scope, owner, role)...).
			Order(sqlb.OrderBy(AttachmentPosition, sqlb.SortAsc), sqlb.OrderBy(AttachmentID, sqlb.SortAsc))
		err := s.Each(ctx, q, func() error {
			if deletedAt != nil {
				trashed = append(trashed, id)
			} else {
				live = append(live, id)
			}
			return nil
		})
		if err != nil {
			return err
		}
		want := slices.Clone(ids)
		slices.Sort(live)
		slices.Sort(want)
		if !slices.Equal(live, want) {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: reorder must list each of the %d attachments exactly once", len(live))
		}
		for i, aid := range append(slices.Clone(ids), trashed...) {
			upd := sqlb.NewUpdate(s.dialect).Table(TableAttachments).Set(sqlb.Set(AttachmentPosition, int64(i+1))).
				Where(sqlb.Eq(AttachmentScope, scope), sqlb.Eq(AttachmentID, aid))
			if _, err := s.Exec(ctx, upd); err != nil {
				return err
			}
		}
		return s.emit(ctx, Event{Kind: EventAttachmentsOrdered, Scope: scope, Owner: owner, Role: role})
	})
}
