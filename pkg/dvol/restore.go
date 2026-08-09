package dvol

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// RestoreOptions selects what a bundle restore touches.
type RestoreOptions struct {
	// Volumes limits the restore to these volumes. Empty means every volume in
	// the bundle.
	Volumes []string
	// AllowMissing downgrades "a requested volume is not in the bundle" from an
	// error to a warning. Compose restores use it because the volume list comes
	// from the compose file rather than from the operator, and a volume that did
	// not exist when the bundle was written is legitimately absent.
	AllowMissing bool
}

// manifestProbeLimit bounds how much of a leading manifest.json is buffered
// while deciding whether an archive is a bundle.
const manifestProbeLimit = 1 << 20

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
func (s *Store) RestoreSource(name, target, backupPath string) (err error) {
	progress := s.operationProgress("restore")
	s.logger().Info("restore started", LogField{Key: "volume", Value: name}, LogField{Key: "archive", Value: backupPath}, LogField{Key: "target", Value: target})
	defer func() {
		if err != nil {
			s.logger().Error("restore failed", LogField{Key: "volume", Value: name}, LogField{Key: "error", Value: err})
			return
		}
		progress.Complete()
		s.logger().Info("restore completed", LogField{Key: "volume", Value: name}, LogField{Key: "target", Value: target})
	}()
	if err := checkName(name); err != nil {
		return err
	}
	if _, err := os.Stat(backupPath); err != nil {
		return fmt.Errorf("backup not found: %w", err)
	}
	if err := checkRestoreTarget(target); err != nil {
		return err
	}
	tr, closeArchive, err := s.openArchiveForRestore(backupPath, progress)
	if err != nil {
		return err
	}
	defer closeArchive()
	return s.restoreSingleVolume(name, target, tr, progress, func() error { return closeArchive() })
}

// RestoreSourceReader is the stream equivalent of RestoreSource. The input is
// decompressed and extracted directly into staging; no archive spool file is
// created. The live target is changed only after a complete valid stream.
func (s *Store) RestoreSourceReader(name, target string, input io.Reader) (err error) {
	progress := s.operationProgress("restore stream")
	s.logger().Info("restore stream started", LogField{Key: "volume", Value: name}, LogField{Key: "target", Value: target})
	defer func() {
		if err != nil {
			s.logger().Error("restore stream failed", LogField{Key: "volume", Value: name}, LogField{Key: "error", Value: err})
			return
		}
		progress.Complete()
		s.logger().Info("restore stream completed", LogField{Key: "volume", Value: name})
	}()
	if input == nil {
		return fmt.Errorf("nil restore stream reader")
	}
	if err := checkName(name); err != nil {
		return err
	}
	if err := checkRestoreTarget(target); err != nil {
		return err
	}
	tracked := progressReader{reader: input, progress: progress, stream: true}
	tr, r, err := openArchiveReader(tracked)
	if err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished {
			_ = r.Close()
		}
	}()
	s.logger().Debug("restore stream decoder opened", LogField{Key: "volume", Value: name})
	return s.restoreSingleVolume(name, target, tr, progress, func() error {
		finished = true
		return finishArchiveReader(r)
	})
}

// restoreSingleVolume stages a single-volume archive and swaps it into target.
func (s *Store) restoreSingleVolume(name, target string, tr *tar.Reader, progress *Progress, finish func() error) error {
	staging, rec, err := s.newStaging(filepath.Dir(target), ".restore-*")
	if err != nil {
		return err
	}
	defer rec.discard()
	defer os.RemoveAll(staging)
	s.logger().Debug("restore staging created", LogField{Key: "volume", Value: name}, LogField{Key: "staging", Value: staging})

	entries := 0
	firstEntry := true
	err = extractTarStream(tr, progress, s.logger(), func(hdr *tar.Header, payload io.Reader) (string, string, io.Reader, bool, error) {
		if firstEntry {
			firstEntry = false
			// A bundle restored through the single-volume path used to succeed
			// and replace the volume with the bundle layout (manifest.json plus
			// one directory per volume), destroying the data it was pointed at.
			isBundle, buffered, err := probeBundleManifest(hdr, payload)
			if err != nil {
				return "", "", nil, false, err
			}
			if isBundle {
				return "", "", nil, false, ErrBundleArchive
			}
			if buffered != nil {
				payload = buffered
			}
		}
		entries++
		return hdr.Name, staging, payload, true, nil
	})
	if err != nil {
		return err
	}
	if err := finish(); err != nil {
		return err
	}
	if entries == 0 {
		return fmt.Errorf("backup archive contains no entries")
	}
	return s.replaceStage(rec, staging, target)
}

// RestoreBundle restores volumes from a bundle archive into the store-managed
// layout. Only volumes in names are restored (all of them if names is empty).
func (s *Store) RestoreBundle(path string, names []string) (err error) {
	return s.RestoreBundleTargets(path, RestoreOptions{Volumes: names}, func(volume string) (string, error) {
		if err := s.ensureVolume(volume); err != nil {
			return "", err
		}
		return s.VolumePath(volume), nil
	})
}

// RestoreBundleSources restores a bundle into arbitrary Docker volume
// mountpoints. Each target gets a staging directory on its own filesystem so
// the final rename also works when Docker volumes and the dvol backup store
// are on different filesystems.
func (s *Store) RestoreBundleSources(path string, targets map[string]string) error {
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return s.RestoreBundleTargets(path, RestoreOptions{Volumes: names}, StaticTargets(targets))
}

// RestoreBundleTargets restores a bundle archive, resolving each volume's
// target directory only after the manifest has been read and matched against
// the request. The whole archive is consumed in a single pass.
func (s *Store) RestoreBundleTargets(path string, opts RestoreOptions, resolve VolumeTargetResolver) (err error) {
	progress := s.operationProgress("restore bundle")
	s.logger().Info("restore bundle started", LogField{Key: "archive", Value: path}, LogField{Key: "volumes", Value: strings.Join(opts.Volumes, ",")})
	defer func() {
		if err != nil {
			s.logger().Error("restore bundle failed", LogField{Key: "error", Value: err})
			return
		}
		progress.Complete()
		s.logger().Info("restore bundle completed")
	}()
	if resolve == nil {
		return fmt.Errorf("nil volume target resolver")
	}
	tr, closeArchive, err := s.openArchiveForRestore(path, progress)
	if err != nil {
		return err
	}
	defer closeArchive()
	return s.restoreBundleStream(tr, opts, resolve, progress, func() error { return closeArchive() })
}

// RestoreBundleSourcesReader restores a complete bundle from one input stream.
// It stages each volume beside its target so it works even when volumes are on
// different filesystems, while consuming the compressed archive exactly once
// and without an archive spool file.
func (s *Store) RestoreBundleSourcesReader(input io.Reader, targets map[string]string) error {
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return s.RestoreBundleReader(input, RestoreOptions{Volumes: names}, StaticTargets(targets))
}

// RestoreBundleReader is RestoreBundleTargets for a stream.
func (s *Store) RestoreBundleReader(input io.Reader, opts RestoreOptions, resolve VolumeTargetResolver) (err error) {
	progress := s.operationProgress("restore bundle stream")
	s.logger().Info("restore bundle stream started", LogField{Key: "volumes", Value: strings.Join(opts.Volumes, ",")})
	defer func() {
		if err != nil {
			s.logger().Error("restore bundle stream failed", LogField{Key: "error", Value: err})
			return
		}
		progress.Complete()
		s.logger().Info("restore bundle stream completed")
	}()
	if input == nil {
		return fmt.Errorf("nil restore stream reader")
	}
	if resolve == nil {
		return fmt.Errorf("nil volume target resolver")
	}
	tracked := progressReader{reader: input, progress: progress, stream: true}
	tr, r, err := openArchiveReader(tracked)
	if err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished {
			_ = r.Close()
		}
	}()
	return s.restoreBundleStream(tr, opts, resolve, progress, func() error {
		finished = true
		return finishArchiveReader(r)
	})
}

type stagedVolume struct {
	name    string
	root    string // <staging>/<volume>
	staging string
	target  string
	rec     *replacement
	entries int
}

// restoreBundleStream is the single implementation behind every bundle
// restore. It reads the archive once, stages every selected volume next to its
// own target and swaps them only after the complete stream validated.
func (s *Store) restoreBundleStream(tr *tar.Reader, opts RestoreOptions, resolve VolumeTargetResolver, progress *Progress, finish func() error) (err error) {
	want := map[string]bool{}
	for _, name := range opts.Volumes {
		if err := checkName(name); err != nil {
			return fmt.Errorf("volume %q: %w", name, err)
		}
		want[name] = true
	}

	staged := map[string]*stagedVolume{}
	order := []string{}
	cleanup := func() {
		for _, v := range staged {
			v.rec.discard()
			_ = os.RemoveAll(v.staging)
		}
	}
	defer cleanup()

	manifestSeen := false
	firstEntry := true
	inManifest := map[string]bool{}
	err = extractTarStream(tr, progress, s.logger(), func(hdr *tar.Header, payload io.Reader) (string, string, io.Reader, bool, error) {
		if firstEntry {
			firstEntry = false
			isBundle, buffered, probeErr := probeBundleManifest(hdr, payload)
			if probeErr != nil {
				return "", "", nil, false, probeErr
			}
			if !isBundle {
				return "", "", nil, false, ErrNotBundleArchive
			}
			manifest, mErr := decodeBundleManifest(buffered)
			if mErr != nil {
				return "", "", nil, false, mErr
			}
			manifestSeen = true
			for _, name := range manifest.Volumes {
				inManifest[name] = true
			}
			selected, sErr := s.selectBundleVolumes(manifest, want, opts.AllowMissing)
			if sErr != nil {
				return "", "", nil, false, sErr
			}
			// Targets are resolved only now: a resolver that creates the Docker
			// volume must not run before the archive proved it carries it. Resolve
			// every target before staging and reject aliases: restoring two logical
			// volumes onto the same directory would silently let the last swap win.
			type resolvedTarget struct {
				name   string
				target string
			}
			resolved := make([]resolvedTarget, 0, len(selected))
			targetOwners := make(map[string]string, len(selected))
			for _, name := range selected {
				target, rErr := resolve(name)
				if rErr != nil {
					return "", "", nil, false, rErr
				}
				if rErr := checkRestoreTarget(target); rErr != nil {
					return "", "", nil, false, rErr
				}
				canonical, rErr := resolvedPath(target)
				if rErr != nil {
					return "", "", nil, false, fmt.Errorf("resolve restore target %q: %w", target, rErr)
				}
				if owner, exists := targetOwners[canonical]; exists {
					return "", "", nil, false, fmt.Errorf("volumes %q and %q resolve to the same restore target %q", owner, name, target)
				}
				targetOwners[canonical] = name
				resolved = append(resolved, resolvedTarget{name: name, target: target})
			}
			for _, item := range resolved {
				name, target := item.name, item.target
				staging, rec, sErr := s.newStaging(filepath.Dir(target), "."+name+".restore-*")
				if sErr != nil {
					return "", "", nil, false, sErr
				}
				root := filepath.Join(staging, name)
				if mkErr := os.MkdirAll(root, 0o755); mkErr != nil {
					rec.discard()
					_ = os.RemoveAll(staging)
					return "", "", nil, false, mkErr
				}
				staged[name] = &stagedVolume{name: name, root: root, staging: staging, target: target, rec: rec}
				order = append(order, name)
			}
			s.logger().Debug("restore bundle staging created", LogField{Key: "volumes", Value: len(staged)})
			return "", "", nil, false, nil
		}
		if !manifestSeen {
			return "", "", nil, false, ErrNotBundleArchive
		}
		vol, rest, ok := splitBundlePath(hdr.Name)
		if !ok {
			return "", "", nil, false, fmt.Errorf("malformed bundle entry %q", hdr.Name)
		}
		if !inManifest[vol] {
			// The archive carries data the manifest does not declare: it is not
			// the bundle it claims to be.
			return "", "", nil, false, fmt.Errorf("bundle contains unexpected volume %q", vol)
		}
		if hdr.Typeflag == tar.TypeLink {
			linkVol, _, linkOK := splitBundlePath(hdr.Linkname)
			if !linkOK || linkVol != vol {
				return "", "", nil, false, fmt.Errorf("bundle hard link %q points outside volume %q", hdr.Name, vol)
			}
		}
		v := staged[vol]
		if v == nil {
			return "", "", nil, false, nil // declared but not requested
		}
		rel := vol
		if rest != "" {
			rel += "/" + rest
		}
		v.entries++
		return rel, v.staging, payload, true, nil
	})
	if err != nil {
		return err
	}
	if err := finish(); err != nil {
		return err
	}
	if !manifestSeen {
		return ErrNotBundleArchive
	}
	if len(order) == 0 {
		return fmt.Errorf("bundle contains no volumes matching the request")
	}
	// Validate every staged root BEFORE touching any live volume: a single
	// malformed volume (a root swapped into a symlink by a hostile archive)
	// must not leave the already-swapped volumes half restored.
	for _, name := range order {
		v := staged[name]
		if v.entries == 0 {
			return fmt.Errorf("bundle contains no volume %q", name)
		}
		fi, err := os.Lstat(v.root)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("refusing to restore volume %q: archive entry is not a directory", name)
		}
	}
	s.logger().Debug("restore bundle staged", LogField{Key: "volumes", Value: len(order)})
	for _, name := range order {
		v := staged[name]
		if err := s.replaceStage(v.rec, v.root, v.target); err != nil {
			return err
		}
	}
	return nil
}

// selectBundleVolumes intersects the manifest with the requested volumes.
func (s *Store) selectBundleVolumes(manifest bundleManifest, want map[string]bool, allowMissing bool) ([]string, error) {
	inManifest := map[string]bool{}
	selected := make([]string, 0, len(manifest.Volumes))
	for _, name := range manifest.Volumes {
		if err := checkName(name); err != nil {
			return nil, fmt.Errorf("bundle manifest lists invalid volume %q", name)
		}
		if inManifest[name] {
			return nil, fmt.Errorf("bundle manifest lists volume %q twice", name)
		}
		inManifest[name] = true
		if len(want) == 0 || want[name] {
			selected = append(selected, name)
		}
	}
	if len(want) > 0 {
		missing := make([]string, 0)
		for name := range want {
			if !inManifest[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			if !allowMissing {
				return nil, fmt.Errorf("bundle contains no volume %q", missing[0])
			}
			s.logger().Warn("requested volumes are not in the bundle and stay untouched",
				LogField{Key: "volumes", Value: strings.Join(missing, ",")})
		}
		// A volume present in the archive but never requested is never restored,
		// and it is not an error: the operator asked for a subset.
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("bundle contains no volumes matching the request")
	}
	sort.Strings(selected)
	return selected, nil
}

// newStaging creates a staging directory registered in the replacement journal
// so a crash cannot leak it.
func (s *Store) newStaging(dir, pattern string) (string, *replacement, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	staging, err := os.MkdirTemp(dir, pattern)
	if err != nil {
		return "", nil, err
	}
	rec, err := s.beginStaging(staging)
	if err != nil {
		_ = os.RemoveAll(staging)
		return "", nil, err
	}
	return staging, rec, nil
}

func checkRestoreTarget(target string) error {
	if target == "" {
		return fmt.Errorf("empty restore target")
	}
	fi, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("volume target %q: %w", target, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("volume target %q is a symlink", target)
	}
	if !fi.IsDir() {
		return fmt.Errorf("volume target %q is not a directory", target)
	}
	return nil
}

// openArchiveForRestore opens a file archive and returns a reader plus an
// idempotent finisher that drains and validates the compression trailer.
func (s *Store) openArchiveForRestore(path string, progress *Progress) (*tar.Reader, func() error, error) {
	tr, r, f, err := openArchiveWithProgress(path, progress)
	if err != nil {
		return nil, nil, err
	}
	done := false
	closeArchive := func() error {
		if done {
			return nil
		}
		done = true
		err := finishArchiveReader(r)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}
	return tr, closeArchive, nil
}

// probeBundleManifest reports whether a header is the manifest of a dvol
// bundle. The payload is buffered so a volume that legitimately contains its
// own manifest.json can still be extracted from the buffer.
func probeBundleManifest(hdr *tar.Header, payload io.Reader) (isBundle bool, buffered *bytes.Reader, err error) {
	name := strings.TrimPrefix(hdr.Name, "./")
	if name != bundleManifestName || hdr.Typeflag != tar.TypeReg {
		return false, nil, nil
	}
	if hdr.Size > manifestProbeLimit {
		// Treating an oversized leading manifest as an ordinary volume file
		// lets a padded, otherwise valid bundle bypass archive-kind detection.
		// Refuse the ambiguous archive without buffering an attacker-controlled
		// amount of memory.
		return false, nil, fmt.Errorf("bundle manifest exceeds %d bytes", manifestProbeLimit)
	}
	data, err := io.ReadAll(io.LimitReader(payload, hdr.Size))
	if err != nil {
		return false, nil, fmt.Errorf("reading archive: %w", err)
	}
	buffered = bytes.NewReader(data)
	var mf bundleManifest
	if err := json.Unmarshal(data, &mf); err != nil || mf.Format != bundleFormat {
		return false, buffered, nil
	}
	return true, bytes.NewReader(data), nil
}

func decodeBundleManifest(r *bytes.Reader) (bundleManifest, error) {
	var mf bundleManifest
	if r == nil {
		return mf, ErrNotBundleArchive
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return mf, err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return mf, err
	}
	if err := json.Unmarshal(data, &mf); err != nil {
		return mf, fmt.Errorf("invalid bundle manifest: %w", err)
	}
	if mf.Format != bundleFormat {
		return mf, ErrNotBundleArchive
	}
	if len(mf.Volumes) == 0 {
		return mf, fmt.Errorf("bundle manifest lists no volumes")
	}
	return mf, nil
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

// routeFunc maps an archive entry to its destination. It may replace the entry
// payload (used when the first entry had to be buffered to classify it).
type routeFunc func(hdr *tar.Header, payload io.Reader) (rel, base string, body io.Reader, keep bool, err error)

// extractTarStream materialises the entries of an already-open tar stream.
//
// File, symlink and device metadata is applied as soon as the entry exists;
// only directories are deferred to the end and replayed in reverse, because a
// directory's mode could otherwise block writing its children and its mtime
// would be bumped by every entry created inside it. Deferring only directories
// keeps peak memory proportional to the number of directories rather than to
// the number of files.
func extractTarStream(tr *tar.Reader, progress *Progress, logger *Logger, route routeFunc) error {
	var dirs []fileMeta
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}
		rel, base, body, keep, err := route(hdr, tr)
		if err != nil {
			return err
		}
		if progress != nil {
			progress.logger.Trace("restore entry", LogField{Key: "path", Value: hdr.Name}, LogField{Key: "size", Value: hdr.Size})
		}
		if !keep {
			continue
		}
		if body == nil {
			body = tr
		}
		target, err := safeJoin(base, rel)
		if err != nil {
			return err
		}
		materialised, err := extractEntry(hdr, body, target, base, progress, logger)
		if err != nil {
			return err
		}
		if !materialised {
			continue
		}
		meta := fileMeta{
			path:   target,
			isLink: hdr.Typeflag == tar.TypeSymlink,
			mode:   fileModeFromTar(hdr.Mode),
			uid:    hdr.Uid,
			gid:    hdr.Gid,
			atime:  hdr.AccessTime,
			mtime:  hdr.ModTime,
			xattrs: xattrsFromPAX(hdr.PAXRecords),
		}
		if hdr.Typeflag == tar.TypeDir {
			dirs = append(dirs, meta)
			continue
		}
		if err := meta.apply(logger); err != nil {
			return err
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := dirs[i].apply(logger); err != nil {
			return err
		}
	}
	return nil
}

// fileMeta carries the metadata to apply to an extracted entry.
type fileMeta struct {
	path   string
	isLink bool
	mode   os.FileMode
	uid    int
	gid    int
	atime  time.Time
	mtime  time.Time
	xattrs map[string]string
}

func (m fileMeta) apply(logger *Logger) error {
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
	// Extended attributes come last of the permission-shaped metadata: chown
	// clears security.capability and chmod rewrites the POSIX ACL mask, so both
	// have to happen first.
	if len(m.xattrs) > 0 {
		applyXattrs(m.path, m.xattrs, logger)
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
	ts := []unix.Timespec{
		unix.NsecToTimespec(atime.UnixNano()),
		unix.NsecToTimespec(mtime.UnixNano()),
	}
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW)
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
	if vol == "" {
		return "", "", false
	}
	rest = p[i+1:]
	if rest == "" {
		return vol, "", true
	}
	// Tar paths always use '/'. Accept the conventional trailing slash on
	// directory headers, but reject every other non-canonical spelling. In
	// particular, "vol/../sibling" used to be routed outside vol's staged root
	// and could make a non-empty bundle replace the live volume with emptiness.
	trimmed := strings.TrimSuffix(rest, "/")
	clean := path.Clean(trimmed)
	if trimmed == "" || clean != trimmed || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", false
	}
	rest = clean
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

// extractEntry materialises a single archive entry at target. base is the
// volume dir, used to resolve hard links. It reports whether the entry produced
// a filesystem object: metadata must never be applied to a path that was
// skipped, which used to abort every restore of an archive containing a FIFO.
func extractEntry(hdr *tar.Header, body io.Reader, target, base string, progress *Progress, logger *Logger) (bool, error) {
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := checkNoSymlinkAncestors(base, target); err != nil {
			return false, err
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			return false, err
		}
	case tar.TypeReg:
		if err := checkNoSymlinkAncestors(base, target); err != nil {
			return false, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return false, err
		}
		of, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode&0o777))
		if err != nil {
			return false, err
		}
		reader := body
		if progress != nil {
			reader = progressReader{reader: body, progress: progress}
		}
		sparse := newSparseFileWriter(of)
		if _, err := io.Copy(sparse, io.LimitReader(reader, hdr.Size)); err != nil {
			_ = of.Close()
			return false, err
		}
		if err := sparse.Close(); err != nil {
			_ = of.Close()
			return false, err
		}
		if err := of.Close(); err != nil {
			return false, err
		}
	case tar.TypeSymlink:
		if filepath.Clean(target) == filepath.Clean(base) {
			return false, fmt.Errorf("refusing to extract symlink at volume root %q", hdr.Name)
		}
		if err := checkNoSymlinkAncestors(base, filepath.Dir(target)); err != nil {
			return false, err
		}
		if err := removeExistingEntry(target); err != nil {
			return false, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return false, err
		}
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return false, err
		}
	case tar.TypeLink:
		if err := checkNoSymlinkAncestors(base, target); err != nil {
			return false, err
		}
		if err := removeExistingEntry(target); err != nil {
			return false, err
		}
		ln, err := safeJoin(base, hdr.Linkname)
		if err != nil {
			return false, err
		}
		if err := checkNoSymlinkAncestors(base, ln); err != nil {
			return false, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return false, err
		}
		if err := os.Link(ln, target); err != nil {
			return false, err
		}
	case tar.TypeFifo:
		if err := checkNoSymlinkAncestors(base, target); err != nil {
			return false, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return false, err
		}
		if err := removeExistingEntry(target); err != nil {
			return false, err
		}
		if err := unix.Mkfifo(target, uint32(hdr.Mode&0o777)); err != nil {
			logger.Warn("FIFO not restored", LogField{Key: "path", Value: target}, LogField{Key: "error", Value: err})
			return false, nil
		}
	case tar.TypeChar, tar.TypeBlock:
		if err := checkNoSymlinkAncestors(base, target); err != nil {
			return false, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return false, err
		}
		mode := uint32(hdr.Mode & 0o777)
		if hdr.Typeflag == tar.TypeChar {
			mode |= unix.S_IFCHR
		} else {
			mode |= unix.S_IFBLK
		}
		if err := removeExistingEntry(target); err != nil {
			return false, err
		}
		if hdr.Devmajor < 0 || hdr.Devmajor > 1<<32-1 || hdr.Devminor < 0 || hdr.Devminor > 1<<32-1 {
			return false, fmt.Errorf("invalid device numbers for %q: major=%d minor=%d", hdr.Name, hdr.Devmajor, hdr.Devminor)
		}
		dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))
		maxInt := uint64(^uint(0) >> 1)
		if dev > maxInt {
			return false, fmt.Errorf("encoded device number for %q overflows int on this platform", hdr.Name)
		}
		if err := unix.Mknod(target, mode, int(dev)); err != nil {
			// Creating device nodes needs CAP_MKNOD. A restore running without
			// it must not fail outright: the rest of the volume is still valid.
			logger.Warn("device node not restored", LogField{Key: "path", Value: target}, LogField{Key: "error", Value: err})
			return false, nil
		}
	default:
		logger.Warn("skipping unsupported archive entry",
			LogField{Key: "path", Value: hdr.Name}, LogField{Key: "type", Value: string(rune(hdr.Typeflag))})
		return false, nil
	}
	return true, nil
}

func removeExistingEntry(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing archive entry %q: %w", path, err)
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

// ArchiveReport describes what an archive contains, without extracting it.
type ArchiveReport struct {
	Path     string   `json:"path"`
	Bundle   bool     `json:"bundle"`
	Volumes  []string `json:"volumes,omitempty"`
	Entries  int      `json:"entries"`
	Bytes    int64    `json:"bytes"`
	Symlinks int      `json:"symlinks"`
	Hardlink int      `json:"hardlinks"`
}

// InspectArchive classifies an archive by reading only its first entry. It is
// the cheap counterpart of VerifyArchive: no payload beyond a leading
// manifest.json is read, so it can be used to pick a bundle out of a directory.
func InspectArchive(path string) (ArchiveReport, error) {
	report := ArchiveReport{Path: path}
	tr, r, f, err := openArchiveWithProgress(path, nil)
	if err != nil {
		return report, err
	}
	defer f.Close()
	defer r.Close()
	hdr, err := tr.Next()
	if err == io.EOF {
		return report, fmt.Errorf("archive contains no entries")
	}
	if err != nil {
		return report, fmt.Errorf("reading archive: %w", err)
	}
	isBundle, buffered, err := probeBundleManifest(hdr, tr)
	if err != nil {
		return report, err
	}
	if !isBundle {
		return report, nil
	}
	manifest, err := decodeBundleManifest(buffered)
	if err != nil {
		return report, err
	}
	report.Bundle = true
	report.Volumes = manifest.Volumes
	return report, nil
}

// VerifyArchive reads an archive end to end: it decodes every tar header,
// consumes every payload and validates the compression trailer. It never
// writes anything, so it is the safe way to confirm a backup is readable
// before an incident forces a real restore.
func (s *Store) VerifyArchive(path string) (ArchiveReport, error) {
	report := ArchiveReport{Path: path}
	tr, r, f, err := openArchiveWithProgress(path, nil)
	if err != nil {
		return report, err
	}
	defer f.Close()
	first := true
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = r.Close()
			return report, fmt.Errorf("reading archive: %w", err)
		}
		var payload io.Reader = tr
		if first {
			first = false
			isBundle, buffered, probeErr := probeBundleManifest(hdr, tr)
			if probeErr != nil {
				_ = r.Close()
				return report, probeErr
			}
			if isBundle {
				manifest, mErr := decodeBundleManifest(buffered)
				if mErr != nil {
					_ = r.Close()
					return report, mErr
				}
				report.Bundle = true
				report.Volumes = manifest.Volumes
			}
			if buffered != nil {
				payload = buffered
			}
		}
		report.Entries++
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			report.Symlinks++
		case tar.TypeLink:
			report.Hardlink++
		}
		n, err := io.Copy(io.Discard, io.LimitReader(payload, hdr.Size))
		if err != nil {
			_ = r.Close()
			return report, fmt.Errorf("reading archive payload of %q: %w", hdr.Name, err)
		}
		report.Bytes += n
	}
	if err := finishArchiveReader(r); err != nil {
		return report, err
	}
	if report.Entries == 0 {
		return report, fmt.Errorf("archive contains no entries")
	}
	return report, nil
}
