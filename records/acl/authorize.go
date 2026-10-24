package acl

import (
	"net/http"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// LevelFor is the default level a file action needs: reading needs Read;
// replacing, trashing, restoring and changing visibility or text need
// Write; purging needs Admin. The owner routes and set-attachment are not
// decided by grants (ok false). WithLevelFor and WithLevelForTarget
// replace it.
//
// It takes records.Action, which recordshttp.Action is an alias of, so the
// plugin decides on actions without importing the HTTP layer.
func LevelFor(a records.Action) (Level, bool) {
	switch a {
	case records.ActionRead:
		return Read, true
	case records.ActionReplace, records.ActionTrash, records.ActionRestore, records.ActionSetVisibility, records.ActionSetText:
		return Write, true
	case records.ActionPurge:
		return Admin, true
	}
	return "", false
}

// Authorizer returns a recordshttp Config.Authorize that allows a file
// action when one of the request's subjects holds the level the plugin's
// action map gives it on the file (LevelFor, or the function of
// WithLevelFor or WithLevelForTarget; Can), and asks next otherwise: the
// application's own rule (the file's
// uploader may do anything, owners decide on their rows), which also
// decides every action the map does not name. A nil next refuses what
// grants do not allow. subjects returns who the request speaks for (the
// user, its groups); none is anonymous and gets nothing from grants. An
// error from Can (a level the map names that WithLevels does not define,
// a Container that fails) refuses the request: it never falls through to
// next.
func Authorizer(p *Plugin, s *records.Store, subjects func(*http.Request) []string, next func(*http.Request, records.Action, records.ActionTarget) error) func(*http.Request, records.Action, records.ActionTarget) error {
	return func(r *http.Request, a records.Action, t records.ActionTarget) error {
		if level, ok := p.levelFor(r.Context(), a, t); ok && t.FileID != "" {
			allowed, err := p.Can(r.Context(), s, t.Scope, subjects(r), t.FileID, level)
			if err != nil {
				return err
			}
			if allowed {
				return nil
			}
		}
		if next == nil {
			return filesystem.Errorf(filesystem.CodePermission, "acl: no grant allows %s", a)
		}
		return next(r, a, t)
	}
}
