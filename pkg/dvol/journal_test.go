package dvol

// Replacement-journal suite. The journal is what makes an interrupted restore
// recoverable. The previous mechanism scanned <root>/volumes for rollback
// directories named after a store-managed volume, which never matched the path
// the CLI actually restores into (a Docker mountpoint outside the store), so a
// crash left the volume without its data directory and nothing repaired it.

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// crash abandons a journal record the way a killed process would: the lock is
// released by the dying process but the record file stays behind.
func crash(t *testing.T, rec *replacement) {
	t.Helper()
	if err := rec.file.Close(); err != nil {
		t.Fatal(err)
	}
	rec.closed = true
}

func TestReplacementGroupCrashRecoveryIsAllOrNothing(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "before-commit"
		if committed {
			name = "after-commit"
		}
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			group, err := s.beginReplacementGroup()
			if err != nil {
				t.Fatal(err)
			}
			groupName := filepath.Base(group.path)
			names := []string{"app", "db"}
			targets := map[string]string{}
			records := make([]*replacement, 0, len(names))
			for _, volume := range names {
				target := dockerLikeTarget(t, volume)
				writeVolumeFile(t, target, "data.txt", "old-"+volume)
				staging, err := os.MkdirTemp(filepath.Dir(target), ".group-stage-*")
				if err != nil {
					t.Fatal(err)
				}
				writeVolumeFile(t, staging, "data.txt", "new-"+volume)
				rec, err := s.beginStaging(staging)
				if err != nil {
					t.Fatal(err)
				}
				if err := rec.setGroup(groupName); err != nil {
					t.Fatal(err)
				}
				if _, err := s.prepareStage(stageReplacement{rec: rec, staging: staging, target: target}); err != nil {
					t.Fatal(err)
				}
				targets[volume] = target
				records = append(records, rec)
			}
			if committed {
				if err := group.setPublished(); err != nil {
					t.Fatal(err)
				}
			}
			for _, rec := range records {
				crash(t, rec)
			}
			crashGroup(t, group)

			if _, err := NewStore(Config{Root: s.cfg.Root}); err != nil {
				t.Fatalf("recover replacement group: %v", err)
			}
			for _, volume := range names {
				got, err := os.ReadFile(filepath.Join(targets[volume], "data.txt"))
				if err != nil {
					t.Fatal(err)
				}
				want := "old-" + volume
				if committed {
					want = "new-" + volume
				}
				if string(got) != want {
					t.Fatalf("%s recovered %q, want %q", volume, got, want)
				}
			}
			entries, err := os.ReadDir(s.journalDir())
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("replacement group recovery left journal files: %v", entries)
			}
		})
	}
}

func crashGroup(t *testing.T, group *replacementGroup) {
	t.Helper()
	if err := group.file.Close(); err != nil {
		t.Fatal(err)
	}
	group.closed = true
}

// dockerLikeTarget builds a target that mimics a Docker volume mountpoint:
// <somewhere>/<volume>/_data, entirely outside the dvol repository.
func dockerLikeTarget(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	target := filepath.Join(dir, "_data")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestJournalRecoversCrashBeforePublish(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	target := dockerLikeTarget(t, "web")
	if err := os.WriteFile(filepath.Join(target, "state"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	staging, rec, err := s.newStaging(filepath.Dir(target), ".restore-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "state"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Reproduce the exact non-atomic window: the old target has been renamed
	// aside and the staged one is not in place yet.
	if err := rec.setTarget(target); err != nil {
		t.Fatal(err)
	}
	if err := rec.setStageRoot(staging); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(filepath.Dir(target), "._data.dvol-old-crash")
	if err := rec.setBackup(backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, backup); err != nil {
		t.Fatal(err)
	}
	crash(t, rec)

	if _, err := NewStore(Config{Root: root}); err != nil {
		t.Fatalf("NewStore recovery: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "state"))
	if err != nil || string(got) != "old" {
		t.Fatalf("old volume data was not recovered: %q err=%v", got, err)
	}
	if _, err := os.Lstat(backup); !os.IsNotExist(err) {
		t.Fatalf("rollback directory remains: %v", err)
	}
	if _, err := os.Lstat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging directory leaked: %v", err)
	}
}

func TestJournalRecoversCrashAfterPublish(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	target := dockerLikeTarget(t, "web")
	if err := os.WriteFile(filepath.Join(target, "state"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	staging, rec, err := s.newStaging(filepath.Dir(target), ".restore-*")
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(filepath.Dir(target), "._data.dvol-old-crash")
	if err := os.Mkdir(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "state"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rec.setTarget(target); err != nil {
		t.Fatal(err)
	}
	if err := rec.setStageRoot(filepath.Join(staging, "published-root")); err != nil {
		t.Fatal(err)
	}
	if err := rec.setBackup(backup); err != nil {
		t.Fatal(err)
	}
	if err := rec.setPublished(); err != nil {
		t.Fatal(err)
	}
	crash(t, rec)

	if _, err := NewStore(Config{Root: root}); err != nil {
		t.Fatalf("NewStore recovery: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "state"))
	if err != nil || string(got) != "new" {
		t.Fatalf("published volume was not preserved: %q err=%v", got, err)
	}
	for _, leftover := range []string{backup, staging} {
		if _, err := os.Lstat(leftover); !os.IsNotExist(err) {
			t.Fatalf("leftover %s remains: %v", leftover, err)
		}
	}
}

func TestJournalRollsBackRenameNotDurablyPublished(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	target := dockerLikeTarget(t, "web")
	if err := os.WriteFile(filepath.Join(target, "state"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	staging, rec, err := s.newStaging(filepath.Dir(target), ".restore-*")
	if err != nil {
		t.Fatal(err)
	}
	stageRoot := filepath.Join(staging, "root")
	if err := os.Mkdir(stageRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageRoot, "state"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(filepath.Dir(target), "._data.dvol-old-uncommitted")
	if err := rec.setStageRoot(stageRoot); err != nil {
		t.Fatal(err)
	}
	if err := rec.setTarget(target); err != nil {
		t.Fatal(err)
	}
	if err := rec.setBackup(backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stageRoot, target); err != nil {
		t.Fatal(err)
	}
	// Crash before setPublished: target exists, but that is no longer enough
	// to infer commit because another process could also have recreated it.
	crash(t, rec)

	if _, err := NewStore(Config{Root: root}); err != nil {
		t.Fatalf("NewStore recovery: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "state"))
	if err != nil || string(got) != "old" {
		t.Fatalf("uncommitted publication was not rolled back: %q err=%v", got, err)
	}
	if _, err := os.Lstat(backup); !os.IsNotExist(err) {
		t.Fatalf("rollback directory remains: %v", err)
	}
}

func TestJournalCleansStagingOnlyCrash(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	target := dockerLikeTarget(t, "web")
	if err := os.WriteFile(filepath.Join(target, "state"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	staging, rec, err := s.newStaging(filepath.Dir(target), ".restore-*")
	if err != nil {
		t.Fatal(err)
	}
	crash(t, rec)

	if _, err := NewStore(Config{Root: root}); err != nil {
		t.Fatalf("NewStore recovery: %v", err)
	}
	if _, err := os.Lstat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging directory leaked after a crash before the swap: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "state"))
	if err != nil || string(got) != "live" {
		t.Fatalf("live target was touched: %q err=%v", got, err)
	}
}

// TestJournalDoesNotDisturbInFlightReplacement pins the concurrency contract:
// opening a second store (another dvol process starting up) must not undo a
// swap another process is performing right now.
func TestJournalDoesNotDisturbInFlightReplacement(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	target := dockerLikeTarget(t, "web")
	if err := os.WriteFile(filepath.Join(target, "state"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	staging, rec, err := s.newStaging(filepath.Dir(target), ".restore-*")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.discard()
	if err := os.WriteFile(filepath.Join(staging, "state"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rec.setTarget(target); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(filepath.Dir(target), "._data.dvol-old-inflight")
	if err := rec.setBackup(backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, backup); err != nil {
		t.Fatal(err)
	}

	// A second process starts while the swap is mid-flight.
	if _, err := NewStore(Config{Root: root}); err != nil {
		t.Fatalf("concurrent NewStore: %v", err)
	}
	if _, err := os.Lstat(backup); err != nil {
		t.Fatalf("in-flight rollback copy was taken away by a concurrent store: %v", err)
	}
	if _, err := os.Lstat(staging); err != nil {
		t.Fatalf("in-flight staging was removed by a concurrent store: %v", err)
	}

	// Finish the swap normally.
	if err := os.Rename(staging, target); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(backup); err != nil {
		t.Fatal(err)
	}
	rec.discard()
	got, err := os.ReadFile(filepath.Join(target, "state"))
	if err != nil || string(got) != "new" {
		t.Fatalf("swap did not complete: %q err=%v", got, err)
	}
}

// TestRestoreCrashRecoveryEndToEnd walks the whole path: a real restore is
// interrupted between the two renames and the next store start repairs it.
func TestRestoreCrashRecoveryEndToEnd(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "data.txt"), []byte("archived"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive, err := s.BackupSourceWithOptions("web", source, BackupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	target := dockerLikeTarget(t, "web")
	if err := os.WriteFile(filepath.Join(target, "live.txt"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Stage the archive exactly like RestoreSource does, then crash.
	staging, rec, err := s.newStaging(filepath.Dir(target), ".restore-*")
	if err != nil {
		t.Fatal(err)
	}
	tr, closeArchive, err := s.openArchiveForRestore(archive, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := extractTarStream(tr, nil, s.logger(), s.cfg.BestEffortMetadata, func(hdr *tar.Header, payload io.Reader) (string, string, io.Reader, bool, error) {
		return hdr.Name, staging, payload, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := closeArchive(); err != nil {
		t.Fatal(err)
	}
	if err := rec.setTarget(target); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(filepath.Dir(target), "._data.dvol-old-e2e")
	if err := rec.setBackup(backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, backup); err != nil {
		t.Fatal(err)
	}
	crash(t, rec)

	if _, err := NewStore(Config{Root: root}); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "live.txt"))
	if err != nil || string(got) != "live" {
		t.Fatalf("interrupted restore lost the live data: %q err=%v", got, err)
	}
}

// TestConcurrentStoreOpensDoNotRace opens many stores at once against the same
// repository, the way several dvol invocations would.
func TestConcurrentStoreOpensDoNotRace(t *testing.T) {
	root := t.TempDir()
	if _, err := NewStore(Config{Root: root}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := NewStore(Config{Root: root})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
