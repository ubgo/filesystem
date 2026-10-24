package fsent_test

import (
	"context"
	"errors"
	"testing"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"

	"github.com/ubgo/filesystem"
	fsent "github.com/ubgo/filesystem/contrib/ent"
	"github.com/ubgo/filesystem/records"
)

// mutation is an ent.Mutation of one op; the read-only hook must refuse it
// without calling anything else on it (the embedded interface is nil, so
// any other call panics).
type mutation struct {
	ent.Mutation
	op ent.Op
}

func (m mutation) Op() ent.Op { return m.op }

// TestHooksRefuseEveryOp runs each mixin's hook in-process (the generated
// client in TestGenerateBuildAndRead runs in another process, which
// coverage does not see) on every mutation op: each is refused with
// ErrReadOnly, a permission error, and the next mutator is never called.
func TestHooksRefuseEveryOp(t *testing.T) {
	ops := []ent.Op{ent.OpCreate, ent.OpUpdate, ent.OpUpdateOne, ent.OpDelete, ent.OpDeleteOne}
	for name, hooks := range map[string][]ent.Hook{"file": fsent.FileMixin{}.Hooks(), "attachment": fsent.AttachmentMixin{}.Hooks()} {
		if len(hooks) != 1 {
			t.Fatalf("%s mixin has %d hooks", name, len(hooks))
		}
		reached := false
		next := ent.MutateFunc(func(context.Context, ent.Mutation) (ent.Value, error) { reached = true; return nil, nil })
		guarded := hooks[0](next)
		for _, op := range ops {
			v, err := guarded.Mutate(t.Context(), mutation{op: op})
			if !errors.Is(err, fsent.ErrReadOnly) || filesystem.CodeOf(err) != filesystem.CodePermission || v != nil {
				t.Errorf("%s %v = %v, %v", name, op, v, err)
			}
		}
		if reached {
			t.Fatalf("%s: a refused mutation reached the database", name)
		}
	}
}

// TestAnnotationsAndEdges: each mixin names its records table, and the
// edges carry the names, field and cardinality the generated client
// relies on.
func TestAnnotationsAndEdges(t *testing.T) {
	for want, list := range map[string][]any{
		records.TableNameFiles:       {fsent.FileMixin{}.Annotations()[0]},
		records.TableNameAttachments: {fsent.AttachmentMixin{}.Annotations()[0]},
	} {
		a, ok := list[0].(entsql.Annotation)
		if !ok || a.Table != want {
			t.Errorf("annotation = %#v, want table %q", list[0], want)
		}
	}
	to := fsent.FileAttachments(Attachment.Type).Descriptor()
	if to.Name != fsent.EdgeAttachments || to.Inverse || to.Unique || to.Type != "Attachment" {
		t.Errorf("FileAttachments = %+v", to)
	}
	from := fsent.AttachmentFile(File.Type).Descriptor()
	if from.Name != fsent.EdgeFile || !from.Inverse || from.RefName != fsent.EdgeAttachments || from.Field != records.ColAttachmentFileID ||
		!from.Unique || !from.Required || !from.Immutable || from.Type != "File" {
		t.Errorf("AttachmentFile = %+v", from)
	}
}
