package filesystem

import (
	"context"
	"encoding/base64"
	"encoding/json"
)

// listSource is one ordered stream a merged listing draws from: the routed
// and overlay disks list several disks as one.
type listSource struct {
	// page returns the source's page at its own cursor ("" for the first),
	// with keys already translated to the merged disk's key space and in
	// ascending order.
	page func(ctx context.Context, cursor string) (Page, error)
	// keep drops entries the merged listing must not show; nil keeps all.
	keep func(Entry) bool
}

// mergeCursor is the state a merged listing resumes from: the last key it
// returned, and per source the cursor of the page that source was on and
// whether it is exhausted. Resuming refetches each source's page and skips
// keys up to Last, so keys added or deleted between pages do not shift it.
type mergeCursor struct {
	Last    string   `json:"l"`
	Cursors []string `json:"c"`
	Done    []bool   `json:"d"`
}

// mergeResolve decides what the merged listing shows for one key. heads
// holds, per source, its entry at that key or nil. It returns the entry to
// show and whether to show one at all.
type mergeResolve func(ctx context.Context, heads []*Entry) (Entry, bool, error)

// sourceState is one source's position inside a merged page.
type sourceState struct {
	entries []Entry // the current page, filtered, keys after the resume point
	pos     int
	cursor  string // the cursor that fetched entries
	next    string // the page's NextCursor
	done    bool
}

// mergeLists merges sources into one page in ascending key order, so a
// listing over several disks pages like a listing over one. Each key is
// shown once, as resolve decides.
func mergeLists(ctx context.Context, sources []listSource, opts ListOptions, resolve mergeResolve) (Page, error) {
	cur, err := decodeMergeCursor(opts.Cursor, len(sources))
	if err != nil {
		return Page{}, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}
	states := make([]*sourceState, len(sources))
	for i := range sources {
		states[i] = &sourceState{cursor: cur.Cursors[i], done: cur.Done[i]}
		if !states[i].done {
			if err := fill(ctx, sources[i], states[i], cur.Last); err != nil {
				return Page{}, err
			}
		}
	}
	var out []Entry
	last := cur.Last
	for len(out) < limit {
		lowest, found := "", false
		for _, s := range states {
			if !s.done && (!found || s.entries[s.pos].Key < lowest) {
				lowest, found = s.entries[s.pos].Key, true
			}
		}
		if !found {
			break
		}
		heads := make([]*Entry, len(states))
		for i, s := range states {
			if s.done || s.entries[s.pos].Key != lowest {
				continue
			}
			e := s.entries[s.pos]
			heads[i] = &e
			s.pos++
			if s.pos == len(s.entries) {
				if s.next == "" {
					s.done = true
				} else {
					s.cursor = s.next
					if err := fill(ctx, sources[i], s, lowest); err != nil {
						return Page{}, err
					}
				}
			}
		}
		e, show, err := resolve(ctx, heads)
		if err != nil {
			return Page{}, err
		}
		if show {
			out = append(out, e)
		}
		last = lowest
	}
	page := Page{Entries: out}
	next := mergeCursor{Last: last, Cursors: make([]string, len(states)), Done: make([]bool, len(states))}
	more := false
	for i, s := range states {
		next.Cursors[i], next.Done[i] = s.cursor, s.done
		more = more || !s.done
	}
	if more {
		page.NextCursor = encodeMergeCursor(next)
	}
	return page, nil
}

// fill loads s's page at s.cursor, keeping the entries src keeps whose key
// is after last, and moves on to the next page while that leaves nothing.
func fill(ctx context.Context, src listSource, s *sourceState, last string) error {
	for {
		p, err := src.page(ctx, s.cursor)
		if err != nil {
			return err
		}
		s.entries, s.pos, s.next = s.entries[:0], 0, p.NextCursor
		for _, e := range p.Entries {
			if e.Key > last && (src.keep == nil || src.keep(e)) {
				s.entries = append(s.entries, e)
			}
		}
		if len(s.entries) > 0 {
			return nil
		}
		if p.NextCursor == "" {
			s.done = true
			return nil
		}
		s.cursor = p.NextCursor
	}
}

// encodeMergeCursor makes the state an opaque cursor string.
func encodeMergeCursor(c mergeCursor) string {
	// Marshalling strings and bools cannot fail.
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeMergeCursor reads a cursor issued by mergeLists for n sources; ""
// starts at the beginning.
func decodeMergeCursor(s string, n int) (mergeCursor, error) {
	c := mergeCursor{Cursors: make([]string, n), Done: make([]bool, n)}
	if s == "" {
		return c, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil || len(c.Cursors) != n || len(c.Done) != n {
		return mergeCursor{}, Errorf(CodeInvalidArgument, "list cursor was not issued by this disk")
	}
	return c, nil
}
