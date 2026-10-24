package filesystem

import (
	"sort"
	"strings"
)

// PageOf builds one listing page from every object under dir, for drivers
// that can enumerate their keys (memory, local disk) and so do not need to
// implement directory folding and cursors themselves. objects must hold
// only keys inside dir (see KeyInDir); their order does not matter.
//
// It sorts by key, folds sub-directories into IsDir entries when the listing
// is not recursive, skips everything up to and including opts.Cursor, and
// cuts the page at opts.Limit (DefaultPageSize when 0). The cursor is the
// key of the last entry returned, so a listing stays correct when keys are
// added or deleted between pages.
func PageOf(objects []Entry, dir string, opts ListOptions) Page {
	entries := objects
	if !opts.Recursive {
		entries = foldDirs(objects, dir)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	start := 0
	if opts.Cursor != "" {
		start = sort.Search(len(entries), func(i int) bool { return entries[i].Key > opts.Cursor })
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}
	end := min(start+limit, len(entries))
	page := Page{Entries: append([]Entry(nil), entries[start:end]...)}
	if end < len(entries) {
		page.NextCursor = entries[end-1].Key
	}
	return page
}

// foldDirs keeps direct children of dir and replaces deeper keys with one
// directory entry per immediate sub-directory.
func foldDirs(objects []Entry, dir string) []Entry {
	out := make([]Entry, 0, len(objects))
	seen := make(map[string]bool)
	for _, e := range objects {
		rel := RelativeKey(e.Key, dir)
		first, _, nested := strings.Cut(rel, separator)
		if !nested {
			out = append(out, e)
			continue
		}
		sub := JoinKey(dir, first)
		if !seen[sub] {
			seen[sub] = true
			out = append(out, Entry{Key: sub, IsDir: true})
		}
	}
	return out
}
