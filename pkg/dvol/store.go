package dvol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// nameRe is the volume naming rule. Docker volumes follow
// [a-zA-Z0-9][a-zA-Z0-9_.-]*; enforce the same so names are safe on disk.
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

type meta struct {
	Name      string            `json:"name"`
	CreatedAt time.Time         `json:"created_at"`
	Options   map[string]string `json:"options,omitempty"`
}

// NewStore returns a Store rooted at cfg.Root (DefaultRoot if empty).
func NewStore(cfg Config) (*Store, error) {
	if cfg.Root == "" {
		cfg.Root = DefaultRoot
	}
	s := &Store{cfg: cfg}
	for _, d := range []string{cfg.Root, s.volumesDir(), s.metaDir(), s.backupsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return s, nil
}

func (s *Store) volumesDir() string { return filepath.Join(s.cfg.Root, volumesDir) }
func (s *Store) metaDir() string    { return filepath.Join(s.cfg.Root, metaDir) }
func (s *Store) backupsDir() string { return filepath.Join(s.cfg.Root, backupsDir) }

// VolumePath returns the data directory of a volume.
func (s *Store) VolumePath(name string) string { return filepath.Join(s.volumesDir(), name) }

func (s *Store) metaPath(name string) string { return filepath.Join(s.metaDir(), name+".json") }

func checkName(name string) error {
	if !nameRe.MatchString(name) {
		return ErrInvalidName
	}
	return nil
}

// Create makes a new volume. An empty directory is created on disk and the
// metadata is persisted.
func (s *Store) Create(name string, opts map[string]string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if _, err := os.Stat(s.metaPath(name)); err == nil {
		return ErrExists
	}
	if err := os.MkdirAll(s.VolumePath(name), 0o755); err != nil {
		return err
	}
	return s.writeMeta(name, opts)
}

func (s *Store) writeMeta(name string, opts map[string]string) error {
	if opts == nil {
		opts = map[string]string{}
	}
	m := meta{Name: name, CreatedAt: time.Now().UTC(), Options: opts}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(s.metaPath(name), b, 0o644)
}

// Get returns the volume and its metadata, or ErrNotFound.
func (s *Store) Get(name string) (*Volume, error) {
	b, err := os.ReadFile(s.metaPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("corrupt metadata for %q: %w", name, err)
	}
	return s.volumeFromMeta(m), nil
}

func (s *Store) volumeFromMeta(m meta) *Volume {
	return &Volume{
		Name:      m.Name,
		CreatedAt: m.CreatedAt,
		Options:   m.Options,
		Size:      dirSize(s.VolumePath(m.Name)),
	}
}

// List returns all volumes sorted by name.
func (s *Store) List() ([]*Volume, error) {
	entries, err := os.ReadDir(s.metaDir())
	if err != nil {
		return nil, err
	}
	vols := make([]*Volume, 0, len(entries))
	for _, e := range entries {
		name := nameFromMetaFile(e.Name())
		if name == "" {
			continue
		}
		v, err := s.Get(name)
		if err != nil {
			return nil, err
		}
		vols = append(vols, v)
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Name < vols[j].Name })
	return vols, nil
}

// Remove deletes the volume directory and its metadata.
func (s *Store) Remove(name string) error {
	if _, err := os.Stat(s.metaPath(name)); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	if err := os.RemoveAll(s.VolumePath(name)); err != nil {
		return err
	}
	return os.Remove(s.metaPath(name))
}

// nameFromMetaFile strips the ".json" suffix, returns "" if invalid.
func nameFromMetaFile(f string) string {
	if len(f) <= len(".json") || f[len(f)-len(".json"):] != ".json" {
		return ""
	}
	return f[:len(f)-len(".json")]
}

func dirSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
