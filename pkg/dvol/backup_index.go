package dvol

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const flatBackupIndexVersion = 1

const (
	flatBackupIndexName = ".dvol-index.json"
	flatBackupLockName  = ".dvol-index.lock"
)

// flatBackupIndex records the logical volume for archives stored directly in
// the backup destination. The pending state makes the archive/index update
// recoverable if the process stops between publishing the archive and marking
// the index entry complete.
type flatBackupIndex struct {
	Version int                    `json:"version"`
	Entries []flatBackupIndexEntry `json:"entries"`
}

type flatBackupIndexEntry struct {
	Volume    string    `json:"volume"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Pending   bool      `json:"pending,omitempty"`
}

func (s *Store) flatIndexPath() string {
	return filepath.Join(s.backupsDir(), flatBackupIndexName)
}

func (s *Store) flatLockPath() string {
	return filepath.Join(s.backupsDir(), flatBackupLockName)
}

func (s *Store) loadFlatIndex() (flatBackupIndex, bool, error) {
	b, err := os.ReadFile(s.flatIndexPath())
	if os.IsNotExist(err) {
		return flatBackupIndex{Version: flatBackupIndexVersion}, false, nil
	}
	if err != nil {
		return flatBackupIndex{}, false, err
	}
	var idx flatBackupIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return flatBackupIndex{}, false, fmt.Errorf("decode flat backup metadata: %w", err)
	}
	if idx.Version != flatBackupIndexVersion {
		return flatBackupIndex{}, false, fmt.Errorf("unsupported flat backup metadata version %d", idx.Version)
	}

	changed := false
	seen := make(map[string]bool, len(idx.Entries))
	kept := idx.Entries[:0]
	for _, entry := range idx.Entries {
		if err := checkName(entry.Volume); err != nil || !isArchiveFileName(entry.Name) || seen[entry.Name] {
			return flatBackupIndex{}, false, fmt.Errorf("invalid flat backup metadata entry %q", entry.Name)
		}
		seen[entry.Name] = true
		path := filepath.Join(s.backupsDir(), entry.Name)
		info, statErr := os.Stat(path)
		if os.IsNotExist(statErr) {
			// A pending entry without its archive is a crash before publish;
			// a completed entry without its archive was removed externally.
			changed = true
			continue
		}
		if statErr != nil {
			return flatBackupIndex{}, false, statErr
		}
		if !info.Mode().IsRegular() {
			return flatBackupIndex{}, false, fmt.Errorf("flat backup %q is not a regular file", entry.Name)
		}
		if entry.Pending {
			// The archive publication is atomic. If it exists, the pending
			// operation completed far enough to be safely visible.
			entry.Pending = false
			changed = true
		}
		kept = append(kept, entry)
	}
	idx.Entries = kept
	return idx, changed, nil
}

func (s *Store) withFlatIndexLock(fn func(*flatBackupIndex) error) error {
	if err := os.MkdirAll(s.backupsDir(), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.flatLockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), syscall.LOCK_UN)

	idx, _, err := s.loadFlatIndex()
	if err != nil {
		return err
	}
	if err := fn(&idx); err != nil {
		return err
	}
	return s.writeFlatIndex(idx)
}

func (s *Store) readFlatIndex() (flatBackupIndex, error) {
	var result flatBackupIndex
	if _, err := os.Stat(s.flatIndexPath()); os.IsNotExist(err) {
		return result, nil
	}
	if err := os.MkdirAll(s.backupsDir(), 0o755); err != nil {
		return result, err
	}
	lock, err := os.OpenFile(s.flatLockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return result, err
	}
	defer unix.Flock(int(lock.Fd()), syscall.LOCK_UN)
	idx, changed, err := s.loadFlatIndex()
	if err != nil {
		return result, err
	}
	if changed {
		if err := s.writeFlatIndex(idx); err != nil {
			return result, err
		}
	}
	result = idx
	return result, err
}

func (s *Store) flatBackupInfos(volume string) ([]BackupInfo, error) {
	idx, err := s.readFlatIndex()
	if err != nil {
		return nil, err
	}
	infos := make([]BackupInfo, 0)
	for _, entry := range idx.Entries {
		if entry.Volume != volume {
			continue
		}
		path := filepath.Join(s.backupsDir(), entry.Name)
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		infos = append(infos, BackupInfo{
			Name:      entry.Name,
			Path:      path,
			Size:      info.Size(),
			CreatedAt: info.ModTime().UTC(),
		})
	}
	return infos, nil
}

func (s *Store) flatBackupMatches(volume, pattern string) ([]string, error) {
	if filepath.Base(pattern) != pattern {
		return nil, nil
	}
	idx, err := s.readFlatIndex()
	if err != nil {
		return nil, err
	}
	matches := make([]string, 0)
	for _, entry := range idx.Entries {
		if entry.Volume != volume {
			continue
		}
		matched, err := filepath.Match(pattern, entry.Name)
		if err != nil {
			return nil, err
		}
		if matched {
			matches = append(matches, filepath.Join(s.backupsDir(), entry.Name))
		}
	}
	return matches, nil
}

func (s *Store) writeFlatIndex(idx flatBackupIndex) error {
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.backupsDir(), ".dvol-index-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if err := f.Chmod(0o644); err != nil {
		cleanup()
		return err
	}
	if _, err := f.Write(b); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.flatIndexPath()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(s.backupsDir())
}

// writeFlatBackup serializes reservation, archive publication and metadata
// completion under one lock. This prevents another backup from mistaking a
// live pending reservation for a stale crash entry.
func (s *Store) writeFlatBackup(volume, name, path string, opts BackupOptions, populate func(*tar.Writer) error) error {
	return s.writeFlatBackupWithProgress(volume, name, path, opts, nil, populate)
}

func (s *Store) writeFlatBackupWithProgress(volume, name, path string, opts BackupOptions, progress *Progress, populate func(*tar.Writer) error) error {
	return s.withFlatIndexLock(func(idx *flatBackupIndex) error {
		previous := append([]flatBackupIndexEntry(nil), idx.Entries...)
		if opts.Overwrite {
			filtered := idx.Entries[:0]
			for _, entry := range idx.Entries {
				if entry.Name != name {
					filtered = append(filtered, entry)
				}
			}
			idx.Entries = filtered
		}
		for _, entry := range idx.Entries {
			if entry.Name == name {
				return fmt.Errorf("backup archive %q already exists", name)
			}
		}
		if _, err := os.Lstat(path); err == nil {
			if !opts.Overwrite {
				return fmt.Errorf("backup archive %q already exists", name)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		idx.Entries = append(idx.Entries, flatBackupIndexEntry{
			Volume:    volume,
			Name:      name,
			CreatedAt: time.Now().UTC(),
			Pending:   true,
		})
		if err := s.writeFlatIndex(*idx); err != nil {
			idx.Entries = previous
			return err
		}
		if err := writeArchiveWithProgress(path, opts, progress, populate); err != nil {
			idx.Entries = previous
			_ = s.writeFlatIndex(*idx)
			return err
		}
		idx.Entries[len(idx.Entries)-1].Pending = false
		return nil
	})
}

func isArchiveFileName(name string) bool {
	return filepath.Base(name) == name &&
		(strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tar.xz") ||
			strings.HasSuffix(name, ".tar.zst") || strings.HasSuffix(name, ".tar.lz4"))
}
