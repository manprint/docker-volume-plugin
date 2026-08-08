package dvol

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/klauspost/pgzip"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
)

// Compression is the archive compression format.
type Compression string

const (
	CompressionGzip Compression = "gzip"
	CompressionXz   Compression = "xz"
	CompressionZstd Compression = "zstd"
	CompressionLz4  Compression = "lz4"
)

// BackupOptions controls the compression of backup archives.
type BackupOptions struct {
	// Compression format; empty means gzip (backward-compatible default).
	Compression Compression
	// Level is the compression level; 0 means the balanced default for the
	// selected format. gzip: 1-9, xz: 0-9 presets, zstd: 1-22, lz4: 1-9
	// high-compression presets.
	Level int
	// Threads is the requested compression concurrency; 0 means half of the
	// host CPU count (minimum 1). xz is currently single-threaded and ignores
	// the default value, but accepts an explicit value of 1.
	Threads int
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
	// Overwrite permits replacing an existing archive atomically. The default
	// is collision-safe: an existing archive causes an error.
	Overwrite bool
	// TimestampUTC selects UTC for generated timestamp names. The default is
	// the local timezone for backward compatibility.
	TimestampUTC bool
}

func (o BackupOptions) norm() BackupOptions {
	if o.Compression == "" {
		o.Compression = CompressionGzip
	}
	return o
}

// archiveSuffixes are the suffixes trimmed from a caller-supplied archive name
// so `--archive-name foo.tar.zst` and `--archive-name foo` behave the same.
var archiveSuffixes = []string{".tar.gz", ".tar.xz", ".tar.zst", ".tar.zstd", ".tar.lz4"}

func trimArchiveSuffix(name string) string {
	for _, suffix := range archiveSuffixes {
		if trimmed, ok := strings.CutSuffix(name, suffix); ok {
			return trimmed
		}
	}
	return name
}

// Validate reports an error for an unsupported format or out-of-range level.
func (o BackupOptions) Validate() error {
	if o.ArchiveName != "" {
		if err := checkArchiveStem(trimArchiveSuffix(o.ArchiveName)); err != nil {
			return err
		}
	}
	if o.TimestampValue != "" {
		if err := checkArchiveStem(o.TimestampValue); err != nil {
			return fmt.Errorf("invalid timestamp value: %w", err)
		}
	}
	switch o.norm().Compression {
	case CompressionGzip:
		if o.Level < 0 || o.Level > 9 {
			return fmt.Errorf("invalid gzip level %d (1-9, 0 = balanced default)", o.Level)
		}
	case CompressionXz:
		if o.Level < 0 || o.Level > 9 {
			return fmt.Errorf("invalid xz level %d (0-9 presets, 0 = default)", o.Level)
		}
		if o.Threads < 0 || o.Threads > 1 {
			return fmt.Errorf("xz supports only one compression thread")
		}
	case CompressionZstd:
		if o.Level < 0 || o.Level > 22 {
			return fmt.Errorf("invalid zstd level %d (1-22, 0 = balanced default)", o.Level)
		}
	case CompressionLz4:
		if o.Level < 0 || o.Level > 9 {
			return fmt.Errorf("invalid lz4 level %d (1-9 high-compression presets, 0 = balanced default)", o.Level)
		}
	default:
		return fmt.Errorf("unsupported compression %q (use gzip, xz, zstd or lz4)", o.Compression)
	}
	if o.Threads < 0 {
		return fmt.Errorf("invalid compression threads %d (must be >= 0)", o.Threads)
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
	case CompressionLz4:
		return "lz4"
	default:
		return "gz"
	}
}

func balancedLevel(compression Compression) int {
	switch compression {
	case CompressionGzip, CompressionXz, CompressionLz4:
		return 6
	case CompressionZstd:
		return 6
	default:
		return 0
	}
}

func compressionThreads(threads int) int {
	if threads > 0 {
		return threads
	}
	n := runtime.NumCPU() / 2
	if n < 1 {
		return 1
	}
	return n
}

// lz4CompressionLevel maps the 1-9 CLI presets onto pierrec/lz4 levels
// (Level1 = 1<<9 ... Level9 = 1<<17). Level 0 means "balanced default", which
// is 6 like every other format, so lz4.Fast is never selected implicitly.
func lz4CompressionLevel(level int) lz4.CompressionLevel {
	if level <= 0 {
		level = balancedLevel(CompressionLz4)
	}
	return lz4.CompressionLevel(1 << (8 + level))
}

// newWriter wraps w with the configured compressor.
func (o BackupOptions) newWriter(w io.Writer) (io.WriteCloser, error) {
	o = o.norm()
	switch o.Compression {
	case CompressionGzip:
		lvl := o.Level
		if lvl == 0 {
			lvl = balancedLevel(CompressionGzip)
		}
		zw, err := pgzip.NewWriterLevel(w, lvl)
		if err != nil {
			return nil, err
		}
		if err := zw.SetConcurrency(1<<20, compressionThreads(o.Threads)); err != nil {
			_ = zw.Close()
			return nil, err
		}
		return zw, nil
	case CompressionXz:
		if o.Level == 0 {
			o.Level = balancedLevel(CompressionXz)
		}
		cfg := xz.WriterConfig{DictCap: xzDictCap(o.Level)}
		return cfg.NewWriter(w)
	case CompressionZstd:
		level := o.Level
		if level == 0 {
			level = balancedLevel(CompressionZstd)
		}
		var eopts []zstd.EOption
		eopts = append(eopts,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
			zstd.WithEncoderConcurrency(compressionThreads(o.Threads)),
		)
		return zstd.NewWriter(w, eopts...)
	case CompressionLz4:
		zw := lz4.NewWriter(w)
		if err := zw.Apply(
			lz4.CompressionLevelOption(lz4CompressionLevel(o.Level)),
			lz4.ConcurrencyOption(compressionThreads(o.Threads)),
		); err != nil {
			return nil, err
		}
		return zw, nil
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
	return strings.HasSuffix(n, ".tar.gz") || strings.HasSuffix(n, ".tar.xz") ||
		strings.HasSuffix(n, ".tar.zst") || strings.HasSuffix(n, ".tar.lz4")
}

// openArchive opens a backup archive, detecting the compression format from
// the file extension. Returns tar reader, decompressor and file; callers must
// close both the decompressor and the file.
func openArchive(path string) (*tar.Reader, io.ReadCloser, *os.File, error) {
	return openArchiveWithProgress(path, nil)
}

func openArchiveWithProgress(path string, progress *Progress) (*tar.Reader, io.ReadCloser, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	var r io.ReadCloser
	input := io.Reader(f)
	if progress != nil {
		input = progressReader{reader: f, progress: progress, stream: true}
	}
	switch {
	case strings.HasSuffix(path, ".tar.gz"):
		g, e := gzip.NewReader(input)
		if e == nil {
			r = g
		}
		err = e
	case strings.HasSuffix(path, ".tar.xz"):
		x, e := xz.NewReader(input)
		if e == nil {
			r = io.NopCloser(x)
		}
		err = e
	case strings.HasSuffix(path, ".tar.zst"):
		z, e := zstd.NewReader(input)
		if e == nil {
			r = readCloser{
				Reader: z,
				close:  func() error { z.Close(); return nil },
			}
		}
		err = e
	case strings.HasSuffix(path, ".tar.lz4"):
		r = readCloser{
			Reader: lz4.NewReader(input),
			close:  func() error { return nil },
		}
	default:
		f.Close()
		return nil, nil, nil, fmt.Errorf("unsupported archive format %q (use .tar.gz, .tar.xz, .tar.zst or .tar.lz4)", path)
	}
	if err != nil {
		f.Close()
		return nil, nil, nil, err
	}
	return tar.NewReader(r), r, f, nil
}

// openArchiveReader detects a supported compression stream from its magic
// bytes. Unlike openArchive it does not require a path or a temporary file and
// is used by restore --stdin.
func openArchiveReader(input io.Reader) (*tar.Reader, io.ReadCloser, error) {
	br := bufio.NewReader(input)
	header, err := br.Peek(6)
	if err != nil && len(header) < 4 {
		return nil, nil, fmt.Errorf("reading archive stream header: %w", err)
	}
	var r io.ReadCloser
	switch {
	case len(header) >= 2 && header[0] == 0x1f && header[1] == 0x8b:
		g, e := gzip.NewReader(br)
		if e != nil {
			return nil, nil, e
		}
		r = g
	case len(header) >= 6 && string(header[:6]) == "\xfd7zXZ\x00":
		x, e := xz.NewReader(br)
		if e != nil {
			return nil, nil, e
		}
		r = io.NopCloser(x)
	case len(header) >= 4 && header[0] == 0x28 && header[1] == 0xb5 && header[2] == 0x2f && header[3] == 0xfd:
		z, e := zstd.NewReader(br)
		if e != nil {
			return nil, nil, e
		}
		r = readCloser{Reader: z, close: func() error { z.Close(); return nil }}
	case len(header) >= 4 && header[0] == 0x04 && header[1] == 0x22 && header[2] == 0x4d && header[3] == 0x18:
		r = readCloser{Reader: lz4.NewReader(br), close: func() error { return nil }}
	default:
		return nil, nil, fmt.Errorf("unsupported archive stream (expected gzip, xz, zstd or lz4)")
	}
	return tar.NewReader(r), r, nil
}

// readCloser couples a Reader with a Close func, adapting types whose Close
// does not return an error (e.g. *zstd.Decoder) to io.ReadCloser.
type readCloser struct {
	io.Reader
	close func() error
}

func (rc readCloser) Close() error { return rc.close() }
