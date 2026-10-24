package s3_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/aws/smithy-go"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// Values of the assume-role tests; the secrets must never be printed.
const (
	roleARN        = "arn:aws:iam::444455556666:role/acme-storage"
	roleExternalID = "ext-id-acme-7f3c9a"
	roleSession    = "tenant-acme"
	roleKeyID      = "ASIAROLEKEY"
	roleSecret     = "role-secret-access-key"
	roleToken      = "role-session-token"
)

// fakeAssumer is an AssumeRoler that records each call and returns
// credentials valid for life.
type fakeAssumer struct {
	mu    sync.Mutex
	calls []*sts.AssumeRoleInput
	life  time.Duration
	err   error
}

func (f *fakeAssumer) AssumeRole(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	if f.err != nil {
		return nil, f.err
	}
	return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{
		AccessKeyId: aws.String(roleKeyID), SecretAccessKey: aws.String(roleSecret), SessionToken: aws.String(roleToken),
		Expiration: aws.Time(time.Now().Add(f.life)),
	}}, nil
}

func (f *fakeAssumer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func roleOpts() fss3.RoleOptions {
	return fss3.RoleOptions{RoleARN: roleARN, ExternalID: roleExternalID, SessionName: roleSession}
}

// TestRoleCredentialsAssume proves the role, the external ID, the session
// name and the duration reach STS, the default duration is one hour, and
// the credentials are kept until they near expiry, then assumed again.
func TestRoleCredentialsAssume(t *testing.T) {
	ctx := t.Context()
	f := &fakeAssumer{life: time.Hour}
	p, err := fss3.RoleCredentials(f, roleOpts())
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 0 {
		t.Fatalf("RoleCredentials called STS %d times before Retrieve", f.count())
	}
	for range 3 {
		c, err := p.Retrieve(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if c.AccessKeyID != roleKeyID || c.SecretAccessKey != roleSecret || c.SessionToken != roleToken || !c.CanExpire {
			t.Fatalf("credentials = %+v", c)
		}
	}
	if f.count() != 1 {
		t.Errorf("fresh credentials assumed %d times, want 1 (cached)", f.count())
	}
	in := f.calls[0]
	if aws.ToString(in.RoleArn) != roleARN || aws.ToString(in.ExternalId) != roleExternalID || aws.ToString(in.RoleSessionName) != roleSession || aws.ToInt32(in.DurationSeconds) != int32(fss3.DefaultRoleDuration/time.Second) {
		t.Errorf("AssumeRole input = role %q ext %q session %q duration %d", aws.ToString(in.RoleArn), aws.ToString(in.ExternalId), aws.ToString(in.RoleSessionName), aws.ToInt32(in.DurationSeconds))
	}

	// Credentials inside the renewal window are assumed again on every
	// Retrieve, so a request never goes out with nearly-expired keys.
	near := &fakeAssumer{life: 30 * time.Second}
	o := roleOpts()
	o.Duration = 2 * time.Hour
	o.Client = near
	p, err = fss3.RoleCredentials(nil, o)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := p.Retrieve(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if near.count() != 2 {
		t.Errorf("nearly-expired credentials assumed %d times, want 2 (renewed)", near.count())
	}
	if got := aws.ToInt32(near.calls[0].DurationSeconds); got != int32(2*time.Hour/time.Second) {
		t.Errorf("duration = %d s", got)
	}
}

// TestRoleCredentialsRefusals: every required field is required (the
// external ID above all), the duration is bounded, an STS refusal is
// CodePermission, and no error carries the external ID.
func TestRoleCredentialsRefusals(t *testing.T) {
	f := &fakeAssumer{life: time.Hour}
	for name, mut := range map[string]func(*fss3.RoleOptions){
		"no role":         func(o *fss3.RoleOptions) { o.RoleARN = "" },
		"no external ID":  func(o *fss3.RoleOptions) { o.ExternalID = "" },
		"no session":      func(o *fss3.RoleOptions) { o.SessionName = "" },
		"too short":       func(o *fss3.RoleOptions) { o.Duration = time.Minute },
		"too long":        func(o *fss3.RoleOptions) { o.Duration = 13 * time.Hour },
		"no STS client":   func(o *fss3.RoleOptions) {},
		"negative length": func(o *fss3.RoleOptions) { o.Duration = -time.Hour },
	} {
		o := roleOpts()
		mut(&o)
		client := fss3.AssumeRoler(f)
		if name == "no STS client" {
			client = nil
		}
		_, err := fss3.RoleCredentials(client, o)
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), roleExternalID) {
			t.Errorf("%s: error leaks the external ID: %v", name, err)
		}
	}
	deny := &fakeAssumer{err: &smithy.GenericAPIError{Code: "AccessDenied", Message: "not authorized to perform sts:AssumeRole"}}
	o := roleOpts()
	o.Client = deny
	_, err := fss3.NewRoleClient(t.Context(), aws.Config{Region: "us-east-1"}, o)
	if filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("refused AssumeRole at opening = %v, want CodePermission", err)
	}
	if _, err := fss3.NewRoleClient(t.Context(), aws.Config{}, fss3.RoleOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("NewRoleClient without a role = %v", err)
	}
}

// TestRoleCredentialsPrinting: the provider, and an S3 client built on it
// after the credentials were retrieved, print neither the keys nor the
// external ID under any verb.
func TestRoleCredentialsPrinting(t *testing.T) {
	f := &fakeAssumer{life: time.Hour}
	o := roleOpts()
	o.Client = f
	client, err := fss3.NewRoleClient(t.Context(), aws.Config{Region: "us-east-1"}, o)
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 {
		t.Errorf("NewRoleClient assumed the role %d times, want 1 (checked at opening)", f.count())
	}
	creds := client.Options().Credentials
	if _, err := creds.Retrieve(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{creds, client, client.Options()} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			out := fmt.Sprintf(verb, v)
			for _, secret := range []string{roleSecret, roleToken, roleExternalID} {
				if strings.Contains(out, secret) {
					t.Errorf("%s of %T leaks %q: %s", verb, v, secret, out)
				}
			}
		}
	}
	if g, ok := creds.(fmt.GoStringer); !ok || g.GoString() != fmt.Sprint(creds) {
		t.Errorf("provider GoString differs from String")
	}
	if s := fmt.Sprint(creds); !strings.Contains(s, roleARN) || !strings.Contains(s, roleSession) {
		t.Errorf("provider prints %q, want the role and session for debugging", s)
	}
}

// stsServer is an STS endpoint answering AssumeRole (the query protocol)
// and recording the form of each call.
func stsServer(t *testing.T, life time.Duration) (*httptest.Server, func() []url.Values) {
	t.Helper()
	var (
		mu    sync.Mutex
		forms []url.Values
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		forms = append(forms, r.PostForm)
		mu.Unlock()
		if r.PostForm.Get("ExternalId") != roleExternalID {
			w.Header().Set("Content-Type", "text/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `<ErrorResponse><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>external id mismatch</Message></Error><RequestId>r</RequestId></ErrorResponse>`)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		_, _ = fmt.Fprintf(w, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials><AccessKeyId>%s</AccessKeyId><SecretAccessKey>%s</SecretAccessKey><SessionToken>%s</SessionToken><Expiration>%s</Expiration></Credentials><AssumedRoleUser><Arn>%s/%s</Arn><AssumedRoleId>AROA:%s</AssumedRoleId></AssumedRoleUser></AssumeRoleResult><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></AssumeRoleResponse>`,
			roleKeyID, roleSecret, roleToken, time.Now().Add(life).UTC().Format(time.RFC3339), roleARN, roleSession, roleSession)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return append([]url.Values(nil), forms...)
	}
}

// tokenRecorder records the access key ID and the session token of each
// S3 request.
type tokenRecorder struct {
	next   http.Handler
	mu     sync.Mutex
	keys   []string
	tokens []string
}

func (a *tokenRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, cred, _ := strings.Cut(r.Header.Get("Authorization"), "Credential=")
	key, _, _ := strings.Cut(cred, "/")
	a.mu.Lock()
	a.keys = append(a.keys, key)
	a.tokens = append(a.tokens, r.Header.Get("X-Amz-Security-Token"))
	a.mu.Unlock()
	a.next.ServeHTTP(w, r)
}

// TestRoleDSN opens one tenant's disk from a DSN through a Mux: the
// application's own keys (the *_env parameters) only call STS, with the
// tenant's external ID, and every S3 request is signed with the assumed
// role's key and session token. A wrong external ID fails at opening, and
// the role parameters are validated.
func TestRoleDSN(t *testing.T) {
	ctx := t.Context()
	stsSrv, forms := stsServer(t, time.Hour)
	_, bucket, proxy := fakeS3WithProxy(t)
	rec := &tokenRecorder{next: proxy}
	s3Srv := httptest.NewServer(rec)
	t.Cleanup(s3Srv.Close)
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	t.Setenv("AWS_ENDPOINT_URL_STS", stsSrv.URL)
	t.Setenv("APP_KEY", "APPKEY")
	t.Setenv("APP_SECRET", "app-secret")
	mux := filesystem.NewMux()
	_ = mux.Register(fss3.Scheme, fss3.Opener)
	base := "s3://" + bucket + "?region=us-east-1&path_style=true&endpoint=" + url.QueryEscape(s3Srv.URL) + "&access_key_env=APP_KEY&secret_key_env=APP_SECRET"
	role := "&role_arn=" + url.QueryEscape(roleARN) + "&role_session_name=" + roleSession + "&role_duration=30m"
	d, err := mux.Open(ctx, "acme", base+role+"&external_id="+roleExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(rec.keys) == 0 {
		t.Fatal("no S3 request recorded")
	}
	for i, k := range rec.keys {
		if k != roleKeyID || rec.tokens[i] != roleToken {
			t.Errorf("S3 request %d signed with key %q token %q, want the role's", i, k, rec.tokens[i])
		}
	}
	got := forms()
	if len(got) != 1 {
		t.Fatalf("STS called %d times, want 1", len(got))
	}
	if f := got[0]; f.Get("Action") != "AssumeRole" || f.Get("RoleArn") != roleARN || f.Get("RoleSessionName") != roleSession || f.Get("DurationSeconds") != "1800" {
		t.Errorf("STS form = %v", f)
	}
	if _, err := mux.Open(ctx, "evil", base+role+"&external_id=guessed"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("wrong external ID = %v, want CodePermission", err)
	}
	for name, q := range map[string]string{
		"no external ID":   role,
		"bad duration":     strings.Replace(role, "30m", "soon", 1) + "&external_id=x",
		"role params only": "&external_id=x&role_session_name=s",
	} {
		_, err := mux.Open(ctx, "bad", base+q)
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestRoleClientSigns proves NewRoleClient's client signs S3 requests with
// the assumed credentials, not the base configuration's.
func TestRoleClientSigns(t *testing.T) {
	ctx := t.Context()
	_, bucket, proxy := fakeS3WithProxy(t)
	rec := &tokenRecorder{next: proxy}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	o := roleOpts()
	o.Client = &fakeAssumer{life: time.Hour}
	base := aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("APPKEY", "app-secret", "")}
	client, err := fss3.NewRoleClient(ctx, base, o, func(so *s3.Options) { so.BaseEndpoint, so.UsePathStyle = aws.String(srv.URL), true })
	if err != nil {
		t.Fatal(err)
	}
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("acme", drv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(rec.keys) == 0 || rec.keys[0] != roleKeyID || rec.tokens[0] != roleToken {
		t.Errorf("signed with %v / %v, want the role's key and token", rec.keys, rec.tokens)
	}
}
