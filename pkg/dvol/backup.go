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

const (
	bundleFormat       = "dvol-bundle-v1"
	bundleManifestName = "manifest.json"
)

// backupDir returns the backups directory for a volume.
func (s *Store) backupDir(name string) string {
	return filepath.Join(s.backupsDir(), name)
}

// BackupsDir returns the backups directory for a volume (exported helper).
func (s *Store) BackupsDir(name string) string {
	return s.backupDir(name)
}

// readOptions carries the source-reading policy down the traversal.
type readOptions struct {
	allowAtime bool
	logger     *Logger
}

func (s *Store) readOptions() readOptions {
	return readOptions{allowAtime: s.cfg.AllowAtime, logger: s.logger()}
}

// Backup tars the volume data into backups/<name>/<name>-<ts>.tar.gz.
// The archive is root-preserved: entry ownership, permissions, extended
// attributes and symlinks are kept, and the volume root directory entry is
// included as ".".
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
func (s *Store) BackupSourceWithOptions(name, source string, opts BackupOptions) (path string, err error) {
	progress := s.operationProgress("backup")
	s.logger().Info("backup started", LogField{Key: "volume", Value: name}, LogField{Key: "source", Value: source}, LogField{Key: "compression", Value: opts.norm().Compression})
	defer func() {
		if err != nil {
			s.logger().Error("backup failed", LogField{Key: "volume", Value: name}, LogField{Key: "error", Value: err})
			return
		}
		progress.Complete()
		s.logger().Info("backup completed", LogField{Key: "volume", Value: name}, LogField{Key: "archive", Value: path})
	}()
	if err := checkName(name); err != nil {
		return "", err
	}
	if err := opts.Validate(); err != nil {
		return "", err
	}
	if _, err := validateBackupSource(source, s.readOptions()); err != nil {
		return "", err
	}
	s.logger().Debug("backup source validated", LogField{Key: "volume", Value: name}, LogField{Key: "source", Value: source})
	bdir := s.backupDir(name)
	if opts.SameDestination {
		bdir = s.backupsDir()
	}
	archiveName := archiveFilename(name, opts)
	path = filepath.Join(bdir, archiveName)
	if err := ensureBackupDestinationOutsideSources(path, source); err != nil {
		return "", err
	}
	s.logger().Debug("backup destination prepared", LogField{Key: "volume", Value: name}, LogField{Key: "archive", Value: path})
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		return "", err
	}
	s.sweepStaleTempArchives(bdir)
	tree := s.treeWriter(source, "", progress)
	if opts.SameDestination {
		if err := s.writeFlatBackupWithProgress(name, archiveName, path, opts, progress, tree); err != nil {
			return "", err
		}
	} else if err := writeArchiveWithProgress(path, s.cfg.archiveMode(), opts, progress, tree); err != nil {
		return "", err
	}
	return path, nil
}

// BackupSourceToWriter writes a complete single-volume archive to w. It is
// intentionally stream-only: no archive temporary file is created.
func (s *Store) BackupSourceToWriter(name, source string, opts BackupOptions, w io.Writer) (err error) {
	progress := s.operationProgress("backup stream")
	s.logger().Info("backup stream started", LogField{Key: "volume", Value: name}, LogField{Key: "compression", Value: opts.norm().Compression})
	defer func() {
		if err != nil {
			s.logger().Error("backup stream failed", LogField{Key: "volume", Value: name}, LogField{Key: "error", Value: err})
			return
		}
		progress.Complete()
		s.logger().Info("backup stream completed", LogField{Key: "volume", Value: name})
	}()
	if w == nil {
		return fmt.Errorf("nil backup stream writer")
	}
	if err := checkName(name); err != nil {
		return err
	}
	if err := opts.Validate(); err != nil {
		return err
	}
	if _, err := validateBackupSource(source, s.readOptions()); err != nil {
		return err
	}
	s.logger().Debug("backup stream source validated", LogField{Key: "volume", Value: name}, LogField{Key: "source", Value: source})
	return writeArchiveToWithProgress(w, opts, progress, s.treeWriter(source, "", progress))
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

// prepareBundle validates the volume list and their sources.
func (s *Store) prepareBundle(names []string, sources map[string]string, prefix string) ([]string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("no volumes to back up")
	}
	if err := checkArchiveStem(prefix); err != nil {
		return nil, fmt.Errorf("invalid bundle name: %w", err)
	}
	clean := append([]string{}, names...)
	sort.Strings(clean)
	for i, name := range clean {
		if i > 0 && clean[i-1] == name {
			return nil, fmt.Errorf("duplicate volume %q in bundle", name)
		}
		if err := checkName(name); err != nil {
			return nil, fmt.Errorf("volume %q: %w", name, err)
		}
		source, ok := sources[name]
		if !ok {
			return nil, fmt.Errorf("no source for volume %q", name)
		}
		if _, err := validateBackupSource(source, s.readOptions()); err != nil {
			return nil, err
		}
	}
	return clean, nil
}

// BackupBundleSourcesWithOptions backs up arbitrary directories as a single
// bundle. sources maps each logical volume name to its source directory.
func (s *Store) BackupBundleSourcesWithOptions(names []string, sources map[string]string, outDir, prefix string, opts BackupOptions) (path string, err error) {
	progress := s.operationProgress("backup bundle")
	s.logger().Info("backup bundle started", LogField{Key: "volumes", Value: strings.Join(names, ",")}, LogField{Key: "compression", Value: opts.norm().Compression})
	defer func() {
		if err != nil {
			s.logger().Error("backup bundle failed", LogField{Key: "error", Value: err})
			return
		}
		progress.Complete()
		s.logger().Info("backup bundle completed", LogField{Key: "archive", Value: path})
	}()
	if err := opts.Validate(); err != nil {
		return "", err
	}
	clean, err := s.prepareBundle(names, sources, prefix)
	if err != nil {
		return "", err
	}
	s.logger().Debug("backup bundle sources validated", LogField{Key: "volumes", Value: strings.Join(clean, ",")})

	path = filepath.Join(outDir, archiveFilename(prefix, opts))
	for _, name := range clean {
		if err := ensureBackupDestinationOutsideSources(path, sources[name]); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	s.sweepStaleTempArchives(outDir)
	s.logger().Debug("backup bundle destination prepared", LogField{Key: "archive", Value: path})
	if err := writeArchiveWithProgress(path, s.cfg.archiveMode(), opts, progress, s.bundleWriter(clean, sources, progress)); err != nil {
		return "", err
	}
	return path, nil
}

// BackupBundleSourcesToWriter writes one multi-volume bundle directly to w.
// The stream contains manifest.json and one top-level directory per volume;
// no archive temporary file is created.
func (s *Store) BackupBundleSourcesToWriter(names []string, sources map[string]string, opts BackupOptions, w io.Writer) (err error) {
	progress := s.operationProgress("backup bundle stream")
	s.logger().Info("backup bundle stream started", LogField{Key: "volumes", Value: strings.Join(names, ",")}, LogField{Key: "compression", Value: opts.norm().Compression})
	defer func() {
		if err != nil {
			s.logger().Error("backup bundle stream failed", LogField{Key: "error", Value: err})
			return
		}
		progress.Complete()
		s.logger().Info("backup bundle stream completed")
	}()
	if w == nil {
		return fmt.Errorf("nil backup stream writer")
	}
	if err := opts.Validate(); err != nil {
		return err
	}
	// The stream never builds a path, so the prefix is irrelevant here.
	clean, err := s.prepareBundle(names, sources, "stream")
	if err != nil {
		return err
	}
	return writeArchiveToWithProgress(w, opts, progress, s.bundleWriter(clean, sources, progress))
}

// treeWriter returns a populate function for a single-volume archive.
func (s *Store) treeWriter(source, prefix string, progress *Progress) func(*tar.Writer) error {
	return func(tw *tar.Writer) error {
		return s.addTree(tw, source, prefix, progress)
	}
}

// bundleWriter returns a populate function writing manifest.json followed by
// one top-level directory per volume.
func (s *Store) bundleWriter(names []string, sources map[string]string, progress *Progress) func(*tar.Writer) error {
	return func(tw *tar.Writer) error {
		mf := bundleManifest{Format: bundleFormat, Created: time.Now().UTC(), Volumes: names}
		mb, err := json.Marshal(mf)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: bundleManifestName, Mode: 0o644, Size: int64(len(mb)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(mb); err != nil {
			return err
		}
		for _, name := range names {
			if err := s.addTree(tw, sources[name], name, progress); err != nil {
				return err
			}
		}
		return nil
	}
}

// writeArchive builds an archive in a private file and publishes it only
// after the complete stream has been closed and synced. A failed or
// interrupted write therefore cannot expose a partial archive under its
// final name or replace an existing valid archive.
func writeArchive(path string, opts BackupOptions, populate func(*tar.Writer) error) error {
	return writeArchiveWithProgress(path, DefaultArchiveMode, opts, nil, populate)
}

type archivePublicationOps struct {
	close   func(*os.File) error
	syncDir func(string) error
}

var defaultArchivePublicationOps = archivePublicationOps{
	close:   (*os.File).Close,
	syncDir: syncDir,
}

func writeArchiveWithProgress(path string, mode os.FileMode, opts BackupOptions, progress *Progress, populate func(*tar.Writer) error) error {
	return writeArchiveWithProgressOps(path, mode, opts, progress, populate, defaultArchivePublicationOps)
}

func writeArchiveWithProgressOps(path string, mode os.FileMode, opts BackupOptions, progress *Progress, populate func(*tar.Writer) error, publish archivePublicationOps) error {
	f, err := os.CreateTemp(filepath.Dir(path), tempArchivePrefix+"*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// The lock is held until the archive is published. A concurrent sweep can
	// then tell an abandoned temporary file (lockable) from one that another
	// process is still writing (not lockable).
	if err := unix.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if err := writeArchiveToWithProgress(f, opts, progress, populate); err != nil {
		cleanup()
		return err
	}
	// os.CreateTemp starts at 0600. Keep the in-progress archive private even
	// when the requested published mode is world-readable: a failed backup was
	// never authorized for publication and may contain sensitive partial data.
	if err := f.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if opts.Overwrite {
		if err := os.Rename(tmp, path); err != nil {
			cleanup()
			return err
		}
	} else {
		// Link publishes the completed file without replacing an existing
		// archive. This matters for user-supplied names and timestamp collisions.
		if err := os.Link(tmp, path); err != nil {
			cleanup()
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%w: %s (use --overwrite to replace it)", ErrArchiveExists, path)
			}
			return err
		}
		_ = os.Remove(tmp)
	}
	if err := publish.close(f); err != nil {
		return &ArchivePublishedError{Path: path, Err: fmt.Errorf("close archive: %w", err)}
	}
	// The rename is atomic, but syncing the parent makes the new directory
	// entry durable across a power loss on filesystems that support fsync.
	if err := publish.syncDir(filepath.Dir(path)); err != nil {
		return &ArchivePublishedError{Path: path, Err: fmt.Errorf("sync archive directory: %w", err)}
	}
	return nil
}

const tempArchivePrefix = ".dvol-backup-"

// sweepStaleTempArchives removes temporary archives abandoned by a crashed
// backup. A temporary file another process is still writing keeps an exclusive
// lock and is left alone.
func (s *Store) sweepStaleTempArchives(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), tempArchivePrefix) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		f, err := os.OpenFile(path, os.O_RDWR, 0o644)
		if err != nil {
			continue
		}
		if err := unix.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			continue
		}
		_ = os.Remove(path)
		_ = f.Close()
		s.logger().Warn("removed abandoned temporary archive", LogField{Key: "path", Value: path})
	}
}

// writeArchiveToWithProgress writes one complete tar/compressed stream to w. It
// never creates an archive temporary file and is therefore suitable for stdout.
func writeArchiveToWithProgress(w io.Writer, opts BackupOptions, progress *Progress, populate func(*tar.Writer) error) error {
	if progress != nil {
		w = progressWriter{writer: w, progress: progress}
	}
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

// ListArchives returns the archive files in dir sorted by creation time.
func ListArchives(dir string) ([]BackupInfo, error) {
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
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		infos = append(infos, BackupInfo{
			Name:      e.Name(),
			Path:      p,
			Size:      info.Size(),
			CreatedAt: info.ModTime().UTC(),
		})
	}
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].CreatedAt.Equal(infos[j].CreatedAt) {
			return infos[i].Name < infos[j].Name
		}
		return infos[i].CreatedAt.Before(infos[j].CreatedAt)
	})
	return infos, nil
}

// ListArchives is the method form of the package-level ListArchives.
func (s *Store) ListArchives(dir string) ([]BackupInfo, error) { return ListArchives(dir) }

// ts returns a filesystem-safe timestamp. Nanosecond precision prevents two
// backups landing in the same second from truncating each other.
func ts() string { return time.Now().UTC().Format("20060102-150405.000000000") }

func archiveFilename(defaultName string, opts BackupOptions) string {
	base := defaultName
	if opts.ArchiveName != "" {
		base = trimArchiveSuffix(opts.ArchiveName)
	}
	if opts.Timestamp {
		timestamp := opts.TimestampValue
		if timestamp == "" {
			now := time.Now()
			if opts.TimestampUTC {
				now = now.UTC()
			}
			timestamp = now.Format(TimestampLayout)
		}
		base += "_" + timestamp
	} else if opts.ArchiveName == "" {
		base += "-" + ts()
	}
	return fmt.Sprintf("%s.tar.%s", base, opts.norm().ext())
}

// TimestampLayout is the layout used by --timestamp.
const TimestampLayout = "02012006_150405"

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

// validateBackupSource rejects symlink roots and verifies that the source can
// be opened without write access or atime side effects. Docker volume
// mountpoints are real directories, so accepting a symlink here would only
// add a race in which a path can be redirected outside the volume.
func validateBackupSource(source string, opts readOptions) (os.FileInfo, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return nil, fmt.Errorf("volume source %q: %w", source, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("volume source %q must not be a symlink", source)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("volume source %q is not a directory", source)
	}
	dir, actual, err := openReadOnlyDir(source, info, opts)
	if err != nil {
		return nil, fmt.Errorf("volume source %q cannot be opened safely: %w", source, err)
	}
	if err := dir.Close(); err != nil {
		return nil, fmt.Errorf("volume source %q: close: %w", source, err)
	}
	return actual, nil
}

// resolvedPath follows only the existing portion of path. This lets us detect
// a destination whose parent is a symlink into the source even when the final
// archive file does not exist yet.
func resolvedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	var suffix []string
	cur := abs
	for {
		if _, err := os.Lstat(cur); err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ensureBackupDestinationOutsideSources prevents the archive, temporary file,
// flat index and lock file from ever being created inside a source volume.
// Writing an archive into the volume being backed up would violate the core
// read-only contract even though the tar traversal itself only reads.
func ensureBackupDestinationOutsideSources(destination string, sources ...string) error {
	dest, err := resolvedPath(destination)
	if err != nil {
		return fmt.Errorf("resolve backup destination %q: %w", destination, err)
	}
	for _, source := range sources {
		root, err := resolvedPath(source)
		if err != nil {
			return fmt.Errorf("resolve volume source %q: %w", source, err)
		}
		if pathWithin(root, dest) {
			return fmt.Errorf("backup destination %q is inside source volume %q", destination, source)
		}
	}
	return nil
}

const baseReadOnlyFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW

// openReadOnlyFD opens name relative to dirfd for reading only, never following
// a final symlink and, by default, without updating access times.
//
// O_NOATIME is refused with EPERM for a file the caller does not own and is
// rejected outright by some filesystems. The strict default keeps the
// documented "a backup never modifies the source" invariant: rather than
// silently touching atimes the operation fails and names the flag that opts in.
func openReadOnlyFD(dirfd int, name string, extra int, opts readOptions) (int, error) {
	flags := baseReadOnlyFlags | extra
	fd, err := unix.Openat(dirfd, name, flags|unix.O_NOATIME, 0)
	if err == nil {
		return fd, nil
	}
	if !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EINVAL) {
		return -1, err
	}
	if !opts.allowAtime {
		return -1, fmt.Errorf("%w (%v); pass --allow-atime to accept access-time updates on the source", ErrAtimeUnavailable, err)
	}
	fd, retryErr := unix.Openat(dirfd, name, flags, 0)
	if retryErr != nil {
		return -1, retryErr
	}
	opts.logger.Warn("source opened without O_NOATIME", LogField{Key: "entry", Value: name}, LogField{Key: "reason", Value: err})
	return fd, nil
}

func openReadOnlyDir(path string, expected os.FileInfo, opts readOptions) (*os.File, os.FileInfo, error) {
	fd, err := openReadOnlyFD(unix.AT_FDCWD, path, unix.O_DIRECTORY, opts)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	actual, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !os.SameFile(expected, actual) || !actual.IsDir() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("source directory changed while opening")
	}
	return f, actual, nil
}

func openReadOnlyAt(parent *os.File, name string, expected os.FileInfo, opts readOptions) (*os.File, os.FileInfo, error) {
	extra := 0
	if expected.IsDir() {
		extra = unix.O_DIRECTORY
	}
	fd, err := openReadOnlyFD(int(parent.Fd()), name, extra, opts)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name))
	actual, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !os.SameFile(expected, actual) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("source entry %q changed while opening", name)
	}
	return f, actual, nil
}

// readlinkAt reads a symlink target through the parent descriptor and restores
// the access time Linux updates as a side effect.
func readlinkAt(parent *os.File, name string, expected os.FileInfo, opts readOptions) (string, error) {
	// Opening the symlink itself with O_PATH|O_NOFOLLOW avoids following it
	// and avoids the atime update caused by pathname readlink on some Linux
	// filesystems. readlinkat with an empty path reads the O_PATH descriptor.
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	var original unix.Stat_t
	if err := unix.Fstat(fd, &original); err != nil {
		return "", err
	}
	// Detect a symlink swapped in between the lstat and this open.
	if st, ok := expected.Sys().(*syscall.Stat_t); ok {
		if uint64(st.Ino) != uint64(original.Ino) || uint64(st.Dev) != uint64(original.Dev) {
			return "", fmt.Errorf("source entry %q changed while reading its link target", name)
		}
	}
	buf := make([]byte, 256)
	for {
		n, err := unix.Readlinkat(fd, "", buf)
		if err != nil {
			return "", err
		}
		if n < len(buf) {
			if err := restoreSymlinkTimes(fd, original, name, opts); err != nil {
				return "", err
			}
			return string(buf[:n]), nil
		}
		buf = append(buf, make([]byte, len(buf))...)
	}
}

// restoreSymlinkTimes puts back the access time Linux updated while the link
// target was read. On a read-only source the restore cannot be performed, but
// neither could the atime have been updated: the timestamps are re-read and the
// backup only fails when the source really did change.
func restoreSymlinkTimes(fd int, original unix.Stat_t, name string, opts readOptions) error {
	times := []unix.Timespec{original.Atim, original.Mtim}
	if err := unix.UtimesNanoAt(fd, "", times, unix.AT_EMPTY_PATH); err == nil {
		return nil
	} else {
		var current unix.Stat_t
		if statErr := unix.Fstat(fd, &current); statErr == nil &&
			current.Atim == original.Atim && current.Mtim == original.Mtim {
			// Nothing changed (read-only mount, noatime or relatime): the
			// invariant holds without the write.
			return nil
		}
		if opts.allowAtime {
			opts.logger.Warn("symlink access time not restored",
				LogField{Key: "entry", Value: name}, LogField{Key: "error", Value: err})
			return nil
		}
		return fmt.Errorf("restore symlink timestamps for %q: %w (pass --allow-atime to accept it)", name, err)
	}
}

type readOnlyTreeEntry struct {
	path   string
	rel    string
	info   os.FileInfo
	link   string
	file   *os.File
	xattrs map[string]string
}

// walkReadOnly traverses a source using directory descriptors. It never
// follows a symlink and opens regular files with O_NOATIME, so reading a
// backup cannot mutate access times on the source. Opened descriptors are
// checked against the original lstat result to detect replacement races.
func walkReadOnly(root string, opts readOptions, visit func(readOnlyTreeEntry) error) error {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	rootDir, _, err := openReadOnlyDir(root, rootInfo, opts)
	if err != nil {
		return err
	}
	defer rootDir.Close()
	return walkReadOnlyDir(rootDir, root, "", opts, visit)
}

const directoryReadBatchSize = 4096

func walkReadOnlyDir(dir *os.File, dirPath, prefix string, opts readOptions, visit func(readOnlyTreeEntry) error) error {
	// Read bounded batches: the traversal re-stats every entry anyway, and a
	// directory with millions of children must not allocate one giant name
	// slice. Sorting each batch retains stable local ordering without O(n) peak
	// memory for the whole directory.
	for {
		names, readErr := dir.Readdirnames(directoryReadBatchSize)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(dirPath, name)
			info, err := os.Lstat(path)
			if err != nil {
				if os.IsNotExist(err) {
					// The entry vanished between readdir and lstat. A live volume
					// legitimately does this; the archive simply will not contain it.
					opts.logger.Warn("source entry disappeared during backup", LogField{Key: "path", Value: path})
					continue
				}
				return err
			}
			rel := name
			if prefix != "" {
				rel = filepath.Join(prefix, name)
			}
			entry := readOnlyTreeEntry{path: path, rel: filepath.ToSlash(rel), info: info}
			switch {
			case info.Mode()&os.ModeSymlink != 0:
				entry.link, err = readlinkAt(dir, name, info, opts)
				if err != nil {
					if os.IsNotExist(err) {
						opts.logger.Warn("source entry disappeared during backup", LogField{Key: "path", Value: path})
						continue
					}
					return err
				}
				entry.xattrs, err = readPathXattrs(path)
				if err != nil {
					return fmt.Errorf("read extended attributes of symlink %q: %w", path, err)
				}
				if err := visit(entry); err != nil {
					return err
				}
			case info.IsDir() || info.Mode().IsRegular():
				child, actual, err := openReadOnlyAt(dir, name, info, opts)
				if err != nil {
					if os.IsNotExist(err) {
						opts.logger.Warn("source entry disappeared during backup", LogField{Key: "path", Value: path})
						continue
					}
					return err
				}
				entry.info = actual
				entry.file = child
				err = visit(entry)
				if err == nil && actual.IsDir() {
					err = walkReadOnlyDir(child, path, rel, opts, visit)
				}
				closeErr := child.Close()
				if err != nil {
					return err
				}
				if closeErr != nil {
					return closeErr
				}
			default:
				entry.xattrs, err = readPathXattrs(path)
				if err != nil {
					return fmt.Errorf("read extended attributes of special entry %q: %w", path, err)
				}
				if err := visit(entry); err != nil {
					return err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	return nil
}

// addTreeTo writes the volume root entry followed by every file under dir.
// With prefix "" the root entry is "." (a plain volume backup); otherwise
// entries are prefixed with "<prefix>/" (a bundle). Hard-linked files are
// written once plus TypeLink entries, preserving inode sharing in both the
// plain and the bundle layout.
func addTreeTo(tw *tar.Writer, dir, prefix string) error {
	return (&Store{}).addTree(tw, dir, prefix, nil)
}

func (s *Store) addTree(tw *tar.Writer, dir, prefix string, progress *Progress) error {
	opts := s.readOptions()
	rootInfo, err := validateBackupSource(dir, opts)
	if err != nil {
		return err
	}
	rootName := prefix
	if rootName == "" {
		rootName = "."
	} else {
		rootName = prefix + "/"
	}
	rootHeader, err := dirHeader(rootName, rootInfo, dir, opts)
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(rootHeader); err != nil {
		return err
	}
	seen := map[[2]uint64]string{} // (dev,ino) -> first archive name
	changed := 0
	skipped := 0
	err = walkReadOnly(dir, opts, func(entry readOnlyTreeEntry) error {
		name := entry.rel
		if prefix != "" {
			name = prefix + "/" + name
		}
		if progress != nil {
			progress.logger.Trace("backup entry", LogField{Key: "path", Value: name}, LogField{Key: "size", Value: entry.info.Size()})
		}
		// A unix socket cannot be represented in a tar archive. Databases and
		// message brokers routinely leave one inside their data volume, so
		// aborting the whole backup for it would make those volumes
		// un-backupable; the socket carries no state worth preserving.
		if entry.info.Mode()&os.ModeSocket != 0 {
			skipped++
			if s.cfg.StrictEntries {
				return fmt.Errorf("source entry %q is a socket and cannot be archived", entry.path)
			}
			opts.logger.Warn("skipping unsupported source entry", LogField{Key: "path", Value: entry.path}, LogField{Key: "type", Value: "socket"})
			return nil
		}
		if entry.info.Mode().IsRegular() {
			if st, ok := entry.info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
				if first, ok := seen[key]; ok {
					hdr, err := tar.FileInfoHeader(entry.info, "")
					if err != nil {
						return err
					}
					hdr.Name = name
					hdr.Typeflag = tar.TypeLink
					hdr.Linkname = first
					hdr.Size = 0
					hdr.Mode |= specialModeBits(entry.info.Mode())
					hdr.Format = tar.FormatPAX
					return tw.WriteHeader(hdr)
				}
				seen[key] = name
			}
		}
		hdr, err := tar.FileInfoHeader(entry.info, entry.link)
		if err != nil {
			return fmt.Errorf("archive header for %q: %w", entry.path, err)
		}
		hdr.Name = name
		hdr.Mode |= specialModeBits(entry.info.Mode())
		if entry.info.IsDir() {
			hdr.Name += "/"
		}
		// PAX is mandatory, not opportunistic: with an unspecified format the
		// tar writer settles on USTAR, which stores whole seconds only and
		// drops the access time entirely. A restored tree would then differ
		// from its source by up to a second on every single entry.
		hdr.Format = tar.FormatPAX
		attrs := entry.xattrs
		if entry.file != nil {
			attrs, err = readXattrs(int(entry.file.Fd()))
			if err != nil {
				return fmt.Errorf("read extended attributes of %q: %w", entry.path, err)
			}
		}
		if len(attrs) > 0 {
			hdr.PAXRecords = attrs
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !entry.info.Mode().IsRegular() {
			return nil
		}
		if entry.file == nil {
			return fmt.Errorf("regular source entry %q was not opened read-only", entry.path)
		}
		grew, err := copyFileEntry(tw, entry.file, hdr.Size, progress)
		if err != nil {
			return fmt.Errorf("archive %q: %w", entry.path, err)
		}
		if grew {
			changed++
			opts.logger.Warn("file changed while it was archived; stored the size seen at open time",
				LogField{Key: "path", Value: entry.path}, LogField{Key: "size", Value: hdr.Size})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if changed > 0 || skipped > 0 {
		opts.logger.Warn("backup completed with source anomalies",
			LogField{Key: "changed_files", Value: changed},
			LogField{Key: "skipped_entries", Value: skipped})
	}
	return nil
}

// copyFileEntry writes exactly size bytes to the archive.
//
// A live volume mutates while it is read. tar headers commit to a size before
// the payload, so a file that grows makes the tar writer reject the extra bytes
// ("write too long") and a file that shrinks leaves the entry short ("missed
// writing N bytes"): either way the whole backup used to abort. The header size
// is authoritative instead: a growing file is truncated to the size it had when
// it was opened, a shrinking file is zero-padded, and the caller warns. This is
// what GNU tar does with "file changed as we read it".
func copyFileEntry(tw *tar.Writer, f *os.File, size int64, progress *Progress) (changed bool, err error) {
	var src io.Reader = f
	if progress != nil {
		src = progressReader{reader: f, progress: progress}
	}
	copied, err := io.CopyN(tw, src, size)
	if err != nil && err != io.EOF {
		return false, err
	}
	if copied < size {
		if _, err := io.CopyN(tw, zeroReader{}, size-copied); err != nil {
			return true, err
		}
		return true, nil
	}
	var probe [1]byte
	if n, _ := f.Read(probe[:]); n > 0 {
		return true, nil
	}
	return false, nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
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

func dirHeader(name string, info os.FileInfo, path string, opts readOptions) (*tar.Header, error) {
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return nil, err
	}
	hdr.Name = name
	hdr.Typeflag = tar.TypeDir
	hdr.Mode |= specialModeBits(info.Mode())
	hdr.Format = tar.FormatPAX
	dir, _, err := openReadOnlyDir(path, info, opts)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	attrs, err := readXattrs(int(dir.Fd()))
	if err != nil {
		return nil, err
	}
	if len(attrs) > 0 {
		hdr.PAXRecords = attrs
	}
	return hdr, nil
}

// ListBackups returns the backups of a volume sorted by creation time.
func (s *Store) ListBackups(name string) ([]BackupInfo, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	nested, err := ListArchives(s.backupDir(name))
	if err != nil {
		return nil, err
	}
	flat, err := s.flatBackupInfos(name)
	if err != nil {
		return nil, err
	}
	infos := append(nested, flat...)
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].CreatedAt.Equal(infos[j].CreatedAt) {
			return infos[i].Name < infos[j].Name
		}
		return infos[i].CreatedAt.Before(infos[j].CreatedAt)
	})
	return infos, nil
}

// BundleDir returns the directory used for compose (multi-volume) bundles.
func (s *Store) BundleDir() string {
	return filepath.Join(s.backupsDir(), "compose")
}

// ResolveBackup returns an archive path for a volume backup: arg may be an
// exact or glob path, or a filename/glob resolved against backups/<vol>/.
func (s *Store) ResolveBackup(vol, arg string) (string, error) {
	if err := checkName(vol); err != nil {
		return "", err
	}
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
