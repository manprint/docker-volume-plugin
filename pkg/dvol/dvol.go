// Package dvol implements the archive repository and backup/restore logic for
// Docker volumes, plus Docker Compose parsing.
package dvol

import (
	"errors"
	"fmt"
	"os"
	"time"
)

const (
	// DefaultRoot is the fallback archive repository location. The CLI normally
	// chooses a user-writable repository under the current user's home.
	DefaultRoot = "/var/lib/dvol"

	volumesDir = "volumes"
	metaDir    = "meta"
	backupsDir = "backups"
	stateDir   = "state"

	// DefaultArchiveMode is the permission applied to published archives and to
	// the flat backup index.
	DefaultArchiveMode os.FileMode = 0o644
)

// Config carries the paths and policies used by the store.
type Config struct {
	Root string
	// BackupsDir is where archives are stored. Empty keeps them under
	// <Root>/backups. Useful to push backups to a dedicated directory.
	BackupsDir string
	// Logger receives operation, diagnostic and progress messages. It is
	// optional; nil keeps the library silent for embedders that do not need
	// diagnostics.
	Logger *Logger
	// AllowAtime permits reading a source whose files cannot be opened with
	// O_NOATIME (a file owned by another user when not running as root, or a
	// filesystem that rejects the flag such as some NFS mounts). The default is
	// the strict behaviour: rather than silently updating the source access
	// times, the backup fails and asks the operator to opt in.
	AllowAtime bool
	// ArchiveMode is the permission applied to published archives and to the
	// flat backup index. Zero means DefaultArchiveMode (0644).
	ArchiveMode os.FileMode
	// SkipUnsupportedEntries keeps a backup running when the source contains an
	// entry that cannot be represented in a tar archive (unix sockets). It is
	// enabled by default; set StrictEntries to turn those into errors.
	StrictEntries bool
	// BestEffortMetadata lets restore continue after it cannot recreate a FIFO,
	// device node, extended attribute, ACL or file capability. The safe default
	// is strict: an incomplete result returns an error and is not published.
	BestEffortMetadata bool
}

func (c Config) archiveMode() os.FileMode {
	if c.ArchiveMode == 0 {
		return DefaultArchiveMode
	}
	return c.ArchiveMode
}

// Volume is a named volume and its metadata.
type Volume struct {
	Name      string
	CreatedAt time.Time
	Options   map[string]string
	// Size is best-effort size in bytes of the volume data.
	Size int64
}

// Store manages backup archives and the staging data used by backup/restore.
type Store struct {
	cfg Config
	// replaceRename is an internal fault-injection seam for the directory-swap
	// transaction tests. Production stores leave it nil and use os.Rename.
	replaceRename func(oldPath, newPath string) error
}

// VolumeTargetResolver returns the directory a bundle volume must be restored
// into. It is invoked only after the bundle manifest has been read and checked
// against the requested volume list, so a resolver that creates the underlying
// Docker volume cannot leave stray volumes behind when the archive turns out
// not to match the request.
type VolumeTargetResolver func(volume string) (string, error)

// StaticTargets adapts a plain volume->directory map to a VolumeTargetResolver.
func StaticTargets(targets map[string]string) VolumeTargetResolver {
	return func(volume string) (string, error) {
		target, ok := targets[volume]
		if !ok || target == "" {
			return "", fmt.Errorf("no restore target for volume %q", volume)
		}
		return target, nil
	}
}

var (
	ErrNotFound    = errors.New("volume not found")
	ErrExists      = errors.New("volume already exists")
	ErrInvalidName = errors.New("invalid volume name")
	// ErrBundleArchive is returned when a multi-volume bundle is passed to a
	// single-volume restore. Restoring it would replace the volume with the
	// bundle layout (manifest.json plus one directory per volume).
	ErrBundleArchive = errors.New("archive is a multi-volume dvol bundle: restore it with --bundle")
	// ErrNotBundleArchive is returned when a single-volume archive is passed to
	// a bundle restore.
	ErrNotBundleArchive = errors.New("archive is not a dvol bundle: restore it without --bundle")
	// ErrArchiveExists is returned when publishing an archive would replace an
	// existing one and --overwrite was not requested.
	ErrArchiveExists = errors.New("backup archive already exists")
	// ErrArchivePublished classifies a late durability error after the final
	// archive path has already become visible. Callers must not retry blindly:
	// the Path carried by ArchivePublishedError names the completed archive.
	ErrArchivePublished = errors.New("backup archive was published but durability confirmation failed")
	// ErrAtimeUnavailable is returned when the source cannot be read without
	// updating its access times and AllowAtime is not set.
	ErrAtimeUnavailable = errors.New("source cannot be read without updating access times")
)

// ArchivePublishedError reports that archive publication succeeded, but a
// subsequent close or parent-directory sync failed. The archive at Path is
// complete; only its persistence across an immediate system failure is
// uncertain.
type ArchivePublishedError struct {
	Path string
	Err  error
}

func (e *ArchivePublishedError) Error() string {
	return fmt.Sprintf("%v: %s: %v; the completed archive exists, verify it before retrying", ErrArchivePublished, e.Path, e.Err)
}

func (e *ArchivePublishedError) Unwrap() []error {
	return []error{ErrArchivePublished, e.Err}
}
