package records

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strconv"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// singleRoleIDBytes is how much of the SHA-256 a single role's attachment
// ID keeps: 128 bits, as random IDs have.
const singleRoleIDBytes = 16

// singleRoleID is the one attachment ID an owner's single role can have.
// Because it is derived rather than random, the primary key admits one
// row per owner and role: two concurrent writers cannot both attach,
// which no portable unique index could express (MySQL has no partial
// indexes, and locking zero rows does not stop a Postgres insert).
func singleRoleID(scope string, o Owner, role string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + o.Type + "\x00" + o.ID + "\x00" + role))
	return IDPrefixAttachment + hex.EncodeToString(sum[:singleRoleIDBytes])
}

// slotID is the attachment ID of slot n (from 1) of an owner's limited
// role (WithRoleLimit): derived, like singleRoleID, so the primary key
// admits at most the role's limit of rows per owner.
func slotID(scope string, o Owner, role string, n int) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + o.Type + "\x00" + o.ID + "\x00" + role + "\x00" + strconv.Itoa(n)))
	return IDPrefixAttachment + hex.EncodeToString(sum[:singleRoleIDBytes])
}

// freeSlot returns the ID of the first free slot of an owner's limited
// role, CodeExists when fileID is already attached there, and
// CodeQuotaExceeded when every slot is taken.
func (s *Store) freeSlot(ctx context.Context, scope, fileID string, owner Owner, role string) (string, error) {
	var id, file string
	used := map[string]bool{}
	q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(AttachmentID, &id), sqlb.Bind(AttachmentFileID, &file)).
		From(TableAttachments).Where(ownerWhere(scope, owner, role)...)
	attached := false
	if err := s.Each(ctx, q, func() error { used[id], attached = true, attached || file == fileID; return nil }); err != nil {
		return "", err
	}
	if attached {
		return "", filesystem.Errorf(filesystem.CodeExists, "records: file %q is already attached to %s %q as %q", fileID, owner.Type, owner.ID, role)
	}
	limit := s.roleLimits[role]
	for n := 1; n <= limit; n++ {
		if slot := slotID(scope, owner, role, n); !used[slot] {
			return slot, nil
		}
	}
	return "", filesystem.Errorf(filesystem.CodeQuotaExceeded, "records: %s %q holds at most %d files as %q", owner.Type, owner.ID, limit, role)
}

// Previous says what SetAttachment does with the files it detaches.
type Previous string

// The closed set of Previous values.
const (
	// PreviousKeep only detaches them.
	PreviousKeep Previous = "keep"
	// PreviousTrash moves them to the trash (restorable).
	PreviousTrash Previous = "trash"
	// PreviousPurge deletes them for good, objects after the commit.
	PreviousPurge Previous = "purge"
)

// PreviousValues lists every Previous.
var PreviousValues = []Previous{PreviousKeep, PreviousTrash, PreviousPurge}

// SetAttachment makes fileID the only file of owner under role, in one
// transaction: the user's new avatar replacing the old. The files it
// detaches are kept, trashed or purged as previous says, but only when no
// other attachment uses them: a file shared with another owner is only
// detached. Setting the file already attached alone does nothing. Two
// concurrent calls on a single role (WithSingleRoles) cannot both win:
// the loser fails and can retry.
func (s *Store) SetAttachment(ctx context.Context, scope, fileID string, owner Owner, role string, previous Previous) (Attachment, error) {
	if previous != PreviousKeep && previous != PreviousTrash && previous != PreviousPurge {
		return Attachment{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: unknown Previous %q", previous)
	}
	var out Attachment
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		current, err := s.Attachments(ctx, scope, owner, role)
		if err != nil {
			return err
		}
		if len(current) == 1 && current[0].File.ID == fileID {
			out = current[0].Attachment
			return nil
		}
		// Attachments leaves out trashed files; detach those too.
		var a Attachment
		var all []Attachment
		q := sqlb.NewSelect(s.dialect).Columns(attachmentBindings(&a)...).From(TableAttachments).Where(ownerWhere(scope, owner, role)...)
		if err := s.Each(ctx, q, func() error { all = append(all, a); return nil }); err != nil {
			return err
		}
		for _, old := range all {
			if err := s.Detach(ctx, scope, old.ID); err != nil {
				return err
			}
		}
		if out, err = s.Attach(ctx, scope, fileID, owner, role); err != nil {
			return err
		}
		for _, old := range all {
			if old.FileID == fileID || previous == PreviousKeep {
				continue
			}
			if _, err := s.disposeCounted(ctx, scope, old.FileID, previous); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Attachment{}, err
	}
	return out, nil
}

// disposeCounted trashes or purges a detached file that nothing else
// uses, reporting whether it did.
func (s *Store) disposeCounted(ctx context.Context, scope, fileID string, previous Previous) (bool, error) {
	var n int64
	q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(sqlb.Count(), &n)).From(TableAttachments).
		Where(sqlb.Eq(AttachmentScope, scope), sqlb.Eq(AttachmentFileID, fileID))
	if err := s.One(ctx, q); err != nil {
		return false, err
	}
	switch {
	case n > 0:
		return false, nil
	case previous == PreviousTrash:
		return true, s.Trash(ctx, scope, fileID)
	}
	return true, s.Purge(ctx, scope, fileID)
}

// PurgeOwner deletes what an owner leaves behind when the application
// deletes it (a user closing their account, a post removed): every
// attachment of owner, in every role, and every file attached only to it,
// purged for good with its objects deleted after the commit. A file
// another owner still uses is only detached. Call it in the transaction
// that deletes the owner's own row, so both happen or neither. Returns
// how many files were purged.
func (s *Store) PurgeOwner(ctx context.Context, scope string, owner Owner) (int, error) {
	if !s.ownerTypes[owner.Type] {
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: owner type %q is not registered (WithOwnerTypes)", owner.Type)
	}
	purged := 0
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var a Attachment
		var all []Attachment
		q := sqlb.NewSelect(s.dialect).Columns(attachmentBindings(&a)...).From(TableAttachments).
			Where(sqlb.Eq(AttachmentScope, scope), sqlb.Eq(AttachmentOwnerType, owner.Type), sqlb.Eq(AttachmentOwnerID, owner.ID))
		if err := s.Each(ctx, q, func() error { all = append(all, a); return nil }); err != nil {
			return err
		}
		for _, old := range all {
			if err := s.Detach(ctx, scope, old.ID); err != nil {
				return err
			}
		}
		done := map[string]bool{}
		for _, old := range all {
			if done[old.FileID] {
				continue // attached under several roles
			}
			done[old.FileID] = true
			gone, err := s.disposeCounted(ctx, scope, old.FileID, PreviousPurge)
			if err != nil {
				return err
			}
			if gone {
				purged++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return purged, nil
}

// PurgeUnattached purges, for good and with their objects deleted after
// the commit, the files of scope that uploadedBy recorded (File.UploadedBy)
// and that no attachment uses, live or trashed: the uploads PurgeOwner
// cannot find, because it works from attachments (a file recorded and
// never attached, or detached with PreviousKeep). An owner is an
// application row and an uploader is an actor, and only the application
// knows which actor an owner was, so account deletion calls both in one
// transaction: PurgeOwner(ctx, scope, owner), then PurgeUnattached(ctx,
// scope, actor). Files the actor uploaded that another owner uses are
// attached, so they are kept.
//
// "Unattached" means no fs_attachments row: a file that a plugin places
// somewhere (a folder of a shared drive) is still unattached. filters, as
// in FileQuery.Filters, narrow the files further (records.FileCreatedAt
// before a date, a plugin's condition); a file a plugin refuses to purge
// (BeforePurge) fails the whole call. An empty uploadedBy is
// CodeInvalidArgument: it stands for every upload made without an actor,
// which is never one person's. Returns how many files were purged.
func (s *Store) PurgeUnattached(ctx context.Context, scope, uploadedBy string, filters ...sqlb.Predicate) (int, error) {
	if uploadedBy == "" {
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: PurgeUnattached needs an uploader; \"\" is every upload made without an actor")
	}
	purged := 0
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		attached := sqlb.NewSelect(s.dialect).Exprs(AttachmentFileID).From(TableAttachments).Where(sqlb.Eq(AttachmentScope, scope))
		where := append([]sqlb.Predicate{
			sqlb.Eq(FileScope, scope), sqlb.Eq(FileUploadedBy, uploadedBy), sqlb.NotInSubquery(FileID, attached),
		}, filters...)
		var id string
		var ids []string
		q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(FileID, &id)).From(TableFiles).Where(where...).Order(sqlb.OrderBy(FileID, sqlb.SortAsc))
		if err := s.Each(ctx, q, func() error { ids = append(ids, id); return nil }); err != nil {
			return err
		}
		for _, id := range ids {
			// Checked again under the file's lock: an Attach that raced the
			// listing above has committed by now and is seen, so the file
			// it attached is kept.
			_, err := s.lockedFile(ctx, scope, id, lockUpdate)
			if filesystem.CodeOf(err) == filesystem.CodeNotFound {
				continue // purged meanwhile
			}
			if err != nil {
				return err
			}
			used, err := s.attached(ctx, scope, id)
			if err != nil {
				return err
			}
			if used {
				continue
			}
			if err := s.Purge(ctx, scope, id); err != nil {
				return err
			}
			purged++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return purged, nil
}
