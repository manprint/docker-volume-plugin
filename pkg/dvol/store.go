package dvol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
	"unicode"
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
	// Only the repository root is created eagerly. The archive destination may
	// be a directory inside a Docker volume source; the backup path validates
	// that relationship before creating any destination directory. The
	// store-managed volume directories are created on demand by Create, so a
	// pure archive client does not get empty volumes/ and meta/ trees.
	if err := os.MkdirAll(cfg.Root, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", cfg.Root, err)
	}
	if err := s.recoverReplacements(); err != nil {
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

func (s *Store) ensureVolumeDirs() error {
	for _, d := range []string{s.volumesDir(), s.metaDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return nil
}

func checkName(name string) error {
	if !nameRe.MatchString(name) {
		return ErrInvalidName
	}
	return nil
}

// checkArchiveStem validates a caller-supplied archive name stem or bundle
// prefix. It must be a plain file name component: anything that could escape
// the destination directory is rejected before a path is built from it.
func checkArchiveStem(stem string) error {
	if stem == "" {
		return fmt.Errorf("empty archive name")
	}
	if stem == "." || stem == ".." || filepath.Base(stem) != stem ||
		containsAny(stem, `/\:*?[]`) {
		return fmt.Errorf("invalid archive name %q (use a file name, not a path)", stem)
	}
	for _, r := range stem {
		if unicode.IsControl(r) {
			return fmt.Errorf("invalid archive name %q (control characters are not allowed)", stem)
		}
	}
	// A leading dot would hide the archive and could collide with the control
	// files dvol keeps in the destination (.dvol-index.json, .dvol-reserve-*,
	// .dvol-backup-*).
	if stem[0] == '.' {
		return fmt.Errorf("invalid archive name %q (must not start with a dot)", stem)
	}
	return nil
}

func containsAny(s, chars string) bool {
	for _, c := range chars {
		for _, r := range s {
			if r == c {
				return true
			}
		}
	}
	return false
}

// Create makes a new volume. An empty directory is created on disk and the
// metadata is persisted.
func (s *Store) Create(name string, opts map[string]string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := s.ensureVolumeDirs(); err != nil {
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
	path := s.metaPath(name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return ErrExists
		}
		return err
	}
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	committed = true
	return syncDir(s.metaDir())
}

// Get returns the volume and its metadata, or ErrNotFound. The Size field is
// not populated (computing it walks the whole volume); use Size or List.
func (s *Store) Get(name string) (*Volume, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
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
	if checkName(name) != nil {
		return 0
	}
	return dirSize(s.VolumePath(name))
}

// List returns all volumes sorted by name, with Size populated.
func (s *Store) List() ([]*Volume, error) {
	entries, err := os.ReadDir(s.metaDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []*Volume{}, nil
		}
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
	if err := checkName(name); err != nil {
		return err
	}
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
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
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
