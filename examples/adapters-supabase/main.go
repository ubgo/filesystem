// Command adapters-supabase stores files in Supabase Storage through its
// S3 protocol with the S3 driver: a DSN with the project's S3 endpoint,
// path-style addressing and keys read from the environment, then a write,
// a stat, a listing, a read and a delete.
//
// It runs offline: an in-process S3 (gofakes3) answers under the path
// Supabase's endpoint has, /storage/v1/s3, and nowhere else, so the
// program proves the DSN shape reaches that path. It was not run against a
// real Supabase project; behaviour specific to Supabase (its limits, which
// S3 features it honours) is not exercised.
//
//	go run ./adapters-supabase            (from the examples directory)
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// Decisions of this program: the bucket, the project's region, and the
// environment variables holding the S3 access key pair from the Supabase
// dashboard.
const (
	bucket       = "avatars"
	region       = "us-east-1"
	accessKeyEnv = "SUPABASE_S3_ACCESS_KEY_ID"
	secretKeyEnv = "SUPABASE_S3_SECRET_ACCESS_KEY"
)

// s3Path is the path of Supabase Storage's S3 endpoint under the project
// URL (https://<project-ref>.supabase.co/storage/v1/s3).
const s3Path = "/storage/v1/s3"

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// fakeProject stands in for a Supabase project: S3 requests under s3Path
// reach gofakes3 with the prefix removed; any other path is a 404. It
// records each request's path so the program can show where they went.
type fakeProject struct {
	s3 http.Handler

	mu    sync.Mutex
	paths []string
}

func (f *fakeProject) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if !strings.HasPrefix(r.URL.Path, s3Path+"/") {
		http.NotFound(w, r)
		return
	}
	http.StripPrefix(s3Path, f.s3).ServeHTTP(w, r)
}

// supabaseDSN is the DSN of a bucket in a Supabase project: the S3
// endpoint is the project URL plus /storage/v1/s3, addressing is
// path-style, and the keys come from the environment, never the DSN.
// ds:def id=supabasedsn-cqjtcxsy
func supabaseDSN(projectURL string) string {
	q := url.Values{}
	q.Set(fss3.ParamRegion, region)
	q.Set(fss3.ParamEndpoint, projectURL+s3Path)
	q.Set(fss3.ParamPathStyle, "true")
	q.Set(fss3.ParamAccessKeyEnv, accessKeyEnv)
	q.Set(fss3.ParamSecretKeyEnv, secretKeyEnv)
	return fss3.Scheme + "://" + bucket + "?" + q.Encode()
}

// openDisk registers the S3 opener and opens the Supabase bucket.
// ds:def id=opendisk-jg5srcsy
func openDisk(ctx context.Context, projectURL string) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		return nil, err
	}
	return mux.Open(ctx, "avatars", supabaseDSN(projectURL))
}

// storeProfile writes a user's profile file, reads its entry and lists
// the user's folder, reads it back, then deletes it.
// ds:def id=storeprofile-8ybfcf7y
func storeProfile(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	key := "users/42/profile.json"
	e, err := disk.Write(ctx, key, strings.NewReader(`{"name":"Ada"}`), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stored: %s %d bytes %s\n", e.Key, e.Size, e.ContentType)
	for entry, err := range disk.List(ctx, "users/42", filesystem.ListOptions{}) {
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "listed: %s\n", entry.Key)
	}
	b, err := disk.ReadAll(ctx, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "read back: %s\n", b)
	if err := disk.Delete(ctx, key); err != nil {
		return err
	}
	ok, err := disk.Exists(ctx, key)
	fmt.Fprintf(out, "exists after delete: %t\n", ok)
	return err
}

// run starts the stand-in project, opens the bucket through its S3
// endpoint and stores a file, printing each step to out.
func run(ctx context.Context, out io.Writer) error {
	backend := s3mem.New()
	if err := backend.CreateBucket(bucket); err != nil {
		return err
	}
	project := &fakeProject{s3: gofakes3.New(backend).Server()}
	srv := httptest.NewServer(project)
	defer srv.Close()
	for env, value := range map[string]string{accessKeyEnv: "demo-access-key", secretKeyEnv: "demo-secret"} {
		if err := os.Setenv(env, value); err != nil {
			return err
		}
	}
	disk, err := openDisk(ctx, srv.URL)
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	fmt.Fprintf(out, "capabilities: %s\n", disk.Capabilities())
	if err := storeProfile(ctx, out, disk); err != nil {
		return err
	}
	project.mu.Lock()
	first := project.paths[0]
	project.mu.Unlock()
	fmt.Fprintf(out, "first request: %s\n", first)
	_, err = disk.Write(ctx, "users/42/profile.json", strings.NewReader("{}"), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "if-not-exists: %s\n", filesystem.CodeOf(err))
	return nil
}
