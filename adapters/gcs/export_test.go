package gcs

import (
	"cloud.google.com/go/storage"
	"google.golang.org/api/iamcredentials/v1"
)

// SigningOf returns the signing options a driver was configured with, so a
// test can check what the opener resolved without calling Google.
func SigningOf(d *Driver) *storage.SignedURLOptions { return d.cfg.Signing }

// IAMOf returns the IAM Credentials client a driver signs keyless URLs
// with, so a test can check the opener set one.
func IAMOf(d *Driver) *iamcredentials.Service { return d.cfg.IAM }
