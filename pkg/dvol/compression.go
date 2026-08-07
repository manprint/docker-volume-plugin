package dvol

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Compression is the archive compression format.
type Compression string

const (
	CompressionGzip Compression = "gzip"
	CompressionXz   Compression = "xz"
	CompressionZstd Compression = "zstd"
)

// BackupOptions controls the compression of backup archives.
type BackupOptions struct {
	// Compression format; empty means gzip (backward-compatible default).
	Compression Compression
	// Level is the compression level; 0 means the format default.
	// gzip: 1-9, xz: 0-9 presets, zstd: 1-22.
	Level int
	// ArchiveName is an optional archive name stem. The compression suffix is
	// added automatically; a .tar.* suffix supplied by callers is accepted.
	ArchiveName string
	// Timestamp adds _ddmmyyyy_HHMMSS to ArchiveName (or the volume name).
	Timestamp bool
	// TimestampValue is an optional timestamp captured by the caller and
	// reused for every archive in one operation. Empty means generate it when
	// the archive name is built.
	TimestampValue string
	// SameDestination stores a single-volume archive directly in the backup
	// destination instead of creating a directory named after the volume.
	SameDestination bool
}

func (o BackupOptions) norm() BackupOptions {
	if o.Compression == "" {
		o.Compression = CompressionGzip
	}
	return o
}

// Validate reports an error for an unsupported format or out-of-range level.
func (o BackupOptions) Validate() error {
	if o.ArchiveName != "" {
		for _, suffix := range []string{".tar.gz", ".tar.xz", ".tar.zst", ".tar.zstd"} {
			o.ArchiveName = strings.TrimSuffix(o.ArchiveName, suffix)
		}
		if o.ArchiveName == "" || o.ArchiveName == "." || o.ArchiveName == ".." ||
			strings.ContainsAny(o.ArchiveName, `/\\:`) {
			return fmt.Errorf("invalid archive name %q (use a file name, not a path)", o.ArchiveName)
		}
	}
	switch o.norm().Compression {
	case CompressionGzip:
		if o.Level < 0 || o.Level > 9 {
			return fmt.Errorf("invalid gzip level %d (1-9, 0 = default)", o.Level)
		}
	case CompressionXz:
		if o.Level < 0 || o.Level > 9 {
			return fmt.Errorf("invalid xz level %d (0-9 presets, 0 = default)", o.Level)
		}
	case CompressionZstd:
		if o.Level < 0 || o.Level > 22 {
			return fmt.Errorf("invalid zstd level %d (1-22, 0 = default)", o.Level)
		}
	default:
		return fmt.Errorf("unsupported compression %q (use gzip, xz or zstd)", o.Compression)
	}
	return nil
}

// ext returns the file extension (without dot) for the archive: gz, xz, zst.
func (o BackupOptions) ext() string {
	switch o.norm().Compression {
	case CompressionXz:
		return "xz"
	case CompressionZstd:
		return "zst"
	default:
		return "gz"
	}
}

// newWriter wraps w with the configured compressor.
func (o BackupOptions) newWriter(w io.Writer) (io.WriteCloser, error) {
	o = o.norm()
	switch o.Compression {
	case CompressionGzip:
		lvl := o.Level
		if lvl == 0 {
			lvl = gzip.DefaultCompression
		}
		return gzip.NewWriterLevel(w, lvl)
	case CompressionXz:
		cfg := xz.WriterConfig{DictCap: xzDictCap(o.Level)}
		return cfg.NewWriter(w)
	case CompressionZstd:
		var eopts []zstd.EOption
		if o.Level != 0 {
			eopts = append(eopts, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(o.Level)))
		}
		return zstd.NewWriter(w, eopts...)
	}
	return nil, fmt.Errorf("unsupported compression %q", o.Compression)
}

// xzDictCap maps the xz -0..-9 presets to LZMA2 dictionary sizes (CLI 7z/lzma
// convention). DictCap must be a power of two; 0 means the xz default preset.
func xzDictCap(level int) int {
	switch level {
	case 0:
		return 8 << 20 // default preset 6
	case 1:
		return 1 << 20
	case 2:
		return 2 << 20
	case 3, 4:
		return 4 << 20
	case 5, 6:
		return 8 << 20
	case 7:
		return 16 << 20
	case 8:
		return 32 << 20
	case 9:
		return 64 << 20
	}
	return 8 << 20
}

// isArchiveName reports whether n looks like a dvol backup archive.
func isArchiveName(n string) bool {
	return strings.HasSuffix(n, ".tar.gz") || strings.HasSuffix(n, ".tar.xz") || strings.HasSuffix(n, ".tar.zst")
}

// openArchive opens a backup archive, detecting the compression format from
// the file extension. Returns tar reader, decompressor and file; callers must
// close both the decompressor and the file.
func openArchive(path string) (*tar.Reader, io.ReadCloser, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	var r io.ReadCloser
	switch {
	case strings.HasSuffix(path, ".tar.gz"):
		g, e := gzip.NewReader(f)
		if e == nil {
			r = g
		}
		err = e
	case strings.HasSuffix(path, ".tar.xz"):
		x, e := xz.NewReader(f)
		if e == nil {
			r = io.NopCloser(x)
		}
		err = e
	case strings.HasSuffix(path, ".tar.zst"):
		z, e := zstd.NewReader(f)
		if e == nil {
			r = readCloser{
				Reader: z,
				close:  func() error { z.Close(); return nil },
			}
		}
		err = e
	default:
		f.Close()
		return nil, nil, nil, fmt.Errorf("unsupported archive format %q (use .tar.gz, .tar.xz or .tar.zst)", path)
	}
	if err != nil {
		f.Close()
		return nil, nil, nil, err
	}
	return tar.NewReader(r), r, f, nil
}

// readCloser couples a Reader with a Close func, adapting types whose Close
// does not return an error (e.g. *zstd.Decoder) to io.ReadCloser.
type readCloser struct {
	io.Reader
	close func() error
}

func (rc readCloser) Close() error { return rc.close() }
