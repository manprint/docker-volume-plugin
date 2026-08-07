package dvol

import (
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

func TestStoreRecoversInterruptedReplace(t *testing.T) {
	t.Run("before publish", func(t *testing.T) {
		root := t.TempDir()
		s, err := NewStore(Config{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Create("web", nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.VolumePath("web"), "state"), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}

		rollback, err := os.MkdirTemp(filepath.Dir(s.VolumePath("web")), ".web.dvol-old-*")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(rollback); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(s.VolumePath("web"), rollback); err != nil {
			t.Fatal(err)
		}

		if _, err := NewStore(Config{Root: root}); err != nil {
			t.Fatalf("NewStore recovery: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(s.VolumePath("web"), "state"))
		if err != nil || string(got) != "old" {
			t.Fatalf("old volume was not recovered: %q err=%v", got, err)
		}
		if _, err := os.Lstat(rollback); !os.IsNotExist(err) {
			t.Fatalf("rollback directory remains: %v", err)
		}
	})

	t.Run("after publish", func(t *testing.T) {
		root := t.TempDir()
		s, err := NewStore(Config{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Create("web", nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.VolumePath("web"), "state"), []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}

		rollback, err := os.MkdirTemp(filepath.Dir(s.VolumePath("web")), ".web.dvol-old-*")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(rollback); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(rollback, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rollback, "state"), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := NewStore(Config{Root: root}); err != nil {
			t.Fatalf("NewStore recovery: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(s.VolumePath("web"), "state"))
		if err != nil || string(got) != "new" {
			t.Fatalf("published volume was not preserved: %q err=%v", got, err)
		}
		if _, err := os.Lstat(rollback); !os.IsNotExist(err) {
			t.Fatalf("completed rollback directory remains: %v", err)
		}
	})
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

func TestInvalidName(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("../evil", nil); err != ErrInvalidName {
		t.Fatalf("expected ErrInvalidName, got %v", err)
	}
	if err := s.Create("/abs", nil); err != ErrInvalidName {
		t.Fatalf("expected ErrInvalidName, got %v", err)
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
