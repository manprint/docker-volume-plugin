package dvol

import (
	"bytes"
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
	tars, r, f, err := openArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	defer r.Close()
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

func TestExternalBundleSourcesRoundTrip(t *testing.T) {
	s := newTestStore(t)
	sources := map[string]string{}
	for _, name := range []string{"legacy_app", "legacy_db"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("content-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
		sources[name] = dir
	}
	archive, err := s.BackupBundleSourcesWithOptions(
		[]string{"legacy_db", "legacy_app"}, sources, filepath.Join(t.TempDir(), "compose"), "legacy", BackupOptions{},
	)
	if err != nil {
		t.Fatalf("BackupBundleSourcesWithOptions: %v", err)
	}

	targets := map[string]string{}
	for _, name := range []string{"legacy_app", "legacy_db"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("replace me"), 0o644); err != nil {
			t.Fatal(err)
		}
		targets[name] = dir
	}
	if err := s.RestoreBundleSources(archive, targets); err != nil {
		t.Fatalf("RestoreBundleSources: %v", err)
	}
	for _, name := range []string{"legacy_app", "legacy_db"} {
		got, err := os.ReadFile(filepath.Join(targets[name], "data.txt"))
		if err != nil || string(got) != "content-"+name {
			t.Fatalf("%s data wrong: %q err=%v", name, got, err)
		}
		if _, err := os.Stat(filepath.Join(targets[name], "old.txt")); !os.IsNotExist(err) {
			t.Fatalf("%s old target data should have been replaced", name)
		}
	}
}

func TestExternalBundleStreamRoundTripAndTruncation(t *testing.T) {
	s := newTestStore(t)
	sources := map[string]string{}
	for _, name := range []string{"stream_app", "stream_db"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("stream-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
		sources[name] = dir
	}
	var stream bytes.Buffer
	if err := s.BackupBundleSourcesToWriter([]string{"stream_db", "stream_app"}, sources, BackupOptions{Compression: CompressionZstd, Threads: 1}, &stream); err != nil {
		t.Fatalf("BackupBundleSourcesToWriter: %v", err)
	}
	targets := map[string]string{}
	for _, name := range []string{"stream_app", "stream_db"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		targets[name] = dir
	}
	if err := s.RestoreBundleSourcesReader(bytes.NewReader(stream.Bytes()), targets); err != nil {
		t.Fatalf("RestoreBundleSourcesReader: %v", err)
	}
	for name, target := range targets {
		got, err := os.ReadFile(filepath.Join(target, "data.txt"))
		if err != nil || string(got) != "stream-"+name {
			t.Fatalf("%s data: %q err=%v", name, got, err)
		}
		if _, err := os.Stat(filepath.Join(target, "keep.txt")); !os.IsNotExist(err) {
			t.Fatalf("%s old data survived stream restore", name)
		}
	}

	for name, target := range targets {
		if err := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("still-keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		data := stream.Bytes()
		if err := s.RestoreBundleSourcesReader(bytes.NewReader(data[:len(data)-5]), map[string]string{name: target}); err == nil {
			t.Fatal("expected truncated bundle to fail")
		}
		got, err := os.ReadFile(filepath.Join(target, "keep.txt"))
		if err != nil || string(got) != "still-keep" {
			t.Fatalf("%s changed after truncated bundle: %q err=%v", name, got, err)
		}
	}
}

func TestRestoreBundleAllAndRecreatesVolumes(t *testing.T) {
	s := newTestStore(t)
	for _, n := range []string{"app", "db"} {
		if err := s.Create(n, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.VolumePath(n), "data.txt"), []byte("c-"+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bundleDir := filepath.Join(t.TempDir(), "backups", "compose")
	path, err := s.BackupBundle([]string{"db", "app"}, bundleDir, "p")
	if err != nil {
		t.Fatal(err)
	}
	// destroy the volumes entirely (data + metadata), then restore everything
	if err := s.Remove("app"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("db"); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreBundle(path, nil); err != nil {
		t.Fatalf("RestoreBundle(all): %v", err)
	}
	for _, n := range []string{"app", "db"} {
		v, err := s.Get(n)
		if err != nil {
			t.Fatalf("volume %q metadata missing after bundle restore: %v", n, err)
		}
		if v.Name != n {
			t.Fatalf("unexpected name %q", v.Name)
		}
		got, err := os.ReadFile(filepath.Join(s.VolumePath(n), "data.txt"))
		if err != nil || string(got) != "c-"+n {
			t.Fatalf("%s data wrong: %q err=%v", n, got, err)
		}
	}
}

func TestRestoreBundleCorruptPreservesVolumes(t *testing.T) {
	s := newTestStore(t)
	for _, n := range []string{"app", "db"} {
		if err := s.Create(n, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.VolumePath(n), "keep.txt"), []byte("keep-"+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bundleDir := filepath.Join(t.TempDir(), "backups", "compose")
	path, err := s.BackupBundle([]string{"app", "db"}, bundleDir, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreBundle(path, nil); err == nil {
		t.Fatal("expected corrupt bundle restore to fail")
	}
	for _, n := range []string{"app", "db"} {
		got, err := os.ReadFile(filepath.Join(s.VolumePath(n), "keep.txt"))
		if err != nil || string(got) != "keep-"+n {
			t.Fatalf("%s damaged by failed restore: %q err=%v", n, got, err)
		}
	}
}
