package acl

import (
	"context"
	"math"
)

// Match is one unexpired grant that bears on a check (Can, CanFolder), as
// a Precedence sees it: the grant as stored, where its resource sits
// relative to the resource checked, and whether it bears on the level
// asked.
type Match struct {
	// Grant is the matching grant as stored, its Effect included; a
	// precedence must not change it.
	Grant Grant
	// Depth is how far the grant's resource is from the resource checked:
	// 0 for the file itself (Can) or the folder itself (CanFolder), n for
	// the n-th folder above (1 is the folder a file is in), ContainerDepth
	// for a grant on a container (WithContainers), which sits beside the
	// folder path rather than on it.
	Depth int
	// Applies reports whether the grant bears on the level asked: an allow
	// that gives it (directly or through the levels it includes), a deny
	// that refuses it (a deny of the level or of a level it includes, or a
	// deny of a level the plugin no longer defines, which refuses
	// everything). A grant that does not apply, such as an allow of Read
	// for a request for Write, is listed so a precedence sees the whole
	// picture, and the built-in ones ignore it.
	Applies bool
}

// ContainerDepth is Match.Depth for a grant on a container.
const ContainerDepth = -1

// Denies reports whether the match's grant takes access away. Any effect
// other than Allow counts, an effect this code does not know included, so
// a precedence that branches on it fails closed.
func (m Match) Denies() bool { return m.Grant.Effect != Allow }

// Precedence decides a check from the grants that match it: one of the
// subjects asked for, unexpired, on the resource or on a resource above
// or holding it. It returns whether the check passes. An error fails the
// check (Can returns it, and Authorizer refuses the request), so a
// precedence that cannot decide never grants. matches is empty when no
// grant bears on the request, and is never kept or modified by the
// plugin after the call.
//
// Why it is a function and not a choice of two: deny-overrides and
// most-specific-wins are the common models, but applications also rank
// by subject ("a user's own grant beats its group's") or by effect per
// level; each needs the whole set of matches, which is what this gets.
type Precedence func(ctx context.Context, level Level, matches []Match) (bool, error)

// DenyOverrides is the default Precedence: access when some grant gives
// the level and no deny refuses it, wherever either sits on the path. A
// deny on a folder therefore cannot be reopened by an allow on a file
// below it. It never errors.
func DenyOverrides(_ context.Context, _ Level, matches []Match) (bool, error) {
	allowed := false
	for _, m := range matches {
		if !m.Applies {
			continue
		}
		if m.Denies() {
			return false, nil
		}
		allowed = true
	}
	return allowed, nil
}

// MostSpecific is the Precedence of NTFS and SharePoint's broken
// inheritance: the applying grant nearest the resource decides, so an
// explicit allow on a file reopens a deny on its folder ("contractor
// denied on /team, allowed on /team/brief.pdf"), and a deny on a file
// closes an allow on its folder. Grants on containers count as farther
// than every folder. At one depth a deny wins, so two grants at the same
// distance never open what one of them refuses. It never errors.
//
// Install it with WithPrecedence(MostSpecific): it is never the default,
// since it lets a narrower grant undo a broader deny.
func MostSpecific(_ context.Context, _ Level, matches []Match) (bool, error) {
	best := math.MaxInt
	allowed, denied := false, false
	for _, m := range matches {
		if !m.Applies {
			continue
		}
		d := m.Depth
		if d == ContainerDepth {
			d = math.MaxInt - 1 // beside the path: farther than every folder
		}
		switch {
		case d < best:
			best, allowed, denied = d, !m.Denies(), m.Denies()
		case d == best:
			allowed = allowed || !m.Denies()
			denied = denied || m.Denies()
		}
	}
	return allowed && !denied, nil
}
