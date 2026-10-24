package records

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// EventKind names a change the Store made.
type EventKind string

// The closed set of event kinds.
const (
	EventFileCreated  EventKind = "file.created"
	EventFileTrashed  EventKind = "file.trashed"
	EventFileRestored EventKind = "file.restored"
	// EventFileReplaced is a new content for the same file (Replace,
	// ReplaceObject).
	EventFileReplaced EventKind = "file.replaced"
	// EventFileVisibilityChanged is a file made public or private
	// (SetVisibility).
	EventFileVisibilityChanged EventKind = "file.visibility_changed"
	// EventFileStatusChanged is a file's processing status or detail
	// changed (SetStatus).
	EventFileStatusChanged EventKind = "file.status_changed"
	// EventFileReleased is a quarantined file released by its scan
	// (PassScan, Scan), right after its file.status_changed: the signal
	// for a plugin that skipped the file while it was quarantined to do
	// its work now (thumbnails, text extraction, labelling).
	EventFileReleased EventKind = "file.released"
	// EventFileMoved is a file's object moved to another disk (MoveFile);
	// Detail has DetailFrom and DetailTo, the disk names.
	EventFileMoved EventKind = "file.moved"
	// EventFilePurged also stands for the purged file's attachments, which
	// go with it without events of their own.
	EventFilePurged         EventKind = "file.purged"
	EventAttachmentCreated  EventKind = "attachment.created"
	EventAttachmentDeleted  EventKind = "attachment.deleted"
	EventAttachmentsOrdered EventKind = "attachments.reordered"
)

// EventKindValues lists every EventKind.
var EventKindValues = []EventKind{
	EventFileCreated, EventFileTrashed, EventFileRestored, EventFileReplaced, EventFileVisibilityChanged, EventFileStatusChanged, EventFileReleased, EventFileMoved, EventFilePurged,
	EventAttachmentCreated, EventAttachmentDeleted, EventAttachmentsOrdered,
}

// Event is one change. Which fields are set depends on Kind:
//
//   - file.created, file.trashed, file.restored, file.replaced,
//     file.visibility_changed, file.status_changed, file.released,
//     file.purged: FileID and File.
//   - file.moved: FileID, File (on its new disk) and Detail with
//     DetailFrom and DetailTo.
//   - attachment.created, attachment.deleted: FileID, File, AttachmentID,
//     Owner and Role.
//   - attachments.reordered: Owner and Role.
//   - a plugin's kinds ("<plugin name>.<action>", sent with Store.Emit):
//     whatever that plugin documents, usually FileID, Subject and Detail.
//
// ID, Kind, Scope and At are always set. The Store's own kinds never set
// Subject, and only file.moved sets Detail.
type Event struct {
	// ID identifies the event for consumers that may see it more than once
	// (webhook deliveries, billing feeds): a consumer that keeps the IDs it
	// processed can drop a redelivery. The Store gives every event a random
	// ID with IDPrefixEvent; a plugin may set its own, deterministic one
	// with Emit when it could report the same fact twice (a daily total
	// reported again after a crash), at most MaxEventIDBytes long.
	ID           string
	Kind         EventKind
	Scope        string
	At           time.Time
	FileID       string
	AttachmentID string
	Owner        Owner
	Role         string
	// File is the file the event is about as the change left it (as it
	// was, for file.purged, whose row is gone), so a hook, a webhook or a
	// log has its name, size, visibility and status without a query; nil
	// for attachments.reordered, and for a plugin's event unless that
	// plugin sets it. Hooks must not change it.
	File *File
	// Subject is the ID of what a plugin's event is about when that is
	// not a file: a folder, a share link. "" otherwise.
	Subject string
	// Detail carries a plugin event's particulars as documented by that
	// plugin, with keys it names as constants (a folder's new parent, the
	// tag added). Hooks must not change it. nil when there are none.
	Detail map[string]string
}

// EventHooks is a plugin told of every change the Store makes: audit logs,
// search indexes, webhooks, cache invalidation. OnEvent runs inside the
// change's transaction, after the change, so its own writes (via
// Store.Conn) commit or roll back with it, and an error rolls the change
// back. Work that must not run for a rolled-back change, such as calling
// a webhook, belongs in Store.AfterCommit. Trashing a trashed file,
// restoring a live one and detaching an unknown attachment change nothing
// and have no event.
type EventHooks interface {
	OnEvent(ctx context.Context, s *Store, e Event) error
}

// Detail keys of the Store's own events.
const (
	// DetailFrom and DetailTo are the old and new disk of file.moved.
	DetailFrom = "from"
	DetailTo   = "to"
)

// MaxEventKindBytes is the longest event kind: the width of an indexed
// short column (SQLTypes.Short), where a log such as the audit plugin
// stores it.
const MaxEventKindBytes = 64

// pluginActionPattern is what follows "<plugin name>." in a plugin's
// event kind.
var pluginActionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Emit tells every EventHooks plugin of e on behalf of the installed
// plugin from, so a plugin's own changes (a folder moved, a tag added, a
// share link created) reach audit logs and webhooks like the Store's.
// e.Kind must be from's name, a dot and an action of [a-z][a-z0-9_]*, at
// most MaxEventKindBytes in all, and never one of EventKindValues: a
// plugin cannot speak for the Store or for another plugin. Emit must run
// inside the transaction of the change it reports (Store.Tx), so the
// hooks' writes commit or roll back with that change; called outside one,
// or with a plugin that is not installed, it is CodeInvalidArgument. At is
// stamped by the Store. An error from a hook is returned, and the caller
// must fail its transaction with it, as the Store does.
func (s *Store) Emit(ctx context.Context, from Plugin, e Event) error {
	if from == nil || !s.installed(from.Name()) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: Emit from a plugin that is not installed")
	}
	action, ok := strings.CutPrefix(string(e.Kind), from.Name()+".")
	if !ok || !pluginActionPattern.MatchString(action) || len(e.Kind) > MaxEventKindBytes || slices.Contains(EventKindValues, e.Kind) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: plugin %q cannot emit %q: kinds are %q followed by [a-z][a-z0-9_]*, at most %d bytes", from.Name(), e.Kind, from.Name()+".", MaxEventKindBytes)
	}
	if txFrom(ctx) == nil {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: Emit must run inside the change's transaction (Store.Tx)")
	}
	return s.emit(ctx, e)
}

// installed reports whether a plugin named name is installed.
func (s *Store) installed(name string) bool {
	for _, p := range s.plugins {
		if p.Name() == name {
			return true
		}
	}
	return false
}

// fileRef is a copy of f for an Event, so no hook can change the caller's
// value.
func fileRef(f File) *File { return &f }

// MaxEventIDBytes is the longest Event.ID: the width of an indexed key
// column (SQLTypes.Key), where outboxes such as the webhooks plugin's
// store it.
const MaxEventIDBytes = 191

// IDPrefixEvent starts the IDs the Store gives events (Event.ID).
const IDPrefixEvent = "evt_"

// emit tells every EventHooks plugin of e, stamping its time and, when the
// emitter set none, its ID. The ID is random rather than from WithIDs, so
// an application's own ID scheme for files is not consumed by events.
func (s *Store) emit(ctx context.Context, e Event) error {
	e.At = s.clock()
	if e.ID == "" {
		e.ID = defaultID(IDPrefixEvent)
	} else if len(e.ID) > MaxEventIDBytes {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: event ID is longer than %d bytes", MaxEventIDBytes)
	}
	for _, p := range s.plugins {
		if h, ok := p.(EventHooks); ok {
			if err := h.OnEvent(ctx, s, e); err != nil {
				return err
			}
		}
	}
	return nil
}
