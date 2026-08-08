package dvol

// One test per defect found in the 2026-08-08 review (see REVIEW.md). Each
// test states the failure it locks out, so a regression reports the original
// symptom rather than an anonymous assertion failure.

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// craftBundleTar builds a bundle-shaped archive with a valid manifest.
func craftBundleTar(t *testing.T, volumes []string, items []tarItem) string {
	t.Helper()
	manifest, err := json.Marshal(bundleManifest{Format: bundleFormat, Created: time.Now().UTC(), Volumes: volumes})
	if err != nil {
		t.Fatal(err)
	}
	all := []tarItem{{
		hdr:  tar.Header{Name: bundleManifestName, Typeflag: tar.TypeReg, Mode: 0o644},
		data: manifest,
	}}
	return craftTar(t, append(all, items...))
}

func writeVolumeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// B1: a bundle restored through the single-volume path replaced the volume with
// the bundle layout (manifest.json plus one directory per volume) and reported
// success.
// ---------------------------------------------------------------------------

func TestRestoreRejectsBundleArchiveOnSingleVolume(t *testing.T) {
	s := newTestStore(t)
	for _, n := range []string{"app", "db"} {
		if err := s.Create(n, nil); err != nil {
			t.Fatal(err)
		}
		writeVolumeFile(t, s.VolumePath(n), "data.txt", "c-"+n)
	}
	bundle, err := s.BackupBundle([]string{"app", "db"}, t.TempDir(), "proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	writeVolumeFile(t, s.VolumePath("web"), "real.txt", "live data")
	before := snapshotTree(t, s.VolumePath("web"))

	err = s.Restore("web", bundle)
	if !errors.Is(err, ErrBundleArchive) {
		t.Fatalf("restoring a bundle as a single volume must be refused, got %v", err)
	}
	assertTreeUnchanged(t, s.VolumePath("web"), "volume after refused bundle restore", before, snapshotTree(t, s.VolumePath("web")))
}

func TestRestoreStreamRejectsBundleArchiveOnSingleVolume(t *testing.T) {
	s := newTestStore(t)
	sources := map[string]string{}
	for _, n := range []string{"app", "db"} {
		dir := t.TempDir()
		writeVolumeFile(t, dir, "data.txt", "c-"+n)
		sources[n] = dir
	}
	var stream bytes.Buffer
	if err := s.BackupBundleSourcesToWriter([]string{"app", "db"}, sources, BackupOptions{Threads: 1}, &stream); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	writeVolumeFile(t, target, "live.txt", "live")
	before := snapshotTree(t, target)

	err := s.RestoreSourceReader("web", target, bytes.NewReader(stream.Bytes()))
	if !errors.Is(err, ErrBundleArchive) {
		t.Fatalf("restoring a bundle stream as a single volume must be refused, got %v", err)
	}
	assertTreeUnchanged(t, target, "target after refused bundle stream", before, snapshotTree(t, target))
}

func TestRestoreBundleRejectsSingleVolumeArchive(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	writeVolumeFile(t, s.VolumePath("web"), "data.txt", "single")
	archive, err := s.Backup("web")
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	writeVolumeFile(t, target, "live.txt", "live")
	err = s.RestoreBundleSources(archive, map[string]string{"web": target})
	if !errors.Is(err, ErrNotBundleArchive) {
		t.Fatalf("restoring a single-volume archive as a bundle must be refused, got %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "live.txt")); err != nil || string(got) != "live" {
		t.Fatalf("target was modified by the refused restore: %q err=%v", got, err)
	}
}

// A volume may legitimately hold its own manifest.json; only dvol's own bundle
// format is refused, and the file must still round-trip byte for byte.
func TestVolumeManifestJSONIsNotMistakenForABundle(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	payload := `{"name":"my-app","format":"webmanifest","icons":[]}`
	writeVolumeFile(t, s.VolumePath("web"), "manifest.json", payload)
	writeVolumeFile(t, s.VolumePath("web"), "index.html", "hello")
	archive, err := s.Backup("web")
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := s.RestoreSource("web", target, archive); err != nil {
		t.Fatalf("volume with its own manifest.json must restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "manifest.json"))
	if err != nil || string(got) != payload {
		t.Fatalf("manifest.json content lost: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "index.html")); err != nil || string(got) != "hello" {
		t.Fatalf("sibling entry lost: %q err=%v", got, err)
	}
}

// ---------------------------------------------------------------------------
// B2: a file whose size changed while it was read aborted the whole backup
// ("archive/tar: write too long" / "missed writing N bytes").
// ---------------------------------------------------------------------------

func TestCopyFileEntryPadsShrinkingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Truncate(path, 1024); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "wal.log", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4096}); err != nil {
		t.Fatal(err)
	}
	changed, err := copyFileEntry(tw, f, 4096, nil)
	if err != nil {
		t.Fatalf("shrinking file must not fail the backup: %v", err)
	}
	if !changed {
		t.Fatal("shrinking file was not reported as changed")
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("archive not consistent after padding: %v", err)
	}
	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(body)) != hdr.Size || hdr.Size != 4096 {
		t.Fatalf("payload %d bytes for a %d byte header", len(body), hdr.Size)
	}
	if !bytes.Equal(body[:1024], bytes.Repeat([]byte("a"), 1024)) || !isAllZero(body[1024:]) {
		t.Fatal("padding did not preserve the bytes that were still readable")
	}
}

func TestCopyFileEntryTruncatesGrowingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("b"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Header claims the smaller size the file had when it was opened.
	if err := tw.WriteHeader(&tar.Header{Name: "wal.log", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1024}); err != nil {
		t.Fatal(err)
	}
	changed, err := copyFileEntry(tw, f, 1024, nil)
	if err != nil {
		t.Fatalf("growing file must not fail the backup: %v", err)
	}
	if !changed {
		t.Fatal("growing file was not reported as changed")
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("archive not consistent after truncation: %v", err)
	}
}

// TestBackupSurvivesConcurrentWriter is the integration form: a volume being
// written while it is archived must still produce a valid, restorable archive.
func TestBackupSurvivesConcurrentWriter(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("live", nil); err != nil {
		t.Fatal(err)
	}
	vdir := s.VolumePath("live")
	writeVolumeFile(t, vdir, "stable.txt", "stable")
	growing := filepath.Join(vdir, "wal.log")
	if err := os.WriteFile(growing, bytes.Repeat([]byte("a"), 4<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	shrinking := filepath.Join(vdir, "rotate.log")
	if err := os.WriteFile(shrinking, bytes.Repeat([]byte("c"), 8<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		f, err := os.OpenFile(growing, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		chunk := bytes.Repeat([]byte("b"), 1<<16)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = f.Write(chunk)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = os.Truncate(shrinking, 1<<20)
				time.Sleep(time.Millisecond)
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)

	archive, err := s.BackupWithOptions("live", BackupOptions{Compression: CompressionLz4, Threads: 1})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("backup of a volume under concurrent writes failed: %v", err)
	}

	// The archive must be internally consistent and fully restorable.
	report, err := s.VerifyArchive(archive)
	if err != nil {
		t.Fatalf("archive produced under concurrent writes does not verify: %v", err)
	}
	if report.Entries == 0 {
		t.Fatal("archive is empty")
	}
	target := t.TempDir()
	if err := s.RestoreSource("live", target, archive); err != nil {
		t.Fatalf("archive produced under concurrent writes does not restore: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "stable.txt")); err != nil || string(got) != "stable" {
		t.Fatalf("untouched file was not preserved: %q err=%v", got, err)
	}
}

// ---------------------------------------------------------------------------
// B3: an archive containing a FIFO could never be restored (the metadata pass
// chowned a path the extractor had skipped).
// ---------------------------------------------------------------------------

func TestFifoRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("fifo", nil); err != nil {
		t.Fatal(err)
	}
	vdir := s.VolumePath("fifo")
	writeVolumeFile(t, vdir, "keep.txt", "keep")
	if err := unix.Mkfifo(filepath.Join(vdir, "run.fifo"), 0o640); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	archive, err := s.Backup("fifo")
	if err != nil {
		t.Fatalf("backup of a volume containing a FIFO failed: %v", err)
	}
	target := t.TempDir()
	if err := s.RestoreSource("fifo", target, archive); err != nil {
		t.Fatalf("restore of an archive containing a FIFO failed: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(target, "run.fifo"))
	if err != nil {
		t.Fatalf("FIFO was not restored: %v", err)
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("restored entry is not a FIFO: %v", fi.Mode())
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("FIFO permissions not preserved: %v", fi.Mode().Perm())
	}
	if got, err := os.ReadFile(filepath.Join(target, "keep.txt")); err != nil || string(got) != "keep" {
		t.Fatalf("sibling data lost: %q err=%v", got, err)
	}
}

func TestRestoreSkipsDeviceNodeWithoutPrivileges(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("dev", nil); err != nil {
		t.Fatal(err)
	}
	archive := craftTar(t, []tarItem{
		{hdr: tar.Header{Name: ".", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "keep.txt", Typeflag: tar.TypeReg}, data: []byte("keep")},
		{hdr: tar.Header{Name: "null", Typeflag: tar.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3}},
	})
	if err := s.Restore("dev", archive); err != nil {
		t.Fatalf("an archive with a device node must restore the rest: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(s.VolumePath("dev"), "keep.txt")); err != nil || string(got) != "keep" {
		t.Fatalf("regular entry lost: %q err=%v", got, err)
	}
}

// ---------------------------------------------------------------------------
// B4: a unix socket in the volume aborted the backup entirely.
// ---------------------------------------------------------------------------

func bindSocket(t *testing.T, path string) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("cannot create a unix socket: %v", err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		t.Skipf("cannot bind a unix socket: %v", err)
	}
}

func TestBackupSkipsUnixSocket(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("sock", nil); err != nil {
		t.Fatal(err)
	}
	vdir := s.VolumePath("sock")
	writeVolumeFile(t, vdir, "data.txt", "payload")
	bindSocket(t, filepath.Join(vdir, "mysql.sock"))

	archive, err := s.Backup("sock")
	if err != nil {
		t.Fatalf("a volume containing a socket must still be backed up: %v", err)
	}
	target := t.TempDir()
	if err := s.RestoreSource("sock", target, archive); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "data.txt")); err != nil || string(got) != "payload" {
		t.Fatalf("data next to the socket was lost: %q err=%v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(target, "mysql.sock")); !os.IsNotExist(err) {
		t.Fatalf("socket should not be recreated: %v", err)
	}
}

func TestStrictEntriesRejectsUnixSocket(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(Config{Root: root, StrictEntries: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create("sock", nil); err != nil {
		t.Fatal(err)
	}
	bindSocket(t, filepath.Join(s.VolumePath("sock"), "app.sock"))
	if _, err := s.Backup("sock"); err == nil {
		t.Fatal("StrictEntries must turn an unsupported entry into an error")
	}
}

// ---------------------------------------------------------------------------
// H4: one damaged index entry blocked listing and every new flat backup.
// ---------------------------------------------------------------------------

func TestFlatIndexSurvivesDamagedEntry(t *testing.T) {
	s, destination := newFlatStore(t)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	writeVolumeFile(t, s.VolumePath("web"), "data.txt", "first")
	if _, err := s.BackupWithOptions("web", BackupOptions{ArchiveName: "good", SameDestination: true}); err != nil {
		t.Fatal(err)
	}
	poisoned := `{"version":1,"entries":[
		{"volume":"web","name":"good.tar.gz","created_at":"2020-01-01T00:00:00Z"},
		{"volume":"../evil","name":"x.tar.gz","created_at":"2020-01-01T00:00:00Z"},
		{"volume":"web","name":"not-an-archive","created_at":"2020-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(destination, flatBackupIndexName), []byte(poisoned), 0o644); err != nil {
		t.Fatal(err)
	}

	infos, err := s.ListBackups("web")
	if err != nil {
		t.Fatalf("a damaged entry must not break listing: %v", err)
	}
	if len(infos) != 1 || infos[0].Name != "good.tar.gz" {
		t.Fatalf("valid entry was lost: %+v", infos)
	}
	if _, err := s.BackupWithOptions("web", BackupOptions{ArchiveName: "second", SameDestination: true}); err != nil {
		t.Fatalf("a damaged entry must not block new backups: %v", err)
	}
	infos, err = s.ListBackups("web")
	if err != nil || len(infos) != 2 {
		t.Fatalf("expected two archives, got %+v err=%v", infos, err)
	}
	raw, err := os.ReadFile(filepath.Join(destination, flatBackupIndexName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "../evil") || strings.Contains(string(raw), "not-an-archive") {
		t.Fatalf("damaged entries were not dropped: %s", raw)
	}
}

func TestFlatIndexRejectsUnknownVersion(t *testing.T) {
	s, destination := newFlatStore(t)
	if err := os.WriteFile(filepath.Join(destination, flatBackupIndexName),
		[]byte(`{"version":99,"entries":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListBackups("web"); err == nil {
		t.Fatal("an index written by a newer version must not be silently rewritten")
	}
}

// ---------------------------------------------------------------------------
// M1: O_NOATIME is refused for files the caller does not own. The strict
// default must say so clearly and --allow-atime must provide a way through.
// ---------------------------------------------------------------------------

func TestAtimeStrictnessAndOptIn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can always use O_NOATIME")
	}
	const foreign = "/etc"
	if _, err := os.Stat(foreign); err != nil {
		t.Skip("no foreign-owned directory available")
	}
	if fi, err := os.Stat(foreign); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Geteuid() {
			t.Skip("test directory is owned by the current user")
		}
	}
	_, err := openReadOnlyFD(unix.AT_FDCWD, foreign, unix.O_DIRECTORY, readOptions{})
	if !errors.Is(err, ErrAtimeUnavailable) {
		t.Fatalf("strict mode must report ErrAtimeUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "--allow-atime") {
		t.Fatalf("the error must name the opt-in flag: %v", err)
	}
	fd, err := openReadOnlyFD(unix.AT_FDCWD, foreign, unix.O_DIRECTORY, readOptions{allowAtime: true})
	if err != nil {
		t.Fatalf("--allow-atime must make the source readable: %v", err)
	}
	unix.Close(fd)
}

// ---------------------------------------------------------------------------
// M6: the bundle prefix was interpolated into a path without validation.
// ---------------------------------------------------------------------------

func TestBundlePrefixCannotEscapeDestination(t *testing.T) {
	s := newTestStore(t)
	source := t.TempDir()
	writeVolumeFile(t, source, "data.txt", "x")
	out := filepath.Join(t.TempDir(), "bundles")
	for _, prefix := range []string{"../../escaped", "..", ".", "a/b", ""} {
		if _, err := s.BackupBundleSourcesWithOptions([]string{"web"},
			map[string]string{"web": source}, out, prefix, BackupOptions{}); err == nil {
			t.Fatalf("bundle prefix %q was accepted", prefix)
		}
	}
}

// ---------------------------------------------------------------------------
// M7: extended attributes (and therefore POSIX ACLs and file capabilities)
// were silently dropped.
// ---------------------------------------------------------------------------

func TestXattrsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("attr", nil); err != nil {
		t.Fatal(err)
	}
	vdir := s.VolumePath("attr")
	file := filepath.Join(vdir, "data.txt")
	writeVolumeFile(t, vdir, "data.txt", "payload")
	if err := unix.Lsetxattr(file, "user.dvol_test", []byte("value-\x00\xff"), 0); err != nil {
		t.Skipf("extended attributes unavailable here: %v", err)
	}
	if err := unix.Lsetxattr(vdir, "user.dvol_dir", []byte("dir-value"), 0); err != nil {
		t.Skipf("directory extended attributes unavailable here: %v", err)
	}

	archive, err := s.Backup("attr")
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := s.RestoreSource("attr", target, archive); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := unix.Lgetxattr(filepath.Join(target, "data.txt"), "user.dvol_test", buf)
	if err != nil {
		t.Fatalf("file xattr was not restored: %v", err)
	}
	if string(buf[:n]) != "value-\x00\xff" {
		t.Fatalf("file xattr value differs: %q", buf[:n])
	}
	n, err = unix.Lgetxattr(target, "user.dvol_dir", buf)
	if err != nil {
		t.Fatalf("directory xattr was not restored: %v", err)
	}
	if string(buf[:n]) != "dir-value" {
		t.Fatalf("directory xattr value differs: %q", buf[:n])
	}
}

// ---------------------------------------------------------------------------
// M10: sparse files were materialised block for block on restore.
// ---------------------------------------------------------------------------

func TestSparseFileStaysSparseOnRestore(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("sparse", nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.VolumePath("sparse"), "disk.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const size = 32 << 20
	if err := f.Truncate(size); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("head"), 0); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("tail"), size-4); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	archive, err := s.Backup("sparse")
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := s.RestoreSource("sparse", target, archive); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(target, "disk.img")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(restored)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, got) {
		t.Fatal("restored sparse file differs from the source byte for byte")
	}
	allocated := allocatedBytes(t, restored)
	sourceAllocated := allocatedBytes(t, path)
	if allocated >= size {
		t.Fatalf("restored file is not sparse: %d bytes allocated for a %d byte file", allocated, size)
	}
	// The hole must be reproduced at filesystem-block granularity, not merely
	// "smaller than the apparent size".
	if allocated > sourceAllocated+64*1024 {
		t.Fatalf("restored file allocates %d bytes where the source allocates %d", allocated, sourceAllocated)
	}
}

func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}

// ---------------------------------------------------------------------------
// M3: a name collision reported a raw link(2) error.
// ---------------------------------------------------------------------------

func TestArchiveCollisionErrorNamesTheFlag(t *testing.T) {
	s := newTestStore(t)
	source := t.TempDir()
	writeVolumeFile(t, source, "data.txt", "x")
	out := t.TempDir()
	opts := BackupOptions{ArchiveName: "fixed"}
	if _, err := s.BackupBundleSourcesWithOptions([]string{"web"}, map[string]string{"web": source}, out, "proj", opts); err != nil {
		t.Fatal(err)
	}
	_, err := s.BackupBundleSourcesWithOptions([]string{"web"}, map[string]string{"web": source}, out, "proj", opts)
	if !errors.Is(err, ErrArchiveExists) {
		t.Fatalf("expected ErrArchiveExists, got %v", err)
	}
	if !strings.Contains(err.Error(), "--overwrite") {
		t.Fatalf("collision error must suggest --overwrite: %v", err)
	}
	opts.Overwrite = true
	if _, err := s.BackupBundleSourcesWithOptions([]string{"web"}, map[string]string{"web": source}, out, "proj", opts); err != nil {
		t.Fatalf("--overwrite must replace the bundle: %v", err)
	}
}

// ---------------------------------------------------------------------------
// H3: bundle restores resolved (and created) their targets before checking the
// archive actually carried the requested volumes.
// ---------------------------------------------------------------------------

func TestBundleRestoreResolvesTargetsOnlyAfterManifestCheck(t *testing.T) {
	s := newTestStore(t)
	source := t.TempDir()
	writeVolumeFile(t, source, "data.txt", "x")
	archive, err := s.BackupBundleSourcesWithOptions([]string{"app"},
		map[string]string{"app": source}, t.TempDir(), "proj", BackupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var resolved []string
	resolve := func(volume string) (string, error) {
		resolved = append(resolved, volume)
		dir := filepath.Join(t.TempDir(), volume)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		return dir, nil
	}
	err = s.RestoreBundleTargets(archive, RestoreOptions{Volumes: []string{"app", "cache"}}, resolve)
	if err == nil {
		t.Fatal("a bundle missing a requested volume must be refused by default")
	}
	if len(resolved) != 0 {
		t.Fatalf("targets were resolved before the manifest check: %v", resolved)
	}

	resolved = nil
	if err := s.RestoreBundleTargets(archive,
		RestoreOptions{Volumes: []string{"app", "cache"}, AllowMissing: true}, resolve); err != nil {
		t.Fatalf("AllowMissing must restore what the bundle does carry: %v", err)
	}
	if len(resolved) != 1 || resolved[0] != "app" {
		t.Fatalf("only the volumes present in the bundle may be resolved: %v", resolved)
	}
}

func TestBundleRestoreRejectsUnexpectedVolume(t *testing.T) {
	s := newTestStore(t)
	archive := craftBundleTar(t, []string{"app"}, []tarItem{
		{hdr: tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "app/data.txt", Typeflag: tar.TypeReg}, data: []byte("x")},
		{hdr: tar.Header{Name: "rogue/data.txt", Typeflag: tar.TypeReg}, data: []byte("x")},
	})
	target := t.TempDir()
	err := s.RestoreBundleSources(archive, map[string]string{"app": target})
	if err == nil || !strings.Contains(err.Error(), "unexpected volume") {
		t.Fatalf("an entry outside the manifest must be refused, got %v", err)
	}
}

// A hostile bundle whose second volume root is a symlink must be refused before
// the first volume is swapped in.
func TestBundleRestoreValidatesEveryRootBeforeSwapping(t *testing.T) {
	s := newTestStore(t)
	outside := t.TempDir()
	archive := craftBundleTar(t, []string{"db", "web"}, []tarItem{
		{hdr: tar.Header{Name: "db/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "db/keep.txt", Typeflag: tar.TypeReg}, data: []byte("from-archive")},
		{hdr: tar.Header{Name: "web/", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777}},
	})
	dbTarget, webTarget := t.TempDir(), t.TempDir()
	writeVolumeFile(t, dbTarget, "live.txt", "db-live")
	writeVolumeFile(t, webTarget, "live.txt", "web-live")

	if err := s.RestoreBundleSources(archive, map[string]string{"db": dbTarget, "web": webTarget}); err == nil {
		t.Fatal("a bundle with a symlink volume root must be refused")
	}
	for name, dir := range map[string]string{"db": dbTarget, "web": webTarget} {
		got, err := os.ReadFile(filepath.Join(dir, "live.txt"))
		if err != nil || string(got) != name+"-live" {
			t.Fatalf("%s was modified by the refused bundle: %q err=%v", name, got, err)
		}
	}
}

// ---------------------------------------------------------------------------
// L10 / verification
// ---------------------------------------------------------------------------

func TestVerifyArchiveReportsAndDetectsCorruption(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	vdir := s.VolumePath("web")
	writeVolumeFile(t, vdir, "a.txt", strings.Repeat("payload", 4096))
	if err := os.Symlink("a.txt", filepath.Join(vdir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(vdir, "a.txt"), filepath.Join(vdir, "hard.txt")); err != nil {
		t.Fatal(err)
	}
	archive, err := s.Backup("web")
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.VerifyArchive(archive)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.Bundle || report.Entries < 4 || report.Symlinks != 1 || report.Hardlink != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}

	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(archive, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyArchive(archive); err == nil {
		t.Fatal("verify must detect a corrupted archive")
	}
}

func TestInspectArchiveClassifiesBundles(t *testing.T) {
	s := newTestStore(t)
	source := t.TempDir()
	writeVolumeFile(t, source, "data.txt", "x")
	bundle, err := s.BackupBundleSourcesWithOptions([]string{"app", "db"},
		map[string]string{"app": source, "db": source}, t.TempDir(), "proj", BackupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	report, err := InspectArchive(bundle)
	if err != nil || !report.Bundle {
		t.Fatalf("bundle not recognised: %+v err=%v", report, err)
	}
	if strings.Join(report.Volumes, ",") != "app,db" {
		t.Fatalf("unexpected volumes: %v", report.Volumes)
	}
	single, err := s.BackupSourceWithOptions("web", source, BackupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	report, err = InspectArchive(single)
	if err != nil || report.Bundle {
		t.Fatalf("single-volume archive misclassified: %+v err=%v", report, err)
	}
}

// ---------------------------------------------------------------------------
// Temporary-archive housekeeping
// ---------------------------------------------------------------------------

func TestSweepRemovesAbandonedTempArchiveButNotLiveOnes(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	abandoned := filepath.Join(dir, tempArchivePrefix+"crashed")
	if err := os.WriteFile(abandoned, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	live, err := os.CreateTemp(dir, tempArchivePrefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := unix.Flock(int(live.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	s.sweepStaleTempArchives(dir)
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("abandoned temporary archive was kept: %v", err)
	}
	if _, err := os.Stat(live.Name()); err != nil {
		t.Fatalf("temporary archive of a live backup was removed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Naming
// ---------------------------------------------------------------------------

func TestArchiveNameValidation(t *testing.T) {
	for _, name := range []string{"../evil", "a/b", `a\b`, "a:b", ".", "..", "", ".hidden", ".dvol-index.json"} {
		if err := (BackupOptions{ArchiveName: name}).Validate(); err == nil && name != "" {
			t.Fatalf("archive name %q was accepted", name)
		}
	}
	for _, name := range []string{"good", "good.tar.gz", "good_08082026_101010", "a.b-c_d"} {
		if err := (BackupOptions{ArchiveName: name}).Validate(); err != nil {
			t.Fatalf("archive name %q was rejected: %v", name, err)
		}
	}
	if got := archiveFilename("vol", BackupOptions{ArchiveName: "custom.tar.zst", Compression: CompressionZstd}); got != "custom.tar.zst" {
		t.Fatalf("suffix handling changed: %q", got)
	}
	if got := archiveFilename("vol", BackupOptions{Timestamp: true, TimestampValue: "08082026_101010"}); got != "vol_08082026_101010.tar.gz" {
		t.Fatalf("timestamped name changed: %q", got)
	}
}

func TestDuplicateVolumesInBundleRejected(t *testing.T) {
	s := newTestStore(t)
	source := t.TempDir()
	writeVolumeFile(t, source, "data.txt", "x")
	_, err := s.BackupBundleSourcesWithOptions([]string{"app", "app"},
		map[string]string{"app": source}, t.TempDir(), "proj", BackupOptions{})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("a duplicated volume must be refused, got %v", err)
	}
}

func TestRestoreRejectsSymlinkTarget(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	writeVolumeFile(t, s.VolumePath("web"), "data.txt", "x")
	archive, err := s.Backup("web")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(dir, "target")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreSource("web", link, archive); err == nil {
		t.Fatal("restoring onto a symlinked target must be refused")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("the symlink target was written into: %v", entries)
	}
}

func TestSparseWriterPreservesArbitraryContent(t *testing.T) {
	dir := t.TempDir()
	for _, size := range []int{0, 1, sparseChunk - 1, sparseChunk, sparseChunk + 7, 3*sparseChunk + 11} {
		payload := make([]byte, size)
		for i := range payload {
			if i%(sparseChunk/3) != 0 {
				continue
			}
			payload[i] = byte(i%251) + 1
		}
		path := filepath.Join(dir, fmt.Sprintf("f-%d", size))
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		w := newSparseFileWriter(f)
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("sparse writer changed the content for size %d", size)
		}
	}
}
