package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// DICOM layout: a 128-byte preamble, then the magic "DICM".
const (
	dicomPreamble = 128
	dicomMagic    = "DICM"
	dicomType     = "application/dicom"
)

// dicom is a sniff.Detector for DICOM scans, a format Go's detection
// does not know. It answers "" for anything else, which leaves the
// built-in answer in place.
// ds:def id=dicom-h7r2k9vm
func dicom(head []byte, _ string) string {
	if len(head) >= dicomPreamble+len(dicomMagic) && string(head[dicomPreamble:dicomPreamble+len(dicomMagic)]) == dicomMagic {
		return dicomType
	}
	return ""
}

// scanPolicy allows DICOM scans and nothing else: no octet-stream
// catch-all, so every unknown binary is still refused.
// ds:def id=scanpolicy-q4w8n2xc
var scanPolicy = upload.Policy{MaxSize: maxUpload, Allowed: []string{dicomType}, Detector: dicom}

// customDetector uploads a DICOM scan with and without the detector, an
// unknown binary, and HTML named like a scan, then writes the scan
// straight to a disk given the same detector.
// ds:def id=customdetector-b6t3p8zs
func customDetector(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("scans", memory.New())
	if err != nil {
		return err
	}
	scan := string(bytes.Repeat([]byte{0}, dicomPreamble)) + dicomMagic + "pixel data"
	plain := scanPolicy
	plain.Detector = nil
	for _, c := range []struct {
		label, name, body string
		p                 upload.Policy
	}{
		{"scan without the detector", "chest.dcm", scan, plain},
		{"scan with the detector", "chest.dcm", scan, scanPolicy},
		{"unknown binary", "model.stl", "solid\x00\x01binary", scanPolicy},
		{"HTML named .dcm", "fake.dcm", htmlBytes, scanPolicy},
	} {
		res, err := upload.Receive(ctx, disk, strings.NewReader(c.body), c.name, c.p)
		if err != nil {
			fmt.Fprintf(out, "detector, %s: %s\n", c.label, filesystem.CodeOf(err))
			continue
		}
		fmt.Fprintf(out, "detector, %s: stored as %s\n", c.label, res.ContentType)
	}
	direct, err := filesystem.NewDisk("pacs", memory.New(), filesystem.WithContentDetector(dicom))
	if err != nil {
		return err
	}
	e, err := direct.WriteBytes(ctx, "studies/1.dcm", []byte(scan), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "detector on the disk: Write sniffs %s\n", e.ContentType)
	return nil
}
