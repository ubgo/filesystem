// Command gqlgen serves a GraphQL API whose mutations take files as the
// Upload scalar of the GraphQL multipart request specification, stored
// through contrib/gqlgen under an upload.Policy: uploadFile and
// uploadFiles on a disk, setAvatar as a records row attached to the
// signed-in user. The X-User header stands in for authentication.
//
//	go run .      (from examples/gqlgen; listens on localhost:8195)
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/ubgo/sqlb"
	_ "modernc.org/sqlite" // the records tables live in in-memory SQLite

	"github.com/ubgo/filesystem"
	fsgql "github.com/ubgo/filesystem/contrib/gqlgen"
	"github.com/ubgo/filesystem/examples/gqlgen/graph"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// Decisions of this example, not of the library.
const (
	addr        = "localhost:8195"
	pathGraphQL = "/graphql"
	headerUser  = "X-User"
	diskMedia   = "media"
	// maxFiles caps the files of one request, uploadFiles' list included.
	maxFiles          = 5
	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 5 * time.Second
)

// policy is what every upload must satisfy: images, PDFs and plain text
// up to 1 MiB, stored under incoming/ with random names.
var policy = upload.Policy{
	MaxSize: 1 << 20,
	Allowed: []string{"image/*", "application/pdf", "text/plain"},
	Key:     upload.RandomKey("incoming"),
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h, _, err := newServer(ctx)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: readHeaderTimeout}
	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	log.Printf("listening on http://%s%s", addr, pathGraphQL)
	select {
	case err = <-errs:
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		err = srv.Shutdown(shutdown)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// newServer wires the disk, the records store and the GraphQL handler,
// and returns the resolver for tests to inspect.
func newServer(ctx context.Context) (http.Handler, *graph.Resolver, error) {
	media, err := filesystem.NewDisk(diskMedia, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(media)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1) // one connection keeps the in-memory database alive
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(graph.OwnerUser))
	if err != nil {
		return nil, nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	resolver := &graph.Resolver{Media: media, Store: store, DiskName: diskMedia, Policy: policy, MaxFiles: maxFiles}
	mux := http.NewServeMux()
	mux.Handle(pathGraphQL, withUser(newGraphQL(resolver)))
	return mux, resolver, nil
}

// newGraphQL is the GraphQL handler: JSON posts, multipart uploads capped
// from the policy, and the library's errors presented without the
// backend's message.
// ds:def id=newgraphql-du36vw6u
func newGraphQL(r *graph.Resolver) http.Handler {
	srv := handler.New(graph.NewExecutableSchema(graph.Config{Resolvers: r}))
	srv.AddTransport(transport.POST{})
	srv.AddTransport(fsgql.MultipartForm(r.Policy, r.MaxFiles))
	srv.SetErrorPresenter(fsgql.ErrorPresenter(nil))
	return srv
}

// withUser records the X-User header as the actor of every change, as an
// application's authentication middleware would.
func withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get(headerUser); user != "" {
			r = r.WithContext(records.WithActor(r.Context(), user))
		}
		next.ServeHTTP(w, r)
	})
}
