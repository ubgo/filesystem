package records

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// Retention is a rule of how long files stay attached under an owner type
// and role: an attachment older than MaxAge is removed by SweepRetention,
// and its file purged once nothing else uses it ("chat attachments 90
// days", "support uploads 1 year"). Which rules apply is the
// application's decision; the library only applies them.
type Retention struct {
	// OwnerType is a registered owner type (WithOwnerTypes).
	OwnerType string
	// Role limits the rule to one role; "" applies it to every role of
	// the owner type.
	Role string
	// MaxAge is how long an attachment is kept, from its creation.
	MaxAge time.Duration
}

// WithRetention installs retention rules for SweepRetention. Without it
// the sweep does nothing. A rule needs a valid owner type (registered with
// WithOwnerTypes, in any option order), a valid or empty role and a
// positive MaxAge; a bad rule fails New.
func WithRetention(rules ...Retention) Option {
	return func(s *Store) error {
		for _, r := range rules {
			if err := checkRetention(r); err != nil {
				return err
			}
			s.retention = append(s.retention, r)
		}
		return nil
	}
}

// checkRetention refuses a rule without a valid owner type, with a bad
// role, or with no positive MaxAge.
func checkRetention(r Retention) error {
	if !validOwnerType(r.OwnerType) || (r.Role != "" && !rolePattern.MatchString(r.Role)) || r.MaxAge <= 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: retention rule %+v needs an owner type, a valid or empty role and a positive MaxAge", r)
	}
	return nil
}

// RetentionReport is what SweepRetention did, or would do in a dry run.
type RetentionReport struct {
	// Detached are the attachments removed, oldest first per rule.
	Detached []Attachment
	// Purged are the IDs of the files purged because their last
	// attachment was removed (a file still used elsewhere is kept).
	Purged []string
}

// SweepRetention applies the retention rules at now, in every scope: each
// attachment older than its rule's MaxAge is detached, and its file is
// purged when no attachment is left (Purge, so plugins' hooks run and a
// shared object stays for its other files). Each attachment is handled in
// its own transaction; one that fails does not stop the sweep, and the
// returned error names each. With dryRun nothing changes and the report
// says what a real run would do, so an application can show it, or log
// it, before switching a new rule on. Schedule it; the library starts no
// background work.
func (s *Store) SweepRetention(ctx context.Context, now time.Time, dryRun bool) (RetentionReport, error) {
	return s.sweepRetention(ctx, s.retention, nil, now, dryRun)
}

// SweepRetentionIn applies rules, instead of WithRetention's, to one scope
// only: retention that depends on the tenant ("free plan 30 days, pro 1
// year"). The application decides each scope's rules from its own plans
// and calls it per scope, from the job that would call SweepRetention;
// the library keeps no table of plans. It behaves as SweepRetention in
// every other way (per-attachment transactions, joined errors, dryRun).
// Each rule is checked as WithRetention checks it, and its owner type
// must be registered with WithOwnerTypes; a bad rule is
// CodeInvalidArgument and sweeps nothing. No rules sweep nothing.
// WithRetention's rules still apply to every scope through
// SweepRetention, so a scope swept here should not also match those.
func (s *Store) SweepRetentionIn(ctx context.Context, scope string, rules []Retention, now time.Time, dryRun bool) (RetentionReport, error) {
	for _, r := range rules {
		if err := checkRetention(r); err != nil {
			return RetentionReport{}, err
		}
		if !s.ownerTypes[r.OwnerType] {
			return RetentionReport{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: retention rule for owner type %q, which WithOwnerTypes did not register", r.OwnerType)
		}
	}
	return s.sweepRetention(ctx, rules, &scope, now, dryRun)
}

// sweepRetention is SweepRetention and SweepRetentionIn: rules applied in
// every scope (scope nil) or in one.
func (s *Store) sweepRetention(ctx context.Context, rules []Retention, scope *string, now time.Time, dryRun bool) (RetentionReport, error) {
	var due []Attachment
	for _, r := range rules {
		cutoff := now.UTC().Add(-r.MaxAge)
		where := []sqlb.Predicate{sqlb.Eq(AttachmentOwnerType, r.OwnerType), sqlb.Less(AttachmentCreatedAt, cutoff)}
		if scope != nil {
			where = append(where, sqlb.Eq(AttachmentScope, *scope))
		}
		if r.Role != "" {
			where = append(where, sqlb.Eq(AttachmentRole, r.Role))
		}
		var a Attachment
		q := sqlb.NewSelect(s.dialect).Columns(attachmentBindings(&a)...).From(TableAttachments).Where(where...).
			Order(sqlb.OrderBy(AttachmentCreatedAt, sqlb.SortAsc), sqlb.OrderBy(AttachmentID, sqlb.SortAsc))
		if err := s.Each(ctx, q, func() error { due = append(due, a); return nil }); err != nil {
			return RetentionReport{}, err
		}
	}
	if dryRun {
		return s.retentionPlan(ctx, due)
	}
	var rep RetentionReport
	var errs []error
	for _, a := range due {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		purged, gone := false, false
		err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
			// The file is locked first, as Purge locks it, so an Attach
			// racing the sweep either lands before the check below (and
			// keeps the file) or waits and finds it gone.
			if _, err := s.lockForPurge(ctx, a.Scope, a.FileID); err != nil {
				if filesystem.CodeOf(err) == filesystem.CodeNotFound {
					gone = true // purged meanwhile, with its attachments
					return nil
				}
				return err
			}
			if err := s.Detach(ctx, a.Scope, a.ID); err != nil {
				return err
			}
			used, err := s.attached(ctx, a.Scope, a.FileID)
			if err != nil || used {
				return err
			}
			purged = true
			return s.Purge(ctx, a.Scope, a.FileID)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("records: retention of attachment %q of scope %q: %w", a.ID, a.Scope, err))
			continue
		}
		if gone {
			continue
		}
		rep.Detached = append(rep.Detached, a)
		if purged {
			rep.Purged = append(rep.Purged, a.FileID)
		}
	}
	return rep, errors.Join(errs...)
}

// retentionPlan is SweepRetention's dry run: due are detached, and a file
// is purged when every one of its attachments is due.
func (s *Store) retentionPlan(ctx context.Context, due []Attachment) (RetentionReport, error) {
	rep := RetentionReport{Detached: due}
	isDue := map[string]bool{}
	for _, a := range due {
		isDue[a.ID] = true
	}
	seen := map[string]bool{}
	for _, a := range due {
		if seen[a.Scope+"\x00"+a.FileID] {
			continue
		}
		seen[a.Scope+"\x00"+a.FileID] = true
		var id string
		kept := false
		q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(AttachmentID, &id)).From(TableAttachments).
			Where(sqlb.Eq(AttachmentScope, a.Scope), sqlb.Eq(AttachmentFileID, a.FileID))
		if err := s.Each(ctx, q, func() error { kept = kept || !isDue[id]; return nil }); err != nil {
			return RetentionReport{}, err
		}
		if !kept {
			rep.Purged = append(rep.Purged, a.FileID)
		}
	}
	return rep, nil
}
