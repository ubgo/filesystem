package records

import "github.com/ubgo/filesystem"

// Action names what a request wants to do to files or attachments, for an
// authorization rule: recordshttp's Config.Authorize receives one, and
// permission plugins (acl) map them to their own levels.
//
// Why it lives here and not in recordshttp: a permission plugin decides on
// actions without serving HTTP, so the vocabulary belongs to the layer
// both import. recordshttp.Action is an alias of this type, so code that
// names recordshttp.ActionRead keeps compiling and the two are one type.
type Action string

// The closed set of actions.
const (
	ActionUpload  Action = "upload"  // add files to an owner's role
	ActionList    Action = "list"    // list an owner's files in a role
	ActionReorder Action = "reorder" // change the order of an owner's files
	ActionDetach  Action = "detach"  // remove an attachment
	ActionRead    Action = "read"    // get a link to a file
	ActionReplace Action = "replace" // upload new content for a file
	ActionTrash   Action = "trash"   // move a file to the trash
	ActionRestore Action = "restore" // take a file out of the trash
	ActionPurge   Action = "purge"   // delete a file for good
	// ActionSetAttachment makes an existing file the only one of an
	// owner's role (Store.SetAttachment). recordshttp also authorizes the
	// same request for ActionRead on the file, because attaching it
	// exposes it through the owner's list.
	ActionSetAttachment Action = "set_attachment"
	ActionSetVisibility Action = "set_visibility" // make a file public or private
	ActionSetText       Action = "set_text"       // set a file's alt text, caption and title
)

// ActionValues lists every Action.
var ActionValues = []Action{
	ActionUpload, ActionList, ActionReorder, ActionDetach, ActionRead, ActionReplace,
	ActionTrash, ActionRestore, ActionPurge, ActionSetAttachment, ActionSetVisibility, ActionSetText,
}

// ActionTarget is what an action applies to. Owner and Role are set for
// the owner routes (and for a detach, from the attachment); FileID for the
// file routes, the detach and the set-attachment routes. File is the file
// loaded before the rule is asked, set on every route that names a file,
// nil elsewhere: a rule can decide on its UploadedBy, Visibility or
// DeletedAt without another query. Loading first means a file of another
// scope is not found and never reaches the rule; a rule must not modify
// *File. Change is the change the request asks for, on the routes that
// carry one in their body (see Change). recordshttp.Target is an alias of
// this type.
type ActionTarget struct {
	Scope  string
	Owner  Owner
	Role   string
	FileID string
	File   *File
	Change *Change
}

// Change is what a request asks to change, read from its size-capped body
// before the rule is asked, so a rule can decide on the change as well as
// on the file: "only admins publish", "a caption needs only Read". It is
// set on ActionSetVisibility (Visibility), ActionSetText (Text) and
// ActionSetAttachment (Previous), and nil on every other action. On the
// visibility and text routes it is also nil when the body could not be
// read: the request is then refused with 400 once the rule allows it, and
// a rule refusing it still answers 403 first, so a refused caller learns
// nothing from the body's shape. A rule must not modify it.
//
// Why the body is read first: without it, a rule sees only that the file
// is changing, so "publishing needs Admin, unpublishing needs Write"
// could not be written in any Authorize, OpenFGA and Cedar included.
type Change struct {
	// Visibility is the visibility asked for (ActionSetVisibility).
	Visibility filesystem.Visibility
	// Text is the whole describing text asked for (ActionSetText).
	Text *Text
	// Previous is what happens to the files a set-attachment detaches
	// (ActionSetAttachment), records.PreviousKeep when the body named none.
	Previous Previous
}
