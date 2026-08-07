// Package dvol implements the core of the Docker volume plugin: a local
// volume store plus backup/restore on the host filesystem.
package dvol

import (
	"errors"
	"time"
)

const (
	// DefaultRoot is the backing store location on the host. The plugin
	// bind-mounts it at /data inside its rootfs.
	DefaultRoot = "/var/lib/dvol"

	volumesDir = "volumes"
	metaDir    = "meta"
	backupsDir = "backups"
)

// Config carries the paths used by the store.
type Config struct {
	Root string
	// BackupsDir is where archives are stored. Empty keeps them under
	// <Root>/backups. Useful to push backups to a dedicated directory.
	BackupsDir string
}

// Volume is a named volume and its metadata.
type Volume struct {
	Name      string
	CreatedAt time.Time
	Options   map[string]string
	// Size is best-effort size in bytes of the volume data.
	Size int64
}

// Store manages volumes on disk.
type Store struct {
	cfg Config
}

var (
	ErrNotFound    = errors.New("volume not found")
	ErrExists      = errors.New("volume already exists")
	ErrInvalidName = errors.New("invalid volume name")
)
