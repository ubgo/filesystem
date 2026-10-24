package azure

import (
	"context"
	"errors"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/ubgo/filesystem"
)

// Tags implements filesystem.Tagger with blob index tags: key-value pairs
// Azure indexes for lifecycle rules and Find Blobs by Tags, changed
// without rewriting the blob. Azure allows ten tags per blob.
func (d *Driver) Tags(ctx context.Context, key string) (map[string]string, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	resp, err := d.container.NewBlobClient(key).GetTags(ctx, nil)
	if err != nil {
		return nil, mapErr(err)
	}
	tags := map[string]string{}
	for _, t := range resp.BlobTagSet {
		tags[val(t.Key)] = val(t.Value)
	}
	return tags, nil
}

// SetTags implements filesystem.Tagger, replacing every tag; nil or empty
// removes them all. Azure checks the tag rules (ten tags, key and value
// lengths, allowed characters) and refuses others.
func (d *Driver) SetTags(ctx context.Context, key string, tags map[string]string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if tags == nil {
		tags = map[string]string{}
	}
	_, err := d.container.NewBlobClient(key).SetTags(ctx, tags, nil)
	var resp *azcore.ResponseError
	if errors.As(err, &resp) && resp.StatusCode == http.StatusBadRequest {
		return filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	return mapErr(err)
}
