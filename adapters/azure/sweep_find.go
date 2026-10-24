package azure

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/ubgo/filesystem"
)

// FindAbandoned implements filesystem.AbandonedFinder: the multipart
// uploads whose marker was written before before, which SweepAbandoned
// would end, with the key each was started for. It changes nothing. A
// marker records only when its upload started, so that is LastActivity.
// A marker that vanished (completed or aborted meanwhile) or cannot be
// decoded reports an empty Key, since the sweep ends it all the same; a
// failed request is an error.
func (d *Driver) FindAbandoned(ctx context.Context, before time.Time) ([]filesystem.Abandoned, error) {
	var found []filesystem.Abandoned
	err := d.scanAbandoned(ctx, before, func(id string, started time.Time) error {
		key, err := d.markerTarget(ctx, id)
		if err != nil {
			return err
		}
		found = append(found, filesystem.Abandoned{Kind: filesystem.AbandonedMultipart, ID: id, Key: key, LastActivity: started})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// scanAbandoned calls visit with the ID and start time of every upload
// whose marker was written before before; an error from visit ends the
// scan. Blobs under MultipartDir that are not markers this driver issued
// are skipped. It is the one definition of an abandoned upload both
// SweepAbandoned and FindAbandoned use.
func (d *Driver) scanAbandoned(ctx context.Context, before time.Time, visit func(id string, started time.Time) error) error {
	pager := d.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: to.Ptr(MultipartDir + "/")})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return mapErr(err)
		}
		for _, b := range page.Segment.BlobItems {
			id, ok := strings.CutSuffix(strings.TrimPrefix(val(b.Name), MultipartDir+"/"), markerExt)
			// A marker is written once, so its last modification is when
			// its upload started.
			if !ok || !uploadIDPattern.MatchString(id) || b.Properties == nil || !val(b.Properties.LastModified).Before(before) {
				continue
			}
			if err := visit(id, val(b.Properties.LastModified)); err != nil {
				return err
			}
		}
	}
	return nil
}

// markerTarget is the key upload id was started for, "" when its marker
// is gone or does not decode. Only a failed request is an error: every
// such error carries a filesystem code (mapErr), a decoding one does not.
func (d *Driver) markerTarget(ctx context.Context, id string) (string, error) {
	m, err := d.readMarker(ctx, id)
	var coded *filesystem.Error
	switch {
	case err == nil:
		return m.Key, nil
	case filesystem.CodeOf(err) == filesystem.CodeNotFound, !errors.As(err, &coded):
		return "", nil
	}
	return "", err
}
