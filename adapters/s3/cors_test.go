package s3_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// TestSetCORS proves SetCORS sends the rules as PutBucketCors, the
// recommended direct-upload rule included, and refuses incomplete rules.
func TestSetCORS(t *testing.T) {
	ctx := t.Context()
	d, p := newDisk(t, fss3.Config{})
	client, _ := filesystem.As[*s3.Client](d)
	rule := fss3.DirectUploadCORS("https://app.example.com")
	if err := fss3.SetCORS(ctx, client, "test-bucket", rule, fss3.CORSRule{AllowedOrigins: []string{"https://admin.example.com"}, AllowedMethods: []string{"GET"}}); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	body := p.cors
	p.mu.Unlock()
	for _, want := range []string{
		"<AllowedOrigin>https://app.example.com</AllowedOrigin>",
		"<AllowedMethod>PUT</AllowedMethod>", "<AllowedMethod>POST</AllowedMethod>",
		"<AllowedHeader>x-amz-*</AllowedHeader>", "<AllowedHeader>Content-Type</AllowedHeader>",
		"<ExposeHeader>ETag</ExposeHeader>", "<MaxAgeSeconds>3600</MaxAgeSeconds>",
		"<AllowedOrigin>https://admin.example.com</AllowedOrigin>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PutBucketCors body lacks %s:\n%s", want, body)
		}
	}
	if strings.Count(body, "<CORSRule>") != 2 {
		t.Errorf("want 2 rules:\n%s", body)
	}
	if rule.MaxAge != time.Hour {
		t.Errorf("recommended max age = %v", rule.MaxAge)
	}
	for _, bad := range [][]fss3.CORSRule{nil, {{AllowedMethods: []string{"GET"}}}, {{AllowedOrigins: []string{"https://x"}}}} {
		if err := fss3.SetCORS(ctx, client, "test-bucket", bad...); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("SetCORS(%v) = %v", bad, err)
		}
	}
}
