package locks_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/locks"
)

// adminMayPurge is "an admin may purge a locked file but not overwrite or
// trash it"; the owner holds as by default.
func adminMayPurge(_ context.Context, req locks.HolderRequest) (bool, error) {
	if req.Owner == "admin" {
		return req.Action == locks.ActionPurge, nil
	}
	return req.Owner == req.Lock.Owner, nil
}

// TestHolderAction: the HolderFunc is told which change is asked and on
// which file, so a rule can allow a purge and refuse a replace or a trash
// of the same locked file.
func TestHolderAction(t *testing.T) {
	ctx := t.Context()
	var asked []locks.HolderRequest
	e := newEnv(t, locks.WithHolderFunc(func(ctx context.Context, req locks.HolderRequest) (bool, error) {
		asked = append(asked, req)
		return adminMayPurge(ctx, req)
	}))
	f := e.file(t)
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	admin := records.WithActor(ctx, "admin")
	if err := e.replace(admin, f, "x"); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("admin replace = %v", err)
	}
	if err := e.store.Trash(admin, "t", f.ID); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("admin trash = %v", err)
	}
	if err := e.store.Purge(admin, "t", f.ID); err != nil {
		t.Fatalf("admin purge = %v", err)
	}
	var got []string
	for _, r := range asked {
		got = append(got, fmt.Sprintf("%s:%s", r.Action, r.File.ID))
	}
	want := fmt.Sprintf("[replace:%[1]s trash:%[1]s purge:%[1]s]", f.ID)
	if fmt.Sprint(got) != want {
		t.Fatalf("asked %v, want %s", got, want)
	}
	if asked[0].File.Filename != f.Filename || asked[1].File.DeletedAt == nil {
		t.Fatalf("files asked about: %+v / %+v", asked[0].File, asked[1].File)
	}
}

// uuidLayout writes all 128 random bits as hex in a UUID's layout, under
// WebDAV's opaquelocktoken scheme.
func uuidLayout(r [locks.TokenRandomBytes]byte) string {
	h := hex.EncodeToString(r[:])
	return "opaquelocktoken:" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// TestTokenFunc: a TokenFunc's rendering is the token, exclusive and
// shared, and two locks get different ones.
func TestTokenFunc(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, locks.WithTokenFunc(uuidLayout), locks.WithSharedLocks())
	a, b := e.file(t), e.file(t)
	la, err := e.locks.Lock(ctx, e.store, "t", a.ID, "alice", time.Hour)
	if err != nil || !strings.HasPrefix(la.Token, "opaquelocktoken:") || len(la.Token) != len("opaquelocktoken:")+36 {
		t.Fatalf("Lock = %q, %v", la.Token, err)
	}
	lb, err := e.locks.LockShared(ctx, e.store, "t", b.ID, "bob", time.Hour)
	if err != nil || !strings.HasPrefix(lb.Token, "opaquelocktoken:") || lb.Token == la.Token {
		t.Fatalf("LockShared = %q, %v", lb.Token, err)
	}
}

// TestTokenFuncFloor: a TokenFunc that drops random bits (a UUIDv4's
// version bits, a constant), or answers empty or too long, refuses every
// lock rather than hand out a weaker token.
func TestTokenFuncFloor(t *testing.T) {
	ctx := t.Context()
	for name, fn := range map[string]locks.TokenFunc{
		"uuidv4": func(r [locks.TokenRandomBytes]byte) string {
			r[6] = r[6]&0x0f | 0x40
			r[8] = r[8]&0x3f | 0x80
			return uuidLayout(r)
		},
		"first half": func(r [locks.TokenRandomBytes]byte) string { return hex.EncodeToString(r[:8]) },
		"constant":   func([locks.TokenRandomBytes]byte) string { return "token" },
		"empty":      func([locks.TokenRandomBytes]byte) string { return "" },
		// Fine for the all-zero input, empty once a bit is set: the check
		// must try every bit, not only render the zero input.
		"empty for some input": func(r [locks.TokenRandomBytes]byte) string {
			if r != ([locks.TokenRandomBytes]byte{}) {
				return ""
			}
			return hex.EncodeToString(r[:])
		},
		"long": func(r [locks.TokenRandomBytes]byte) string {
			return hex.EncodeToString(r[:]) + strings.Repeat("x", locks.MaxTokenBytes)
		},
	} {
		e := newEnv(t, locks.WithTokenFunc(fn), locks.WithSharedLocks())
		f := e.file(t)
		if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Hour); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("%s: Lock = %v", name, err)
		}
		if _, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "alice", time.Hour); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("%s: LockShared = %v", name, err)
		}
		if _, held, _ := e.locks.LockOf(ctx, e.store, "t", f.ID); held {
			t.Fatalf("%s: a lock was taken", name)
		}
	}
}
