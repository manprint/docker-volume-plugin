package dvol

// Concurrency suite. Run with -race. Everything here exercises two or more
// operations touching the same repository at once: archive publication, the
// flat index, staging directories and the replacement journal.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func seedVolume(t *testing.T, s *Store, name, content string) {
	t.Helper()
	if err := s.Create(name, nil); err != nil {
		t.Fatal(err)
	}
	writeVolumeFile(t, s.VolumePath(name), "data.txt", content)
	writeVolumeFile(t, s.VolumePath(name), "nested/more.txt", content+"-nested")
}

// TestConcurrentBackupsSameVolumeDistinctNames: many archives of one volume at
// once must all be valid and independently restorable.
func TestConcurrentBackupsSameVolumeDistinctNames(t *testing.T) {
	s := newTestStore(t)
	seedVolume(t, s, "web", "payload")
	const workers = 8
	paths := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], errs[i] = s.BackupWithOptions("web", BackupOptions{
				ArchiveName: fmt.Sprintf("copy-%d", i),
				Threads:     1,
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if _, err := s.VerifyArchive(paths[i]); err != nil {
			t.Fatalf("archive %d does not verify: %v", i, err)
		}
	}
	infos, err := s.ListBackups("web")
	if err != nil || len(infos) != workers {
		t.Fatalf("expected %d archives, got %+v err=%v", workers, infos, err)
	}
}

// TestConcurrentFlatBackupsSameArchiveName: exactly one writer may own an
// archive name; the losers must fail loudly instead of racing on the file.
func TestConcurrentFlatBackupsSameArchiveName(t *testing.T) {
	s, destination := newFlatStore(t)
	seedVolume(t, s, "web", "payload")
	const workers = 6
	results := make(chan error, workers)
	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait()
			_, err := s.BackupWithOptions("web", BackupOptions{
				ArchiveName:     "contended",
				SameDestination: true,
				Threads:         1,
			})
			results <- err
		}()
	}
	start.Done()
	wg.Wait()
	close(results)

	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
			continue
		}
		if !errors.Is(err, ErrArchiveExists) && !strings.Contains(err.Error(), "already in progress") {
			t.Fatalf("unexpected contention error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly one winner, got %d", succeeded)
	}
	infos, err := s.ListBackups("web")
	if err != nil || len(infos) != 1 {
		t.Fatalf("index must record exactly one archive: %+v err=%v", infos, err)
	}
	if _, err := s.VerifyArchive(filepath.Join(destination, "contended.tar.gz")); err != nil {
		t.Fatalf("published archive is not valid: %v", err)
	}
}

// TestListBackupsNotBlockedByInFlightBackup pins the lock split: the index lock
// is never held across an archive write, so listing stays responsive, and a
// live reservation is neither shown to users nor garbage-collected.
func TestListBackupsNotBlockedByInFlightBackup(t *testing.T) {
	s, _ := newFlatStore(t)
	seedVolume(t, s, "web", "payload")
	if _, err := s.BackupWithOptions("web", BackupOptions{ArchiveName: "done", SameDestination: true}); err != nil {
		t.Fatal(err)
	}

	// Simulate a backup that has reserved its name and is still writing.
	reservation, err := s.reserveFlatArchive("busy.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.withFlatIndexLock(func(idx *flatBackupIndex) error {
		idx.Entries = append(idx.Entries, flatBackupIndexEntry{
			Volume: "web", Name: "busy.tar.gz", CreatedAt: time.Now().UTC(), Pending: true,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		infos, err := s.ListBackups("web")
		if err != nil {
			t.Errorf("listing during an in-flight backup failed: %v", err)
			return
		}
		if len(infos) != 1 || infos[0].Name != "done.tar.gz" {
			t.Errorf("pending archive must not be listed: %+v", infos)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ListBackups blocked while a backup was in flight")
	}

	if _, err := s.reserveFlatArchive("busy.tar.gz"); err == nil {
		t.Fatal("a second writer took a reserved archive name")
	}

	// The live reservation must survive the listing's index rewrite.
	idx, _, err := s.loadFlatIndex()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range idx.Entries {
		if e.Name == "busy.tar.gz" {
			found = e.Pending
		}
	}
	if !found {
		t.Fatal("a live reservation was garbage collected by a concurrent reader")
	}

	// Once the writer disappears the reservation becomes collectable.
	reservation.release()
	idx, changed, err := s.loadFlatIndex()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("abandoned reservation was not detected")
	}
	for _, e := range idx.Entries {
		if e.Name == "busy.tar.gz" {
			t.Fatal("abandoned reservation was kept")
		}
	}
}

// TestConcurrentRestoresDifferentVolumes: independent restores must not
// interfere through the shared journal or staging directories.
func TestConcurrentRestoresDifferentVolumes(t *testing.T) {
	s := newTestStore(t)
	const workers = 6
	archives := make([]string, workers)
	targets := make([]string, workers)
	for i := 0; i < workers; i++ {
		name := fmt.Sprintf("vol%d", i)
		source := t.TempDir()
		writeVolumeFile(t, source, "data.txt", name)
		archive, err := s.BackupSourceWithOptions(name, source, BackupOptions{Threads: 1})
		if err != nil {
			t.Fatal(err)
		}
		archives[i] = archive
		target := t.TempDir()
		writeVolumeFile(t, target, "stale.txt", "stale")
		targets[i] = target
	}
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.RestoreSource(fmt.Sprintf("vol%d", i), targets[i], archives[i])
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("restore %d: %v", i, err)
		}
		got, err := os.ReadFile(filepath.Join(targets[i], "data.txt"))
		if err != nil || string(got) != fmt.Sprintf("vol%d", i) {
			t.Fatalf("restore %d produced %q err=%v", i, got, err)
		}
		if _, err := os.Stat(filepath.Join(targets[i], "stale.txt")); !os.IsNotExist(err) {
			t.Fatalf("restore %d did not replace the target", i)
		}
	}
	// No journal record may survive a clean run.
	entries, err := os.ReadDir(s.journalDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("journal records leaked after successful restores: %v", entries)
	}
}

// TestConcurrentBackupWhileRestoring: a backup running against a volume that is
// being replaced must either succeed with a coherent archive or fail cleanly.
// It must never produce an unreadable archive or damage the volume.
func TestConcurrentBackupWhileRestoring(t *testing.T) {
	s := newTestStore(t)
	seedVolume(t, s, "web", "v1")
	archive, err := s.Backup("web")
	if err != nil {
		t.Fatal(err)
	}
	writeVolumeFile(t, s.VolumePath("web"), "data.txt", "v2")

	var wg sync.WaitGroup
	var restoreErr error
	backupResults := make(chan string, 32)
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			restoreErr = s.RestoreSource("web", s.VolumePath("web"), archive)
			if restoreErr != nil {
				break
			}
		}
		close(stop)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			path, err := s.BackupWithOptions("web", BackupOptions{
				ArchiveName: fmt.Sprintf("concurrent-%d", i),
				Threads:     1,
			})
			if err == nil {
				backupResults <- path
			}
		}
	}()
	wg.Wait()
	close(backupResults)
	if restoreErr != nil {
		t.Fatalf("restore failed while backups were running: %v", restoreErr)
	}
	for path := range backupResults {
		if _, err := s.VerifyArchive(path); err != nil {
			t.Fatalf("archive produced during a restore is unreadable: %s: %v", path, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(s.VolumePath("web"), "data.txt"))
	if err != nil || string(got) != "v1" {
		t.Fatalf("volume is not the restored content: %q err=%v", got, err)
	}
}

// TestConcurrentBundleRestoresIndependentTargets exercises the multi-volume
// staging path under contention.
func TestConcurrentBundleRestoresIndependentTargets(t *testing.T) {
	s := newTestStore(t)
	sources := map[string]string{}
	for _, n := range []string{"app", "db"} {
		dir := t.TempDir()
		writeVolumeFile(t, dir, "data.txt", "c-"+n)
		sources[n] = dir
	}
	archive, err := s.BackupBundleSourcesWithOptions([]string{"app", "db"}, sources, t.TempDir(), "proj", BackupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 4
	errs := make([]error, workers)
	targets := make([]map[string]string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		targets[i] = map[string]string{"app": t.TempDir(), "db": t.TempDir()}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.RestoreBundleSources(archive, targets[i])
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("bundle restore %d: %v", i, err)
		}
		for name, dir := range targets[i] {
			got, err := os.ReadFile(filepath.Join(dir, "data.txt"))
			if err != nil || string(got) != "c-"+name {
				t.Fatalf("bundle restore %d volume %s: %q err=%v", i, name, got, err)
			}
		}
	}
}

func TestConcurrentCreateHasSingleWinner(t *testing.T) {
	s := newTestStore(t)
	const workers = 16
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errCh <- s.Create("shared", map[string]string{"worker": fmt.Sprint(i)})
		}(i)
	}
	wg.Wait()
	close(errCh)
	winners := 0
	for err := range errCh {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrExists):
		default:
			t.Fatalf("unexpected concurrent Create error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent Create winners=%d, want exactly one", winners)
	}
	if _, err := s.Get("shared"); err != nil {
		t.Fatalf("winning metadata is unreadable: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 2026-09-11 review — R3: os.CreateTemp publishes a journal record's name
// before its owner can flock it. A NewStore running in another process scanned
// the journal directory, found a record it could lock that carried no complete
// line, and removed it as a crash leftover. The owner then either failed with a
// bare "lock replacement journal: resource temporarily unavailable" — aborting
// an otherwise valid restore — or kept writing to an unlinked inode, leaving
// the swap unjournalled and therefore unrecoverable, which is the crash window
// invariant 14 exists to close.
// ---------------------------------------------------------------------------

// withJournalCreateHook installs the fault-injection seam for one test.
func withJournalCreateHook(t *testing.T, hook func(path string)) {
	t.Helper()
	journalCreateHook = hook
	t.Cleanup(func() { journalCreateHook = nil })
}

func TestJournalRecordSurvivesConcurrentRecoveryRemovingIt(t *testing.T) {
	s := newTestStore(t)
	stolen := 0
	withJournalCreateHook(t, func(path string) {
		// Exactly what a recovery pass does to a record with no complete line.
		if stolen == 0 {
			stolen++
			if err := os.Remove(path); err != nil {
				t.Errorf("simulating recovery: %v", err)
			}
		}
	})
	staging := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	rec, err := s.beginStaging(staging)
	if err != nil {
		t.Fatalf("beginStaging must claim another name, not fail: %v", err)
	}
	defer rec.discard()
	if stolen != 1 {
		t.Fatalf("the race was not exercised (stolen=%d)", stolen)
	}
	if _, err := os.Stat(rec.path); err != nil {
		t.Fatalf("journal record is not on disk, so a crash would be unrecoverable: %v", err)
	}
}

func TestJournalRecordSurvivesConcurrentRecoveryLockingIt(t *testing.T) {
	s := newTestStore(t)
	held := []*os.File(nil)
	t.Cleanup(func() {
		for _, f := range held {
			_ = f.Close()
		}
	})
	locked := 0
	withJournalCreateHook(t, func(path string) {
		// A recovery pass that has taken the lock and not yet removed the file.
		if locked > 0 {
			return
		}
		locked++
		f, err := os.OpenFile(path, os.O_RDWR, 0o600)
		if err != nil {
			t.Errorf("simulating recovery: %v", err)
			return
		}
		if err := unix.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Errorf("simulating recovery lock: %v", err)
		}
		held = append(held, f)
	})
	staging := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	rec, err := s.beginStaging(staging)
	if err != nil {
		t.Fatalf("beginStaging must claim another name, not fail: %v", err)
	}
	defer rec.discard()
	if locked != 1 {
		t.Fatalf("the race was not exercised (locked=%d)", locked)
	}
	if _, err := os.Stat(rec.path); err != nil {
		t.Fatalf("journal record is not on disk: %v", err)
	}
}

func TestReplacementGroupSurvivesConcurrentRecoveryRemovingIt(t *testing.T) {
	s := newTestStore(t)
	stolen := 0
	withJournalCreateHook(t, func(path string) {
		if stolen == 0 && strings.Contains(filepath.Base(path), "group-") {
			stolen++
			if err := os.Remove(path); err != nil {
				t.Errorf("simulating recovery: %v", err)
			}
		}
	})
	group, err := s.beginReplacementGroup()
	if err != nil {
		t.Fatalf("beginReplacementGroup must claim another name, not fail: %v", err)
	}
	defer group.discard()
	if stolen != 1 {
		t.Fatalf("the race was not exercised (stolen=%d)", stolen)
	}
	// A group whose commit marker is not on disk reads as "never committed",
	// which would roll back every member of a completed bundle restore.
	if _, err := os.Stat(group.path); err != nil {
		t.Fatalf("replacement group marker is not on disk: %v", err)
	}
}

// TestConcurrentStoreOpensDoNotStealInFlightJournalRecords is the unseeded
// form of the same defect: another dvol process merely starting up must not
// break a restore that is registering its staging directory.
func TestConcurrentStoreOpensDoNotStealInFlightJournalRecords(t *testing.T) {
	s := newTestStore(t)
	root := s.cfg.Root
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := NewStore(Config{Root: root}); err != nil {
				t.Errorf("concurrent NewStore: %v", err)
				return
			}
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()

	base := t.TempDir()
	for i := 0; i < 300; i++ {
		staging := filepath.Join(base, fmt.Sprintf("stage-%d", i))
		if err := os.MkdirAll(staging, 0o755); err != nil {
			t.Fatal(err)
		}
		rec, err := s.beginStaging(staging)
		if err != nil {
			t.Fatalf("iteration %d: a concurrent store open broke the journal record: %v", i, err)
		}
		if _, err := os.Stat(rec.path); err != nil {
			t.Fatalf("iteration %d: journal record was removed while in flight: %v", i, err)
		}
		rec.discard()
	}
}
