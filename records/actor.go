package records

import (
	"context"

	"github.com/ubgo/filesystem"
)

// MaxActorBytes is the longest actor, the indexed width of the columns
// that store one.
const MaxActorBytes = 191

type actorKey struct{}

// WithActor returns ctx carrying the actor of the changes made with it: a
// user ID, "system:cron", "api-key:…". Set it once per request, in the
// middleware that authenticates; the Store records it as File.UploadedBy,
// and plugins (audit) log it.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFrom returns the actor set with WithActor, or "". It is the
// Store's default actor lookup; Store.Actor is the one to read, since
// WithActorFunc may replace it.
func ActorFrom(ctx context.Context) string {
	actor, _ := ctx.Value(actorKey{}).(string)
	return actor
}

// WithActorFunc replaces ActorFrom as the Store's way of naming who a
// request acts for, for applications that keep their user under their own
// context key or encode a structured actor ("user:42", "admin:7 as
// user:42") in their own type. Store.Actor calls fn, and every place the
// Store and its plugins record an actor reads Store.Actor: File.UploadedBy
// and the share plugin's viewers and depositors.
//
// Invariant: when fn returns "", Store.Actor falls back to ActorFrom, so an
// actor the library itself sets with WithActor (a share link's depositor)
// is still recorded. An actor longer than MaxActorBytes fails the write
// that records it, as one set with WithActor does. A nil fn is
// CodeInvalidArgument.
func WithActorFunc(fn func(ctx context.Context) string) Option {
	return func(s *Store) error {
		if fn == nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: nil actor func")
		}
		s.actor = fn
		return nil
	}
}

// Actor returns who ctx acts for: WithActorFunc's function when it names
// someone, else ActorFrom(ctx). Plugins read the actor here, so one option
// rules the Store and every plugin alike.
func (s *Store) Actor(ctx context.Context) string {
	if s.actor != nil {
		if a := s.actor(ctx); a != "" {
			return a
		}
	}
	return ActorFrom(ctx)
}
