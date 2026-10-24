package tus

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// infoSuffix ends the name of the JSON file tusd's filestore keeps beside
// each session's data file, which is named by the session ID alone.
const infoSuffix = ".info"

// lockSuffixes end the names of the files tusd's filelocker keeps beside
// a session while a request holds or asks for its lock; a crash can leave
// them behind, so Sweep deletes them with their session.
var lockSuffixes = []string{".lock", ".stop"}

// sessionID matches the IDs tusd generates (128 random bits in hex). New
// never lets a client choose an ID, so every session file in TempDir is
// named by one; Sweep touches nothing else.
var sessionID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Sweep deletes the upload sessions in dir (a Config.TempDir) that have
// received nothing since olderThan, and returns how many it deleted.
//
// A finished upload's session is deleted by the handler, whatever the
// outcome; what remains in TempDir is uploads a client started and never
// finished, which tusd's filestore keeps forever. Run Sweep on a schedule
// (an hourly job deleting sessions idle for a day is typical), on every
// instance whose TempDir it is. A session's last activity is the later
// modification time of its data file (written by every PATCH) and its
// .info file, so a session still receiving bytes is never swept; a client
// that resumes after its session was swept gets 404 and starts again, as
// tus clients do.
//
// Only files named by a tusd session ID (with or without the .info
// suffix) are considered, and a swept session's filelocker files
// (.lock, .stop) go with it, so a stray file in the directory is left
// alone; a data file whose .info was never written (a crash during
// creation) is swept too. Sweep carries on past a file it cannot delete and returns the
// failures joined, with the count of sessions it did delete; the next run
// retries them. A missing dir is not an error: nothing was ever uploaded.
func Sweep(ctx context.Context, dir string, olderThan time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	last := map[string]time.Time{}
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), infoSuffix)
		if !e.Type().IsRegular() || !sessionID.MatchString(id) {
			continue
		}
		// An Info error means the file went since the listing (the
		// session finished or was cancelled): nothing to sweep.
		if info, err := e.Info(); err == nil && info.ModTime().After(last[id]) {
			last[id] = info.ModTime()
		}
	}
	var errs []error
	swept := 0
	for id, at := range last {
		if err := ctx.Err(); err != nil {
			return swept, errors.Join(append(errs, err)...)
		}
		if !at.Before(olderThan) {
			continue
		}
		// Either file left behind by a failure is still a session file,
		// so the next run finds it and retries.
		err := errors.Join(removeIfExists(filepath.Join(dir, id)), removeIfExists(filepath.Join(dir, id+infoSuffix)))
		for _, suffix := range lockSuffixes {
			err = errors.Join(err, removeIfExists(filepath.Join(dir, id+suffix)))
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		swept++
	}
	return swept, errors.Join(errs...)
}

// removeIfExists removes path; a file already gone is not a failure.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
