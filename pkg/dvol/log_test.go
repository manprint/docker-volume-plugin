package dvol

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	for value, want := range map[string]LogLevel{
		"TRACE":  LevelTrace,
		"debug":  LevelDebug,
		" Info ": LevelInfo,
		"WARN":   LevelWarn,
		"ERROR":  LevelError,
	} {
		got, err := ParseLogLevel(value)
		if err != nil || got != want {
			t.Fatalf("ParseLogLevel(%q): got=%v err=%v want=%v", value, got, err, want)
		}
	}
	if _, err := ParseLogLevel("verbose"); err == nil {
		t.Fatal("invalid log level accepted")
	}
}

func TestLoggerFiltersAndFormatsFields(t *testing.T) {
	var out bytes.Buffer
	logger := NewLogger(&out, LevelWarn)
	logger.Info("hidden", LogField{Key: "volume", Value: "web"})
	logger.Warn("warning", LogField{Key: "volume", Value: "web"})
	logger.Error("failed", LogField{Key: "error", Value: "permission denied"})
	text := out.String()
	if strings.Contains(text, "hidden") {
		t.Fatalf("INFO was not filtered at WARN: %s", text)
	}
	if !strings.Contains(text, "[WARN] warning volume=\"web\"") ||
		!strings.Contains(text, "[ERROR] failed error=\"permission denied\"") {
		t.Fatalf("unexpected log format: %s", text)
	}
}

func TestProgressUsesDynamicUnits(t *testing.T) {
	if got := FormatBytes(999); got != "999 B" {
		t.Fatalf("FormatBytes(999)=%q", got)
	}
	if got := FormatBytes(1_500_000); got != "1.5 MB" {
		t.Fatalf("FormatBytes(1500000)=%q", got)
	}

	var out bytes.Buffer
	logger := NewLogger(&out, LevelInfo)
	progress := NewProgress(logger, "backup")
	progress.AddData(1_500_000)
	progress.AddStream(2_500_000)
	progress.Complete()
	text := out.String()
	if !strings.Contains(text, "backup progress") ||
		!strings.Contains(text, "data=\"1.5 MB\"") ||
		!strings.Contains(text, "stream=\"2.5 MB\"") {
		t.Fatalf("progress did not report dynamic units: %s", text)
	}
}

func TestBackupLoggingNeverContaminatesArchiveStream(t *testing.T) {
	var logs bytes.Buffer
	s := newTestStore(t)
	s.cfg.Logger = NewLogger(&logs, LevelTrace)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.VolumePath("web"), "data.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := s.BackupSourceToWriter("web", s.VolumePath("web"), BackupOptions{Threads: 1}, &stream); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stream.Bytes(), []byte("[INFO]")) || bytes.Contains(stream.Bytes(), []byte("backup progress")) {
		t.Fatal("diagnostic logging contaminated the binary archive stream")
	}
	tr, r, err := openArchiveReader(bytes.NewReader(stream.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Next(); err != nil {
		t.Fatalf("logged stream is not a valid archive: %v", err)
	}
	if err := finishArchiveReader(r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "backup stream completed") ||
		!strings.Contains(logs.String(), "backup stream progress") ||
		!strings.Contains(logs.String(), "backup stream source validated") {
		t.Fatalf("expected backup lifecycle/progress logs, got: %s", logs.String())
	}
	if err := os.WriteFile(filepath.Join(s.VolumePath("web"), "data.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	if err := s.RestoreSourceReader("web", s.VolumePath("web"), bytes.NewReader(stream.Bytes())); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(s.VolumePath("web"), "data.txt"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("restore output mismatch: %q err=%v", got, err)
	}
	if !strings.Contains(logs.String(), "restore stream progress") || !strings.Contains(logs.String(), "restore stream completed") {
		t.Fatalf("expected restore lifecycle/progress logs, got: %s", logs.String())
	}
}

func TestFailedRestoreLogsActionableErrorWithoutChangingTarget(t *testing.T) {
	var logs bytes.Buffer
	s := newTestStore(t)
	s.cfg.Logger = NewLogger(&logs, LevelInfo)
	if err := s.Create("web", nil); err != nil {
		t.Fatal(err)
	}
	target := s.VolumePath("web")
	marker := filepath.Join(target, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreSourceReader("web", target, bytes.NewReader([]byte("not-an-archive"))); err == nil {
		t.Fatal("invalid archive was accepted")
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "keep" {
		t.Fatalf("failed restore changed target: %q err=%v", got, err)
	}
	text := logs.String()
	if !strings.Contains(text, "restore stream failed") || !strings.Contains(text, "error=") {
		t.Fatalf("failed restore did not identify the problem: %s", text)
	}
}
