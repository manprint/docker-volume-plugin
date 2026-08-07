package dvol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

// NewStore returns an archive store rooted at cfg.Root (DefaultRoot if empty).
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
	if err := s.recoverInterruptedReplacements(); err != nil {
		return nil, fmt.Errorf("recover interrupted volume replacement: %w", err)
	}
	return s, nil
}

func (s *Store) volumesDir() string { return filepath.Join(s.cfg.Root, volumesDir) }
func (s *Store) metaDir() string    { return filepath.Join(s.cfg.Root, metaDir) }

// backupsDir returns the root directory holding all backups. It lives under
// the store root by default, or wherever Config.BackupsDir points to.
func (s *Store) backupsDir() string {
	if s.cfg.BackupsDir != "" {
		return s.cfg.BackupsDir
	}
	return filepath.Join(s.cfg.Root, backupsDir)
}

// VolumePath returns the data directory of a volume.
func (s *Store) VolumePath(name string) string { return filepath.Join(s.volumesDir(), name) }

func (s *Store) metaPath(name string) string { return filepath.Join(s.metaDir(), name+".json") }

// recoverInterruptedReplacements repairs the only non-atomic window in a
// directory swap: a process can die after the old target is renamed aside but
// before the staged target is renamed into place. Rollback directories are
// private and include the target name so they can be recovered on startup.
func (s *Store) recoverInterruptedReplacements() error {
	metaEntries, err := os.ReadDir(s.metaDir())
	if err != nil {
		return err
	}
	volumeEntries, err := os.ReadDir(s.volumesDir())
	if err != nil {
		return err
	}
	changed := false
	for _, metaEntry := range metaEntries {
		if metaEntry.IsDir() || !strings.HasSuffix(metaEntry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(metaEntry.Name(), ".json")
		if checkName(name) != nil {
			continue
		}
		prefix := "." + name + ".dvol-old-"
		var rollback []string
		for _, volumeEntry := range volumeEntries {
			if strings.HasPrefix(volumeEntry.Name(), prefix) {
				if !volumeEntry.IsDir() {
					return fmt.Errorf("rollback path %q is not a directory", volumeEntry.Name())
				}
				rollback = append(rollback, filepath.Join(s.volumesDir(), volumeEntry.Name()))
			}
		}
		if len(rollback) == 0 {
			continue
		}
		if len(rollback) > 1 {
			return fmt.Errorf("multiple interrupted replacements for volume %q", name)
		}

		target := s.VolumePath(name)
		_, targetErr := os.Lstat(target)
		switch {
		case targetErr == nil:
			// The new target was published before the crash; only cleanup was
			// interrupted, so discard the old copy.
			if err := os.RemoveAll(rollback[0]); err != nil {
				return err
			}
			changed = true
		case os.IsNotExist(targetErr):
			// The first rename completed but the publish did not. Restore the
			// old target so the volume name is present again.
			if err := os.Rename(rollback[0], target); err != nil {
				return err
			}
			changed = true
		default:
			return targetErr
		}
	}
	if changed {
		return syncDir(s.volumesDir())
	}
	return nil
}

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

// Get returns the volume and its metadata, or ErrNotFound. The Size field is
// not populated (computing it walks the whole volume); use Size or List.
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
	return &Volume{
		Name:      m.Name,
		CreatedAt: m.CreatedAt,
		Options:   m.Options,
	}, nil
}

// Size returns the total size in bytes of the volume data.
func (s *Store) Size(name string) int64 {
	return dirSize(s.VolumePath(name))
}

// List returns all volumes sorted by name, with Size populated.
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
		v.Size = s.Size(name)
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
