package filesystem

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// Manager holds an application's named disks and its default disk. It is a value the application owns, not a
// global, so tests and multiple apps in one process never share disks.
// It is safe for concurrent use.
type Manager struct {
	mu    sync.RWMutex
	disks map[string]*Disk
	def   string
}

// NewManager returns a Manager holding disks. The first disk is the default
// until SetDefault changes it. Names must be unique.
func NewManager(disks ...*Disk) (*Manager, error) {
	m := &Manager{disks: make(map[string]*Disk)}
	for _, d := range disks {
		if err := m.Add(d); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Add registers d under its name. A second disk with the same name is
// CodeExists. The first disk added becomes the default.
func (m *Manager) Add(d *Disk) error {
	if d == nil {
		return Errorf(CodeInvalidArgument, "nil disk")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.disks[d.name]; dup {
		return Errorf(CodeExists, "disk %q is already registered", d.name)
	}
	m.disks[d.name] = d
	if m.def == "" {
		m.def = d.name
	}
	return nil
}

// SetDefault makes the named disk the default.
func (m *Manager) SetDefault(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.disks[name]; !ok {
		return Errorf(CodeNotFound, "no disk %q", name)
	}
	m.def = name
	return nil
}

// Disk returns the named disk, or CodeNotFound.
func (m *Manager) Disk(name string) (*Disk, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.disks[name]
	if !ok {
		return nil, Errorf(CodeNotFound, "no disk %q", name)
	}
	return d, nil
}

// Default returns the default disk, or CodeNotFound when none is registered.
func (m *Manager) Default() (*Disk, error) {
	m.mu.RLock()
	name := m.def
	m.mu.RUnlock()
	if name == "" {
		return nil, Errorf(CodeNotFound, "no disks registered")
	}
	return m.Disk(name)
}

// Names returns the registered disk names in sorted order.
func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.disks))
	for n := range m.disks {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Check runs Disk.Check on every disk and joins the failures, for a start-up
// or health probe.
func (m *Manager) Check(ctx context.Context) error {
	var errs []error
	for _, n := range m.Names() {
		d, err := m.Disk(n)
		if err == nil {
			err = d.Check(ctx)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close closes every disk and joins the failures. Disks that share a driver
// (scoped and mirror disks) may close it more than once; built-in drivers
// tolerate that.
func (m *Manager) Close() error {
	var errs []error
	for _, n := range m.Names() {
		d, _ := m.Disk(n)
		if err := d.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CopyAcross copies srcKey on src to dstKey on dst. On the same disk it is
// Disk.Copy (server-side when supported); across disks it streams the bytes,
// keeping content type, and metadata and visibility where dst stores them.
func CopyAcross(ctx context.Context, src *Disk, srcKey string, dst *Disk, dstKey string) (Entry, error) {
	if src == dst {
		return src.Copy(ctx, srcKey, dstKey)
	}
	info, err := src.Stat(ctx, srcKey)
	if err != nil {
		return Entry{}, err
	}
	rc, err := src.Read(ctx, srcKey)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = rc.Close() }()
	opts := WriteOptions{ContentType: info.ContentType, SizeHint: info.Size}
	if dst.caps.Metadata {
		opts.Metadata = info.Metadata
	}
	if dst.caps.Headers {
		opts.Headers = info.Headers
	}
	if dst.caps.SetVisibility && info.Visibility.Valid() {
		opts.Visibility = info.Visibility
	}
	return dst.Write(ctx, dstKey, rc, opts)
}

// MoveAcross is CopyAcross followed by deleting the source. It is not
// atomic: if the delete fails, the object exists on both disks and the
// error is returned.
func MoveAcross(ctx context.Context, src *Disk, srcKey string, dst *Disk, dstKey string) (Entry, error) {
	if src == dst {
		return src.Move(ctx, srcKey, dstKey)
	}
	e, err := CopyAcross(ctx, src, srcKey, dst, dstKey)
	if err != nil {
		return Entry{}, err
	}
	if err := src.Delete(ctx, srcKey); err != nil {
		return Entry{}, err
	}
	return e, nil
}
