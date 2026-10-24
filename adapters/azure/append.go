package azure

import (
	"bytes"
	"context"
	"io"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/appendblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"

	"github.com/ubgo/filesystem"
)

// MaxAppendBlock is the most one Append may add: Azure appends a block
// atomically, and 4 MiB is the block limit every service version and
// Azurite accept. Larger bodies are CodeTooLarge; append them in pieces.
const MaxAppendBlock = 4 << 20

// Append implements filesystem.Appender with Azure append blobs. The body
// is read whole first (up to MaxAppendBlock), so a failing body appends
// nothing; it is then one Append Block, which Azure applies atomically,
// so concurrent appends never interleave. opts.MaxSize is Azure's own
// max-size condition, checked with the append. A missing key becomes an
// empty append blob first (never truncating one a concurrent caller just
// created). A key written by Write is a block blob, which cannot be
// appended to: CodeConflict.
func (d *Driver) Append(ctx context.Context, key string, body io.Reader, opts filesystem.AppendOptions) (filesystem.Entry, error) {
	if err := checkKey(key); err != nil {
		return filesystem.Entry{}, err
	}
	chunk, err := io.ReadAll(io.LimitReader(body, MaxAppendBlock+1))
	if err != nil {
		return filesystem.Entry{}, err
	}
	if len(chunk) > MaxAppendBlock {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeTooLarge, "azure: one append is at most %d bytes", MaxAppendBlock)
	}
	if opts.MaxSize > 0 && int64(len(chunk)) > opts.MaxSize {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeTooLarge, "azure: append of %d bytes exceeds MaxSize %d", len(chunk), opts.MaxSize)
	}
	ro := requestOptions(ctx, key)
	if err := onlyScopeAndTags(ro, "Append", true); err != nil {
		return filesystem.Entry{}, err
	}
	ws, err := d.settings(key, ro)
	if err != nil {
		return filesystem.Entry{}, err
	}
	ab := d.container.NewAppendBlobClient(key)
	_, err = ab.Create(ctx, &appendblob.CreateOptions{
		CPKInfo:          d.cpk(),
		CPKScopeInfo:     ws.cpkScope(),
		Tags:             ws.tags,
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETagAny)}},
	})
	if err != nil && !bloberror.HasCode(err, bloberror.BlobAlreadyExists, bloberror.ConditionNotMet) {
		return filesystem.Entry{}, mapErr(err)
	}
	if len(chunk) > 0 {
		o := &appendblob.AppendBlockOptions{CPKInfo: d.cpk(), CPKScopeInfo: ws.cpkScope()}
		if opts.MaxSize > 0 {
			o.AppendPositionAccessConditions = &appendblob.AppendPositionAccessConditions{MaxSize: &opts.MaxSize}
		}
		if d.cfg.ValidateCRC64 {
			o.TransactionalValidation = blob.TransferValidationTypeComputeCRC64()
		}
		_, err = ab.AppendBlock(ctx, streaming.NopCloser(bytes.NewReader(chunk)), o)
		if bloberror.HasCode(err, bloberror.MaxBlobSizeConditionNotMet) {
			return filesystem.Entry{}, filesystem.NewError(filesystem.CodeTooLarge, err)
		}
		if err != nil {
			return filesystem.Entry{}, mapErr(err)
		}
	}
	return d.Stat(ctx, key)
}
