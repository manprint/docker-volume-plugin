package dvol

import (
	"io"
	"os"
)

// archive/tar cannot write sparse files, so a hole in the source is stored as a
// run of zeros. Materialising that run byte for byte on restore turns a 100 GB
// sparse file with 1 GB of data into 100 GB of blocks. sparseFileWriter keeps
// the restored file byte-identical while seeking over fully-zero chunks so the
// filesystem leaves them as holes, which is what cp --sparse and rsync -S do.

const (
	// sparseChunk is how much is buffered before a flush. Large enough that
	// dense files are written in few syscalls.
	sparseChunk = 1 << 20
	// sparseBlock is the granularity at which holes are detected. Scanning the
	// buffer in filesystem-block units and coalescing the runs keeps the writes
	// large while still reproducing a hole that starts a few kilobytes into the
	// file: a 1 MiB granularity would allocate a whole megabyte around every
	// isolated byte of data.
	sparseBlock = 4096
)

type sparseFileWriter struct {
	file    *os.File
	pending int64 // zero bytes skipped but not yet materialised
	written int64
	buf     []byte
}

func newSparseFileWriter(f *os.File) *sparseFileWriter {
	return &sparseFileWriter{file: f, buf: make([]byte, 0, sparseChunk)}
}

func (w *sparseFileWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		space := sparseChunk - len(w.buf)
		n := len(p)
		if n > space {
			n = space
		}
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		if len(w.buf) == sparseChunk {
			if err := w.flushChunk(); err != nil {
				return 0, err
			}
		}
	}
	return total, nil
}

// flushChunk splits the buffer into maximal runs of zero and non-zero blocks,
// seeking over the former and writing the latter in one call each.
func (w *sparseFileWriter) flushChunk() error {
	buf := w.buf
	for len(buf) > 0 {
		zero := isAllZero(buf[:min(sparseBlock, len(buf))])
		end := 0
		for end < len(buf) {
			next := min(end+sparseBlock, len(buf))
			if isAllZero(buf[end:next]) != zero {
				break
			}
			end = next
		}
		if zero {
			w.pending += int64(end)
		} else {
			if w.pending > 0 {
				if _, err := w.file.Seek(w.pending, io.SeekCurrent); err != nil {
					return err
				}
				w.pending = 0
			}
			if _, err := w.file.Write(buf[:end]); err != nil {
				return err
			}
		}
		w.written += int64(end)
		buf = buf[end:]
	}
	w.buf = w.buf[:0]
	return nil
}

// Close flushes the tail and materialises a trailing hole by truncating the
// file to the exact size that was written.
func (w *sparseFileWriter) Close() error {
	if err := w.flushChunk(); err != nil {
		return err
	}
	if w.pending > 0 {
		if err := w.file.Truncate(w.written); err != nil {
			return err
		}
		w.pending = 0
	}
	return nil
}

func isAllZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
