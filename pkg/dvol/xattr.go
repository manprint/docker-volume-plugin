package dvol

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// Extended attributes carry POSIX ACLs (system.posix_acl_access), file
// capabilities (security.capability) and SELinux labels. Dropping them silently
// turns a restored volume into something subtly different from the source, so
// they travel in the archive as PAX records using the SCHILY.xattr.* convention
// understood by GNU tar and bsdtar.

const xattrPAXPrefix = "SCHILY.xattr."

// xattrsUnsupported reports whether an error means "this filesystem or this
// caller cannot do xattrs", as opposed to a real failure.
//
// ERANGE is deliberately absent. It does not mean "unsupported": it means the
// attribute set grew between the sizing call and the reading call, and treating
// it as unsupported silently dropped every extended attribute of that entry —
// hence its POSIX ACL and its file capabilities — from the archive. It is
// retried instead.
func xattrsUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EACCES) ||
		errors.Is(err, unix.EPERM)
}

// xattrSizeAttempts bounds the retries of a size-then-read xattr sequence. Each
// retry needs the attribute set to change again between the two calls.
const xattrSizeAttempts = 8

// readSizedXattr runs the "ask for the size, then read that many bytes" pattern
// both libattr calls require, retrying while a concurrent writer keeps changing
// the size. size and read report the same errno semantics as the l/f variants.
func readSizedXattr(size func() (int, error), read func([]byte) (int, error)) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < xattrSizeAttempts; attempt++ {
		n, err := size()
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return nil, nil
		}
		buf := make([]byte, n)
		n, err = read(buf)
		if err == nil {
			return buf[:n], nil
		}
		if !errors.Is(err, unix.ERANGE) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// collectXattrs turns a NUL-separated attribute name list into PAX records.
// An attribute the caller may not read is reported rather than silently
// dropped: a backup that quietly loses a security label is worse than one that
// says which label it could not carry.
func collectXattrs(names []byte, value func(string) ([]byte, error), path string, logger *Logger) (map[string]string, error) {
	records := map[string]string{}
	for _, name := range strings.Split(string(names), "\x00") {
		if name == "" {
			continue
		}
		v, err := value(name)
		if err != nil {
			if xattrsUnsupported(err) {
				logger.Warn("extended attribute not archived",
					LogField{Key: "path", Value: path},
					LogField{Key: "xattr", Value: name},
					LogField{Key: "error", Value: err})
				continue
			}
			return nil, err
		}
		records[xattrPAXPrefix+name] = string(v)
	}
	if len(records) == 0 {
		return nil, nil
	}
	return records, nil
}

// readXattrs collects the extended attributes reachable through an open
// descriptor. A filesystem without xattr support yields no records instead of
// an error.
func readXattrs(fd int, path string, logger *Logger) (map[string]string, error) {
	names, err := readSizedXattr(
		func() (int, error) { return unix.Flistxattr(fd, nil) },
		func(buf []byte) (int, error) { return unix.Flistxattr(fd, buf) },
	)
	if err != nil {
		if xattrsUnsupported(err) {
			return nil, nil
		}
		return nil, err
	}
	return collectXattrs(names, func(name string) ([]byte, error) {
		return readSizedXattr(
			func() (int, error) { return unix.Fgetxattr(fd, name, nil) },
			func(buf []byte) (int, error) { return unix.Fgetxattr(fd, name, buf) },
		)
	}, path, logger)
}

// readPathXattrs reads attributes without following the final path component.
// It is used for symlinks and special files, which cannot be opened through the
// ordinary read-only descriptor path used for regular files and directories.
func readPathXattrs(path string, logger *Logger) (map[string]string, error) {
	names, err := readSizedXattr(
		func() (int, error) { return unix.Llistxattr(path, nil) },
		func(buf []byte) (int, error) { return unix.Llistxattr(path, buf) },
	)
	if err != nil {
		if xattrsUnsupported(err) {
			return nil, nil
		}
		return nil, err
	}
	return collectXattrs(names, func(name string) ([]byte, error) {
		return readSizedXattr(
			func() (int, error) { return unix.Lgetxattr(path, name, nil) },
			func(buf []byte) (int, error) { return unix.Lgetxattr(path, name, buf) },
		)
	}, path, logger)
}

// xattrsFromPAX extracts the xattr subset of a header's PAX records.
func xattrsFromPAX(records map[string]string) map[string]string {
	if len(records) == 0 {
		return nil
	}
	out := map[string]string{}
	for key, value := range records {
		if name, ok := strings.CutPrefix(key, xattrPAXPrefix); ok && name != "" {
			out[name] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// applyXattrs restores extended attributes on path without following symlinks.
// Attributes the current process is not allowed to set are always reported.
// Strict restores return the joined errors so an incomplete staging tree is
// never published; best-effort restores explicitly opt into warning-only mode.
func applyXattrs(path string, attrs map[string]string, logger *Logger, bestEffort bool) error {
	var result error
	for name, value := range attrs {
		if err := unix.Lsetxattr(path, name, []byte(value), 0); err != nil {
			if logger != nil {
				logger.Warn("extended attribute not restored",
					LogField{Key: "path", Value: path},
					LogField{Key: "xattr", Value: name},
					LogField{Key: "error", Value: err})
			}
			result = errors.Join(result, fmt.Errorf("restore extended attribute %q on %q: %w", name, path, err))
		}
	}
	if bestEffort {
		return nil
	}
	return result
}
