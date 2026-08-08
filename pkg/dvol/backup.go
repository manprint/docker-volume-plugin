package dvol

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// BackupInfo describes a single backup archive.
type BackupInfo struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// bundleManifest is written into bundle archives (multi-volume backups).
type bundleManifest struct {
	Format  string    `json:"format"`
	Created time.Time `json:"created_at"`
	Volumes []string  `json:"volumes"`
}

const bundleFormat = "dvol-bundle-v1"

// backupDir returns the backups directory for a volume.
func (s *Store) backupDir(name string) string {
	return filepath.Join(s.backupsDir(), name)
}

// BackupsDir returns the backups directory for a volume (exported helper).
func (s *Store) BackupsDir(name string) string {
	return s.backupDir(name)
}

// Backup tars the volume data into backups/<name>/<name>-<ts>.tar.gz.
// The archive is root-preserved: entry ownership, permissions and symlinks
// are kept, and the volume root directory entry is included as ".".
func (s *Store) Backup(name string) (string, error) {
	return s.BackupWithOptions(name, BackupOptions{})
}

// BackupWithOptions is Backup with an explicit compression format and level.
func (s *Store) BackupWithOptions(name string, opts BackupOptions) (string, error) {
	if err := opts.Validate(); err != nil {
		return "", err
	}
	if _, err := s.Get(name); err != nil {
		return "", err
	}
	return s.BackupSourceWithOptions(name, s.VolumePath(name), opts)
}

// BackupSourceWithOptions backs up an arbitrary directory under the logical
// volume name. It is used by docker dvol for Docker volumes whose driver is
// not dvol; the directory is normally the volume's Docker mountpoint.
func (s *Store) BackupSourceWithOptions(name, source string, opts BackupOptions) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	if err := opts.Validate(); err != nil {
		return "", err
	}
	fi, err := os.Stat(source)
	if err != nil {
		return "", fmt.Errorf("volume source %q: %w", source, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("volume source %q is not a directory", source)
	}
	bdir := s.backupDir(name)
	if opts.SameDestination {
		bdir = s.backupsDir()
	}
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		return "", err
	}
	archiveName := archiveFilename(name, opts)
	path := filepath.Join(bdir, archiveName)
	if opts.SameDestination {
		if err := s.writeFlatBackup(name, archiveName, path, opts, func(tw *tar.Writer) error {
			return addTreeTo(tw, source, "")
		}); err != nil {
			return "", err
		}
	} else if err := writeArchive(path, opts, func(tw *tar.Writer) error {
		return addTreeTo(tw, source, "")
	}); err != nil {
		return "", err
	}
	return path, nil
}

// BackupSourceToWriter writes a complete single-volume archive to w. It is
// intentionally stream-only: no archive temporary file is created.
func (s *Store) BackupSourceToWriter(name, source string, opts BackupOptions, w io.Writer) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := opts.Validate(); err != nil {
		return err
	}
	fi, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("volume source %q: %w", source, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("volume source %q is not a directory", source)
	}
	return writeArchiveTo(w, opts, func(tw *tar.Writer) error {
		return addTreeTo(tw, source, "")
	})
}

// BackupBundle backs up several volumes into a single archive stored at
// <outDir>/<prefix>-<ts>.tar.gz. Each volume is stored under its own
// <volume>/ prefix and a manifest.json records the contents.
func (s *Store) BackupBundle(names []string, outDir, prefix string) (string, error) {
	return s.BackupBundleWithOptions(names, outDir, prefix, BackupOptions{})
}

// BackupBundleWithOptions is BackupBundle with an explicit compression.
func (s *Store) BackupBundleWithOptions(names []string, outDir, prefix string, opts BackupOptions) (string, error) {
	if err := opts.Validate(); err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no volumes to back up")
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, err := s.Get(name); err != nil {
			return "", err
		}
	}
	clean := append([]string{}, names...) // names in order
	sort.Strings(clean)
	sources := make(map[string]string, len(clean))
	for _, name := range clean {
		sources[name] = s.VolumePath(name)
	}
	return s.BackupBundleSourcesWithOptions(clean, sources, outDir, prefix, opts)
}

// BackupBundleSourcesWithOptions backs up arbitrary directories as a single
// bundle. sources maps each logical volume name to its source directory.
func (s *Store) BackupBundleSourcesWithOptions(names []string, sources map[string]string, outDir, prefix string, opts BackupOptions) (string, error) {
	if err := opts.Validate(); err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no volumes to back up")
	}
	clean := append([]string{}, names...)
	sort.Strings(clean)
	for _, name := range clean {
		if err := checkName(name); err != nil {
			return "", err
		}
		source, ok := sources[name]
		if !ok {
			return "", fmt.Errorf("no source for volume %q", name)
		}
		fi, err := os.Stat(source)
		if err != nil {
			return "", fmt.Errorf("volume source %q: %w", source, err)
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("volume source %q is not a directory", source)
		}
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(outDir, archiveFilename(prefix, opts))
	if err := writeArchive(path, opts, func(tw *tar.Writer) error {
		mf := bundleManifest{Format: bundleFormat, Created: time.Now().UTC(), Volumes: clean}
		mb, err := json.Marshal(mf)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(mb)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(mb); err != nil {
			return err
		}
		for _, name := range clean {
			if err := addTreeTo(tw, sources[name], name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return "", err
	}
	return path, nil
}

// BackupBundleSourcesToWriter writes one multi-volume bundle directly to w.
// The stream contains manifest.json and one top-level directory per volume;
// no archive temporary file is created.
func (s *Store) BackupBundleSourcesToWriter(names []string, sources map[string]string, opts BackupOptions, w io.Writer) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("no volumes to back up")
	}
	clean := append([]string{}, names...)
	sort.Strings(clean)
	for _, name := range clean {
		if err := checkName(name); err != nil {
			return err
		}
		source, ok := sources[name]
		if !ok {
			return fmt.Errorf("no source for volume %q", name)
		}
		fi, err := os.Stat(source)
		if err != nil {
			return fmt.Errorf("volume source %q: %w", source, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("volume source %q is not a directory", source)
		}
	}
	return writeArchiveTo(w, opts, func(tw *tar.Writer) error {
		mf := bundleManifest{Format: bundleFormat, Created: time.Now().UTC(), Volumes: clean}
		mb, err := json.Marshal(mf)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(mb)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(mb); err != nil {
			return err
		}
		for _, name := range clean {
			if err := addTreeTo(tw, sources[name], name); err != nil {
				return err
			}
		}
		return nil
	})
}

// writeArchive builds an archive in a private file and publishes it only
// after the complete stream has been closed and synced. A failed or
// interrupted write therefore cannot expose a partial archive under its
// final name or replace an existing valid archive.
func writeArchive(path string, opts BackupOptions, populate func(*tar.Writer) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".dvol-backup-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if err := f.Chmod(0o644); err != nil {
		cleanup()
		return err
	}

	if err := writeArchiveTo(f, opts, populate); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if opts.Overwrite {
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	} else {
		// Link publishes the completed file without replacing an existing
		// archive. This matters for user-supplied names and timestamp collisions.
		if err := os.Link(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		_ = os.Remove(tmp)
	}
	// The rename is atomic, but syncing the parent makes the new directory
	// entry durable across a power loss on filesystems that support fsync.
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("archive published but directory sync failed: %w", err)
	}
	return nil
}

// writeArchiveTo writes one complete tar/compressed stream to w. It never
// creates an archive temporary file and is therefore suitable for stdout.
func writeArchiveTo(w io.Writer, opts BackupOptions, populate func(*tar.Writer) error) error {
	compressed, err := opts.newWriter(w)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(compressed)
	if err := populate(tw); err != nil {
		_ = tw.Close()
		_ = compressed.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		_ = compressed.Close()
		return err
	}
	return compressed.Close()
}

// ListArchives returns the .tar.gz files in dir sorted by creation time.
func (s *Store) ListArchives(dir string) ([]BackupInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []BackupInfo{}, nil
		}
		return nil, err
	}
	infos := make([]BackupInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isArchiveName(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		infos = append(infos, BackupInfo{
			Name:      e.Name(),
			Path:      p,
			Size:      info.Size(),
			CreatedAt: info.ModTime().UTC(),
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].CreatedAt.Before(infos[j].CreatedAt) })
	return infos, nil
}

// ts returns a filesystem-safe timestamp. Nanosecond precision prevents two
// backups landing in the same second from truncating each other.
func ts() string { return time.Now().UTC().Format("20060102-150405.000000000") }

func archiveFilename(defaultName string, opts BackupOptions) string {
	base := defaultName
	if opts.ArchiveName != "" {
		base = opts.ArchiveName
		for _, suffix := range []string{".tar.gz", ".tar.xz", ".tar.zst", ".tar.zstd", ".tar.lz4"} {
			base = strings.TrimSuffix(base, suffix)
		}
	}
	if opts.Timestamp {
		timestamp := opts.TimestampValue
		if timestamp == "" {
			now := time.Now()
			if opts.TimestampUTC {
				now = now.UTC()
			}
			timestamp = now.Format("02012006_150405")
		}
		base += "_" + timestamp
	} else if opts.ArchiveName == "" {
		base += "-" + ts()
	}
	return fmt.Sprintf("%s.tar.%s", base, opts.norm().ext())
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// addTreeTo writes the volume root entry followed by every file under dir.
// With prefix "" the root entry is "." (a plain volume backup); otherwise
// entries are prefixed with "<prefix>/" (a bundle). Hard-linked files are
// written once plus TypeLink entries, preserving inode sharing in both the
// plain and the bundle layout.
func addTreeTo(tw *tar.Writer, dir, prefix string) error {
	rootInfo, err := os.Stat(dir)
	if err != nil {
		return err
	}
	rootName := prefix
	if rootName == "" {
		rootName = "."
	} else {
		rootName = prefix + "/"
	}
	if err := tw.WriteHeader(dirHeader(rootName, rootInfo)); err != nil {
		return err
	}
	seen := map[[2]uint64]string{} // (dev,ino) -> first archive name
	return filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil // root entry already written
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, dir+string(filepath.Separator)))
		name := rel
		if prefix != "" {
			name = prefix + "/" + rel
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(p)
			if err != nil {
				return err
			}
		}
		if info.Mode().IsRegular() {
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
				if first, ok := seen[key]; ok {
					hdr, err := tar.FileInfoHeader(info, "")
					if err != nil {
						return err
					}
					hdr.Name = name
					hdr.Typeflag = tar.TypeLink
					hdr.Linkname = first
					hdr.Mode |= specialModeBits(info.Mode())
					return tw.WriteHeader(hdr)
				}
				seen[key] = name
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		hdr.Mode |= specialModeBits(info.Mode())
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		sf, err := os.Open(p)
		if err != nil {
			return err
		}
		defer sf.Close()
		_, err = io.Copy(tw, sf)
		return err
	})
}

// specialModeBits maps os file-mode bits (setuid/setgid/sticky) onto the
// numeric mode used in tar headers. tar.FileInfoHeader only records the
// 0777 perm bits, dropping these.
func specialModeBits(m fs.FileMode) int64 {
	b := int64(0)
	if m&fs.ModeSetuid != 0 {
		b |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		b |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		b |= 0o1000
	}
	return b
}

// fileModeFromTar converts the numeric Unix mode stored by tar into Go's
// os.FileMode representation. The permission bits share the low nine bits,
// but setuid/setgid/sticky are represented by separate Go flags.
func fileModeFromTar(mode int64) os.FileMode {
	m := os.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

func dirHeader(name string, info os.FileInfo) *tar.Header {
	hdr, _ := tar.FileInfoHeader(info, "")
	hdr.Name = name
	hdr.Typeflag = tar.TypeDir
	hdr.Mode |= specialModeBits(info.Mode())
	return hdr
}

// Restore replaces the volume data with the contents of a backup archive.
// If the volume does not exist it is created first. The archive is fully
// validated into a staging directory before the live volume is touched, so a
// corrupt or untrusted archive cannot destroy the current data.
func (s *Store) Restore(name, backupPath string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if _, err := os.Stat(backupPath); err != nil {
		return fmt.Errorf("backup not found: %w", err)
	}
	if err := s.ensureVolume(name); err != nil {
		return err
	}
	return s.RestoreSource(name, s.VolumePath(name), backupPath)
}

// RestoreSource replaces an arbitrary Docker volume mountpoint with the
// contents of a backup archive. Unlike Restore, it does not create dvol
// metadata; the Docker Engine already owns the volume metadata.
func (s *Store) RestoreSource(name, target, backupPath string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if _, err := os.Stat(backupPath); err != nil {
		return fmt.Errorf("backup not found: %w", err)
	}
	if fi, err := os.Stat(target); err == nil && !fi.IsDir() {
		return fmt.Errorf("volume target %q is not a directory", target)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("volume target %q: %w", target, err)
	}
	staging, err := os.MkdirTemp(filepath.Dir(target), ".restore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	n := 0
	if err := extractTarTo(staging, backupPath, func(name string) (string, bool, error) {
		n++
		return name, true, nil
	}); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("backup archive %q contains no entries", backupPath)
	}
	return replaceStage(staging, target)
}

// RestoreSourceReader is the stream equivalent of RestoreSource. The input is
// decompressed and extracted directly into staging; no archive spool file is
// created. The live target is changed only after a complete valid stream.
func (s *Store) RestoreSourceReader(name, target string, input io.Reader) error {
	if err := checkName(name); err != nil {
		return err
	}
	if fi, err := os.Stat(target); err == nil && !fi.IsDir() {
		return fmt.Errorf("volume target %q is not a directory", target)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("volume target %q: %w", target, err)
	}
	staging, err := os.MkdirTemp(filepath.Dir(target), ".restore-stream-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	n := 0
	tr, r, err := openArchiveReader(input)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := extractTarReader(tr, func(entry *tar.Header) (string, string, bool, error) {
		n++
		return entry.Name, staging, true, nil
	}); err != nil {
		return err
	}
	if err := finishArchiveReader(r); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("archive stream contains no entries")
	}
	return replaceStage(staging, target)
}

// RestoreBundle restores volumes from a bundle archive. Only volumes in names
// are restored (all of them if names is empty). Each volume is replaced. The
// whole archive is extracted to a staging directory first, so a corrupt
// archive leaves every volume untouched.
func (s *Store) RestoreBundle(path string, names []string) error {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	staging, err := os.MkdirTemp(s.volumesDir(), ".bundle-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	if err := extractTarTo(staging, path, func(name string) (string, bool, error) {
		if name == "manifest.json" {
			return "", false, nil
		}
		vol, rest, ok := splitBundlePath(name)
		if !ok {
			return "", false, fmt.Errorf("malformed bundle entry %q", name)
		}
		if len(want) > 0 && !want[vol] {
			return "", false, nil
		}
		if rest == "" {
			return vol, true, nil
		}
		return vol + "/" + rest, true, nil
	}); err != nil {
		return err
	}

	entries, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("bundle contains no volumes matching the request")
	}
	// Validate every staged volume root BEFORE touching any live volume: a
	// single malformed volume (e.g. root swapped into a symlink by a hostile
	// archive) must not leave the already-staged volumes partially restored.
	staged := make([]string, 0, len(entries))
	for _, e := range entries {
		dir := filepath.Join(staging, e.Name())
		fi, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("refusing to restore volume %q: archive entry is not a directory", e.Name())
		}
		staged = append(staged, e.Name())
	}
	for _, vol := range staged {
		if err := s.ensureVolume(vol); err != nil {
			return err
		}
		if err := replaceStage(filepath.Join(staging, vol), s.VolumePath(vol)); err != nil {
			return err
		}
	}
	return nil
}

// RestoreBundleSources restores a bundle into arbitrary Docker volume
// mountpoints. Each target gets a staging directory on its own filesystem so
// the final rename also works when Docker volumes and the dvol backup store
// are on different filesystems.
func (s *Store) RestoreBundleSources(path string, targets map[string]string) error {
	if len(targets) == 0 {
		return fmt.Errorf("no volume targets supplied")
	}
	type stagedVolume struct {
		name    string
		staging string
		cleanup string
		target  string
	}
	names := make([]string, 0, len(targets))
	for name, target := range targets {
		if err := checkName(name); err != nil {
			return err
		}
		if target == "" {
			return fmt.Errorf("empty target for volume %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	staged := make([]stagedVolume, 0, len(names))
	cleanup := func() {
		for _, v := range staged {
			_ = os.RemoveAll(v.cleanup)
		}
	}
	defer cleanup()

	for _, name := range names {
		target := targets[name]
		if fi, err := os.Stat(target); err == nil && !fi.IsDir() {
			return fmt.Errorf("volume target %q is not a directory", target)
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("volume target %q: %w", target, err)
		}
		stage, err := os.MkdirTemp(filepath.Dir(target), "."+name+".restore-*")
		if err != nil {
			return err
		}
		count := 0
		err = extractTarTo(stage, path, func(entry string) (string, bool, error) {
			if entry == "manifest.json" {
				return "", false, nil
			}
			vol, rest, ok := splitBundlePath(entry)
			if !ok {
				return "", false, fmt.Errorf("malformed bundle entry %q", entry)
			}
			if vol != name {
				return "", false, nil
			}
			count++
			if rest == "" {
				return vol, true, nil
			}
			return vol + "/" + rest, true, nil
		})
		if err != nil {
			_ = os.RemoveAll(stage)
			return err
		}
		if count == 0 {
			_ = os.RemoveAll(stage)
			return fmt.Errorf("bundle contains no volume %q", name)
		}
		entries, err := os.ReadDir(stage)
		if err != nil {
			_ = os.RemoveAll(stage)
			return err
		}
		if len(entries) != 1 || entries[0].Name() != name {
			_ = os.RemoveAll(stage)
			return fmt.Errorf("bundle contains invalid staged root for volume %q", name)
		}
		root := filepath.Join(stage, name)
		fi, err := os.Lstat(root)
		if err != nil {
			_ = os.RemoveAll(stage)
			return err
		}
		if !fi.IsDir() {
			_ = os.RemoveAll(stage)
			return fmt.Errorf("refusing to restore volume %q: archive entry is not a directory", name)
		}
		staged = append(staged, stagedVolume{name: name, staging: root, cleanup: stage, target: target})
	}

	for _, v := range staged {
		if err := replaceStage(v.staging, v.target); err != nil {
			return err
		}
	}
	return nil
}

// RestoreBundleSourcesReader restores a complete bundle from one input
// stream. It stages each volume beside its Docker mountpoint so it works even
// when volumes are on different filesystems, while consuming the compressed
// archive exactly once and without an archive spool file.
func (s *Store) RestoreBundleSourcesReader(input io.Reader, targets map[string]string) error {
	if len(targets) == 0 {
		return fmt.Errorf("no volume targets supplied")
	}
	names := make([]string, 0, len(targets))
	for name, target := range targets {
		if err := checkName(name); err != nil {
			return err
		}
		if target == "" {
			return fmt.Errorf("empty target for volume %q", name)
		}
		if fi, err := os.Stat(target); err == nil && !fi.IsDir() {
			return fmt.Errorf("volume target %q is not a directory", target)
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("volume target %q: %w", target, err)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	type stagedVolume struct {
		name, stage, cleanup, target string
		metas                        []fileMeta
		entries                      int
	}
	staged := make(map[string]*stagedVolume, len(names))
	cleanup := func() {
		for _, v := range staged {
			_ = os.RemoveAll(v.cleanup)
		}
	}
	defer cleanup()
	for _, name := range names {
		stage, err := os.MkdirTemp(filepath.Dir(targets[name]), "."+name+".restore-stream-*")
		if err != nil {
			return err
		}
		staged[name] = &stagedVolume{name: name, stage: filepath.Join(stage, name), cleanup: stage, target: targets[name]}
		if err := os.MkdirAll(staged[name].stage, 0o755); err != nil {
			return err
		}
	}

	tr, r, err := openArchiveReader(input)
	if err != nil {
		return err
	}
	defer r.Close()
	var manifest bundleManifest
	manifestSeen := false
	if err := extractTarReader(tr, func(entry *tar.Header) (string, string, bool, error) {
		if entry.Name == "manifest.json" {
			data, err := io.ReadAll(tr)
			if err != nil {
				return "", "", false, err
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				return "", "", false, fmt.Errorf("invalid bundle manifest: %w", err)
			}
			manifestSeen = true
			return "", "", false, nil
		}
		vol, rest, ok := splitBundlePath(entry.Name)
		if !ok {
			return "", "", false, fmt.Errorf("malformed bundle entry %q", entry.Name)
		}
		v := staged[vol]
		if v == nil {
			return "", "", false, fmt.Errorf("bundle contains unexpected volume %q", vol)
		}
		rel := vol
		if rest != "" {
			rel += "/" + rest
		}
		v.entries++
		return rel, filepath.Dir(v.stage), true, nil
	}); err != nil {
		return err
	}
	if err := finishArchiveReader(r); err != nil {
		return err
	}
	if !manifestSeen || manifest.Format != bundleFormat {
		return fmt.Errorf("invalid or missing bundle manifest")
	}
	if len(manifest.Volumes) != len(names) {
		return fmt.Errorf("bundle volume list does not match restore targets")
	}
	for _, name := range manifest.Volumes {
		if staged[name] == nil {
			return fmt.Errorf("bundle volume %q is not a restore target", name)
		}
	}
	for _, name := range names {
		v := staged[name]
		if v.entries == 0 {
			return fmt.Errorf("bundle contains no volume %q", name)
		}
		fi, err := os.Lstat(v.stage)
		if err != nil || !fi.IsDir() {
			if err != nil {
				return err
			}
			return fmt.Errorf("refusing to restore volume %q: staged root is not a directory", name)
		}
	}
	for _, name := range names {
		if err := replaceStage(staged[name].stage, staged[name].target); err != nil {
			return err
		}
	}
	return nil
}

// replaceStage swaps the fully-extracted staging directory into target. The
// staging root is verified to be a real directory: an archive entry turning
// it into a symlink refuses the swap. The previous target is kept under a
// backup name until the swap completes, so a failed rename (or a crash within
// replaceStage) leaves the old data recoverable instead of deleting it.
func replaceStage(staging, target string) error {
	fi, err := os.Lstat(staging)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("refusing to replace volume with non-directory %q (entry escaped the archive?)", staging)
	}
	if _, err := os.Lstat(target); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// target already gone (e.g. user deleted the data dir while keeping
		// the metadata): nothing to preserve, just move the staging in place.
		return os.Rename(staging, target)
	}
	// Keep the old target in a private, collision-free path. A fixed suffix
	// would collide with a valid volume name such as "foo.dvol-old".
	backup, err := os.MkdirTemp(filepath.Dir(target), "."+filepath.Base(target)+".dvol-old-*")
	if err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	if err := os.Rename(target, backup); err != nil {
		return err
	}
	if err := os.Rename(staging, target); err != nil {
		if rollbackErr := os.Rename(backup, target); rollbackErr != nil {
			return fmt.Errorf("replace volume: %w (rollback failed: %v)", err, rollbackErr)
		}
		return err
	}
	return os.RemoveAll(backup)
}

// extractTarTo streams the entries of a backup archive into dir. filter maps
// an entry name to the relative target path inside dir; ok=false skips the
// entry, err aborts the extraction.
func extractTarTo(dir, path string, filter func(name string) (rel string, ok bool, err error)) error {
	tr, r, f, err := openArchive(path)
	if err != nil {
		return err
	}
	defer f.Close()
	defer r.Close()
	if err := extractTarReader(tr, func(hdr *tar.Header) (string, string, bool, error) {
		rel, ok, err := filter(hdr.Name)
		return rel, dir, ok, err
	}); err != nil {
		return err
	}
	return finishArchiveReader(r)
}

// finishArchiveReader drains the decompressor after tar reaches its end. This
// is required for gzip/xz/zstd/lz4 trailers and checksums to be validated; tar
// can otherwise stop at its zero blocks before the compressor reports a
// truncated input.
func finishArchiveReader(r io.ReadCloser) error {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return fmt.Errorf("finishing archive stream: %w", err)
	}
	if err := r.Close(); err != nil {
		return fmt.Errorf("closing archive stream: %w", err)
	}
	return nil
}

// extractTarReader extracts entries from an already-open tar stream. route
// returns a path relative to base and whether the entry should be kept. It
// also applies metadata only after EOF, which is essential for stream restore
// atomicity and for directory timestamps.
func extractTarReader(tr *tar.Reader, route func(*tar.Header) (rel, base string, ok bool, err error)) error {
	var metas []fileMeta
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}
		rel, base, ok, err := route(hdr)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		target, err := safeJoin(base, rel)
		if err != nil {
			return err
		}
		if err := extractEntry(hdr, tr, target, base); err != nil {
			return err
		}
		metas = append(metas, fileMeta{
			path:   target,
			isLink: hdr.Typeflag == tar.TypeSymlink,
			mode:   fileModeFromTar(hdr.Mode),
			uid:    hdr.Uid,
			gid:    hdr.Gid,
			atime:  hdr.AccessTime,
			mtime:  hdr.ModTime,
		})
	}
	for i := len(metas) - 1; i >= 0; i-- {
		if err := metas[i].apply(); err != nil {
			return err
		}
	}
	return nil
}

// fileMeta carries the metadata to apply to an extracted entry after the
// archive stream has been fully materialized.
type fileMeta struct {
	path   string
	isLink bool
	mode   os.FileMode
	uid    int
	gid    int
	atime  time.Time
	mtime  time.Time
}

func (m fileMeta) apply() error {
	if m.isLink {
		if err := os.Lchown(m.path, m.uid, m.gid); err != nil {
			return err
		}
	} else if err := os.Chown(m.path, m.uid, m.gid); err != nil {
		return err
	}
	if !m.isLink {
		// Apply ownership before mode: on Linux chown may clear setuid/setgid.
		// Special bits only take effect via os.Chmod, not through OpenFile's
		// create mode. Symlinks are skipped: on Linux symlink permission bits
		// are ignored and os.Chmod follows the link, so chmodding a symlink
		// would modify its target outside the volume.
		if err := os.Chmod(m.path, m.mode); err != nil {
			return err
		}
	}
	return applyTimes(m.path, m.isLink, m.atime, m.mtime)
}

// applyTimes sets access and modification times. Symlinks need the nofollow
// variant so the link itself (not its target) is touched.
func applyTimes(path string, isLink bool, atime, mtime time.Time) error {
	if mtime.IsZero() {
		return nil
	}
	if atime.IsZero() {
		atime = time.Now()
	}
	if isLink {
		ts := []unix.Timespec{
			unix.NsecToTimespec(atime.UnixNano()),
			unix.NsecToTimespec(mtime.UnixNano()),
		}
		return unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW)
	}
	return os.Chtimes(path, atime, mtime)
}

// splitBundlePath splits "<vol>/<rest>" into volume name and relative path.
// A bare "<vol>/" yields rest "". Returns ok=false for malformed entries.
func splitBundlePath(p string) (vol, rest string, ok bool) {
	if p == "" || strings.HasPrefix(p, "/") {
		return "", "", false
	}
	i := strings.IndexByte(p, '/')
	if i < 0 {
		return "", "", false
	}
	vol = p[:i]
	rest = strings.Trim(p[i+1:], "/")
	if vol == "" {
		return "", "", false
	}
	return vol, rest, true
}

func (s *Store) ensureVolume(name string) error {
	if _, err := s.Get(name); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := s.Create(name, nil); err != nil {
			return err
		}
	}
	return nil
}

// extractEntry materializes a single archive entry at target. base is the
// volume dir, used to resolve hard links.
func extractEntry(hdr *tar.Header, tr *tar.Reader, target, base string) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := checkNoSymlinkAncestors(base, target); err != nil {
			return err
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
	case tar.TypeReg:
		if err := checkNoSymlinkAncestors(base, target); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		of, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
		if err != nil {
			return err
		}
		if _, err := io.Copy(of, tr); err != nil {
			of.Close()
			return err
		}
		if err := of.Close(); err != nil {
			return err
		}
	case tar.TypeSymlink:
		if filepath.Clean(target) == filepath.Clean(base) {
			return fmt.Errorf("refusing to extract symlink at volume root %q", hdr.Name)
		}
		if err := checkNoSymlinkAncestors(base, filepath.Dir(target)); err != nil {
			return err
		}
		os.Remove(target)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return err
		}
	case tar.TypeLink:
		if err := checkNoSymlinkAncestors(base, target); err != nil {
			return err
		}
		os.Remove(target)
		ln, err := safeJoin(base, hdr.Linkname)
		if err != nil {
			return err
		}
		if err := checkNoSymlinkAncestors(base, ln); err != nil {
			return err
		}
		if err := os.Link(ln, target); err != nil {
			return err
		}
	default:
		// skip fifos, devices, char, etc.
	}
	return nil
}

// checkNoSymlinkAncestors rejects extracting under a path whose existing
// components are symlinks. Migration through a symlink previously created by
// the archive would let entries escape the volume root.
func checkNoSymlinkAncestors(base, p string) error {
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == "." {
		return nil
	}
	cur := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to extract through symlink %q", cur)
		}
	}
	return nil
}

// safeJoin resolves p inside base and rejects escapes (zip-slip): absolute
// paths and paths escaping base via "..".
func safeJoin(base, p string) (string, error) {
	if filepath.IsAbs(filepath.FromSlash(p)) {
		return "", fmt.Errorf("unsafe path in archive: %q", p)
	}
	clean := filepath.Clean(filepath.FromSlash(p))
	if clean == "." {
		return base, nil
	}
	joined := filepath.Join(base, clean)
	rel, err := filepath.Rel(base, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path in archive: %q", p)
	}
	return joined, nil
}

// ListBackups returns the backups of a volume sorted by creation time.
func (s *Store) ListBackups(name string) ([]BackupInfo, error) {
	nested, err := s.ListArchives(s.backupDir(name))
	if err != nil {
		return nil, err
	}
	flat, err := s.flatBackupInfos(name)
	if err != nil {
		return nil, err
	}
	infos := append(nested, flat...)
	sort.Slice(infos, func(i, j int) bool { return infos[i].CreatedAt.Before(infos[j].CreatedAt) })
	return infos, nil
}

// BundleDir returns the directory used for compose (multi-volume) bundles.
func (s *Store) BundleDir() string {
	return filepath.Join(s.backupsDir(), "compose")
}

// ResolveBackup returns an archive path for a volume backup: arg may be an
// exact or glob path, or a filename/glob resolved against backups/<vol>/.
func (s *Store) ResolveBackup(vol, arg string) (string, error) {
	if matches, err := filepath.Glob(arg); err == nil && len(matches) == 1 {
		return matches[0], nil
	}
	matches, err := filepath.Glob(filepath.Join(s.backupDir(vol), arg))
	if err != nil {
		return "", err
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("multiple backups matching %q for volume %q", arg, vol)
	}
	flat, err := s.flatBackupMatches(vol, arg)
	if err != nil {
		return "", err
	}
	if len(flat) == 1 {
		return flat[0], nil
	}
	if len(flat) > 1 {
		return "", fmt.Errorf("multiple backups matching %q for volume %q", arg, vol)
	}
	return "", fmt.Errorf("no backup matching %q for volume %q", arg, vol)
}
