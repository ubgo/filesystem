package records

import "context"

// HeldObject is an object a plugin stores beside a file's content: a kept
// version, an image variant. Such objects take space a file's Size does
// not show, so a plugin that holds one reports it (Store.Hold) and its
// end (Store.Release), and plugins that account for space (quota) hear of
// both through UsageHooks.
type HeldObject struct {
	// Plugin is the Name of the plugin holding the object.
	Plugin string
	Scope  string
	// FileID is the file the object belongs to.
	FileID string
	Disk   string
	Key    string
	Size   int64
}

// UsageHooks is a plugin told of every object another plugin holds or
// releases. Both run inside the holder's transaction (use Store.Conn for
// statements), so a counter changes with the holder's rows or not at all.
// An OnHold error, such as CodeQuotaExceeded, aborts the holder's change;
// an OnRelease error aborts it too, so a release is never half counted.
type UsageHooks interface {
	OnHold(ctx context.Context, s *Store, o HeldObject) error
	OnRelease(ctx context.Context, s *Store, o HeldObject) error
}

// HeldCounter is a plugin that can report the bytes it holds in a scope,
// so a recount (quota) can add them to the files' own sizes.
type HeldCounter interface {
	HeldBytes(ctx context.Context, s *Store, scope string) (int64, error)
}

// Hold tells every installed UsageHooks plugin, in install order, that o
// is now held, and returns the first error, which stops the rest. Call it
// inside the transaction that records the object, and fail that
// transaction with the error, so a refused hold leaves no row behind.
func (s *Store) Hold(ctx context.Context, o HeldObject) error {
	return s.usage(ctx, o, UsageHooks.OnHold)
}

// Release tells every installed UsageHooks plugin, in install order, that
// o is no longer held, and returns the first error, which stops the rest.
// Call it inside the transaction that removes the object's row; the
// object itself is deleted after the commit (DeleteObjectAfterCommit).
func (s *Store) Release(ctx context.Context, o HeldObject) error {
	return s.usage(ctx, o, UsageHooks.OnRelease)
}

func (s *Store) usage(ctx context.Context, o HeldObject, call func(UsageHooks, context.Context, *Store, HeldObject) error) error {
	for _, p := range s.plugins {
		if h, ok := p.(UsageHooks); ok {
			if err := call(h, ctx, s, o); err != nil {
				return err
			}
		}
	}
	return nil
}

// HeldBytes sums the bytes every installed HeldCounter plugin holds in
// scope; 0 when none is installed. The first error is returned.
func (s *Store) HeldBytes(ctx context.Context, scope string) (int64, error) {
	var total int64
	for _, p := range s.plugins {
		if c, ok := p.(HeldCounter); ok {
			n, err := c.HeldBytes(ctx, s, scope)
			if err != nil {
				return 0, err
			}
			total += n
		}
	}
	return total, nil
}
