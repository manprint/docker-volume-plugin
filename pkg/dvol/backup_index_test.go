package dvol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newFlatStore(t *testing.T) (*Store, string) {
	t.Helper()
	destination := t.TempDir()
	s, err := NewStore(Config{Root: t.TempDir(), BackupsDir: destination})
	if err != nil {
		t.Fatal(err)
	}
	return s, destination
}

func TestFlatBackupStoresAndResolvesMultipleVolumes(t *testing.T) {
	s, destination := newFlatStore(t)
	for _, name := range []string{"pg_data", "filestore_data"} {
		if err := s.Create(name, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.VolumePath(name), "data.txt"), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const stamp = "07082026_223010"
	paths := make(map[string]string)
	for name, archive := range map[string]string{
		"pg_data":        "postgres-odoo",
		"filestore_data": "filestore-odoo",
	} {
		path, err := s.BackupWithOptions(name, BackupOptions{
			ArchiveName:     archive,
			Timestamp:       true,
			TimestampValue:  stamp,
			SameDestination: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		paths[name] = path
		if filepath.Dir(path) != destination {
			t.Fatalf("flat archive path=%q, want direct child of %q", path, destination)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("flat archive permissions: info=%v err=%v", info, err)
		}
		if !strings.Contains(filepath.Base(path), "_"+stamp+".") {
			t.Fatalf("timestamp missing from %q", path)
		}
	}
	if info, err := os.Stat(filepath.Join(destination, flatBackupIndexName)); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("flat index permissions: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "pg_data")); !os.IsNotExist(err) {
		t.Fatalf("flat backup unexpectedly created per-volume directory: %v", err)
	}

	for volume, path := range paths {
		infos, err := s.ListBackups(volume)
		if err != nil || len(infos) != 1 || infos[0].Path != path {
			t.Fatalf("ListBackups(%q): %+v err=%v", volume, infos, err)
		}
		got, err := s.ResolveBackup(volume, filepath.Base(path))
		if err != nil || got != path {
			t.Fatalf("ResolveBackup(%q): got=%q want=%q err=%v", volume, got, path, err)
		}
	}
}

func TestFlatBackupRejectsArchiveCollision(t *testing.T) {
	s, _ := newFlatStore(t)
	for _, name := range []string{"one", "two"} {
		if err := s.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	opts := BackupOptions{ArchiveName: "same", SameDestination: true}
	if _, err := s.BackupWithOptions("one", opts); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BackupWithOptions("two", opts); err == nil {
		t.Fatal("expected flat archive collision")
	}
	infos, err := s.ListBackups("one")
	if err != nil || len(infos) != 1 {
		t.Fatalf("first archive was not preserved: %+v err=%v", infos, err)
	}
	if got, _ := s.ListBackups("two"); len(got) != 0 {
		t.Fatalf("collision created metadata for second volume: %+v", got)
	}
}

func TestFlatBackupIndexRecoversPendingPublishedArchive(t *testing.T) {
	s, destination := newFlatStore(t)
	archive := "web-backup.tar.gz"
	if err := os.WriteFile(filepath.Join(destination, archive), []byte("published"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := flatBackupIndex{
		Version: flatBackupIndexVersion,
		Entries: []flatBackupIndexEntry{{
			Volume: "web", Name: archive, CreatedAt: time.Now().UTC(), Pending: true,
		}},
	}
	b, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, flatBackupIndexName), b, 0o644); err != nil {
		t.Fatal(err)
	}

	infos, err := s.ListBackups("web")
	if err != nil || len(infos) != 1 || infos[0].Name != archive {
		t.Fatalf("pending published archive not recovered: %+v err=%v", infos, err)
	}
	updated, err := os.ReadFile(filepath.Join(destination, flatBackupIndexName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), `"pending": true`) {
		t.Fatal("recovered archive remained pending")
	}
}

func TestFlatBackupIndexDropsUnpublishedEntry(t *testing.T) {
	s, destination := newFlatStore(t)
	idx := flatBackupIndex{
		Version: flatBackupIndexVersion,
		Entries: []flatBackupIndexEntry{{
			Volume: "web", Name: "missing.tar.gz", Pending: true,
		}},
	}
	b, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, flatBackupIndexName), b, 0o644); err != nil {
		t.Fatal(err)
	}
	infos, err := s.ListBackups("web")
	if err != nil || len(infos) != 0 {
		t.Fatalf("unpublished entry was not removed: %+v err=%v", infos, err)
	}
	updated, err := os.ReadFile(filepath.Join(destination, flatBackupIndexName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), "missing.tar.gz") {
		t.Fatal("stale archive metadata remained")
	}
}

func TestFlatBackupIndexSerializesConcurrentBackups(t *testing.T) {
	s, _ := newFlatStore(t)
	const count = 8
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("volume-%d", i)
		if err := s.Create(name, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.VolumePath(name), "data"), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("volume-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.BackupWithOptions(name, BackupOptions{
				ArchiveName:     name,
				SameDestination: true,
			})
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
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("volume-%d", i)
		infos, err := s.ListBackups(name)
		if err != nil || len(infos) != 1 {
			t.Fatalf("concurrent flat backup %q: %+v err=%v", name, infos, err)
		}
	}
}
