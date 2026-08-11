package dvol

import (
	"bytes"
	"encoding/json"
	"errors"
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
	Group     string `json:"group,omitempty"`
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

type replacementGroupRecord struct {
	Version   int  `json:"version"`
	Published bool `json:"published"`
}

type replacementGroup struct {
	path     string
	file     *os.File
	record   replacementGroupRecord
	closed   bool
	preserve bool
}

func (s *Store) beginReplacementGroup() (*replacementGroup, error) {
	dir := s.journalDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "group-*.json")
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("lock replacement group: %w", err)
	}
	g := &replacementGroup{
		path: f.Name(), file: f,
		record: replacementGroupRecord{Version: replacementRecordVersion},
	}
	if err := g.append(); err != nil {
		g.discard()
		return nil, err
	}
	if err := syncDir(dir); err != nil {
		g.discard()
		return nil, err
	}
	return g, nil
}

func (g *replacementGroup) append() error {
	b, err := json.Marshal(g.record)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := g.file.Write(b); err != nil {
		return err
	}
	return g.file.Sync()
}

func (g *replacementGroup) setPublished() error {
	g.record.Published = true
	return g.append()
}

func (g *replacementGroup) discard() {
	if g == nil || g.closed {
		return
	}
	g.closed = true
	_ = g.file.Close()
	if !g.preserve {
		_ = removeDurably(g.path)
	}
}

func removeDurably(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDir(filepath.Dir(path))
}

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
	if err := syncDir(dir); err != nil {
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

func (r *replacement) setGroup(group string) error {
	if filepath.Base(group) != group || !strings.HasPrefix(group, "group-") {
		return fmt.Errorf("invalid replacement group %q", group)
	}
	r.record.Group = group
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
	_ = r.file.Close()
	if !r.preserve {
		_ = removeDurably(r.path)
	}
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
	return s.cleanupReplacementGroups(dir)
}

func (s *Store) cleanupReplacementGroups(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "group-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		f, err := os.OpenFile(path, os.O_RDWR, 0o600)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := unix.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			_ = f.Close()
			return err
		}
		_ = f.Close()
		removed = true
	}
	if removed {
		return syncDir(dir)
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
		return removeDurably(path)
	}
	s.logger().Warn("recovering interrupted volume replacement",
		LogField{Key: "target", Value: record.Target},
		LogField{Key: "staging", Value: record.Staging},
		LogField{Key: "rollback", Value: record.Backup})
	effectivePublished := record.Published
	if record.Group != "" {
		published, err := s.replacementGroupPublished(record.Group)
		if err != nil {
			return err
		}
		effectivePublished = &published
	}

	if effectivePublished == nil && record.Backup != "" && record.Target != "" {
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
	} else if effectivePublished != nil && record.Target != "" {
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
		if *effectivePublished {
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
	return removeDurably(path)
}

func (s *Store) replacementGroupPublished(name string) (bool, error) {
	if filepath.Base(name) != name || !strings.HasPrefix(name, "group-") {
		return false, fmt.Errorf("invalid replacement group %q", name)
	}
	data, err := os.ReadFile(filepath.Join(s.journalDir(), name))
	if err != nil {
		if os.IsNotExist(err) {
			// A missing commit marker is fail-safe: the group never reached its
			// durable commit point, so every member must roll back.
			return false, nil
		}
		return false, err
	}
	last := replacementGroupRecord{}
	found := false
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record replacementGroupRecord
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		if record.Version != replacementRecordVersion {
			return false, fmt.Errorf("unsupported replacement group version %d", record.Version)
		}
		last, found = record, true
	}
	if !found {
		return false, nil
	}
	return last.Published, nil
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
type stageReplacement struct {
	rec     *replacement
	staging string
	target  string
}

type preparedStage struct {
	store *Store
	stageReplacement
	backup        string
	targetExisted bool
}

func (s *Store) renameForReplacement(oldPath, newPath string) error {
	if s.replaceRename != nil {
		return s.replaceRename(oldPath, newPath)
	}
	return os.Rename(oldPath, newPath)
}

// syncStageFilesystems makes extracted file contents and metadata durable
// before any live target is renamed. Every staging directory is created beside
// its target, so syncing the target parent flushes the filesystem that contains
// the corresponding staging tree. Devices are de-duplicated for bundles.
func syncStageFilesystems(items []stageReplacement) error {
	seen := map[uint64]bool{}
	for _, item := range items {
		parent := filepath.Dir(item.target)
		fd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open restore filesystem %q for sync: %w", parent, err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("inspect restore filesystem %q: %w", parent, err)
		}
		dev := uint64(stat.Dev)
		if !seen[dev] {
			if err := unix.Syncfs(fd); err != nil {
				_ = unix.Close(fd)
				return fmt.Errorf("sync extracted restore data on %q: %w", parent, err)
			}
			seen[dev] = true
		}
		if err := unix.Close(fd); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) prepareStage(item stageReplacement) (*preparedStage, error) {
	fi, err := os.Lstat(item.staging)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("refusing to replace volume with non-directory %q", item.staging)
	}
	if err := item.rec.setStageRoot(item.staging); err != nil {
		return nil, err
	}
	if err := item.rec.setTarget(item.target); err != nil {
		return nil, err
	}
	p := &preparedStage{store: s, stageReplacement: item}
	if _, err := os.Lstat(item.target); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		if err := s.renameForReplacement(item.staging, item.target); err != nil {
			return nil, err
		}
		if err := syncDir(filepath.Dir(item.target)); err != nil {
			return nil, p.rollback(err)
		}
		return p, nil
	}

	p.targetExisted = true
	backup, err := os.MkdirTemp(filepath.Dir(item.target), "."+filepath.Base(item.target)+".dvol-old-*")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(backup); err != nil {
		return nil, err
	}
	p.backup = backup
	if err := item.rec.setBackup(backup); err != nil {
		return nil, err
	}
	if err := s.renameForReplacement(item.target, backup); err != nil {
		return nil, err
	}
	if err := s.renameForReplacement(item.staging, item.target); err != nil {
		if rollbackErr := os.Rename(backup, item.target); rollbackErr != nil {
			item.rec.preserve = true
			return nil, fmt.Errorf("replace volume: %w (rollback failed: %v)", err, rollbackErr)
		}
		return nil, err
	}
	if err := syncDir(filepath.Dir(item.target)); err != nil {
		return nil, p.rollback(err)
	}
	return p, nil
}

func (p *preparedStage) rollback(cause error) error {
	if _, err := os.Lstat(p.target); err == nil {
		if err := os.Rename(p.target, p.staging); err != nil {
			p.rec.preserve = true
			return errors.Join(cause, fmt.Errorf("moving unpublished target aside: %w", err))
		}
	} else if !os.IsNotExist(err) {
		p.rec.preserve = true
		return errors.Join(cause, err)
	}
	if p.targetExisted {
		if err := os.Rename(p.backup, p.target); err != nil {
			p.rec.preserve = true
			return errors.Join(cause, fmt.Errorf("restore previous target: %w", err))
		}
	}
	if err := syncDir(filepath.Dir(p.target)); err != nil {
		p.rec.preserve = true
		return errors.Join(cause, fmt.Errorf("sync rolled back target: %w", err))
	}
	return cause
}

func (p *preparedStage) finalizeCommitted() bool {
	if err := p.rec.setPublished(); err != nil {
		p.rec.preserve = true
		p.store.logger().Warn("restored volume but journal finalization is deferred",
			LogField{Key: "target", Value: p.target}, LogField{Key: "error", Value: err})
		return false
	}
	return p.cleanupCommitted()
}

func (p *preparedStage) cleanupCommitted() bool {
	if p.backup != "" {
		if err := os.RemoveAll(p.backup); err != nil {
			p.rec.preserve = true
			p.store.logger().Warn("restored volume but old copy cleanup is deferred",
				LogField{Key: "target", Value: p.target}, LogField{Key: "backup", Value: p.backup}, LogField{Key: "error", Value: err})
			return false
		}
	}
	if err := syncDir(filepath.Dir(p.target)); err != nil {
		p.rec.preserve = true
		p.store.logger().Warn("restored volume but cleanup durability is deferred",
			LogField{Key: "target", Value: p.target}, LogField{Key: "error", Value: err})
		return false
	}
	p.rec.discard()
	return true
}

func (s *Store) replaceStage(rec *replacement, staging, target string) error {
	owned := false
	var err error
	if rec == nil {
		rec, err = s.beginStaging(staging)
		if err != nil {
			return err
		}
		owned = true
		defer rec.discard()
	}
	if err := syncStageFilesystems([]stageReplacement{{rec: rec, staging: staging, target: target}}); err != nil {
		return err
	}
	p, err := s.prepareStage(stageReplacement{rec: rec, staging: staging, target: target})
	if err != nil {
		return err
	}
	if err := rec.setPublished(); err != nil {
		return p.rollback(fmt.Errorf("record published volume: %w", err))
	}
	if !p.cleanupCommitted() && owned {
		rec.preserve = true
	}
	return nil
}

func (s *Store) replaceStagesAtomically(items []stageReplacement) error {
	if len(items) == 0 {
		return nil
	}
	if len(items) == 1 {
		return s.replaceStage(items[0].rec, items[0].staging, items[0].target)
	}
	if err := syncStageFilesystems(items); err != nil {
		return err
	}
	group, err := s.beginReplacementGroup()
	if err != nil {
		return err
	}
	defer group.discard()
	groupName := filepath.Base(group.path)
	for _, item := range items {
		if err := item.rec.setGroup(groupName); err != nil {
			return err
		}
	}

	prepared := make([]*preparedStage, 0, len(items))
	rollbackAll := func(cause error) error {
		for i := len(prepared) - 1; i >= 0; i-- {
			cause = prepared[i].rollback(cause)
		}
		return cause
	}
	for _, item := range items {
		p, err := s.prepareStage(item)
		if err != nil {
			return rollbackAll(err)
		}
		prepared = append(prepared, p)
	}
	if err := group.setPublished(); err != nil {
		return rollbackAll(fmt.Errorf("commit replacement group: %w", err))
	}

	allFinalized := true
	for _, p := range prepared {
		if !p.finalizeCommitted() {
			allFinalized = false
		}
	}
	if !allFinalized {
		group.preserve = true
	}
	return nil
}
