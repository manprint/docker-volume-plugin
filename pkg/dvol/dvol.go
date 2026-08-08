// Package dvol implements the archive repository and backup/restore logic for
// Docker volumes, plus Docker Compose parsing.
package dvol

import (
	"errors"
	"time"
)

const (
	// DefaultRoot is the fallback archive repository location. The CLI normally
	// chooses a user-writable repository under the current user's home.
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
	// Logger receives operation, diagnostic and progress messages. It is
	// optional; nil keeps the library silent for embedders that do not need
	// diagnostics.
	Logger *Logger
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
}

var (
	ErrNotFound    = errors.New("volume not found")
	ErrExists      = errors.New("volume already exists")
	ErrInvalidName = errors.New("invalid volume name")
)
