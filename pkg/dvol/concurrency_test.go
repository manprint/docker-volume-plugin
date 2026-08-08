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
	"testing"
	"time"
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
