package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vbackuper/internal/store"
)

// Result describes a finished backup artifact.
type Result struct {
	Path      string
	SizeBytes int64
	SHA256    string
}

// Run performs a backup of the given target into destDir. The archive name is
// derived from fileBase plus a type-specific extension.
func Run(ctx context.Context, t *store.Target, destDir, fileBase string, logs *LogBuffer) (Result, error) {
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return Result{}, fmt.Errorf("создание каталога бэкапов: %w", err)
	}

	var (
		res Result
		err error
	)
	switch t.Type {
	case store.TypeDir:
		res, err = runDir(ctx, t, filepath.Join(destDir, fileBase+".tar.zst"), logs)
	case store.TypePostgres:
		res, err = runPostgres(ctx, t, filepath.Join(destDir, fileBase+".dump"), logs)
	case store.TypeMySQL:
		res, err = runMySQL(ctx, t, filepath.Join(destDir, fileBase+".sql.zst"), logs)
	default:
		return Result{}, fmt.Errorf("неизвестный тип цели: %q", t.Type)
	}
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// Test checks that the target is reachable and readable.
func Test(ctx context.Context, t *store.Target) error {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	switch t.Type {
	case store.TypeDir:
		cfg, err := t.DirConfig()
		if err != nil {
			return err
		}
		info, err := os.Stat(cfg.Path)
		if err != nil {
			return fmt.Errorf("каталог %s: %w", cfg.Path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s не является каталогом", cfg.Path)
		}
		if _, err := os.ReadDir(cfg.Path); err != nil {
			return fmt.Errorf("каталог %s недоступен для чтения: %w", cfg.Path, err)
		}
		return nil
	case store.TypePostgres:
		return testPostgres(ctx, t)
	case store.TypeMySQL:
		return testMySQL(ctx, t)
	default:
		return fmt.Errorf("неизвестный тип цели: %q", t.Type)
	}
}

// LogBuffer is a goroutine-safe ring buffer keeping the tail of a log.
type LogBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

// NewLogBuffer creates a buffer that keeps at most max bytes.
func NewLogBuffer(max int) *LogBuffer {
	if max <= 0 {
		max = 16 << 10
	}
	return &LogBuffer{max: max}
}

// Write implements io.Writer.
func (l *LogBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	if len(l.buf) > l.max {
		l.buf = append([]byte(nil), l.buf[len(l.buf)-l.max:]...)
	}
	return len(p), nil
}

// Printf writes a formatted line with a timestamp.
func (l *LogBuffer) Printf(format string, args ...any) {
	line := fmt.Sprintf("[%s] ", time.Now().Format("15:04:05")) + fmt.Sprintf(format, args...) + "\n"
	_, _ = l.Write([]byte(line))
}

// String returns the buffered log.
func (l *LogBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.buf)
}

// Tail returns at most n trailing bytes of the log, trimmed.
func (l *LogBuffer) Tail(n int) string {
	s := strings.TrimSpace(l.String())
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func resultFor(path string, h hash.Hash) (Result, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Result{}, err
	}
	return Result{Path: path, SizeBytes: fi.Size(), SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func logSuffix(logs *LogBuffer) string {
	if tail := logs.Tail(700); tail != "" {
		return " — " + tail
	}
	return ""
}

func newHasher() hash.Hash { return sha256.New() }
