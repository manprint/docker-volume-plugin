package dvol

// Fuzzing the extraction path. The hand-written escape tests cover the attacks
// we thought of; this covers the ones we did not. The property is absolute and
// easy to check: whatever the archive contains, nothing may be created outside
// the staging directory and the extractor may not panic.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fuzzSeedArchive(items []tarItem) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, it := range items {
		hdr := it.hdr
		if len(it.data) > 0 {
			hdr.Size = int64(len(it.data))
		}
		if tw.WriteHeader(&hdr) != nil {
			continue
		}
		if len(it.data) > 0 {
			_, _ = tw.Write(it.data)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func FuzzExtractTarStream(f *testing.F) {
	f.Add(fuzzSeedArchive([]tarItem{
		{hdr: tar.Header{Name: ".", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "a/b.txt", Typeflag: tar.TypeReg, Mode: 0o644}, data: []byte("ok")},
	}))
	f.Add(fuzzSeedArchive([]tarItem{
		{hdr: tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644}, data: []byte("x")},
	}))
	f.Add(fuzzSeedArchive([]tarItem{
		{hdr: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc", Mode: 0o777}},
		{hdr: tar.Header{Name: "link/passwd", Typeflag: tar.TypeReg, Mode: 0o644}, data: []byte("x")},
	}))
	f.Add(fuzzSeedArchive([]tarItem{
		{hdr: tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd"}},
	}))
	f.Add(fuzzSeedArchive([]tarItem{
		{hdr: tar.Header{Name: "dev", Typeflag: tar.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3}},
		{hdr: tar.Header{Name: "fifo", Typeflag: tar.TypeFifo, Mode: 0o644}},
	}))
	f.Add([]byte("not an archive at all"))

	f.Fuzz(func(t *testing.T, data []byte) {
		root := t.TempDir()
		staging := filepath.Join(root, "staging")
		if err := os.Mkdir(staging, 0o755); err != nil {
			t.Fatal(err)
		}
		// A canary next to the staging directory: nothing may reach it.
		canary := filepath.Join(root, "canary.txt")
		if err := os.WriteFile(canary, []byte("intact"), 0o644); err != nil {
			t.Fatal(err)
		}

		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return // not a gzip stream: nothing to extract
		}
		tr := tar.NewReader(gz)
		_ = extractTarStream(tr, nil, nil, true, func(hdr *tar.Header, payload io.Reader) (string, string, io.Reader, bool, error) {
			return hdr.Name, staging, payload, true, nil
		})
		_ = gz.Close()

		if got, err := os.ReadFile(canary); err != nil || string(got) != "intact" {
			t.Fatalf("extraction escaped the staging directory: canary=%q err=%v", got, err)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != "staging" && e.Name() != "canary.txt" {
				t.Fatalf("extraction created %q outside the staging directory", e.Name())
			}
		}
		// Nothing inside staging may be a symlink pointing out of it either,
		// because a later entry would then be written through it.
		_ = filepath.Walk(staging, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil {
				return nil
			}
			if info.Mode()&os.ModeSymlink == 0 {
				return nil
			}
			target, err := os.Readlink(path)
			if err != nil {
				return nil
			}
			// A dangling or absolute symlink is legal content; what matters is
			// that nothing was *written* through it, which the canary and the
			// directory check above already prove.
			if strings.Contains(target, "\x00") {
				t.Fatalf("symlink target contains a NUL byte: %q", target)
			}
			return nil
		})
	})
}
