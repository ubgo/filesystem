package jobs_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs"
	"github.com/ubgo/filesystem/memory"
)

// refusing is a memory driver whose writes of keys containing "bad" fail.
type refusing struct{ *memory.Driver }

func (r refusing) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	if strings.Contains(key, "bad") {
		_, _ = io.Copy(io.Discard, body)
		return filesystem.Entry{}, filesystem.ErrPermission
	}
	return r.Driver.Write(ctx, key, body, o)
}

// unlistable fails List.
type unlistable struct{ *memory.Driver }

func (unlistable) List(context.Context, string, filesystem.ListOptions) (filesystem.Page, error) {
	return filesystem.Page{}, filesystem.ErrUnavailable
}

func disk(t *testing.T, name string, d filesystem.Driver) *filesystem.Disk {
	t.Helper()
	out, err := filesystem.NewDisk(name, d)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPromote(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	clock := now.Add(-time.Hour)
	staging := disk(t, "fast", memory.New(memory.WithClock(func() time.Time { return clock })))
	store := disk(t, "durable", refusing{memory.New()})
	for _, k := range []string{"pending/a.png", "pending/2026/b.png", "pending/bad.png", "other/kept.png"} {
		_, _ = staging.WriteBytes(ctx, k, []byte("data "+k), filesystem.WriteOptions{})
	}
	clock = now // written just now: too young to move
	_, _ = staging.WriteBytes(ctx, "pending/fresh.png", []byte("fresh"), filesystem.WriteOptions{})

	job := jobs.Promote(staging, "pending", store, time.Minute)
	if job.Name != jobs.NamePromote {
		t.Errorf("name %q", job.Name)
	}
	res, err := job.Run(ctx, now)
	if res.Count != 2 || filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("promote: %+v, %v", res, err)
	}
	for key, onStore := range map[string]bool{"a.png": true, "2026/b.png": true, "bad.png": false, "fresh.png": false} {
		ok, _ := store.Exists(ctx, key)
		left, _ := staging.Exists(ctx, "pending/"+key)
		if ok != onStore || left == onStore {
			t.Errorf("%s: on store %v, left on staging %v", key, ok, left)
		}
	}
	if got, _ := store.ReadAll(ctx, "a.png"); string(got) != "data pending/a.png" {
		t.Errorf("promoted bytes %q", got)
	}
	if ok, _ := staging.Exists(ctx, "other/kept.png"); !ok {
		t.Error("an object outside the staging directory was moved")
	}
	// With no minimum age, the fresh upload goes too.
	if res, _ := jobs.Promote(staging, "/pending/", store, 0).Run(ctx, now); res.Count != 1 {
		t.Errorf("minAge 0 moved %d", res.Count)
	}
}

func TestPromoteRefusals(t *testing.T) {
	ctx := t.Context()
	store := disk(t, "durable", memory.New())
	for name, j := range map[string]jobs.Job{
		"root":         jobs.Promote(disk(t, "fast", memory.New()), "/", store, 0),
		"negative age": jobs.Promote(disk(t, "fast", memory.New()), "pending", store, -time.Second),
	} {
		if _, err := j.Run(ctx, time.Now()); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := jobs.Promote(disk(t, "fast", memory.New()), "a/../..", store, 0).Run(ctx, time.Now()); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("traversal: %v", err)
	}
	if _, err := jobs.Promote(disk(t, "fast", unlistable{memory.New()}), "pending", store, 0).Run(ctx, time.Now()); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("listing failure: %v", err)
	}
}
