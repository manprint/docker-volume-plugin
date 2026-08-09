package dvol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// The replacement journal makes every directory swap recoverable, whatever the
// target is. The previous design scanned <root>/volumes for rollback
// directories named after a store-managed volume; that never matched the paths
// the CLI actually uses (a Docker mountpoint such as
// /var/lib/docker/volumes/<vol>/_data, whose rollback copy is named after the
// "_data" basename and lives outside the store). A crash during a real restore
// therefore left the volume without its data directory and nothing recovered
// it.
//
// A journal record is one small file per in-flight operation, exclusively
// flocked by its owner for the whole operation. Recovery only touches records
// it can lock, so a concurrent process cannot undo a swap that is still in
// flight. Records are append-only JSON lines: a crash in the middle of an
// update leaves a partial trailing line, and the last complete line wins.

const replacementRecordVersion = 1

type replacementRecord struct {
	Version   int    `json:"version"`
	Staging   string `json:"staging"`
	StageRoot string `json:"stage_root,omitempty"`
	Target    string `json:"target,omitempty"`
	Backup    string `json:"backup,omitempty"`
	// Pointer distinguishes new records explicitly marked uncommitted from
	// legacy v1 records, whose recovery inferred publication from target
	// existence.
	Published *bool `json:"published,omitempty"`
}

// replacement is an open, exclusively locked journal record.
type replacement struct {
	path     string
	file     *os.File
	record   replacementRecord
	closed   bool
	preserve bool
}

func (s *Store) journalDir() string { return filepath.Join(s.cfg.Root, stateDir, "replacements") }

// beginStaging registers a staging directory so an interrupted restore cannot
// leak it. The record is upgraded with the target and rollback paths when the
// swap starts.
func (s *Store) beginStaging(staging string) (*replacement, error) {
	dir := s.journalDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "replace-*.json")
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("lock replacement journal: %w", err)
	}
	published := false
	r := &replacement{
		path:   f.Name(),
		file:   f,
		record: replacementRecord{Version: replacementRecordVersion, Staging: staging, Published: &published},
	}
	if err := r.append(); err != nil {
		r.discard()
		return nil, err
	}
	return r, nil
}

func (r *replacement) append() error {
	b, err := json.Marshal(r.record)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := r.file.Write(b); err != nil {
		return err
	}
	return r.file.Sync()
}

func (r *replacement) setTarget(target string) error {
	r.record.Target = target
	return r.append()
}

func (r *replacement) setStageRoot(staging string) error {
	r.record.StageRoot = staging
	return r.append()
}

func (r *replacement) setBackup(backup string) error {
	r.record.Backup = backup
	return r.append()
}

func (r *replacement) setPublished() error {
	published := true
	r.record.Published = &published
	return r.append()
}

// discard drops the record without acting on it. It is used both on the
// success path (the operation completed, nothing left to recover) and when the
// record could not be initialised.
func (r *replacement) discard() {
	if r == nil || r.closed {
		return
	}
	r.closed = true
	if !r.preserve {
		_ = os.Remove(r.path)
	}
	_ = r.file.Close()
}

// recoverReplacements repairs operations interrupted by a crash. Records still
// locked by a live process are skipped.
func (s *Store) recoverReplacements() error {
	dir := s.journalDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "replace-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if err := s.recoverReplacement(path); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) recoverReplacement(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Another process owns this operation; leave it alone.
		s.logger().Debug("replacement journal entry in flight", LogField{Key: "record", Value: path})
		return nil
	}
	defer unix.Flock(int(f.Fd()), syscall.LOCK_UN)

	record, ok, err := readReplacementRecord(f)
	if err != nil {
		return err
	}
	if !ok {
		// No complete line: nothing had started yet.
		return os.Remove(path)
	}
	s.logger().Warn("recovering interrupted volume replacement",
		LogField{Key: "target", Value: record.Target},
		LogField{Key: "staging", Value: record.Staging},
		LogField{Key: "rollback", Value: record.Backup})

	if record.Published == nil && record.Backup != "" && record.Target != "" {
		// Legacy v1 record: preserve the original existence-based recovery
		// semantics so upgrades can repair operations started by older dvol.
		_, targetErr := os.Lstat(record.Target)
		switch {
		case targetErr == nil:
			// The new target was published before the crash; only the cleanup
			// of the old copy was interrupted.
			if err := os.RemoveAll(record.Backup); err != nil {
				return err
			}
		case os.IsNotExist(targetErr):
			// The old target was renamed aside but the new one was never
			// published. Put the old data back.
			if _, err := os.Lstat(record.Backup); err == nil {
				if err := os.Rename(record.Backup, record.Target); err != nil {
					return err
				}
				if err := syncDir(filepath.Dir(record.Target)); err != nil {
					return err
				}
			}
		default:
			return targetErr
		}
	} else if record.Published != nil && record.Target != "" {
		targetInfo, targetErr := os.Lstat(record.Target)
		targetExists := targetErr == nil
		if targetErr != nil && !os.IsNotExist(targetErr) {
			return targetErr
		}
		_ = targetInfo
		backupExists := false
		if record.Backup != "" {
			if _, err := os.Lstat(record.Backup); err == nil {
				backupExists = true
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		if *record.Published {
			if targetExists {
				if backupExists {
					if err := os.RemoveAll(record.Backup); err != nil {
						return err
					}
				}
			} else if backupExists {
				// A committed replacement was removed after the crash. Recover the
				// only complete copy still available rather than deleting it.
				if err := os.Rename(record.Backup, record.Target); err != nil {
					return err
				}
			}
		} else if backupExists {
			// Publication was never durably recorded. Roll back even if the new
			// target rename happened immediately before the crash.
			if targetExists {
				if err := os.RemoveAll(record.Target); err != nil {
					return err
				}
			}
			if err := os.Rename(record.Backup, record.Target); err != nil {
				return err
			}
		} else if targetExists && record.StageRoot != "" {
			if _, err := os.Lstat(record.StageRoot); os.IsNotExist(err) {
				// Target did not exist before the operation and the stage root has
				// moved: remove the uncommitted publication.
				if err := os.RemoveAll(record.Target); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
		}
		if err := syncDir(filepath.Dir(record.Target)); err != nil {
			return err
		}
	}
	if record.Staging != "" {
		if err := os.RemoveAll(record.Staging); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

// readReplacementRecord returns the last complete JSON line of a journal file.
func readReplacementRecord(f *os.File) (replacementRecord, bool, error) {
	data, err := os.ReadFile(f.Name())
	if err != nil {
		return replacementRecord{}, false, err
	}
	var last replacementRecord
	found := false
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record replacementRecord
		if err := json.Unmarshal(line, &record); err != nil {
			// Partial trailing line from a crash mid-write: keep the previous
			// complete record.
			continue
		}
		if record.Version != replacementRecordVersion {
			return replacementRecord{}, false, fmt.Errorf("unsupported replacement journal version %d", record.Version)
		}
		last = record
		found = true
	}
	return last, found, nil
}

// replaceStage swaps the fully-extracted staging directory into target. The
// staging root is verified to be a real directory: an archive entry turning it
// into a symlink refuses the swap. The previous target is kept under a private
// backup name until the swap completes, and every step is journalled so a crash
// inside the non-atomic window is repaired by the next NewStore.
func (s *Store) replaceStage(rec *replacement, staging, target string) error {
	fi, err := os.Lstat(staging)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("refusing to replace volume with non-directory %q (entry escaped the archive?)", staging)
	}
	if rec == nil {
		rec, err = s.beginStaging(staging)
		if err != nil {
			return err
		}
		defer rec.discard()
	}
	if err := rec.setStageRoot(staging); err != nil {
		return err
	}
	if err := rec.setTarget(target); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// target already gone (e.g. the user deleted the data dir while keeping
		// the metadata): nothing to preserve, just move the staging in place.
		if err := os.Rename(staging, target); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(target)); err != nil {
			if rollbackErr := os.Rename(target, staging); rollbackErr != nil {
				rec.preserve = true
				return fmt.Errorf("sync published volume: %w (rollback failed: %v)", err, rollbackErr)
			}
			return err
		}
		if err := rec.setPublished(); err != nil {
			if rollbackErr := os.Rename(target, staging); rollbackErr != nil {
				rec.preserve = true
				return fmt.Errorf("record published volume: %w (rollback failed: %v)", err, rollbackErr)
			}
			return err
		}
		return nil
	}
	// Keep the old target in a private, collision-free path. A fixed suffix
	// would collide with a valid volume name such as "foo.dvol-old".
	backup, err := os.MkdirTemp(filepath.Dir(target), "."+filepath.Base(target)+".dvol-old-*")
	if err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	if err := rec.setBackup(backup); err != nil {
		return err
	}
	if err := os.Rename(target, backup); err != nil {
		return err
	}
	if err := os.Rename(staging, target); err != nil {
		if rollbackErr := os.Rename(backup, target); rollbackErr != nil {
			return fmt.Errorf("replace volume: %w (rollback failed: %v)", err, rollbackErr)
		}
		return err
	}
	rollback := func(cause error) error {
		if err := os.Rename(target, staging); err != nil {
			rec.preserve = true
			return fmt.Errorf("%w (moving unpublished target aside failed: %v)", cause, err)
		}
		if err := os.Rename(backup, target); err != nil {
			rec.preserve = true
			return fmt.Errorf("%w (rollback failed: %v)", cause, err)
		}
		return cause
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		return rollback(err)
	}
	if err := rec.setPublished(); err != nil {
		return rollback(err)
	}
	if err := os.RemoveAll(backup); err != nil {
		rec.preserve = true
		s.logger().Warn("restored volume but old copy cleanup is deferred",
			LogField{Key: "target", Value: target}, LogField{Key: "backup", Value: backup}, LogField{Key: "error", Value: err})
		return nil
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		rec.preserve = true
		s.logger().Warn("restored volume but cleanup durability is deferred",
			LogField{Key: "target", Value: target}, LogField{Key: "error", Value: err})
	}
	return nil
}
