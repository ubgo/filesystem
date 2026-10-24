package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// Header names of a signed delivery, as Standard Webhooks
// (standardwebhooks.com) defines them, so its receiver libraries in any
// language check these deliveries unchanged.
const (
	HeaderID        = "Webhook-Id"
	HeaderTimestamp = "Webhook-Timestamp"
	HeaderSignature = "Webhook-Signature"
)

// secretPrefix starts every secret, as in Standard Webhooks.
const secretPrefix = "whsec_"

// signatureVersion prefixes each signature in HeaderSignature.
const signatureVersion = "v1,"

// DefaultTolerance is how far a delivery's timestamp may be from the
// receiver's clock before VerifySignature refuses it as a replay.
const DefaultTolerance = 5 * time.Minute

var b64 = base64.StdEncoding

// Sign sets the three headers on h for body sent as message id at time
// at: HeaderSignature is "v1," and the base64 HMAC-SHA256, under the
// secret's key, of "id.timestamp.body". A malformed secret is
// CodeInvalidArgument.
func Sign(h http.Header, secret Secret, id string, at time.Time, body []byte) error {
	ts := strconv.FormatInt(at.Unix(), 10)
	sig, err := signature(secret, id, ts, body)
	if err != nil {
		return err
	}
	h.Set(HeaderID, id)
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderSignature, signatureVersion+sig)
	return nil
}

// SignAll is Sign with several secrets, for an endpoint that rotates:
// HeaderSignature holds one "v1,<base64>" per secret, space-separated, in
// the order given, as Standard Webhooks allows, so a receiver holding any
// one of them accepts the delivery. With one secret it is exactly Sign.
func SignAll(h http.Header, secrets []Secret, id string, at time.Time, body []byte) error {
	if len(secrets) == 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "webhooks: no secret to sign with")
	}
	ts := strconv.FormatInt(at.Unix(), 10)
	sigs := make([]string, len(secrets))
	for i, sec := range secrets {
		sig, err := signature(sec, id, ts, body)
		if err != nil {
			return err
		}
		sigs[i] = signatureVersion + sig
	}
	h.Set(HeaderID, id)
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderSignature, strings.Join(sigs, " "))
	return nil
}

// SignRequest is one delivery attempt to sign. It is a struct so fields
// can be added without breaking Signers.
type SignRequest struct {
	// Header is the request's header: set the signature headers on it.
	Header http.Header
	// Endpoint is where the delivery goes, with its active secrets
	// (Secrets, newest first).
	Endpoint Endpoint
	// MessageID is the event's ID, the same on every attempt: Standard
	// Webhooks' Webhook-Id, for the receiver to drop repeats.
	MessageID string
	// At is the attempt's time, by the Store's clock.
	At time.Time
	// Body is exactly the bytes sent.
	Body []byte
}

// Signer signs a delivery. The default, StandardSigner, is Standard
// Webhooks v1; an application plugs in ed25519 (v1a), GitHub-style
// X-Hub-Signature-256, or a key that never leaves its KMS. An error fails
// the attempt (retried after Backoff), so nothing is sent unsigned.
type Signer interface {
	Sign(ctx context.Context, req SignRequest) error
}

// StandardSigner is the default Signer: SignAll with the endpoint's
// active secrets, HMAC-SHA256 in the Standard Webhooks v1 format. An
// Endpoint built by hand with only Secret set is signed with that one.
type StandardSigner struct{}

// Sign implements Signer.
func (StandardSigner) Sign(_ context.Context, req SignRequest) error {
	secrets := req.Endpoint.Secrets
	if len(secrets) == 0 && req.Endpoint.Secret != "" {
		secrets = []Secret{req.Endpoint.Secret}
	}
	return SignAll(req.Header, secrets, req.MessageID, req.At, req.Body)
}

func signature(secret Secret, id, ts string, body []byte) (string, error) {
	key, err := b64.DecodeString(strings.TrimPrefix(secret.Reveal(), secretPrefix))
	if err != nil || len(key) == 0 {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "webhooks: malformed secret")
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + ts + "."))
	m.Write(body)
	return b64.EncodeToString(m.Sum(nil)), nil
}

// VerifySignature is the receiving side of Sign, for an application that
// receives its own webhooks or tests them: it accepts body when one of
// the space-separated signatures in h matches and the timestamp is within
// tolerance of now (DefaultTolerance when 0), and refuses it with
// CodePermission otherwise. Compare in constant time; never trust a body
// before this passes.
func VerifySignature(h http.Header, secret Secret, body []byte, now time.Time, tolerance time.Duration) error {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	ts := h.Get(HeaderTimestamp)
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return filesystem.Errorf(filesystem.CodePermission, "webhooks: missing or malformed %s", HeaderTimestamp)
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return filesystem.Errorf(filesystem.CodePermission, "webhooks: timestamp is %s from now, outside %s", d, tolerance)
	}
	want, err := signature(secret, h.Get(HeaderID), ts, body)
	if err != nil {
		return err
	}
	for sig := range strings.FieldsSeq(h.Get(HeaderSignature)) {
		if got, ok := strings.CutPrefix(sig, signatureVersion); ok && hmac.Equal([]byte(got), []byte(want)) {
			return nil
		}
	}
	return filesystem.Errorf(filesystem.CodePermission, "webhooks: no valid signature")
}
