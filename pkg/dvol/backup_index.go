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
	flatBackupIndexName   = ".dvol-index.json"
	flatBackupLockName    = ".dvol-index.lock"
	flatReservationPrefix = ".dvol-reserve-"
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
	Volume          string              `json:"volume"`
	Name            string              `json:"name"`
	CreatedAt       time.Time           `json:"created_at"`
	Pending         bool                `json:"pending,omitempty"`
	Previous        *flatBackupPrevious `json:"previous,omitempty"`
	PreviousArchive bool                `json:"previous_archive,omitempty"`
	PreviousDev     uint64              `json:"previous_dev,omitempty"`
	PreviousIno     uint64              `json:"previous_ino,omitempty"`
}

type flatBackupPrevious struct {
	Volume    string    `json:"volume"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

func fileIdentity(info os.FileInfo) (dev, ino uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}

func settleFlatPending(entry flatBackupIndexEntry, info os.FileInfo) (flatBackupIndexEntry, bool) {
	if entry.PreviousArchive {
		dev, ino, ok := fileIdentity(info)
		if ok && dev == entry.PreviousDev && ino == entry.PreviousIno {
			if entry.Previous == nil {
				return flatBackupIndexEntry{}, false
			}
			return flatBackupIndexEntry{
				Volume: entry.Previous.Volume, Name: entry.Previous.Name, CreatedAt: entry.Previous.CreatedAt,
			}, true
		}
	}
	entry.Pending = false
	entry.Previous = nil
	entry.PreviousArchive = false
	entry.PreviousDev = 0
	entry.PreviousIno = 0
	return entry, true
}

func (s *Store) flatIndexPath() string {
	return filepath.Join(s.backupsDir(), flatBackupIndexName)
}

func (s *Store) flatLockPath() string {
	return filepath.Join(s.backupsDir(), flatBackupLockName)
}

func (s *Store) flatReservationPath(name string) string {
	return filepath.Join(s.backupsDir(), flatReservationPrefix+name+".lock")
}

// loadFlatIndex reads the index and repairs it in memory.
//
// A backup tool must never let damaged metadata stop the operator from taking a
// new backup: a single unusable entry is dropped with a warning instead of
// failing the whole index. Only a structurally undecodable file or a version
// this build cannot rewrite is fatal.
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
			s.logger().Warn("dropping invalid flat backup metadata entry",
				LogField{Key: "volume", Value: entry.Volume}, LogField{Key: "archive", Value: entry.Name})
			changed = true
			continue
		}
		path := filepath.Join(s.backupsDir(), entry.Name)
		info, statErr := os.Stat(path)
		if os.IsNotExist(statErr) {
			if entry.Pending && s.reservationHeld(entry.Name) {
				// Another process is writing this archive right now: keep the
				// reservation so a concurrent backup cannot take the name.
				seen[entry.Name] = true
				kept = append(kept, entry)
				continue
			}
			// A pending entry without its archive is a crash before publish;
			// a completed entry without its archive was removed externally.
			changed = true
			continue
		}
		if statErr != nil {
			return flatBackupIndex{}, false, statErr
		}
		if !info.Mode().IsRegular() {
			s.logger().Warn("dropping flat backup metadata entry: not a regular file",
				LogField{Key: "archive", Value: entry.Name})
			changed = true
			continue
		}
		if entry.Pending {
			if s.reservationHeld(entry.Name) {
				// An overwrite keeps the previous archive at the final path while
				// the replacement is written. Only settle the transaction after
				// its owner has released the reservation lock.
				seen[entry.Name] = true
				kept = append(kept, entry)
				continue
			}
			var keep bool
			entry, keep = settleFlatPending(entry, info)
			changed = true
			if !keep {
				continue
			}
		}
		seen[entry.Name] = true
		kept = append(kept, entry)
	}
	idx.Entries = kept
	return idx, changed, nil
}

// reservationHeld reports whether another process is actively writing the
// archive. The reservation file is exclusively flocked for the whole write, so
// a lock we can take is by definition abandoned.
func (s *Store) reservationHeld(name string) bool {
	f, err := os.OpenFile(s.flatReservationPath(name), os.O_RDWR, 0o644)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = unix.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// flatReservation is an exclusive claim on one archive name.
type flatReservation struct {
	path string
	file *os.File
}

func (s *Store) reserveFlatArchive(name string) (*flatReservation, error) {
	if err := os.MkdirAll(s.backupsDir(), 0o755); err != nil {
		return nil, err
	}
	path := s.flatReservationPath(name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("a backup of archive %q is already in progress", name)
	}
	return &flatReservation{path: path, file: f}, nil
}

func (r *flatReservation) release() {
	if r == nil {
		return
	}
	_ = os.Remove(r.path)
	_ = r.file.Close()
}

// withFlatIndexLock runs fn under the index lock and persists the result. The
// critical section is deliberately short: it never spans an archive write, so
// listing backups is not blocked for the duration of a multi-hour backup.
func (s *Store) withFlatIndexLock(fn func(*flatBackupIndex) error) error {
	if err := os.MkdirAll(s.backupsDir(), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.flatLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
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
	lock, err := os.OpenFile(s.flatLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
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
	return idx, nil
}

func (s *Store) flatBackupInfos(volume string) ([]BackupInfo, error) {
	idx, err := s.readFlatIndex()
	if err != nil {
		return nil, err
	}
	infos := make([]BackupInfo, 0)
	for _, entry := range idx.Entries {
		if entry.Volume != volume || entry.Pending {
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
		if entry.Volume != volume || entry.Pending {
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
	if idx.Version == 0 {
		idx.Version = flatBackupIndexVersion
	}
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
	if err := f.Chmod(s.cfg.archiveMode()); err != nil {
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

// writeFlatBackupWithProgress reserves the archive name, writes the archive
// outside the index lock and then marks the reservation complete.
func (s *Store) writeFlatBackupWithProgress(volume, name, path string, opts BackupOptions, progress *Progress, populate func(*tar.Writer) error) error {
	reservation, err := s.reserveFlatArchive(name)
	if err != nil {
		return err
	}
	defer reservation.release()

	if err := s.withFlatIndexLock(func(idx *flatBackupIndex) error {
		var previous *flatBackupPrevious
		filtered := idx.Entries[:0]
		for _, entry := range idx.Entries {
			if entry.Name == name {
				if !opts.Overwrite {
					return fmt.Errorf("%w: %s (use --overwrite to replace it)", ErrArchiveExists, name)
				}
				previous = &flatBackupPrevious{Volume: entry.Volume, Name: entry.Name, CreatedAt: entry.CreatedAt}
				continue
			}
			filtered = append(filtered, entry)
		}
		idx.Entries = filtered
		var previousArchive bool
		var previousDev, previousIno uint64
		if info, err := os.Lstat(path); err == nil {
			if !opts.Overwrite {
				return fmt.Errorf("%w: %s (use --overwrite to replace it)", ErrArchiveExists, name)
			}
			previousArchive = true
			previousDev, previousIno, _ = fileIdentity(info)
		} else if !os.IsNotExist(err) {
			return err
		}
		idx.Entries = append(idx.Entries, flatBackupIndexEntry{
			Volume: volume, Name: name, CreatedAt: time.Now().UTC(), Pending: true,
			Previous: previous, PreviousArchive: previousArchive,
			PreviousDev: previousDev, PreviousIno: previousIno,
		})
		return nil
	}); err != nil {
		return err
	}

	// The archive is written without holding the index lock.
	if writeErr := writeArchiveWithProgress(path, s.cfg.archiveMode(), opts, progress, populate); writeErr != nil {
		_ = s.withFlatIndexLock(func(idx *flatBackupIndex) error {
			filtered := idx.Entries[:0]
			for _, entry := range idx.Entries {
				if entry.Name != name || !entry.Pending {
					filtered = append(filtered, entry)
					continue
				}
				info, statErr := os.Stat(path)
				if statErr == nil {
					if settled, keep := settleFlatPending(entry, info); keep {
						filtered = append(filtered, settled)
					}
				} else if !os.IsNotExist(statErr) {
					return statErr
				}
			}
			idx.Entries = filtered
			return nil
		})
		return writeErr
	}

	return s.withFlatIndexLock(func(idx *flatBackupIndex) error {
		for i := range idx.Entries {
			if idx.Entries[i].Name == name {
				idx.Entries[i].Pending = false
				idx.Entries[i].Previous = nil
				idx.Entries[i].PreviousArchive = false
				idx.Entries[i].PreviousDev = 0
				idx.Entries[i].PreviousIno = 0
				return nil
			}
		}
		idx.Entries = append(idx.Entries, flatBackupIndexEntry{
			Volume:    volume,
			Name:      name,
			CreatedAt: time.Now().UTC(),
		})
		return nil
	})
}

func isArchiveFileName(name string) bool {
	return filepath.Base(name) == name && name != "" && !strings.HasPrefix(name, ".") &&
		isArchiveName(name)
}
