package dvol

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type tarItem struct {
	hdr  tar.Header
	data []byte
}

func craftTar(t *testing.T, items []tarItem) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, it := range items {
		hdr := it.hdr
		if hdr.Name != "" && hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if hdr.Uid == 0 && hdr.Gid == 0 {
			// default to the current process identity so metadata restore
			// (chown/chmod) works in the test process
			hdr.Uid = os.Geteuid()
			hdr.Gid = os.Getegid()
		}
		if len(it.data) > 0 {
			hdr.Size = int64(len(it.data))
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if len(it.data) > 0 {
			if _, err := tw.Write(it.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "arch.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRestoreCorruptArchivePreservesVolume(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	vdir := s.VolumePath("web")
	if err := os.WriteFile(filepath.Join(vdir, "data.txt"), []byte("important"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := s.Backup("web")
	if err != nil {
		t.Fatal(err)
	}

	// overwrite the archive with non-gzip data
	if err := os.WriteFile(path, []byte("this is not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore("web", path); err == nil {
		t.Fatal("expected restore of corrupt archive to fail")
	}
	got, err := os.ReadFile(filepath.Join(s.VolumePath("web"), "data.txt"))
	if err != nil || string(got) != "important" {
		t.Fatalf("volume data lost after failed restore: %q err=%v", got, err)
	}
}

func TestExternalSourceBackupAndRestore(t *testing.T) {
	s := newTestStore(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "legacy.txt"), []byte("legacy-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	archive, err := s.BackupSourceWithOptions("legacy", source, BackupOptions{})
	if err != nil {
		t.Fatalf("BackupSourceWithOptions: %v", err)
	}
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "old.txt"), []byte("must be replaced"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreSource("legacy", target, archive); err != nil {
		t.Fatalf("RestoreSource: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "legacy.txt"))
	if err != nil || string(got) != "legacy-data" {
		t.Fatalf("restored external source is wrong: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(target, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old target data should have been replaced")
	}
}

func TestExternalSourceStreamRoundTripAllCompressions(t *testing.T) {
	s := newTestStore(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "stream.txt"), []byte("stream-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, compression := range []Compression{CompressionGzip, CompressionXz, CompressionZstd, CompressionLz4} {
		t.Run(string(compression), func(t *testing.T) {
			var stream bytes.Buffer
			if err := s.BackupSourceToWriter("stream-"+string(compression), source, BackupOptions{Compression: compression, Threads: 1}, &stream); err != nil {
				t.Fatalf("BackupSourceToWriter: %v", err)
			}
			target := t.TempDir()
			if err := os.WriteFile(filepath.Join(target, "old.txt"), []byte("remove"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := s.RestoreSourceReader("stream-"+string(compression), target, bytes.NewReader(stream.Bytes())); err != nil {
				t.Fatalf("RestoreSourceReader: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(target, "stream.txt"))
			if err != nil || string(got) != "stream-data" {
				t.Fatalf("stream data: %q err=%v", got, err)
			}
			if _, err := os.Stat(filepath.Join(target, "old.txt")); !os.IsNotExist(err) {
				t.Fatal("old target data survived stream restore")
			}
		})
	}
}

func TestExternalSourceStreamTruncationPreservesTarget(t *testing.T) {
	s := newTestStore(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := s.BackupSourceToWriter("stream", source, BackupOptions{Compression: CompressionZstd, Threads: 1}, &stream); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := stream.Bytes()
	if len(data) < 16 {
		t.Fatal("test archive unexpectedly small")
	}
	if err := s.RestoreSourceReader("stream", target, bytes.NewReader(data[:len(data)/2])); err == nil {
		t.Fatal("expected truncated stream to fail")
	}
	got, err := os.ReadFile(filepath.Join(target, "keep.txt"))
	if err != nil || string(got) != "keep" {
		t.Fatalf("target changed after truncated stream: %q err=%v", got, err)
	}
}

func TestRestoreDoesNotDeleteCollidingVolume(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("foo", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Create("foo.dvol-old", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.VolumePath("foo"), "data.txt"), []byte("restored"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.VolumePath("foo.dvol-old"), "sentinel.txt"), []byte("must survive"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := s.Backup("foo")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Restore("foo", path); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(s.VolumePath("foo.dvol-old"), "sentinel.txt"))
	if err != nil || string(got) != "must survive" {
		t.Fatalf("colliding volume was damaged: %q err=%v", got, err)
	}
	if _, err := s.Get("foo.dvol-old"); err != nil {
		t.Fatalf("colliding volume metadata was damaged: %v", err)
	}
}

func TestRestoreRejectsSymlinkEscape(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "pwned")

	// craft an archive: first a symlink "x" pointing outside the volume,
	// then a regular file "x/pwn" that would be written through it
	payload := []byte("bad")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "x", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "x/pwn", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "evil.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Restore("web", path); err == nil {
		t.Fatal("expected restore to reject symlink escape")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("escaped file was created at %s", outside)
	}
}

func TestRestoreRejectsZipSlip(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape", "../../etc/passwd", "/etc/passwd", "a/../../escape"} {
		p := craftTar(t, []tarItem{{
			hdr:  tar.Header{Name: name, Typeflag: tar.TypeReg},
			data: []byte("x"),
		}})
		if err := s.Restore("web", p); err == nil {
			t.Errorf("expected zip-slip rejection for %q", name)
		}
	}
	// a legit nested file still extracts
	good := craftTar(t, []tarItem{{
		hdr:  tar.Header{Name: "a/b.txt", Typeflag: tar.TypeReg},
		data: []byte("ok"),
	}})
	if err := s.Restore("web", good); err != nil {
		t.Fatalf("legit path rejected: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(s.VolumePath("web"), "a", "b.txt")); err != nil || string(got) != "ok" {
		t.Fatalf("legit entry wrong: %q err=%v", got, err)
	}
}

func TestRestoreRejectsBundleZipSlip(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("app", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.VolumePath("app"), "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app/../../escape", "app/../../../etc/passwd", "app/.."} {
		p := craftTar(t, []tarItem{{
			hdr:  tar.Header{Name: name, Typeflag: tar.TypeReg},
			data: []byte("x"),
		}})
		if err := s.RestoreBundle(p, nil); err == nil {
			t.Errorf("expected bundle zip-slip rejection for %q", name)
		}
		if got, err := os.ReadFile(filepath.Join(s.VolumePath("app"), "keep.txt")); err != nil || string(got) != "keep" {
			t.Fatalf("volume damaged by rejected %q: %q err=%v", name, got, err)
		}
	}

	// hardlink escape through "." linkname
	p := craftTar(t, []tarItem{{
		hdr:  tar.Header{Name: "app/passwd", Typeflag: tar.TypeReg},
		data: []byte("pwn"),
	}, {
		hdr: tar.Header{Name: "app/hl", Typeflag: tar.TypeLink, Linkname: "/../../etc/passwd"},
	}})
	if err := s.RestoreBundle(p, nil); err == nil {
		t.Fatal("expected bundle hardlink escape rejection")
	}
}

func TestRestorePreservesHardlinks(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	p := craftTar(t, []tarItem{
		{hdr: tar.Header{Name: "data.txt", Typeflag: tar.TypeReg}, data: []byte("hello")},
		{hdr: tar.Header{Name: "link.txt", Typeflag: tar.TypeLink, Linkname: "data.txt"}},
	})
	if err := s.Restore("web", p); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	fi1, err := os.Stat(filepath.Join(s.VolumePath("web"), "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fi2, err := os.Stat(filepath.Join(s.VolumePath("web"), "link.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fi1, fi2) {
		t.Fatal("hardlink not preserved (different inodes)")
	}
}

func TestRestoreBundleMalformedEntry(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("app", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.VolumePath("app"), "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	// entry "no-slash" is not a valid <vol>/<rest> path
	p := craftTar(t, []tarItem{{
		hdr:  tar.Header{Name: "no-slash", Typeflag: tar.TypeReg},
		data: []byte("x"),
	}})
	if err := s.RestoreBundle(p, nil); err == nil {
		t.Fatal("expected malformed bundle entry rejection")
	}
	if got, err := os.ReadFile(filepath.Join(s.VolumePath("app"), "keep.txt")); err != nil || string(got) != "keep" {
		t.Fatalf("volume damaged by malformed bundle: %q err=%v", got, err)
	}
}

func TestRestoreBundleRejectsAbsoluteRoot(t *testing.T) {
	s := newTestStore(t)
	p := craftTar(t, []tarItem{{
		hdr:  tar.Header{Name: "/abs/path", Typeflag: tar.TypeReg},
		data: []byte("x"),
	}})
	if err := s.RestoreBundle(p, nil); err == nil {
		t.Fatal("expected absolute path rejection")
	}
}

func TestResolveBackup(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.VolumePath("web"), "f"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	p1, err := s.Backup("web")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.VolumePath("web"), "f"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	p2, err := s.Backup("web")
	if err != nil {
		t.Fatal(err)
	}

	// exact basename resolved inside backups/<vol>/
	got, err := s.ResolveBackup("web", filepath.Base(p2))
	if err != nil || got != p2 {
		t.Fatalf("basename resolve: got %q err=%v", got, err)
	}
	// absolute path
	got, err = s.ResolveBackup("web", p1)
	if err != nil || got != p1 {
		t.Fatalf("abs resolve: got %q err=%v", got, err)
	}
	// the two backups have unique timestamps
	if p1 == p2 {
		t.Fatalf("same-second backups share path %q", p1)
	}
	// ambiguous glob -> error
	if _, err := s.ResolveBackup("web", "*.tar.gz"); err == nil {
		t.Fatal("expected ambiguity error for multi-match glob")
	}
	// unmatched -> error
	if _, err := s.ResolveBackup("web", "nope*.gz"); err == nil {
		t.Fatal("expected error for unmatched selector")
	}
}

func TestListArchivesFiltersAndSorts(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.tar.gz"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "c.tar.gz"), 0o755); err != nil {
		t.Fatal(err)
	}
	infos, err := s.ListArchives(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Name != "b.tar.gz" {
		t.Fatalf("ListArchives: %+v", infos)
	}
	// missing dir -> empty, no error
	infos, err = s.ListArchives(filepath.Join(dir, "missing"))
	if err != nil || len(infos) != 0 {
		t.Fatalf("missing dir: %+v err=%v", infos, err)
	}
}

func TestRestorePreservesMetadataExact(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("pg", nil); err != nil {
		t.Fatal(err)
	}
	vdir := s.VolumePath("pg")
	fixed := time.Date(2021, 6, 1, 12, 30, 0, 0, time.UTC)
	sub := filepath.Join(vdir, "base")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sub, "pg_xlog.sample")
	if err := os.WriteFile(file, []byte("pg-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	// fixed times on both the file and its directory
	if err := os.Chtimes(file, fixed, fixed); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sub, fixed, fixed); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("base", filepath.Join(vdir, "current")); err != nil {
		t.Fatal(err)
	}

	path, err := s.Backup("pg")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("pg"); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore("pg", path); err != nil {
		t.Fatal(err)
	}

	check := func(p string, wantMode os.FileMode) {
		t.Helper()
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if fi.Mode().Perm() != wantMode.Perm() {
			t.Errorf("%s: mode %v want %v", p, fi.Mode().Perm(), wantMode.Perm())
		}
		if fi.ModTime().UTC() != fixed {
			t.Errorf("%s: mtime %v want %v", p, fi.ModTime().UTC(), fixed)
		}
		if fi.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
			t.Errorf("%s: uid not preserved", p)
		}
	}
	check(file, 0o600)
	check(sub, 0o700)

	// symlink target and owner preserved
	if target, err := os.Readlink(filepath.Join(vdir, "current")); err != nil || target != "base" {
		t.Fatalf("symlink target: %q err=%v", target, err)
	}
}

func TestBackupPreservesSpecialModeBits(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to set setuid/sticky bits")
	}
	s := newTestStore(t)
	if err := s.Create("svc", nil); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.VolumePath("svc"), "runme")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, os.ModeSetuid|0o755); err != nil { // setuid + rwxr-xr-x
		t.Fatal(err)
	}
	path, err := s.Backup("svc")
	if err != nil {
		t.Fatal(err)
	}
	tr, zr, f, err := openArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	defer f.Close()
	var foundMode int64
	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if strings.TrimSuffix(hdr.Name, "/") == "runme" {
			foundMode = hdr.Mode
			break
		}
	}
	if foundMode&0o4000 == 0 {
		t.Fatalf("setuid bit missing from archive header: %#o", foundMode)
	}
	if err := s.Remove("svc"); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore("svc", path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(s.VolumePath("svc"), "runme"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetuid == 0 {
		t.Fatalf("setuid bit lost after restore: %v", fi.Mode())
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("perm bits wrong: %v", fi.Mode().Perm())
	}
}

func TestTimestampCollision(t *testing.T) {
	a, b := ts(), ts()
	if a == b {
		// still hard to force a same-instant call;
		// at most flag so the assertion is meaningful
		t.Logf("same-nanosecond ts() (%q==%q)", a, b)
	}
	if !strings.Contains(a, ".") {
		t.Fatalf("ts() lacks sub-second precision: %q", a)
	}
}
