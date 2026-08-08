package dvol

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LogLevel controls diagnostic verbosity. A logger emits messages at its
// configured level and all more severe levels.
type LogLevel uint8

const (
	LevelTrace LogLevel = iota
	LevelDebug
	LevelInfo
	LevelWarn
	LevelError
)

func (l LogLevel) String() string {
	switch l {
	case LevelTrace:
		return "TRACE"
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// ParseLogLevel accepts the public CLI values case-insensitively.
func ParseLogLevel(value string) (LogLevel, error) {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "TRACE":
		return LevelTrace, nil
	case "DEBUG":
		return LevelDebug, nil
	case "INFO":
		return LevelInfo, nil
	case "WARN", "WARNING":
		return LevelWarn, nil
	case "ERROR":
		return LevelError, nil
	default:
		return LevelInfo, fmt.Errorf("invalid log level %q (use TRACE, DEBUG, INFO, WARN or ERROR)", value)
	}
}

// LogField is a structured key/value attached to a log line.
type LogField struct {
	Key   string
	Value any
}

// Logger writes concise, timestamped diagnostic lines. It is safe for use by
// concurrent backup operations and never writes to stdout.
type Logger struct {
	mu    sync.Mutex
	out   io.Writer
	level LogLevel
	now   func() time.Time
}

// NewLogger creates a logger writing to out. A nil writer disables output.
func NewLogger(out io.Writer, level LogLevel) *Logger {
	return &Logger{out: out, level: level, now: time.Now}
}

func (l *Logger) Enabled(level LogLevel) bool {
	return l != nil && l.out != nil && level >= l.level
}

func (l *Logger) Log(level LogLevel, message string, fields ...LogField) {
	if !l.Enabled(level) {
		return
	}
	var b strings.Builder
	b.WriteString(l.now().Format(time.RFC3339))
	fmt.Fprintf(&b, " [%s] %s", level, message)
	for _, field := range fields {
		if field.Key == "" {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(field.Key)
		b.WriteByte('=')
		switch value := field.Value.(type) {
		case string:
			b.WriteString(strconv.Quote(value))
		default:
			fmt.Fprint(&b, value)
		}
	}
	b.WriteByte('\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.out, b.String())
}

func (l *Logger) Trace(message string, fields ...LogField) { l.Log(LevelTrace, message, fields...) }
func (l *Logger) Debug(message string, fields ...LogField) { l.Log(LevelDebug, message, fields...) }
func (l *Logger) Info(message string, fields ...LogField)  { l.Log(LevelInfo, message, fields...) }
func (l *Logger) Warn(message string, fields ...LogField)  { l.Log(LevelWarn, message, fields...) }
func (l *Logger) Error(message string, fields ...LogField) { l.Log(LevelError, message, fields...) }

// FormatBytes formats counters with decimal KB/MB/GB units, as used by the
// CLI progress output. Decimal units make streamed byte counts easy to match
// with remote object sizes from rclone.
func FormatBytes(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, unit := range []string{"KB", "MB", "GB", "TB"} {
		value /= 1000
		if value < 1000 || unit == "TB" {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%d B", n)
}

// Progress reports raw data and compressed stream counters periodically.
// Both counters are intentionally independent: compression ratio and restore
// input/output behaviour remain visible to the operator.
type Progress struct {
	logger     *Logger
	operation  string
	mu         sync.Mutex
	data       int64
	stream     int64
	lastData   int64
	lastStream int64
	last       time.Time
}

// NewProgress creates an operation progress reporter. Progress is emitted at
// most once per second or after one MiB of new data, whichever comes first.
func NewProgress(logger *Logger, operation string) *Progress {
	return &Progress{logger: logger, operation: operation, last: time.Now()}
}

func (p *Progress) AddData(n int64) {
	if p == nil || n <= 0 {
		return
	}
	p.mu.Lock()
	p.data += n
	p.reportLocked(false)
	p.mu.Unlock()
}

func (p *Progress) AddStream(n int64) {
	if p == nil || n <= 0 {
		return
	}
	p.mu.Lock()
	p.stream += n
	p.reportLocked(false)
	p.mu.Unlock()
}

func (p *Progress) reportLocked(force bool) {
	now := time.Now()
	if !force && now.Sub(p.last) < time.Second && p.data-p.lastData < 1<<20 && p.stream-p.lastStream < 1<<20 {
		return
	}
	p.last = now
	p.lastData = p.data
	p.lastStream = p.stream
	p.logger.Info(p.operation+" progress",
		LogField{Key: "data", Value: FormatBytes(p.data)},
		LogField{Key: "stream", Value: FormatBytes(p.stream)})
}

func (p *Progress) Complete() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.reportLocked(true)
	p.mu.Unlock()
}

type progressReader struct {
	reader   io.Reader
	progress *Progress
	stream   bool
}

func (r progressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if r.stream {
		r.progress.AddStream(int64(n))
	} else {
		r.progress.AddData(int64(n))
	}
	return n, err
}

type progressWriter struct {
	writer   io.Writer
	progress *Progress
}

func (w progressWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.progress.AddStream(int64(n))
	return n, err
}

func (s *Store) logger() *Logger {
	if s == nil {
		return nil
	}
	return s.cfg.Logger
}

func (s *Store) operationProgress(operation string) *Progress {
	return NewProgress(s.logger(), operation)
}
