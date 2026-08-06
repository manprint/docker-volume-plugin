package dvol

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBackupBundleRoundTrip(t *testing.T) {
	s := newTestStore(t)
	for _, n := range []string{"app", "db"} {
		if err := s.Create(n, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.VolumePath(n), "data.txt"), []byte("content-"+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bundleDir := filepath.Join(t.TempDir(), "backups", "compose")
	path, err := s.BackupBundle([]string{"db", "app"}, bundleDir, "myapp")
	if err != nil {
		t.Fatalf("BackupBundle: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("bundle missing: %v", err)
	}

	infos, err := s.ListArchives(bundleDir)
	if err != nil || len(infos) != 1 {
		t.Fatalf("ListArchives: %+v err=%v", infos, err)
	}

	// destroy both volumes, restore only "app"
	if err := s.Remove("app"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("db"); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreBundle(path, []string{"app"}); err != nil {
		t.Fatalf("RestoreBundle: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(s.VolumePath("app"), "data.txt"))
	if err != nil || string(got) != "content-app" {
		t.Fatalf("app data wrong: %q err=%v", got, err)
	}
	if _, err := os.Stat(s.VolumePath("db")); !os.IsNotExist(err) {
		t.Fatalf("db should not have been restored")
	}
}

func TestBackupBundleManifest(t *testing.T) {
	s := newTestStore(t)
	s.Create("x", nil)
	path, err := s.BackupBundle([]string{"x"}, t.TempDir(), "p")
	if err != nil {
		t.Fatal(err)
	}
	tars, gz, f, err := openTar(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	defer gz.Close()
	found := false
	for {
		hdr, err := tars.Next()
		if err != nil {
			break
		}
		if hdr.Name == "manifest.json" {
			found = true
		}
	}
	if !found {
		t.Fatal("manifest.json missing from bundle")
	}
}
