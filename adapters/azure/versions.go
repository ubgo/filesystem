package azure

import (
	"context"
	"io"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/ubgo/filesystem"
)

// Versions implements filesystem.VersionLister from the blob's versions,
// newest first (a version ID is the RFC 3339 time it was made, so the
// order is the IDs', descending). Azure keeps versions only with blob
// versioning on, which is why the capability is declared by
// Config.Versioned. Azure has no delete markers: after a Delete every
// version remains and none is Latest. A key with no version at all is
// CodeNotFound.
func (d *Driver) Versions(ctx context.Context, key string) ([]filesystem.Version, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	var out []filesystem.Version
	pager := d.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: to.Ptr(key), Include: container.ListBlobsInclude{Versions: true}})
	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapErr(err)
		}
		for _, it := range resp.Segment.BlobItems {
			if val(it.Name) != key || it.VersionID == nil {
				continue
			}
			v := filesystem.Version{ID: val(it.VersionID), Latest: val(it.IsCurrentVersion)}
			if p := it.Properties; p != nil {
				v.Size = val(p.ContentLength)
				v.ModTime = val(p.LastModified)
				v.ETag = etag(p.ETag)
			}
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, filesystem.Errorf(filesystem.CodeNotFound, "azure: %q has no versions", key)
	}
	slices.SortFunc(out, func(a, b filesystem.Version) int { return strings.Compare(b.ID, a.ID) })
	return out, nil
}

// versionClient is the client of version id of key. The Disk refuses an
// empty id, which would address the current blob instead.
func (d *Driver) versionClient(key, id string) (*blob.Client, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	c, err := d.container.NewBlobClient(key).WithVersionID(id)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	return c, nil
}

// ReadVersion implements filesystem.VersionReader: one stored version of
// key (a Version.ID from Versions), current or not. A version that does
// not exist is CodeNotFound.
func (d *Driver) ReadVersion(ctx context.Context, key, id string) (io.ReadCloser, error) {
	c, err := d.versionClient(key, id)
	if err != nil {
		return nil, err
	}
	resp, err := c.DownloadStream(ctx, &blob.DownloadStreamOptions{CPKInfo: d.cpk()})
	if err != nil {
		return nil, mapErr(err)
	}
	return resp.Body, nil
}

// RestoreVersion implements filesystem.VersionReader: it makes version id
// of key the current blob again, with a server-side copy of that version
// over the key; the blob being replaced becomes a version itself, so
// nothing is lost. It works after a Delete too. It needs a server-side
// copy, which a CustomerKey or EncryptionScope rules out, so such a disk
// does not declare VersionRead.
func (d *Driver) RestoreVersion(ctx context.Context, key, id string) error {
	c, err := d.versionClient(key, id)
	if err != nil {
		return err
	}
	_, err = d.copyFrom(ctx, c.URL(), key)
	return err
}
