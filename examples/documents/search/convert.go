package main

import (
	"context"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/office"
	"github.com/ubgo/filesystem/contrib/pdf"
	"github.com/ubgo/filesystem/records"
)

// pdfSuffix names the PDF rendition stored beside an office document.
const pdfSuffix = ".pdf"

// keepPDFRendition converts a recorded office document with LibreOffice
// and stores the PDF beside it, for a viewer that shows PDFs only or a
// download "as PDF". Run it from a job: a conversion takes seconds. The
// PDF comes back as an unlinked work file, so nothing is left on the
// server's disk whatever happens.
// ds:def id=keeppdfrendition-c4n9w7tk
func keepPDFRendition(ctx context.Context, disk *filesystem.Disk, f records.File) (string, error) {
	tool := office.Tool{}
	if !tool.Converts(f.ContentType) {
		return "", filesystem.Errorf(filesystem.CodeUnsupported, "%s is not an office document", f.ContentType)
	}
	src, err := disk.Read(ctx, f.Key)
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	out, err := tool.ConvertToPDF(ctx, src, f.ContentType)
	if err != nil {
		return "", err
	}
	defer func() { _ = out.Close() }()
	key := f.Key + pdfSuffix
	_, err = disk.Write(ctx, key, out, filesystem.WriteOptions{ContentType: pdf.ContentType})
	return key, err
}
