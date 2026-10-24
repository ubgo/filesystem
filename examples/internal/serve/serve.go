// Package serve runs an example's HTTP server until Ctrl+C or SIGTERM and
// then shuts it down gracefully, so main returns normally and its deferred
// clean-up (temporary directories, in-process fake servers) runs instead
// of being skipped by a killed process.
package serve

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ShutdownTimeout bounds how long a stopping server waits for requests in
// flight.
const ShutdownTimeout = 5 * time.Second

// Context returns a context cancelled by Ctrl+C (SIGINT) or SIGTERM, and
// the function that stops listening for them.
func Context() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// UntilDone serves srv until ctx ends, then shuts it down gracefully and
// returns nil; a server that fails to start or stops on its own returns
// that error.
func UntilDone(ctx context.Context, srv *http.Server) error {
	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
