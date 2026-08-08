package dvol

import (
	"errors"
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
func xattrsUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ERANGE) ||
		errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)
}

// readXattrs collects the extended attributes reachable through an open
// descriptor. A filesystem without xattr support yields no records instead of
// an error.
func readXattrs(fd int) (map[string]string, error) {
	size, err := unix.Flistxattr(fd, nil)
	if err != nil {
		if xattrsUnsupported(err) {
			return nil, nil
		}
		return nil, err
	}
	if size <= 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	size, err = unix.Flistxattr(fd, buf)
	if err != nil {
		if xattrsUnsupported(err) {
			return nil, nil
		}
		return nil, err
	}
	records := map[string]string{}
	for _, name := range strings.Split(string(buf[:size]), "\x00") {
		if name == "" {
			continue
		}
		valueSize, err := unix.Fgetxattr(fd, name, nil)
		if err != nil {
			if xattrsUnsupported(err) {
				continue
			}
			return nil, err
		}
		value := make([]byte, valueSize)
		if valueSize > 0 {
			if _, err := unix.Fgetxattr(fd, name, value); err != nil {
				if xattrsUnsupported(err) {
					continue
				}
				return nil, err
			}
		}
		records[xattrPAXPrefix+name] = string(value)
	}
	if len(records) == 0 {
		return nil, nil
	}
	return records, nil
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
// Attributes the current process is not allowed to set (security.*, system.*
// without the matching privilege) are reported to the caller as skipped rather
// than failing the whole restore: losing an SELinux label must not cost the
// operator their data.
func applyXattrs(path string, attrs map[string]string, logger *Logger) {
	for name, value := range attrs {
		if err := unix.Lsetxattr(path, name, []byte(value), 0); err != nil {
			logger.Warn("extended attribute not restored",
				LogField{Key: "path", Value: path},
				LogField{Key: "xattr", Value: name},
				LogField{Key: "error", Value: err})
		}
	}
}
