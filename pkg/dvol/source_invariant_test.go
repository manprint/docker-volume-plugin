package dvol

// Source-invariance suite: guarantees backup operations never modify the
// source volume and that failed/attacker-crafted restores leave both the
// target volume and unrelated files untouched.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// treeNode captures every mutable attribute of a tree entry that dvol is
// supposed to leave untouched.
type treeNode struct {
	mode  os.FileMode
	uid   int
	gid   int
	mtime time.Time
	size  int64
	link  string // symlink target, "" for non-symlinks
	sum   string // sha256 of content, "" for non-regular files
}

// snapshotTree records the full state of a directory tree, root included
// (as rel "."), so even the volume root's own mode/mtime/owner are tracked.
func snapshotTree(t *testing.T, root string) map[string]treeNode {
	t.Helper()
	out := map[string]treeNode{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel := "."
		if p != root {
			re, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = re
		}
		n := treeNode{
			mode:  info.Mode(),
			size:  info.Size(),
			mtime: info.ModTime(),
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			n.uid = int(st.Uid)
			n.gid = int(st.Gid)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			n.link = link
		case info.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h := sha256.Sum256(b)
			n.sum = fmt.Sprintf("%x", h)
			n.size = int64(len(b))
		}
		out[rel] = n
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

// assertTreeUnchanged fails unless before and after describe the same tree.
func assertTreeUnchanged(t *testing.T, root, label string, before, after map[string]treeNode) {
	t.Helper()
	var diffs []string
	for rel, a := range after {
		b, ok := before[rel]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s: unexpected new entry", rel))
			continue
		}
		if a.mode != b.mode {
			diffs = append(diffs, fmt.Sprintf("%s: mode %v -> %v", rel, b.mode, a.mode))
		}
		if a.uid != b.uid || a.gid != b.gid {
			diffs = append(diffs, fmt.Sprintf("%s: owner %d:%d -> %d:%d", rel, b.uid, b.gid, a.uid, a.gid))
		}
		if !a.mtime.Equal(b.mtime) {
			diffs = append(diffs, fmt.Sprintf("%s: mtime %v -> %v", rel, b.mtime, a.mtime))
		}
		if a.size != b.size {
			diffs = append(diffs, fmt.Sprintf("%s: size %d -> %d", rel, b.size, a.size))
		}
		if a.link != b.link {
			diffs = append(diffs, fmt.Sprintf("%s: link %q -> %q", rel, b.link, a.link))
		}
		if a.sum != b.sum {
			diffs = append(diffs, fmt.Sprintf("%s: content changed", rel))
		}
	}
	for rel := range before {
		if _, ok := after[rel]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s: entry removed", rel))
		}
	}
	if len(diffs) > 0 {
		t.Fatalf("%s changed:\n%s", label, strings.Join(diffs, "\n"))
	}
}

// makeRichVolume builds a volume exercising modes, times, symlinks and
// hardlinks, returning the snapshot taken right before the operation under
// test is applied.
func makeRichVolume(t *testing.T, s *Store, name string) map[string]treeNode {
	t.Helper()
	if err := s.Create(name, nil); err != nil {
		t.Fatal(err)
	}
	vdir := s.VolumePath(name)
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
	write("config/db.ini", "[db]\nport=5432")
	if err := os.Chmod(filepath.Join(vdir, "config", "db.ini"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2021, 6, 1, 12, 30, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(vdir, "config", "db.ini"), fixed, fixed); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(vdir, "config"), fixed, fixed); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("app.js", filepath.Join(vdir, "assets", "current.js")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(vdir, "assets", "app.js"), filepath.Join(vdir, "assets", "app.js.hard")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(vdir, "empty-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	return snapshotTree(t, vdir)
}

// TestBackupNeverModifiesSource pins the core invariant: Backup must be
// strictly read-only with respect to the source volume, in every supported
// compression format.
func TestBackupNeverModifiesSource(t *testing.T) {
	for _, c := range []Compression{CompressionGzip, CompressionXz, CompressionZstd} {
		t.Run(string(c), func(t *testing.T) {
			s := newTestStore(t)
			before := makeRichVolume(t, s, "web")
			if _, err := s.BackupWithOptions("web", BackupOptions{Compression: c, Level: 0}); err != nil {
				t.Fatal(err)
			}
			assertTreeUnchanged(t, s.VolumePath("web"), "source volume after Backup("+string(c)+")", before, snapshotTree(t, s.VolumePath("web")))
		})
	}
}

// TestBackupInterruptionKeepsSourceAndExistingArchive verifies the failure
// boundary of the atomic archive writer. A failure after the source has been
// fully read must not alter the source or replace an already valid backup.
func TestBackupInterruptionKeepsSourceAndExistingArchive(t *testing.T) {
	s := newTestStore(t)
	before := makeRichVolume(t, s, "web")
	outDir := t.TempDir()
	final := filepath.Join(outDir, "web.tar.gz")
	if err := os.WriteFile(final, []byte("previous valid archive"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := writeArchive(final, BackupOptions{}, func(tw *tar.Writer) error {
		if err := addTreeTo(tw, s.VolumePath("web"), ""); err != nil {
			return err
		}
		return fmt.Errorf("simulated backup interruption")
	})
	if err == nil {
		t.Fatal("interrupted backup unexpectedly succeeded")
	}

	assertTreeUnchanged(t, s.VolumePath("web"), "source volume after interrupted backup", before, snapshotTree(t, s.VolumePath("web")))
	got, err := os.ReadFile(final)
	if err != nil || string(got) != "previous valid archive" {
		t.Fatalf("existing archive was replaced or damaged: %q err=%v", got, err)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(final) {
		t.Fatalf("temporary archive was left behind after interruption: %v", entries)
	}
}

// TestBackupBundleNeverModifiesSource: the multi-volume path must be equally
// read-only for every volume it packs.
func TestBackupBundleNeverModifiesSource(t *testing.T) {
	s := newTestStore(t)
	beforeWeb := makeRichVolume(t, s, "web")
	beforeDb := makeRichVolume(t, s, "db")
	if _, err := s.BackupBundle([]string{"web", "db"}, filepath.Join(t.TempDir(), "compose"), "app"); err != nil {
		t.Fatal(err)
	}
	assertTreeUnchanged(t, s.VolumePath("web"), "source volume web after BackupBundle", beforeWeb, snapshotTree(t, s.VolumePath("web")))
	assertTreeUnchanged(t, s.VolumePath("db"), "source volume db after BackupBundle", beforeDb, snapshotTree(t, s.VolumePath("db")))
}

// TestRestoreFailureNeverModifiesVolume: every rejected restore must leave
// the existing volume byte- and metadata-identical. Covers corrupt archives,
// zip-slip, symlink escape, symlink volume-root and malformed bundles.
func TestRestoreFailureNeverModifiesVolume(t *testing.T) {
	cases := map[string]func() string{
		"corrupt": func() string {
			p := filepath.Join(t.TempDir(), "bad.tar.gz")
			os.WriteFile(p, []byte("not gzip at all"), 0o644)
			return p
		},
		"zip-slip": func() string {
			return craftTar(t, []tarItem{{hdr: tar.Header{Name: "../../escape", Typeflag: tar.TypeReg}, data: []byte("x")}})
		},
		"symlink-escape": func() string {
			return craftTar(t, []tarItem{
				{hdr: tar.Header{Name: "x", Typeflag: tar.TypeSymlink, Linkname: t.TempDir()}},
				{hdr: tar.Header{Name: "x/pwn", Typeflag: tar.TypeReg}, data: []byte("bad")},
			})
		},
		"symlink-root": func() string {
			return craftTar(t, []tarItem{
				{hdr: tar.Header{Name: ".", Typeflag: tar.TypeSymlink, Linkname: t.TempDir()}},
			})
		},
		"empty-archive": func() string {
			// a valid archive with zero entries (e.g. a truncated/failed
			// backup that still gzips): must be rejected, never wipe
			return craftTar(t, nil)
		},
		"valid-gzip-bad-tar": func() string {
			// gzip stream wraps non-tar garbage: fails mid-extraction
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			gz.Write([]byte("definitely not a tar stream"))
			gz.Close()
			p := filepath.Join(t.TempDir(), "x.tar.gz")
			os.WriteFile(p, buf.Bytes(), 0o644)
			return p
		},
		"hardlink-escape": func() string {
			// symlink ancestor on the hardlink target path must be refused
			return craftTar(t, []tarItem{
				{hdr: tar.Header{Name: "x", Typeflag: tar.TypeSymlink, Linkname: t.TempDir()}},
				{hdr: tar.Header{Name: "x/out", Typeflag: tar.TypeLink, Linkname: "victim"}},
			})
		},
	}
	for label, make := range cases {
		t.Run(label, func(t *testing.T) {
			s := newTestStore(t)
			before := makeRichVolume(t, s, "web")
			if err := s.Restore("web", make()); err == nil {
				t.Fatalf("restore of %q archive unexpectedly succeeded", label)
			}
			assertTreeUnchanged(t, s.VolumePath("web"), "volume after rejected restore "+label, before, snapshotTree(t, s.VolumePath("web")))
		})
	}
}

// TestRestoreBundleFailureNeverModifiesVolumes: bundle-level rejects (bad
// paths, malformed entries, no matching volume) must leave every existing
// volume untouched.
func TestRestoreBundleFailureNeverModifiesVolumes(t *testing.T) {
	s := newTestStore(t)
	beforeWeb := makeRichVolume(t, s, "web")
	beforeDb := makeRichVolume(t, s, "db")

	// malformed entry (no "<vol>/" prefix)
	bad := craftTar(t, []tarItem{{hdr: tar.Header{Name: "no-slash", Typeflag: tar.TypeReg}, data: []byte("x")}})
	if err := s.RestoreBundle(bad, nil); err == nil {
		t.Fatal("malformed bundle accepted")
	}
	assertTreeUnchanged(t, s.VolumePath("web"), "web after malformed bundle", beforeWeb, snapshotTree(t, s.VolumePath("web")))
	assertTreeUnchanged(t, s.VolumePath("db"), "db after malformed bundle", beforeDb, snapshotTree(t, s.VolumePath("db")))

	// bundle without the requested volumes must fail, not silently succeed
	other := craftTar(t, []tarItem{
		{hdr: tar.Header{Name: "other/data.txt", Typeflag: tar.TypeReg}, data: []byte("x")},
	})
	if err := s.RestoreBundle(other, []string{"web"}); err == nil {
		t.Fatal("bundle without requested volume accepted")
	}
	assertTreeUnchanged(t, s.VolumePath("web"), "web after no-match bundle", beforeWeb, snapshotTree(t, s.VolumePath("web")))

	// bundle with one good volume (db) and one whose root is a symlink (web):
	// the whole bundle must be rejected BEFORE any live swap, so db (staged
	// first) must not have been replaced.
	hostile := craftTar(t, []tarItem{
		{hdr: tar.Header{Name: "db/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "db/keep.txt", Typeflag: tar.TypeReg}, data: []byte("db-from-archive")},
		{hdr: tar.Header{Name: "web/", Typeflag: tar.TypeSymlink, Linkname: t.TempDir(), Mode: 0o777}},
	})
	if err := s.RestoreBundle(hostile, nil); err == nil {
		t.Fatal("bundle with symlink volume root accepted")
	}
	assertTreeUnchanged(t, s.VolumePath("db"), "db after hostile bundle (partial swap?)", beforeDb, snapshotTree(t, s.VolumePath("db")))
	assertTreeUnchanged(t, s.VolumePath("web"), "web after hostile bundle (partial swap?)", beforeWeb, snapshotTree(t, s.VolumePath("web")))
}

// TestRestoreDoesNotChmodThroughSymlink: os.Chmod follows symlinks; a
// crafted archive must not be able to flip the mode of a file outside the
// volume through a symlink entry (regression).
func TestRestoreDoesNotChmodThroughSymlink(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := craftTar(t, []tarItem{
		{hdr: tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777}},
		{hdr: tar.Header{Name: "dummy", Typeflag: tar.TypeDir, Mode: 0o755}},
	})
	if err := s.Restore("web", p); err != nil {
		t.Fatalf("restore: %v", err)
	}
	fi, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("outside file mode modified through symlink chmod: %v", fi.Mode().Perm())
	}
}

// TestRestoreRejectsSymlinkVolumeRoot: an archive whose root entry "." is a
// symlink must be rejected and must never leave the volume root as a symlink
// (regression: the swap would redirect all later mounts outside the store).
func TestRestoreRejectsSymlinkVolumeRoot(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.VolumePath("web"), "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := craftTar(t, []tarItem{
		{hdr: tar.Header{Name: ".", Typeflag: tar.TypeSymlink, Linkname: t.TempDir(), Mode: 0o777}},
	})
	if err := s.Restore("web", p); err == nil {
		t.Fatal("symlink volume-root archive accepted")
	}
	fi, err := os.Lstat(s.VolumePath("web"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("volume root is a symlink after rejected restore")
	}
	if got, _ := os.ReadFile(filepath.Join(s.VolumePath("web"), "keep.txt")); string(got) != "keep" {
		t.Fatalf("volume content lost: %q", got)
	}
}

// TestBackupRestorePreservesHardlinkFidelity: hard-linked files must survive
// a backup/restore round-trip as the same inode in both the plain and the
// bundle layouts (regression: they used to be duplicated as full copies).
func TestBackupRestorePreservesHardlinkFidelity(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, s *Store, path string)
	}{
		{"plain", func(t *testing.T, s *Store, _ string) {
			p, err := s.Backup("app")
			if err != nil {
				t.Fatal(err)
			}
			s.Remove("app")
			if err := s.Restore("app", p); err != nil {
				t.Fatal(err)
			}
		}},
		{"bundle", func(t *testing.T, s *Store, bundleDir string) {
			p, err := s.BackupBundle([]string{"app"}, bundleDir, "proj")
			if err != nil {
				t.Fatal(err)
			}
			s.Remove("app")
			if err := s.RestoreBundle(p, nil); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			if err := s.Create("app", nil); err != nil {
				t.Fatal(err)
			}
			vdir := s.VolumePath("app")
			if err := os.WriteFile(filepath.Join(vdir, "data.txt"), []byte("hello"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(filepath.Join(vdir, "data.txt"), filepath.Join(vdir, "link.txt")); err != nil {
				t.Fatal(err)
			}
			tc.run(t, s, filepath.Join(t.TempDir(), "compose"))
			fi1, err := os.Stat(filepath.Join(vdir, "data.txt"))
			if err != nil {
				t.Fatal(err)
			}
			fi2, err := os.Stat(filepath.Join(vdir, "link.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(fi1, fi2) {
				t.Fatal("hardlink not preserved as same inode after restore")
			}
		})
	}
}
