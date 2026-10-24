package records

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// ChangeOption narrows a change to one file (Replace, ReplaceObject,
// Trash, Restore, SetVisibility, Purge) to the state the caller read, so
// a decision made on that state cannot be applied to a file that changed
// since. Each option is checked with the file's row locked, inside the
// change's transaction.
type ChangeOption func(*changeOptions)

// PurgeOption is ChangeOption, the name Purge's options had first.
type PurgeOption = ChangeOption

type changeOptions struct {
	// ifSHA256 is IfUnchanged's digest, lowercase.
	ifSHA256 *string
	// trashed is IfTrashed's state.
	trashed *bool
	// stillMatches is the condition a sweep selected the file by
	// (purgeWhere): Purge re-checks it with the row locked and skips a
	// file that no longer meets it (errNoLongerDue).
	stillMatches []sqlb.Predicate
	// quarantine is QuarantineContent: the replacement's new content is
	// unscanned, so the file becomes StatusQuarantined with it.
	quarantine bool
}

// ErrChanged is the error of a change refused by IfUnchanged
// (CodeConflict): the file's content is no longer the one the caller
// read. Match it with errors.Is, as recordshttp does to answer 412.
var ErrChanged = filesystem.Errorf(filesystem.CodeConflict, "records: the file changed since it was read")

func changeOptionsOf(opts []ChangeOption) changeOptions {
	var o changeOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// IfUnchanged makes a change go ahead only while the file's content is
// still the one with digest sha256 (File.SHA256 as the caller read it,
// either case), and refuse with ErrChanged (CodeConflict), changing
// nothing, when a replacement landed since: optimistic concurrency, so
// an editor saving what it loaded, or a user hiding or deleting the
// version they looked at, cannot act on a newer content they never saw.
// The check runs with the file's row locked. A file recorded without a
// digest has SHA256 "", and IfUnchanged("") matches only such a file.
func IfUnchanged(sha256 string) ChangeOption {
	want := strings.ToLower(sha256)
	return func(o *changeOptions) { o.ifSHA256 = &want }
}

// IfTrashed makes Purge go ahead only if the file is in the trash
// (trashed true) or live (false) when its row is locked, and fail with
// CodeUnavailable, purging nothing, otherwise. A caller that decided to
// purge from a file it read earlier (an authorization rule allowing only
// trashed files to be purged) passes the state it saw, so a Restore or
// Trash that landed in between cannot slip a different file state past
// that decision. Only Purge takes it; any other change refuses it with
// CodeInvalidArgument.
func IfTrashed(trashed bool) ChangeOption {
	return func(o *changeOptions) { o.trashed = &trashed }
}

// errNoLongerDue is Purge's answer, inside a sweep, for a file that no
// longer meets the sweep's condition once its row is locked (restored, or
// its expiry moved, after the sweep selected it): nothing is purged and
// the sweep skips the file without counting it or reporting an error.
var errNoLongerDue = errors.New("records: the file no longer meets the sweep's condition")

// whileMatching makes Purge go ahead only while the file still meets
// where, checked with its row locked; otherwise Purge returns
// errNoLongerDue and changes nothing. It is how a sweep's selection and
// its purges agree although they run in different transactions.
func whileMatching(where []sqlb.Predicate) ChangeOption {
	return func(o *changeOptions) { o.stillMatches = where }
}

// guarded reports whether the options need the file's row locked:
// QuarantineContent does, so its public-file check and a racing
// SetVisibility serialize.
func (o changeOptions) guarded() bool { return o.ifSHA256 != nil || o.trashed != nil || o.quarantine }

// notForPurgeOnly refuses IfTrashed on a change other than Purge.
func (o changeOptions) notForPurgeOnly(call string) error {
	if o.trashed != nil {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: IfTrashed applies to Purge, not %s", call)
	}
	return nil
}

// QuarantineContent makes Replace (or ReplaceObject) quarantine the file
// with its new content (StatusQuarantined, detail cleared): bytes that
// replace scanned ones are unscanned again, and must not be served under
// the file's old verdict. The file must not be public
// (ErrPublicQuarantine). Only a replacement takes it; any other change
// refuses it with CodeInvalidArgument, so it is never silently ignored.
func QuarantineContent() ChangeOption {
	return func(o *changeOptions) { o.quarantine = true }
}

// notForReplaceOnly refuses QuarantineContent on a change other than a
// replacement.
func (o changeOptions) notForReplaceOnly(call string) error {
	if o.quarantine {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: QuarantineContent applies to Replace, not %s", call)
	}
	return nil
}

// recheck runs the options' check again inside the change's
// transaction, with f's row locked: f is the file the change was planned
// on (its object already changed to match), so a file whose object,
// content or trash state moved meanwhile is refused, and the
// transaction's rollback undoes the object change. Without options it
// does nothing, so unguarded changes keep their behaviour.
func (s *Store) recheck(ctx context.Context, f File, o changeOptions) error {
	if !o.guarded() {
		return nil
	}
	cur, err := s.lockedFile(ctx, f.Scope, f.ID, lockUpdate)
	if err != nil {
		return err
	}
	if err := o.check(cur); err != nil {
		return err
	}
	if cur.Disk != f.Disk || cur.Key != f.Key || (cur.DeletedAt == nil) != (f.DeletedAt == nil) || cur.Visibility != f.Visibility {
		return filesystem.Errorf(filesystem.CodeUnavailable, "records: file %q changed while the change was being made; retry", f.ID)
	}
	return nil
}

// check refuses cur when it is not the state the options name.
func (o changeOptions) check(cur File) error {
	if o.ifSHA256 != nil && *o.ifSHA256 != cur.SHA256 {
		return fmt.Errorf("%w: file %q has content %q, not %q", ErrChanged, cur.ID, cur.SHA256, *o.ifSHA256)
	}
	if o.trashed != nil && (cur.DeletedAt != nil) != *o.trashed {
		return filesystem.Errorf(filesystem.CodeUnavailable, "records: file %q was trashed or restored meanwhile; nothing was changed", cur.ID)
	}
	return nil
}
