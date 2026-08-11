package dvol

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	s, err := NewStore(Config{Root: root})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestCreateGetListRemove(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("data", map[string]string{"type": "ssd"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create("data", nil); err == nil {
		t.Fatal("expected ErrExists on duplicate create")
	}
	v, err := s.Get("data")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.Name != "data" || v.Options["type"] != "ssd" {
		t.Fatalf("unexpected volume: %+v", v)
	}
	if _, err := os.Stat(s.VolumePath("data")); err != nil {
		t.Fatalf("volume dir not created: %v", err)
	}

	vols, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(vols) != 1 || vols[0].Name != "data" {
		t.Fatalf("unexpected list: %+v", vols)
	}

	if err := s.Remove("data"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := s.Get("data"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestLegacyListReportsMissingVolumeData(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("dangling", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(s.VolumePath("dangling")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("List hid inconsistent legacy volume state: %v", err)
	}
}

func TestInvalidName(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("../evil", nil); err != ErrInvalidName {
		t.Fatalf("expected ErrInvalidName, got %v", err)
	}
	if err := s.Create("/abs", nil); err != ErrInvalidName {
		t.Fatalf("expected ErrInvalidName, got %v", err)
	}
	for _, name := range []string{"../evil", "/abs"} {
		if _, err := s.Get(name); err != ErrInvalidName {
			t.Fatalf("Get(%q): expected ErrInvalidName, got %v", name, err)
		}
		if err := s.Remove(name); err != ErrInvalidName {
			t.Fatalf("Remove(%q): expected ErrInvalidName, got %v", name, err)
		}
		if _, err := s.ListBackups(name); err != ErrInvalidName {
			t.Fatalf("ListBackups(%q): expected ErrInvalidName, got %v", name, err)
		}
		if _, err := s.ResolveBackup(name, "*.tar.gz"); err != ErrInvalidName {
			t.Fatalf("ResolveBackup(%q): expected ErrInvalidName, got %v", name, err)
		}
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	vdir := s.VolumePath("web")
	write := func(rel, content string) {
		p := filepath.Join(vdir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "hello")
	write("assets/app.js", "var x = 1;")
	if err := os.Symlink("app.js", filepath.Join(vdir, "assets/link.js")); err != nil {
		t.Fatal(err)
	}

	path, err := s.Backup("web")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}

	// corrupt the volume, then restore
	if err := s.Remove("web"); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore("web", path); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(vdir, "assets/app.js"))
	if err != nil || string(got) != "var x = 1;" {
		t.Fatalf("restored content wrong: %q err=%v", got, err)
	}
	if target, err := os.Readlink(filepath.Join(vdir, "assets/link.js")); err != nil || target != "app.js" {
		t.Fatalf("symlink not restored: %q err=%v", target, err)
	}
}

func TestListBackupsEmpty(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("data", nil); err != nil {
		t.Fatal(err)
	}
	infos, err := s.ListBackups("data")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("expected no backups, got %+v", infos)
	}
}
