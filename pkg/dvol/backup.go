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
	bdir := s.backupDir(name)
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(bdir, fmt.Sprintf("%s-%s.tar.%s", name, ts(), opts.norm().ext()))

	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	gz, err := opts.newWriter(f)
	if err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	tw := tar.NewWriter(gz)
	if err := addTreeTo(tw, s.VolumePath(name), ""); err != nil {
		tw.Close()
		gz.Close()
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := closeTar(tw, gz, f, path); err != nil {
		return "", err
	}
	return path, nil
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

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(outDir, fmt.Sprintf("%s-%s.tar.%s", prefix, ts(), opts.norm().ext()))

	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	gz, err := opts.newWriter(f)
	if err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	tw := tar.NewWriter(gz)

	mf := bundleManifest{Format: bundleFormat, Created: time.Now().UTC(), Volumes: clean}
	mb, _ := json.Marshal(mf)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(mb)), Typeflag: tar.TypeReg}); err != nil {
		tw.Close()
		gz.Close()
		f.Close()
		os.Remove(path)
		return "", err
	}
	if _, err := tw.Write(mb); err != nil {
		tw.Close()
		gz.Close()
		f.Close()
		os.Remove(path)
		return "", err
	}

	for _, name := range clean {
		if err := addTreeTo(tw, s.VolumePath(name), name); err != nil {
			tw.Close()
			gz.Close()
			f.Close()
			os.Remove(path)
			return "", err
		}
	}
	if err := closeTar(tw, gz, f, path); err != nil {
		return "", err
	}
	return path, nil
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

func closeTar(tw *tar.Writer, w io.Closer, f *os.File, path string) error {
	if err := tw.Close(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := w.Close(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// addTreeTo writes the volume root entry followed by every file under dir.
// With prefix "" the root entry is "." (a plain volume backup); otherwise
// entries are prefixed with "<prefix>/" (a bundle).
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
	vdir := s.VolumePath(name)
	staging, err := os.MkdirTemp(filepath.Dir(vdir), ".restore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := extractTarTo(staging, backupPath, func(name string) (string, bool, error) {
		return name, true, nil
	}); err != nil {
		return err
	}
	return replaceStage(staging, vdir)
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
	for _, e := range entries {
		vol := e.Name()
		if err := s.ensureVolume(vol); err != nil {
			return err
		}
		if err := replaceStage(filepath.Join(staging, vol), s.VolumePath(vol)); err != nil {
			return err
		}
	}
	return nil
}

// replaceStage atomically swaps the fully-extracted staging directory into
// target, replacing whatever was there.
func replaceStage(staging, target string) error {
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	return os.Rename(staging, target)
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
	var metas []fileMeta
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}
		rel, ok, err := filter(hdr.Name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		target, err := safeJoin(dir, rel)
		if err != nil {
			return err
		}
		if err := extractEntry(hdr, tr, target, dir); err != nil {
			return err
		}
		metas = append(metas, fileMeta{
			path:   target,
			isLink: hdr.Typeflag == tar.TypeSymlink,
			mode:   os.FileMode(hdr.Mode),
			uid:    hdr.Uid,
			gid:    hdr.Gid,
			atime:  hdr.AccessTime,
			mtime:  hdr.ModTime,
		})
	}
	// apply metadata in reverse entry order so children are handled before
	// parents and a directory's mtime survives its own content updates.
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
	// special bits (setuid/setgid/sticky) only take effect via os.Chmod, not
	// through OpenFile's create mode.
	if err := os.Chmod(m.path, m.mode); err != nil {
		return err
	}
	if m.isLink {
		if err := os.Lchown(m.path, m.uid, m.gid); err != nil {
			return err
		}
	} else if err := os.Chown(m.path, m.uid, m.gid); err != nil {
		return err
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
	return s.ListArchives(s.backupDir(name))
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
	if err == nil && len(matches) == 1 {
		return matches[0], nil
	}
	return "", fmt.Errorf("no backup matching %q for volume %q", arg, vol)
}
