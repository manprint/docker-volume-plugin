package dvol

// Fidelity suite: what comes out of a restore must be what went into the
// backup. The source-invariance suite proves dvol does not damage the source;
// this one proves the round trip does not lose anything either, across every
// entry kind, every compression and every transport (file, bundle, stream).

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// entryFingerprint is everything a restore is expected to reproduce. Access
// time is deliberately absent: it is archived and restored, but a verification
// pass that reads the tree would perturb it on filesystems without noatime.
type entryFingerprint struct {
	kind      string
	mode      os.FileMode
	uid, gid  int
	mtime     time.Time
	size      int64
	link      string
	sum       string
	linkGroup string // shared by hard-linked entries
	xattrs    string
}

func (f entryFingerprint) String() string {
	return fmt.Sprintf("kind=%s mode=%v owner=%d:%d mtime=%s size=%d link=%q sum=%s group=%s xattrs=%s",
		f.kind, f.mode, f.uid, f.gid, f.mtime.UTC().Format(time.RFC3339Nano), f.size, f.link, f.sum, f.linkGroup, f.xattrs)
}

func entryKind(mode os.FileMode) string {
	switch {
	case mode&os.ModeSymlink != 0:
		return "symlink"
	case mode.IsDir():
		return "dir"
	case mode&os.ModeNamedPipe != 0:
		return "fifo"
	case mode&os.ModeSocket != 0:
		return "socket"
	case mode&os.ModeDevice != 0:
		return "device"
	default:
		return "file"
	}
}

func readXattrsAt(t *testing.T, path string) string {
	t.Helper()
	size, err := unix.Llistxattr(path, nil)
	if err != nil || size <= 0 {
		return ""
	}
	buf := make([]byte, size)
	size, err = unix.Llistxattr(path, buf)
	if err != nil {
		return ""
	}
	var out []string
	for _, name := range strings.Split(string(buf[:size]), "\x00") {
		if name == "" || !strings.HasPrefix(name, "user.") {
			continue
		}
		value := make([]byte, 512)
		n, err := unix.Lgetxattr(path, name, value)
		if err != nil {
			continue
		}
		out = append(out, name+"="+hex.EncodeToString(value[:n]))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// fingerprintTree walks a directory with the same read-only discipline the
// backup uses and records every attribute the round trip must preserve.
func fingerprintTree(t *testing.T, root string) map[string]entryFingerprint {
	t.Helper()
	out := map[string]entryFingerprint{}
	groups := map[[2]uint64]string{}
	record := func(rel, path string, info os.FileInfo, content io.Reader, link string) {
		f := entryFingerprint{
			kind:   entryKind(info.Mode()),
			mode:   info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky),
			mtime:  info.ModTime(),
			size:   info.Size(),
			link:   link,
			xattrs: readXattrsAt(t, path),
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			f.uid, f.gid = int(st.Uid), int(st.Gid)
			if info.Mode().IsRegular() && st.Nlink > 1 {
				key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
				if first, ok := groups[key]; ok {
					f.linkGroup = first
				} else {
					groups[key] = rel
					f.linkGroup = rel
				}
			}
		}
		if content != nil && info.Mode().IsRegular() {
			h := sha256.New()
			n, err := io.Copy(h, content)
			if err != nil {
				t.Fatalf("hash %s: %v", rel, err)
			}
			f.sum = hex.EncodeToString(h.Sum(nil))
			f.size = n
		}
		if info.IsDir() {
			// A directory's size is allocator noise, not content.
			f.size = 0
		}
		out[rel] = f
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	record(".", root, rootInfo, nil, "")
	if err := walkReadOnly(root, readOptions{}, func(entry readOnlyTreeEntry) error {
		record(entry.rel, entry.path, entry.info, entry.file, entry.link)
		return nil
	}); err != nil {
		t.Fatalf("fingerprint %s: %v", root, err)
	}
	return out
}

func assertSameTree(t *testing.T, label string, want, got map[string]entryFingerprint) {
	t.Helper()
	var diffs []string
	for rel, w := range want {
		g, ok := got[rel]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s: missing after restore", rel))
			continue
		}
		if w != g {
			diffs = append(diffs, fmt.Sprintf("%s:\n  want %s\n  got  %s", rel, w, g))
		}
	}
	for rel := range got {
		if _, ok := want[rel]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s: unexpected entry after restore", rel))
		}
	}
	if len(diffs) > 0 {
		t.Fatalf("%s: restored tree differs from the source:\n%s", label, strings.Join(diffs, "\n"))
	}
}

// makeKitchenSinkVolume builds a tree covering every entry kind and metadata
// dimension dvol claims to preserve. It returns the names of the features it
// could actually create on this filesystem.
func makeKitchenSinkVolume(t *testing.T, dir string) {
	t.Helper()
	mustWrite := func(rel, content string, mode os.FileMode) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}

	mustWrite("index.html", "<html>hello</html>", 0o644)
	mustWrite("config/db.ini", "[db]\nport=5432\n", 0o600)
	mustWrite("config/read-only.conf", "locked", 0o400)
	mustWrite("assets/app.js", "var x = 1;", 0o644)
	mustWrite("weird names/file with spaces.txt", "spaces", 0o644)
	mustWrite("weird names/ünïcødé-ファイル.txt", "unicode", 0o644)
	mustWrite("deep/a/b/c/d/e/f/leaf.txt", "deep", 0o644)

	// empty file and empty directory
	mustWrite("empty.txt", "", 0o644)
	if err := os.MkdirAll(filepath.Join(dir, "empty-dir"), 0o700); err != nil {
		t.Fatal(err)
	}

	// a larger payload that crosses the sparse/copy chunk boundary
	big := make([]byte, 3*sparseChunk+1234)
	for i := range big {
		big[i] = byte(i * 7)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	// sparse file: hole in the middle
	sparse, err := os.Create(filepath.Join(dir, "sparse.img"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sparse.Write([]byte("head")); err != nil {
		t.Fatal(err)
	}
	if err := sparse.Truncate(8 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := sparse.WriteAt([]byte("tail"), (8<<20)-4); err != nil {
		t.Fatal(err)
	}
	if err := sparse.Close(); err != nil {
		t.Fatal(err)
	}

	// symlinks: relative, absolute and dangling
	if err := os.Symlink("app.js", filepath.Join(dir, "assets", "current.js")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "absolute.link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nowhere", filepath.Join(dir, "dangling.link")); err != nil {
		t.Fatal(err)
	}
	// hard links: two extra names for one inode
	if err := os.Link(filepath.Join(dir, "assets", "app.js"), filepath.Join(dir, "assets", "app.js.hard")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "assets", "app.js"), filepath.Join(dir, "hard-elsewhere.js")); err != nil {
		t.Fatal(err)
	}
	// FIFO
	if err := unix.Mkfifo(filepath.Join(dir, "run.fifo"), 0o640); err != nil {
		t.Logf("FIFO unavailable on this filesystem: %v", err)
	}
	// extended attribute
	if err := unix.Lsetxattr(filepath.Join(dir, "index.html"), "user.dvol_fidelity", []byte("\x01\x02binary"), 0); err != nil {
		t.Logf("extended attributes unavailable on this filesystem: %v", err)
	}
	// fixed timestamps with sub-second precision
	fixed := time.Date(2021, 6, 1, 12, 30, 0, 123456789, time.UTC)
	for _, rel := range []string{"index.html", "config/db.ini", "config", "deep/a/b", "empty-dir"} {
		p := filepath.Join(dir, rel)
		if err := os.Chtimes(p, fixed, fixed); err != nil {
			t.Fatal(err)
		}
	}
	// restrictive directory mode, applied last so the writes above succeed
	if err := os.Chmod(filepath.Join(dir, "config"), 0o750); err != nil {
		t.Fatal(err)
	}
}

// TestRoundTripFidelityAllCompressions is the headline consistency check.
func TestRoundTripFidelityAllCompressions(t *testing.T) {
	for _, c := range []Compression{CompressionGzip, CompressionXz, CompressionZstd, CompressionLz4} {
		t.Run(string(c), func(t *testing.T) {
			s := newTestStore(t)
			if err := s.Create("kitchen", nil); err != nil {
				t.Fatal(err)
			}
			source := s.VolumePath("kitchen")
			makeKitchenSinkVolume(t, source)
			want := fingerprintTree(t, source)

			archive, err := s.BackupWithOptions("kitchen", BackupOptions{Compression: c, Threads: 1})
			if err != nil {
				t.Fatalf("backup: %v", err)
			}
			// The source must be untouched by the backup.
			assertSameTree(t, "source after backup", want, fingerprintTree(t, source))

			if _, err := s.VerifyArchive(archive); err != nil {
				t.Fatalf("verify: %v", err)
			}
			target := t.TempDir()
			writeVolumeFile(t, target, "stale.txt", "stale")
			if err := s.RestoreSource("kitchen", target, archive); err != nil {
				t.Fatalf("restore: %v", err)
			}
			assertSameTree(t, "restored volume", want, fingerprintTree(t, target))
		})
	}
}

// TestRoundTripFidelityStream covers the stdout/stdin transport.
func TestRoundTripFidelityStream(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("kitchen", nil); err != nil {
		t.Fatal(err)
	}
	source := s.VolumePath("kitchen")
	makeKitchenSinkVolume(t, source)
	want := fingerprintTree(t, source)

	var stream bytes.Buffer
	if err := s.BackupSourceToWriter("kitchen", source, BackupOptions{Compression: CompressionZstd, Threads: 1}, &stream); err != nil {
		t.Fatalf("stream backup: %v", err)
	}
	assertSameTree(t, "source after stream backup", want, fingerprintTree(t, source))

	target := t.TempDir()
	if err := s.RestoreSourceReader("kitchen", target, bytes.NewReader(stream.Bytes())); err != nil {
		t.Fatalf("stream restore: %v", err)
	}
	assertSameTree(t, "restored volume from stream", want, fingerprintTree(t, target))
}

// TestRoundTripFidelityBundle covers the multi-volume layout, including the
// stream form used by compose-backup --stdout.
func TestRoundTripFidelityBundle(t *testing.T) {
	s := newTestStore(t)
	sources := map[string]string{}
	want := map[string]map[string]entryFingerprint{}
	for _, name := range []string{"app", "db"} {
		dir := t.TempDir()
		makeKitchenSinkVolume(t, dir)
		writeVolumeFile(t, dir, "identity.txt", name)
		sources[name] = dir
		want[name] = fingerprintTree(t, dir)
	}

	t.Run("file", func(t *testing.T) {
		archive, err := s.BackupBundleSourcesWithOptions([]string{"app", "db"}, sources, t.TempDir(), "proj", BackupOptions{Threads: 1})
		if err != nil {
			t.Fatal(err)
		}
		targets := map[string]string{"app": t.TempDir(), "db": t.TempDir()}
		if err := s.RestoreBundleSources(archive, targets); err != nil {
			t.Fatal(err)
		}
		for name, dir := range targets {
			assertSameTree(t, "restored bundle volume "+name, want[name], fingerprintTree(t, dir))
		}
	})

	t.Run("stream", func(t *testing.T) {
		var stream bytes.Buffer
		if err := s.BackupBundleSourcesToWriter([]string{"app", "db"}, sources, BackupOptions{Compression: CompressionLz4, Threads: 1}, &stream); err != nil {
			t.Fatal(err)
		}
		targets := map[string]string{"app": t.TempDir(), "db": t.TempDir()}
		if err := s.RestoreBundleSourcesReader(bytes.NewReader(stream.Bytes()), targets); err != nil {
			t.Fatal(err)
		}
		for name, dir := range targets {
			assertSameTree(t, "restored bundle stream volume "+name, want[name], fingerprintTree(t, dir))
		}
	})
}

// TestArchiveContainsExactlyTheSourceTree checks the archive itself, not just
// the round trip: every source entry appears exactly once, with the right type
// and size, and nothing else is in there.
func TestArchiveContainsExactlyTheSourceTree(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("kitchen", nil); err != nil {
		t.Fatal(err)
	}
	source := s.VolumePath("kitchen")
	makeKitchenSinkVolume(t, source)
	archive, err := s.Backup("kitchen")
	if err != nil {
		t.Fatal(err)
	}

	tr, r, f, err := openArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	defer r.Close()
	inArchive := map[string]*tar.Header{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		name = strings.TrimPrefix(name, "./")
		if name == "" {
			name = "."
		}
		if _, dup := inArchive[name]; dup {
			t.Fatalf("entry %q appears twice in the archive", name)
		}
		copied := *hdr
		inArchive[name] = &copied
		if _, err := io.Copy(io.Discard, tr); err != nil {
			t.Fatal(err)
		}
	}

	onDisk := fingerprintTree(t, source)
	for rel, want := range onDisk {
		hdr, ok := inArchive[rel]
		if !ok {
			if want.kind == "socket" {
				continue // sockets are intentionally skipped
			}
			t.Fatalf("source entry %q is missing from the archive", rel)
		}
		switch want.kind {
		case "dir":
			if hdr.Typeflag != tar.TypeDir {
				t.Fatalf("%q archived as %q, expected a directory", rel, string(rune(hdr.Typeflag)))
			}
		case "symlink":
			if hdr.Typeflag != tar.TypeSymlink || hdr.Linkname != want.link {
				t.Fatalf("%q archived as %q -> %q", rel, string(rune(hdr.Typeflag)), hdr.Linkname)
			}
		case "fifo":
			if hdr.Typeflag != tar.TypeFifo {
				t.Fatalf("%q archived as %q, expected a FIFO", rel, string(rune(hdr.Typeflag)))
			}
		case "file":
			if hdr.Typeflag == tar.TypeLink {
				continue // second name of a hard-linked inode
			}
			if hdr.Typeflag != tar.TypeReg {
				t.Fatalf("%q archived as %q, expected a regular file", rel, string(rune(hdr.Typeflag)))
			}
			if hdr.Size != want.size {
				t.Fatalf("%q archived with size %d, on disk %d", rel, hdr.Size, want.size)
			}
		}
	}
	for rel := range inArchive {
		if _, ok := onDisk[rel]; !ok {
			t.Fatalf("archive contains %q which is not in the source", rel)
		}
	}
}

// TestHardlinkGroupsSurviveRestore is stricter than "same inode": it checks the
// grouping is identical, so two independent files never collapse into one.
func TestHardlinkGroupsSurviveRestore(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("links", nil); err != nil {
		t.Fatal(err)
	}
	dir := s.VolumePath("links")
	writeVolumeFile(t, dir, "a.txt", "group-one")
	writeVolumeFile(t, dir, "b.txt", "group-two")
	for _, link := range []string{"a2.txt", "sub/a3.txt"} {
		p := filepath.Join(dir, link)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(dir, "a.txt"), p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(dir, "b.txt"), filepath.Join(dir, "b2.txt")); err != nil {
		t.Fatal(err)
	}
	want := fingerprintTree(t, dir)

	archive, err := s.Backup("links")
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := s.RestoreSource("links", target, archive); err != nil {
		t.Fatal(err)
	}
	assertSameTree(t, "hard link groups", want, fingerprintTree(t, target))

	// And the groups must really be distinct inodes.
	a, err := os.Stat(filepath.Join(target, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(target, "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(a, b) {
		t.Fatal("independent files were collapsed into one inode")
	}
}

// TestRestoreIsIdempotent: restoring the same archive twice must converge.
func TestRestoreIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("kitchen", nil); err != nil {
		t.Fatal(err)
	}
	source := s.VolumePath("kitchen")
	makeKitchenSinkVolume(t, source)
	want := fingerprintTree(t, source)
	archive, err := s.Backup("kitchen")
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := s.RestoreSource("kitchen", target, archive); err != nil {
			t.Fatalf("restore %d: %v", i, err)
		}
		assertSameTree(t, fmt.Sprintf("restore %d", i), want, fingerprintTree(t, target))
	}
}

// TestBackupOfRestoredVolumeIsEquivalent closes the loop: archiving a restored
// volume must yield the same tree again.
func TestBackupOfRestoredVolumeIsEquivalent(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("kitchen", nil); err != nil {
		t.Fatal(err)
	}
	source := s.VolumePath("kitchen")
	makeKitchenSinkVolume(t, source)
	want := fingerprintTree(t, source)

	first, err := s.BackupWithOptions("kitchen", BackupOptions{ArchiveName: "gen1"})
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := s.RestoreSource("kitchen", target, first); err != nil {
		t.Fatal(err)
	}
	second, err := s.BackupSourceWithOptions("kitchen", target, BackupOptions{ArchiveName: "gen2"})
	if err != nil {
		t.Fatal(err)
	}
	final := t.TempDir()
	if err := s.RestoreSource("kitchen", final, second); err != nil {
		t.Fatal(err)
	}
	assertSameTree(t, "second generation", want, fingerprintTree(t, final))
}

// TestEmptyVolumeRoundTrip: an empty volume is a legal backup, and restoring it
// must empty the target rather than fail.
func TestEmptyVolumeRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("empty", nil); err != nil {
		t.Fatal(err)
	}
	archive, err := s.Backup("empty")
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	writeVolumeFile(t, target, "stale.txt", "stale")
	if err := s.RestoreSource("empty", target, archive); err != nil {
		t.Fatalf("restoring an empty volume: %v", err)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("target not emptied: %v", entries)
	}
}
