package dvol

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestBackupRestoreCompressionFormats(t *testing.T) {
	for _, c := range []Compression{CompressionGzip, CompressionXz, CompressionZstd, CompressionLz4} {
		for _, level := range []int{0, 3} {
			if (c == CompressionGzip || c == CompressionXz) && level == 3 {
				level = 4
			}
			t.Run(string(c)+"-l"+strconv.Itoa(level), func(t *testing.T) {
				s := newTestStore(t)
				if err := s.Create("web", nil); err != nil {
					t.Fatal(err)
				}
				vdir := s.VolumePath("web")
				if err := os.WriteFile(filepath.Join(vdir, "data.txt"), []byte("payload-"+string(c)), 0o640); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("data.txt", filepath.Join(vdir, "link.txt")); err != nil {
					t.Fatal(err)
				}
				threads := 2
				if c == CompressionXz {
					threads = 1
				}
				opts := BackupOptions{Compression: c, Level: level, Threads: threads}
				path, err := s.BackupWithOptions("web", opts)
				if err != nil {
					t.Fatalf("BackupWithOptions(%s): %v", c, err)
				}
				ext := opts.norm().ext()
				if filepath.Ext(path) != "."+ext {
					t.Fatalf("unexpected extension %q (want .%s)", path, ext)
				}

				if err := s.Remove("web"); err != nil {
					t.Fatal(err)
				}
				if err := s.Restore("web", path); err != nil {
					t.Fatalf("Restore(%s): %v", c, err)
				}
				got, err := os.ReadFile(filepath.Join(s.VolumePath("web"), "data.txt"))
				if err != nil || string(got) != "payload-"+string(c) {
					t.Fatalf("content wrong: %q err=%v", got, err)
				}
				if target, err := os.Readlink(filepath.Join(s.VolumePath("web"), "link.txt")); err != nil || target != "data.txt" {
					t.Fatalf("symlink wrong: %q err=%v", target, err)
				}
			})
		}
	}
}

func TestBackupOptionsLevelValidation(t *testing.T) {
	bad := []BackupOptions{
		{Compression: CompressionGzip, Level: 10},
		{Compression: CompressionGzip, Level: -1},
		{Compression: CompressionXz, Level: 10},
		{Compression: CompressionZstd, Level: 0 - 1},
		{Compression: CompressionZstd, Level: 23},
		{Compression: CompressionLz4, Level: 10},
		{Compression: CompressionLz4, Level: -1},
		{Compression: CompressionXz, Threads: 2},
		{Compression: CompressionGzip, Threads: -1},
		{Compression: Compression("bzip2")},
		{Compression: Compression("bz2"), Level: 0},
	}
	for _, o := range bad {
		if err := o.Validate(); err == nil {
			t.Fatalf("expected validation error for %+v", o)
		}
	}
	good := []BackupOptions{
		{Compression: "", Level: 0},
		{Compression: CompressionGzip, Level: 9},
		{Compression: CompressionXz, Level: 9},
		{Compression: CompressionZstd, Level: 22},
		{Compression: CompressionLz4, Level: 9, Threads: 8},
		{Compression: CompressionXz, Level: 6, Threads: 1},
	}
	for _, o := range good {
		if err := o.Validate(); err != nil {
			t.Fatalf("unexpected error for %+v: %v", o, err)
		}
	}
}

func TestListArchivesMixedFormats(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	for n, ext := range map[string]string{"a": "gz", "b": "xz", "c": "zst", "d": "lz4"} {
		if err := os.WriteFile(filepath.Join(dir, n+".tar."+ext), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.gz"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	infos, err := s.ListArchives(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 4 {
		t.Fatalf("expected 4 archives, got %d (%+v)", len(infos), infos)
	}
	for _, b := range infos {
		if !isArchiveName(b.Name) {
			t.Fatalf("%q not seen as archive", b.Name)
		}
	}
}

func TestCompressionBalancedDefaultsAndThreads(t *testing.T) {
	for _, compression := range []Compression{CompressionGzip, CompressionXz, CompressionZstd, CompressionLz4} {
		if got := balancedLevel(compression); got != 6 {
			t.Errorf("balanced level for %s = %d, want 6", compression, got)
		}
		if err := (BackupOptions{Compression: compression}).Validate(); err != nil {
			t.Errorf("default options for %s rejected: %v", compression, err)
		}
	}
	wantThreads := runtime.NumCPU() / 2
	if wantThreads < 1 {
		wantThreads = 1
	}
	if got := compressionThreads(0); got != wantThreads {
		t.Fatalf("default compression threads = %d, want %d", got, wantThreads)
	}
	if got := compressionThreads(3); got != 3 {
		t.Fatalf("explicit compression threads = %d, want 3", got)
	}
}

func TestCustomBackupsDir(t *testing.T) {
	backDir := t.TempDir()
	storeRoot := t.TempDir()
	s, err := NewStore(Config{Root: storeRoot, BackupsDir: backDir})
	if err != nil {
		t.Fatal(err)
	}
	if s.backupsDir() != backDir {
		t.Fatalf("backupsDir=%q want %q", s.backupsDir(), backDir)
	}
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.VolumePath("web"), "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := s.Backup("web")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, backDir+string(filepath.Separator)) {
		t.Fatalf("backup written outside custom dir: %q", path)
	}
	if _, err := os.Stat(filepath.Join(backDir, "web", filepath.Base(path))); err != nil {
		t.Fatalf("backup missing under custom dir: %v", err)
	}
	// restore latest resolves within the custom dir
	infos, err := s.ListBackups("web")
	if err != nil || len(infos) != 1 || infos[0].Path != path {
		t.Fatalf("ListBackups: %+v err=%v", infos, err)
	}
	// default store still uses <root>/backups
	s2 := newTestStore(t)
	if s2.backupsDir() != filepath.Join(s2.cfg.Root, backupsDir) {
		t.Fatalf("default backups dir wrong: %q", s2.backupsDir())
	}
}

func TestRestoreUnsupportedFormat(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "web.tar.bz2")
	if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore("web", p); err == nil {
		t.Fatal("expected unsupported-format error")
	}
}
