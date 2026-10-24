package upload_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

func TestShardedKey(t *testing.T) {
	key, err := upload.ShardedKey("photos", 2)(upload.Info{ContentType: "image/png"})
	shape := regexp.MustCompile(`^photos/([a-z2-7]{2})/([a-z2-7]{2})/([a-z2-7]{26,})\.png$`)
	m := shape.FindStringSubmatch(key)
	if err != nil || m == nil || !strings.HasPrefix(m[3], m[1]+m[2]) {
		t.Fatalf("key %q, %v", key, err)
	}
	if key, err := upload.ShardedKey("", upload.MaxShardDepth)(upload.Info{}); err != nil || strings.Count(key, "/") != upload.MaxShardDepth {
		t.Errorf("max depth key %q, %v", key, err)
	}
	for _, depth := range []int{0, -1, upload.MaxShardDepth + 1} {
		_, err := upload.ShardedKey("photos", depth)(upload.Info{})
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
}

func TestNameKey(t *testing.T) {
	if key, _ := upload.NameKey("drive/u1")(upload.Info{Filename: `..\..\Q3 report.pdf`}); key != "drive/u1/Q3 report.pdf" {
		t.Errorf("key %q", key)
	}
}

// statFails fails Stat, so FreeKey cannot tell whether a name is taken.
type statFails struct{ *memory.Driver }

func (statFails) Stat(context.Context, string) (filesystem.Entry, error) {
	return filesystem.Entry{}, filesystem.ErrUnavailable
}

// unconditional is a memory driver without atomic conditional writes.
type unconditional struct{ *memory.Driver }

func (u unconditional) Capabilities() filesystem.Capabilities {
	c := u.Driver.Capabilities()
	c.ConditionalWrite = false
	return c
}

// noCAS is a memory driver with conditional writes but no compare-and-swap.
type noCAS struct{ *memory.Driver }

func (n noCAS) Capabilities() filesystem.Capabilities {
	c := n.Driver.Capabilities()
	c.CompareAndSwap = false
	return c
}

func TestCollisionPolicies(t *testing.T) {
	ctx := t.Context()
	named := func(c upload.Collision) upload.Policy {
		p := images
		p.Key = upload.NameKey("drive")
		p.Collision = c
		return p
	}
	d := newDisk(t, memory.New())
	receive := func(p upload.Policy, body string) (upload.Result, error) {
		return upload.Receive(ctx, d, strings.NewReader(pngData+body), "photo.png", p)
	}
	if _, err := receive(named(upload.CollisionOverwrite), "first"); err != nil {
		t.Fatal(err)
	}
	// Overwrite replaces.
	if _, err := receive(named(upload.CollisionOverwrite), "second"); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.ReadAll(ctx, "drive/photo.png"); !strings.HasSuffix(string(got), "second") {
		t.Errorf("overwrite kept %q", got)
	}
	// Fail refuses and keeps the original.
	_, err := receive(named(upload.CollisionFail), "third")
	wantCode(t, err, filesystem.CodeExists)
	if got, _ := d.ReadAll(ctx, "drive/photo.png"); !strings.HasSuffix(string(got), "second") {
		t.Errorf("fail replaced the object: %q", got)
	}
	// Rename picks the next free name each time.
	for i := 1; i <= 2; i++ {
		res, err := receive(named(upload.CollisionRename), "renamed")
		if want := fmt.Sprintf("drive/photo (%d).png", i); err != nil || res.Key != want {
			t.Errorf("rename %d = %q, %v; want %q", i, res.Key, err, want)
		}
	}

	// Rename gives up with CodeExists when every candidate is taken.
	full := newDisk(t, memory.New())
	_, _ = full.WriteBytes(ctx, "drive/x.png", nil, filesystem.WriteOptions{})
	for i := 1; i <= upload.MaxRenameAttempts; i++ {
		_, _ = full.WriteBytes(ctx, fmt.Sprintf("drive/x (%d).png", i), nil, filesystem.WriteOptions{})
	}
	_, err = upload.FreeKey(ctx, full, "drive/x.png")
	wantCode(t, err, filesystem.CodeExists)
	if key, err := upload.FreeKey(ctx, full, "drive/noext"); err != nil || key != "drive/noext" {
		t.Errorf("free key %q, %v", key, err)
	}
	_, err = upload.FreeKey(ctx, newDisk(t, statFails{memory.New()}), "a.png")
	wantCode(t, err, filesystem.CodeUnavailable)
	_, err = upload.Receive(ctx, newDisk(t, statFails{memory.New()}), strings.NewReader(pngData), "photo.png", named(upload.CollisionRename))
	wantCode(t, err, filesystem.CodeUnavailable)

	// Fail and rename need atomic conditional writes.
	_, err = upload.Receive(ctx, newDisk(t, unconditional{memory.New()}), strings.NewReader(pngData), "photo.png", named(upload.CollisionFail))
	wantCode(t, err, filesystem.CodeUnsupported)

	// An unknown policy is invalid; Presign refuses anything but overwrite.
	_, err = receive(named("merge"), "")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	pd := newDisk(t, presigning{memory.New()})
	_, err = upload.Presign(ctx, pd, upload.TicketRequest{Filename: "a.png", ContentType: "image/png", Size: 10}, named(upload.CollisionRename), time.Minute)
	wantCode(t, err, filesystem.CodeUnsupported)
}

func TestUppyCollision(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, presigning{memory.New()})
	_, _ = d.WriteBytes(ctx, "drive/big.png", []byte(pngData), filesystem.WriteOptions{})
	create := func(p upload.Policy) *http.Response {
		rec := uppyClient{t, upload.Uppy(d, p, upload.UppyOptions{})}.call(http.MethodPost, "/multipart", `{"filename":"big.png","type":"image/png"}`)
		return rec.Result()
	}
	p := images
	p.Key = upload.NameKey("drive")
	p.Collision = upload.CollisionRename
	resp := create(p)
	var created struct{ Key string }
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&created) != nil || created.Key != "drive/big (1).png" {
		t.Errorf("rename create = %d %q", resp.StatusCode, created.Key)
	}
	sd := newDisk(t, presigningStatFails{presigning{memory.New()}})
	rec := uppyClient{t, upload.Uppy(sd, p, upload.UppyOptions{})}.call(http.MethodPost, "/multipart", `{"filename":"big.png","type":"image/png"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("create with a failing stat = %d %s", rec.Code, rec.Body)
	}
}

// presigningStatFails is presigning with a failing Stat.
type presigningStatFails struct{ presigning }

func (presigningStatFails) Stat(context.Context, string) (filesystem.Entry, error) {
	return filesystem.Entry{}, filesystem.ErrUnavailable
}
