package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/upload"
)

// fetchLimits reads GET /photos/limits, the contract a client follows
// before it uploads. A phone app does this once per session (or per plan
// change), not per photo.
// ds:def id=fetchlimits-vzux344u
func fetchLimits(ctx context.Context, client *http.Client, base string) (upload.Limits, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/photos/limits", nil)
	if err != nil {
		return upload.Limits{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return upload.Limits{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return upload.Limits{}, fmt.Errorf("limits: %s", resp.Status)
	}
	var l upload.Limits
	err = json.NewDecoder(resp.Body).Decode(&l)
	return l, err
}

// downscale is what the client does with the limits: a photo larger than
// MaxWidth × MaxHeight is scaled down to fit (ModeFit never enlarges) and
// re-encoded as JPEG; a photo that already fits is sent as it is. On iOS
// the same step is UIGraphicsImageRenderer plus jpegData(compressionQuality:),
// on Android Bitmap.createScaledBitmap plus compress(JPEG, …); this Go
// version serves a desktop or command-line uploader, and the test.
// ds:def id=downscale-jyrutuxr
func downscale(photo []byte, l upload.Limits) ([]byte, error) {
	info, err := imaging.Probe(bytes.NewReader(photo))
	if err != nil {
		return nil, err
	}
	if (l.MaxWidth == 0 || info.Width <= l.MaxWidth) && (l.MaxHeight == 0 || info.Height <= l.MaxHeight) {
		return photo, nil
	}
	res, err := imaging.Process(bytes.NewReader(photo), imaging.Transform{
		Mode: imaging.ModeFit, Width: l.MaxWidth, Height: l.MaxHeight, Format: imaging.FormatJPEG,
	}, imaging.Options{MaxBytes: l.MaxBytes})
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}
