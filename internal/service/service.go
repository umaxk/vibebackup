package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"vbackuper/internal/backup"
	"vbackuper/internal/config"
	"vbackuper/internal/store"
)

// ErrProjectBusy means a run for the project is already queued or running.
var ErrProjectBusy = errors.New("для этого проекта уже выполняется бэкап")

// Service contains the backup orchestration logic shared by HTTP handlers and
// the scheduler.
type Service struct {
	st  *store.Store
	cfg *config.Config
	log *slog.Logger

	sem     chan struct{}
	mu      sync.Mutex
	running map[int64]bool

	rootCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New creates a Service.
func New(st *store.Store, cfg *config.Config, log *slog.Logger) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		st:      st,
		cfg:     cfg,
		log:     log,
		sem:     make(chan struct{}, cfg.MaxConcurrent),
		running: map[int64]bool{},
		rootCtx: ctx,
		cancel:  cancel,
	}
}

func (s *Service) checkDirPath(p string) error {
	if len(s.cfg.SourceRoots) == 0 {
		return errors.New("каталоги-источники не настроены (VBC_SOURCE_ROOTS)")
	}
	resolved := p
	if r, err := filepath.EvalSymlinks(p); err == nil {
		resolved = r
	}
	for _, root := range s.cfg.SourceRoots {
		if root == string(os.PathSeparator) || root == "/" {
			return nil
		}
		resolvedRoot := root
		if r, err := filepath.EvalSymlinks(root); err == nil {
			resolvedRoot = r
		}
		if resolved == resolvedRoot || strings.HasPrefix(resolved, resolvedRoot+string(os.PathSeparator)) {
			return nil
		}
	}
	return fmt.Errorf("путь должен находиться внутри разрешённых каталогов: %s", strings.Join(s.cfg.SourceRoots, ", "))
}

// ---------------------------------------------------------------- helpers

func (s *Service) checkFreeSpace() error {
	free, err := backup.FreeBytes(s.cfg.BackupDir)
	if err != nil {
		s.log.Warn("не удалось определить свободное место", "err", err)
		return nil
	}
	min := s.settingInt64(context.Background(), "min_free_mb", s.cfg.MinFreeMB)
	if min <= 0 {
		return nil
	}
	if free < min*1024*1024 {
		return fmt.Errorf("недостаточно свободного места в %s: %.0f МБ, требуется минимум %d МБ",
			s.cfg.BackupDir, float64(free)/(1024*1024), min)
	}
	return nil
}

func (s *Service) settingInt64(ctx context.Context, key string, def int64) int64 {
	v, ok, err := s.st.GetSetting(ctx, key)
	if err != nil || !ok {
		return def
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// Settings returns effective settings (defaults merged with saved values).
func (s *Service) Settings(ctx context.Context) map[string]string {
	out := map[string]string{
		"default_retention": strconv.Itoa(s.cfg.DefaultRetention),
		"grace_minutes":     strconv.Itoa(s.cfg.GraceMinutes),
		"min_free_mb":       strconv.FormatInt(s.cfg.MinFreeMB, 10),
	}
	saved, err := s.st.Settings(ctx)
	if err != nil {
		return out
	}
	for k, v := range saved {
		out[k] = v
	}
	return out
}

// SaveSettings validates and persists settings.
func (s *Service) SaveSettings(ctx context.Context, values map[string]string) error {
	for _, key := range []string{"default_retention", "grace_minutes", "min_free_mb"} {
		raw, ok := values[key]
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || n < 0 {
			return fmt.Errorf("значение %q должно быть неотрицательным числом", key)
		}
		if err := s.st.SetSetting(ctx, key, strconv.Itoa(n)); err != nil {
			return err
		}
	}
	return nil
}

// GraceMinutes returns the catch-up window after a restart.
func (s *Service) GraceMinutes(ctx context.Context) int {
	return int(s.settingInt64(ctx, "grace_minutes", int64(s.cfg.GraceMinutes)))
}

// Stop cancels running backups and waits for them to finish.
func (s *Service) Stop(timeout time.Duration) {
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		s.log.Warn("не все задачи завершились за отведённое время")
	}
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

var translitTable = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "h", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "sch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}

func translit(s string) string {
	var b strings.Builder
	for _, r := range s {
		if rep, ok := translitTable[r]; ok {
			b.WriteString(rep)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func cleanList(items []string) []string {
	var out []string
	for _, item := range items {
		for _, part := range strings.FieldsFunc(item, func(r rune) bool {
			return r == '\n' || r == '\r' || r == ',' || r == ';'
		}) {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func triggerLabel(trigger string) string {
	switch trigger {
	case "manual":
		return "вручную"
	case "schedule":
		return "по расписанию"
	case "startup":
		return "догоняющий запуск после старта"
	default:
		return trigger
	}
}

// ListSources returns selectable directories under the configured source roots.
func (s *Service) ListSources(ctx context.Context) []string {
	const maxDepth = 3
	const maxEntries = 5000

	var out []string
	seen := map[string]bool{}
	for _, root := range s.cfg.SourceRoots {
		rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if ctx.Err() != nil {
				return fs.SkipAll
			}
			if !d.IsDir() {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") && p != root {
				return fs.SkipDir
			}
			depth := strings.Count(filepath.Clean(p), string(os.PathSeparator)) - rootDepth
			if depth >= maxDepth {
				return fs.SkipDir
			}
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
			if len(out) >= maxEntries {
				return fs.SkipAll
			}
			return nil
		})
	}
	return out
}
