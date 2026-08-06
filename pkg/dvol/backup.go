package dvol

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	if _, err := s.Get(name); err != nil {
		return "", err
	}
	bdir := s.backupDir(name)
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(bdir, fmt.Sprintf("%s-%s.tar.gz", name, ts()))

	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := addTreeTo(tw, s.VolumePath(name), ""); err != nil {
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
	path := filepath.Join(outDir, fmt.Sprintf("%s-%s.tar.gz", prefix, ts()))

	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	mf := bundleManifest{Format: bundleFormat, Created: time.Now().UTC(), Volumes: clean}
	mb, _ := json.Marshal(mf)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(mb)), Typeflag: tar.TypeReg}); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if _, err := tw.Write(mb); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}

	for _, name := range clean {
		if err := addTreeTo(tw, s.VolumePath(name), name); err != nil {
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
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

func ts() string { return time.Now().UTC().Format("20060102-150405") }

func closeTar(tw *tar.Writer, gz *gzip.Writer, f *os.File, path string) error {
	if err := tw.Close(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := gz.Close(); err != nil {
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

func dirHeader(name string, info os.FileInfo) *tar.Header {
	hdr, _ := tar.FileInfoHeader(info, "")
	hdr.Name = name
	hdr.Typeflag = tar.TypeDir
	return hdr
}

// Restore replaces the volume data with the contents of a backup archive.
// If the volume does not exist it is created first.
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
	if err := os.RemoveAll(vdir); err != nil {
		return err
	}
	if err := os.MkdirAll(vdir, 0o755); err != nil {
		return err
	}

	tr, gz, f, err := openTar(backupPath)
	if err != nil {
		return err
	}
	defer f.Close()
	defer gz.Close()
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}
		target, err := safeJoin(vdir, hdr.Name)
		if err != nil {
			return err
		}
		if err := extractEntry(hdr, tr, target, vdir); err != nil {
			return err
		}
	}
	return nil
}

// RestoreBundle restores volumes from a bundle archive. Only volumes in names
// are restored (all of them if names is empty). Each volume is replaced.
func (s *Store) RestoreBundle(path string, names []string) error {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	tr, gz, f, err := openTar(path)
	if err != nil {
		return err
	}
	defer f.Close()
	defer gz.Close()

	wipeda := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}
		if hdr.Name == "manifest.json" {
			continue
		}
		vol, rest, ok := splitBundlePath(hdr.Name)
		if !ok {
			return fmt.Errorf("malformed bundle entry %q", hdr.Name)
		}
		if len(want) > 0 && !want[vol] {
			continue
		}
		if !wipeda[vol] {
			if err := s.ensureVolume(vol); err != nil {
				return err
			}
			vdir := s.VolumePath(vol)
			if err := os.RemoveAll(vdir); err != nil {
				return err
			}
			if err := os.MkdirAll(vdir, 0o755); err != nil {
				return err
			}
			wipeda[vol] = true
		}
		vdir := s.VolumePath(vol)
		if rest == "" {
			// the <vol>/ root entry: apply perms to the volume dir
			if hdr.Typeflag == tar.TypeDir {
				os.Chmod(vdir, os.FileMode(hdr.Mode))
				os.Chown(vdir, hdr.Uid, hdr.Gid)
			}
			continue
		}
		target, err := safeJoin(vdir, rest)
		if err != nil {
			return err
		}
		if err := extractEntry(hdr, tr, target, vdir); err != nil {
			return err
		}
	}
	return nil
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

func openTar(path string) (*tar.Reader, *gzip.Reader, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, nil, nil, err
	}
	return tar.NewReader(gz), gz, f, nil
}

// extractEntry materializes a single archive entry at target. base is the
// volume dir, used to resolve hard links.
func extractEntry(hdr *tar.Header, tr *tar.Reader, target, base string) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
			return err
		}
		os.Chmod(target, os.FileMode(hdr.Mode))
		os.Chown(target, hdr.Uid, hdr.Gid)
	case tar.TypeReg:
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
		os.Chown(target, hdr.Uid, hdr.Gid)
	case tar.TypeSymlink:
		os.Remove(target)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return err
		}
	case tar.TypeLink:
		os.Remove(target)
		ln, err := safeJoin(base, hdr.Linkname)
		if err != nil {
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

// safeJoin resolves p inside base and rejects escapes (zip-slip).
func safeJoin(base, p string) (string, error) {
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
