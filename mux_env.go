package filesystem

import (
	"context"
	"errors"
	"strings"
)

// Variable names OpenManager reads, each after the caller's prefix.
const (
	// EnvDisks lists the disk names, comma-separated ("media,backups").
	EnvDisks = "DISKS"
	// EnvDefaultDisk names the default disk; empty means the first listed.
	EnvDefaultDisk = "DEFAULT_DISK"
	// EnvDSNSuffix ends each disk's DSN variable: <PREFIX><NAME>_DSN.
	EnvDSNSuffix = "_DSN"
)

// LookupFunc reads one configuration value and reports whether it is set:
// os.LookupEnv in production, a map in tests. Taking it as an argument,
// rather than reading the process environment, keeps OpenManager free of
// hidden state.
type LookupFunc func(key string) (string, bool)

// EnvName is the variable that holds the DSN of disk name under prefix:
// the name upper-cased, with "-" and "." as "_" ("STORAGE_" and
// "media-cdn" give "STORAGE_MEDIA_CDN_DSN").
func EnvName(prefix, name string) string {
	return prefix + strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToUpper(name)) + EnvDSNSuffix
}

// OpenManager builds a Manager from configuration, twelve-factor style:
//
//	STORAGE_DISKS=media,backups
//	STORAGE_MEDIA_DSN=s3://media?region=auto&visibility=public
//	STORAGE_BACKUPS_DSN=file:///var/backups
//	STORAGE_DEFAULT_DISK=media
//
// with prefix "STORAGE_". opts apply to every disk. A missing or invalid
// setting is CodeInvalidArgument naming the variable, never its value
// (DSNs carry credentials), and the disks opened so far are closed. An
// error from opening a disk (an unknown scheme, a bad DSN parameter, a
// failed connection) keeps its code and names both the variable and the
// disk: "STORAGE_MEDIA_DSN: disk media: unknown DSN parameter …".
func (m *Mux) OpenManager(ctx context.Context, prefix string, lookup LookupFunc, opts ...Option) (*Manager, error) {
	if lookup == nil {
		return nil, Errorf(CodeInvalidArgument, "nil LookupFunc")
	}
	list, _ := lookup(prefix + EnvDisks)
	var names []string
	for n := range strings.SplitSeq(list, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, Errorf(CodeInvalidArgument, "%s%s lists no disks", prefix, EnvDisks)
	}
	mgr, _ := NewManager()
	seen := map[string]string{} // variable → disk name
	fail := func(err error) (*Manager, error) { return nil, errors.Join(err, mgr.Close()) }
	for _, name := range names {
		key := EnvName(prefix, name)
		if other, dup := seen[key]; dup {
			return fail(Errorf(CodeInvalidArgument, "disks %q and %q both read %s", other, name, key))
		}
		seen[key] = name
		dsn, ok := lookup(key)
		if !ok || strings.TrimSpace(dsn) == "" {
			return fail(Errorf(CodeInvalidArgument, "%s is not set (disk %q)", key, name))
		}
		d, err := m.Open(ctx, name, dsn, opts...)
		if err != nil {
			// Open already names the disk; the variable is what an
			// operator edits to fix it.
			return fail(prefixed(err, key))
		}
		_ = mgr.Add(d) // names are distinct: equal names share a variable, refused above
	}
	if def, ok := lookup(prefix + EnvDefaultDisk); ok && def != "" {
		if err := mgr.SetDefault(def); err != nil {
			return fail(Errorf(CodeInvalidArgument, "%s%s names %q, which is not in %s%s", prefix, EnvDefaultDisk, def, prefix, EnvDisks))
		}
	}
	return mgr, nil
}
