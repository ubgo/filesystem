// Command start-testing is the application code behind
// docs/start/testing.md. The interesting file is main_test.go: it tests
// this code with a Fake disk, a memory disk from a DSN, SQLite records,
// injected faults and the conformance suite.
//
//	go test ./start-testing     (from the examples directory)
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

const (
	// avatarDir holds one avatar per user, named after the user.
	avatarDir = "avatars"
	// maxAvatarSize is the largest avatar accepted: 2 MiB.
	maxAvatarSize = 2 << 20
	// ownerUser is the owner type photos are attached to.
	ownerUser = "user"
	// rolePhotos is the role of a user's photos.
	rolePhotos = "photos"
	// mediaDisk names the disk the gallery stores photos on.
	mediaDisk = "media"
)

func main() {
	disk, err := filesystem.NewDisk(mediaDisk, memory.New())
	if err != nil {
		log.Fatal(err)
	}
	key, err := NewAvatars(disk).Save(context.Background(), "u1", strings.NewReader("GIF89a"))
	if err != nil {
		log.Fatal(err)
	}
	_, _ = fmt.Fprintln(os.Stdout, "saved", key, "- run `go test ./start-testing` for the tests")
}

// Avatars stores one avatar per user on a disk. It takes the disk as a
// parameter, which is all a test needs to hand it a Fake instead.
// ds:def id=testing-avatars-fkc7929x
type Avatars struct {
	disk *filesystem.Disk
}

// NewAvatars returns the service over disk.
func NewAvatars(disk *filesystem.Disk) *Avatars {
	return &Avatars{disk: disk}
}

// avatarPolicy accepts PNG, JPEG and GIF images of at most 2 MiB and
// stores them under the user's own key, replacing the previous avatar.
// ds:def id=testing-avatarpolicy-kycd9dze
func avatarPolicy(userID string) upload.Policy {
	return upload.Policy{
		MaxSize: maxAvatarSize,
		Allowed: []string{"image/png", "image/jpeg", "image/gif"},
		Key:     func(upload.Info) (string, error) { return avatarDir + "/" + userID, nil },
	}
}

// Save stores body as userID's avatar and returns its key. A body that is
// not an accepted image is refused with CodeRejected and nothing is stored.
// ds:def id=testing-save-m55jh9c4
func (a *Avatars) Save(ctx context.Context, userID string, body io.Reader) (string, error) {
	res, err := upload.Receive(ctx, a.disk, body, "avatar", avatarPolicy(userID))
	if err != nil {
		return "", err
	}
	return res.Key, nil
}

// Remove deletes userID's avatar; removing a missing one succeeds.
func (a *Avatars) Remove(ctx context.Context, userID string) error {
	return a.disk.Delete(ctx, avatarDir+"/"+userID)
}

// Gallery records a user's photos with the records module.
type Gallery struct {
	store *records.Store
}

// NewGallery returns the gallery over store, whose manager must hold the
// "media" disk.
func NewGallery(store *records.Store) *Gallery {
	return &Gallery{store: store}
}

// photoPolicy accepts images of at most 2 MiB under random keys.
func photoPolicy() upload.Policy {
	return upload.Policy{MaxSize: maxAvatarSize, Allowed: []string{"image/*"}, Key: upload.RandomKey(rolePhotos)}
}

// Add uploads a photo and attaches it to the user in one transaction.
// ds:def id=testing-galleryadd-vx94ngb9
func (g *Gallery) Add(ctx context.Context, userID string, body io.Reader, filename string) (records.File, error) {
	var f records.File
	err := g.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if f, err = g.store.Upload(ctx, userID, mediaDisk, body, filename, photoPolicy()); err != nil {
			return err
		}
		_, err = g.store.Attach(ctx, userID, f.ID, records.Owner{Type: ownerUser, ID: userID}, rolePhotos)
		return err
	})
	return f, err
}

// Photos lists the user's photos in upload order.
func (g *Gallery) Photos(ctx context.Context, userID string) ([]records.AttachedFile, error) {
	return g.store.Attachments(ctx, userID, records.Owner{Type: ownerUser, ID: userID}, rolePhotos)
}
