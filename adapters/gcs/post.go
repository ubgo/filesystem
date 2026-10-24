package gcs

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// V4 POST policy constants, from GCS's XML API.
const (
	postAlgorithm      = "GOOG4-RSA-SHA256"
	postCredentialTail = "/auto/storage/goog4_request"
	postDateFormat     = "20060102T150405Z"
	postDayFormat      = "20060102"
	postFieldAlgorithm = "x-goog-algorithm"
	postFieldCred      = "x-goog-credential"
	postFieldDate      = "x-goog-date"
	postFieldPolicy    = "policy"
	postFieldSignature = "x-goog-signature"
	postCondLength     = "content-length-range"
	postCondStartsWith = "starts-with"
	postKeyVariable    = "$key"
	postFieldACL       = "acl"
	// PostHost is the host forms post to: GCS's XML API endpoint, which
	// takes POST uploads in path style (https://PostHost/bucket/).
	PostHost = "storage.googleapis.com"
)

// postSigner returns the function that signs a policy's bytes with the
// disk's Signing (key, SignBytes) or, keyless, through Config.IAM. Without
// any of them a form cannot be signed (PresignPost is not declared).
func (d *Driver) postSigner(ctx context.Context) (func([]byte) ([]byte, error), error) {
	s := d.cfg.Signing
	switch {
	case s.PrivateKey != nil:
		key, err := parseRSAKey(s.PrivateKey)
		if err != nil {
			return nil, err
		}
		return func(b []byte) ([]byte, error) {
			sum := sha256.Sum256(b)
			return rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		}, nil
	case s.SignBytes != nil:
		return s.SignBytes, nil
	default:
		return d.signBlob(ctx, s.GoogleAccessID), nil
	}
}

// canPost reports whether forms can be signed: with a key or SignBytes, or
// keylessly through Config.IAM (the SDK's own keyless path cannot sign a
// policy built here).
func (d *Driver) canPost() bool {
	s := d.cfg.Signing
	return s != nil && (s.PrivateKey != nil || s.SignBytes != nil || d.cfg.IAM != nil)
}

// parseRSAKey reads a PEM private key, PKCS #8 (service account JSON keys)
// or PKCS #1.
func parseRSAKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: signing key is not PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: signing key is not RSA")
	}
	k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	return k, nil
}

// PresignPost implements filesystem.PostPresigner with a V4 POST policy:
// a form a browser posts to https://storage.googleapis.com/<bucket>/,
// where GCS itself refuses an upload outside the policy. The policy pins
// the size range, the exact key or a key prefix (starts-with $key, which
// the SDK's own generator cannot express), the Content-Type when set, the
// stored headers, the disk's KMS key, the call's RequestOptions for the
// form's key, and, in ACL mode, a visibility asked for
// (PostPolicy.Visibility) as the acl field. It is built and signed here, with
// the disk's key, SignBytes, or Google's signBlob through Config.IAM.
// Every pinned value is also a form field the client sends unchanged.
func (d *Driver) PresignPost(ctx context.Context, p filesystem.PostPolicy) (filesystem.PresignedPost, error) {
	sign, err := d.postSigner(ctx)
	if err != nil {
		return filesystem.PresignedPost{}, err
	}
	acl, err := d.presignACL(p.Visibility)
	if err != nil {
		return filesystem.PresignedPost{}, err
	}
	now := d.now().UTC()
	expires := now.Add(p.Expires)
	credential := d.cfg.Signing.GoogleAccessID + "/" + now.Format(postDayFormat) + postCredentialTail
	fields := map[string]string{
		postFieldAlgorithm: postAlgorithm,
		postFieldCred:      credential,
		postFieldDate:      now.Format(postDateFormat),
	}
	// Policy conditions are JSON values of two shapes: one-entry objects
	// pinning a field, and [operator, field, values...] lists.
	conds := []json.RawMessage{
		json.RawMessage(fmt.Sprintf(`[%q,%d,%d]`, postCondLength, p.MinSize, p.MaxSize)),
		mustJSON(map[string]string{"bucket": d.cfg.Bucket}),
	}
	formKey := p.Key
	if p.KeyPrefix != "" {
		formKey = p.KeyPrefix + "/" + filesystem.PostFilenamePlaceholder
	}
	call, err := d.presignSettings(ctx, formKey)
	if err != nil {
		return filesystem.PresignedPost{}, err
	}
	if p.KeyPrefix != "" {
		fields[filesystem.PostFieldKey] = formKey
		conds = append(conds, mustJSON([]string{postCondStartsWith, postKeyVariable, p.KeyPrefix + "/"}))
	} else {
		fields[filesystem.PostFieldKey] = p.Key
		conds = append(conds, mustJSON(map[string]string{filesystem.PostFieldKey: p.Key}))
	}
	for _, f := range []struct{ name, value string }{
		{filesystem.PostFieldContentType, p.ContentType},
		{filesystem.PostFieldCacheControl, p.Headers.CacheControl},
		{filesystem.PostFieldContentDisposition, p.Headers.ContentDisposition},
		{filesystem.PostFieldContentEncoding, p.Headers.ContentEncoding},
		{headerKMSKeyName, d.kmsKey(call, formKey)},
		{strings.ToLower(headerStorageClass), string(call.StorageClass)},
		{postFieldACL, acl},
	} {
		if f.value != "" {
			fields[f.name] = f.value
			conds = append(conds, mustJSON(map[string]string{f.name: f.value}))
		}
	}
	for _, name := range []string{postFieldAlgorithm, postFieldCred, postFieldDate} {
		conds = append(conds, mustJSON(map[string]string{name: fields[name]}))
	}
	doc := mustJSON(struct {
		Conditions []json.RawMessage `json:"conditions"`
		Expiration string            `json:"expiration"`
	}{conds, expires.Format(time.RFC3339)})
	policy := base64.StdEncoding.EncodeToString(doc)
	sig, err := sign([]byte(policy))
	if err != nil {
		return filesystem.PresignedPost{}, mapErr(err)
	}
	fields[postFieldPolicy] = policy
	fields[postFieldSignature] = hex.EncodeToString(sig)
	return filesystem.PresignedPost{URL: "https://" + PostHost + "/" + d.cfg.Bucket + "/", Fields: fields, ExpiresAt: expires}, nil
}

// mustJSON marshals values built here from strings, which cannot fail.
func mustJSON[T any](v T) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
