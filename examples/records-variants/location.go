package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/signedurl"
)

// thumbsDisk is the disk derived files go to: in production a cheaper
// bucket or storage class, or the bucket a CDN rule serves.
const thumbsDisk = "thumbs"

// newSplitDisks is newDisks plus the thumbs disk, both signing their
// links with the same signer.
func newSplitDisks() (*filesystem.Manager, error) {
	disks, err := newDisks()
	if err != nil {
		return nil, err
	}
	signer, err := signedurl.New(signBase, []signedurl.Key{{ID: "k1", Secret: signingSecret}})
	if err != nil {
		return nil, err
	}
	thumbs, err := filesystem.NewDisk(thumbsDisk, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, err
	}
	return disks, disks.Add(thumbs)
}

// thumbsLocation puts every variant on the thumbs disk under
// thumbs/<scope>/<fileID>/; the plugin appends <name>-<digest>-<id><ext>,
// so each generation still writes a key never written before. A disk name
// the Manager lacks fails the request with nothing written.
// ds:def id=thumbslocation-v3n8c5qa
func thumbsLocation(f records.File, _ string) (string, string, error) {
	return thumbsDisk, "thumbs/" + f.Scope + "/" + f.ID + "/", nil
}

// newLocatedPlugin declares the same variants as newPlugin, stored where
// thumbsLocation says.
// ds:def id=newlocatedplugin-p9w2k6xe
func newLocatedPlugin() (*variants.Plugin, error) {
	return variants.New(transforms, imaging.Options{}, variants.WithLocation(thumbsLocation))
}

// objectsOn counts the objects of disk name under prefix.
func objectsOn(ctx context.Context, disks *filesystem.Manager, name, prefix string) (int, error) {
	disk, err := disks.Disk(name)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, err := range disk.List(ctx, prefix, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// otherDisk stores a photo's variants on the thumbs disk while the
// original stays on media, links to one, and purges the file.
func otherDisk(ctx context.Context, w io.Writer, dir string) error {
	db, err := openSQLite(filepath.Join(dir, "located.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	disks, err := newSplitDisks()
	if err != nil {
		return err
	}
	p, err := newLocatedPlugin()
	if err != nil {
		return err
	}
	s, err := newStore(ctx, db, disks, p)
	if err != nil {
		return err
	}
	photo, err := uploadPhoto(ctx, s, 400, 300)
	if err != nil {
		return err
	}
	v, err := p.Get(ctx, s, tenant, photo.ID, nameThumb)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "located thumb: disk %s, key under thumbs/%s/{file}/: %t\n", v.Disk, tenant, strings.HasPrefix(v.Key, "thumbs/"+tenant+"/"+photo.ID+"/"))
	onMedia, err := objectsOn(ctx, disks, diskName, variants.KeyDir)
	if err != nil {
		return err
	}
	onThumbs, err := objectsOn(ctx, disks, thumbsDisk, "thumbs")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "variant objects: %d on %s, %d on %s\n", onMedia, diskName, onThumbs, thumbsDisk)
	u, err := p.URL(ctx, s, tenant, photo.ID, nameThumb, linkTTL)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "thumb link: %s…\n", u[:len(signBase)+len("/"+thumbsDisk+"/thumbs/")])
	if err := s.Purge(ctx, tenant, photo.ID); err != nil {
		return err
	}
	onThumbs, err = objectsOn(ctx, disks, thumbsDisk, "thumbs")
	fmt.Fprintf(w, "after purge: %d variant objects on %s\n", onThumbs, thumbsDisk)
	return err
}
