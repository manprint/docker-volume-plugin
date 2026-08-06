package dvol

import (
	"archive/tar"
	"compress/gzip"
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
	ts := time.Now().UTC().Format("20060102-150405")
	path := filepath.Join(bdir, fmt.Sprintf("%s-%s.tar.gz", name, ts))

	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	if err := s.addTree(tw, s.VolumePath(name)); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := tw.Close(); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := gz.Close(); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// addTree writes the volume root entry "." followed by every file under dir.
func (s *Store) addTree(tw *tar.Writer, dir string) error {
	rootInfo, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(dirHeader(".", rootInfo)); err != nil {
		return err
	}
	return filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil // already written
		}
		name := filepath.ToSlash(strings.TrimPrefix(p, dir+string(filepath.Separator)))
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
	if _, err := s.Get(name); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := s.Create(name, nil); err != nil {
			return err
		}
	}
	vdir := s.VolumePath(name)
	if err := os.RemoveAll(vdir); err != nil {
		return err
	}
	if err := os.MkdirAll(vdir, 0o755); err != nil {
		return err
	}

	f, err := os.Open(backupPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
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
			ln, err := safeJoin(vdir, hdr.Linkname)
			if err != nil {
				return err
			}
			if err := os.Link(ln, target); err != nil {
				return err
			}
		default:
			// skip fifos, devices, char, etc.
		}
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
	entries, err := os.ReadDir(s.backupDir(name))
	if err != nil {
		if os.IsNotExist(err) {
			return []BackupInfo{}, nil
		}
		return nil, err
	}
	infos := make([]BackupInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(s.backupDir(name), e.Name())
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
