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

	"github.com/robfig/cron/v3"

	"vbackuper/internal/backup"
	"vbackuper/internal/config"
	"vbackuper/internal/store"
)

// ErrBusy means a backup for the target is already queued or running.
var ErrBusy = errors.New("для этой цели уже выполняется бэкап")

// ErrSiteBusy means a run for the site is already queued or running.
var ErrSiteBusy = errors.New("для этого сайта уже выполняется бэкап")

// TargetInput is a transport-agnostic description of a target, used by both the
// web forms and the JSON API.
type TargetInput struct {
	Type          string
	Name          string
	Schedule      string
	Enabled       bool
	RetentionKeep int

	DirPath    string
	DirExclude []string

	DBHost     string
	DBPort     int
	DBName     string
	DBUser     string
	DBPassword string
	DBSSLMode  string
}

// Service contains the backup orchestration logic shared by HTTP handlers and
// the scheduler.
type Service struct {
	st  *store.Store
	cfg *config.Config
	log *slog.Logger

	sem          chan struct{}
	mu           sync.Mutex
	running      map[int64]bool
	runningSites map[int64]bool

	rootCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New creates a Service.
func New(st *store.Store, cfg *config.Config, log *slog.Logger) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		st:           st,
		cfg:          cfg,
		log:          log,
		sem:          make(chan struct{}, cfg.MaxConcurrent),
		running:      map[int64]bool{},
		runningSites: map[int64]bool{},
		rootCtx:      ctx,
		cancel:       cancel,
	}
}

// ---------------------------------------------------------------- targets

// CreateTarget validates and stores a new target.
func (s *Service) CreateTarget(ctx context.Context, in TargetInput) (*store.Target, error) {
	t, err := s.targetFromInput(ctx, nil, in)
	if err != nil {
		return nil, err
	}
	if err := s.st.CreateTarget(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// UpdateTarget validates and saves an existing target.
func (s *Service) UpdateTarget(ctx context.Context, id int64, in TargetInput) (*store.Target, error) {
	old, err := s.st.GetTarget(ctx, id)
	if err != nil {
		return nil, err
	}
	t, err := s.targetFromInput(ctx, old, in)
	if err != nil {
		return nil, err
	}
	t.ID = id
	t.CreatedAt = old.CreatedAt
	if err := s.st.UpdateTarget(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Service) targetFromInput(ctx context.Context, old *store.Target, in TargetInput) (*store.Target, error) {
	t := &store.Target{
		Type:          in.Type,
		Name:          strings.TrimSpace(in.Name),
		Schedule:      strings.TrimSpace(in.Schedule),
		Enabled:       in.Enabled,
		RetentionKeep: in.RetentionKeep,
	}
	if t.Name == "" {
		return nil, errors.New("название обязательно")
	}
	if t.RetentionKeep < 0 {
		t.RetentionKeep = 0
	}
	if t.Schedule != "" {
		if _, err := cron.ParseStandard(t.Schedule); err != nil {
			return nil, fmt.Errorf("расписание %q: %w", t.Schedule, err)
		}
	}

	switch in.Type {
	case store.TypeDir:
		p := filepath.Clean(strings.TrimSpace(in.DirPath))
		if p == "" || p == "." {
			return nil, errors.New("путь к каталогу обязателен")
		}
		if err := s.checkDirPath(p); err != nil {
			return nil, err
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("каталог %s: %w", p, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s не является каталогом", p)
		}
		if err := t.SetConfig(store.DirConfig{Path: p, Exclude: cleanList(in.DirExclude)}); err != nil {
			return nil, err
		}
	case store.TypePostgres, store.TypeMySQL:
		cfg := store.DBConfig{
			Host:     strings.TrimSpace(in.DBHost),
			Port:     in.DBPort,
			Database: strings.TrimSpace(in.DBName),
			User:     strings.TrimSpace(in.DBUser),
			Password: in.DBPassword,
			SSLMode:  strings.TrimSpace(in.DBSSLMode),
		}
		if cfg.Host == "" {
			return nil, errors.New("хост базы данных обязателен")
		}
		if cfg.Port == 0 {
			if in.Type == store.TypePostgres {
				cfg.Port = 5432
			} else {
				cfg.Port = 3306
			}
		}
		if cfg.Port < 1 || cfg.Port > 65535 {
			return nil, fmt.Errorf("некорректный порт: %d", cfg.Port)
		}
		if cfg.Database == "" {
			return nil, errors.New("имя базы данных обязательно")
		}
		if cfg.User == "" {
			return nil, errors.New("пользователь базы данных обязателен")
		}
		if cfg.Password == "" && old != nil {
			if oldCfg, err := old.DBConfig(); err == nil {
				cfg.Password = oldCfg.Password
			}
		}
		if cfg.Password == "" {
			return nil, errors.New("пароль базы данных обязателен")
		}
		if in.Type == store.TypePostgres && cfg.SSLMode == "" {
			cfg.SSLMode = "prefer"
		}
		if err := t.SetConfig(cfg); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("неизвестный тип цели: %q", in.Type)
	}

	return t, nil
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

// DeleteTarget removes a target and, optionally, all of its backup files.
func (s *Service) DeleteTarget(ctx context.Context, id int64, removeFiles bool) error {
	if s.IsRunning(id) {
		return ErrBusy
	}
	t, err := s.st.GetTarget(ctx, id)
	if err != nil {
		return err
	}
	backups, err := s.st.ListBackups(ctx, id, 1_000_000)
	if err != nil {
		return err
	}
	if removeFiles {
		for _, b := range backups {
			if err := removeBackupFiles(b); err != nil {
				s.log.Warn("не удалось удалить файл бэкапа", "backup_id", b.ID, "err", err)
			}
		}
	}
	if err := s.st.DeleteTarget(ctx, id); err != nil {
		return err
	}
	if removeFiles {
		s.removeTargetDir(t)
	}
	return nil
}

func (s *Service) removeTargetDir(t *store.Target) {
	dir := filepath.Join(s.cfg.BackupDir, s.slug(t))
	cleanRoot := filepath.Clean(s.cfg.BackupDir)
	cleanDir := filepath.Clean(dir)
	if !strings.HasPrefix(cleanDir, cleanRoot+string(os.PathSeparator)) {
		return
	}
	if err := os.RemoveAll(cleanDir); err != nil {
		s.log.Warn("не удалось удалить каталог бэкапов", "dir", dir, "err", err)
	}
}

// ---------------------------------------------------------------- backups

// StartBackup reserves a slot for the target and starts the backup in the
// background. It returns the ID of the created backup row.
func (s *Service) StartBackup(ctx context.Context, targetID int64, trigger string) (int64, error) {
	t, err := s.st.GetTarget(ctx, targetID)
	if err != nil {
		return 0, err
	}
	switch t.Type {
	case store.TypeDir, store.TypePostgres, store.TypeMySQL:
	default:
		return 0, fmt.Errorf("неизвестный тип цели: %q", t.Type)
	}

	s.mu.Lock()
	if s.running[targetID] {
		s.mu.Unlock()
		return 0, ErrBusy
	}
	s.running[targetID] = true
	s.mu.Unlock()

	b := &store.Backup{TargetID: targetID, StartedAt: time.Now().UTC(), Status: store.StatusQueued}
	if err := s.st.CreateBackup(ctx, b); err != nil {
		s.setRunning(targetID, false)
		return 0, err
	}
	s.wg.Add(1)
	go s.execute(targetID, b.ID, trigger)
	return b.ID, nil
}

func (s *Service) execute(targetID, backupID int64, trigger string) {
	defer s.wg.Done()
	defer s.setRunning(targetID, false)

	logs := backup.NewLogBuffer(16 << 10)
	start := time.Now()
	logs.Printf("запуск (%s)", triggerLabel(trigger))

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-s.rootCtx.Done():
		s.finish(backupID, nil, store.StatusInterrupted, "сервис остановлен", start, logs)
		return
	}

	ctx, cancel := context.WithTimeout(s.rootCtx, s.cfg.JobTimeout)
	defer cancel()

	if err := s.st.MarkBackupRunning(context.Background(), backupID); err != nil {
		s.log.Error("не удалось обновить статус бэкапа", "backup_id", backupID, "err", err)
	}

	t, err := s.st.GetTarget(ctx, targetID)
	if err != nil {
		s.finish(backupID, nil, store.StatusFailed, err.Error(), start, logs)
		return
	}

	if err := s.checkFreeSpace(); err != nil {
		logs.Printf("ошибка: %v", err)
		s.finish(backupID, nil, store.StatusFailed, err.Error(), start, logs)
		return
	}

	local := start.In(s.cfg.Location)
	slug := s.slug(t)
	destDir := filepath.Join(s.cfg.BackupDir, slug, local.Format("2006"), local.Format("01"))
	// The backup ID keeps file names unique even for runs started in the same second.
	fileBase := fmt.Sprintf("%s-%s-b%d", slug, local.Format("20060102T150405"), backupID)

	res, runErr := backup.Run(ctx, t, destDir, fileBase, logs)
	if runErr != nil {
		status := store.StatusFailed
		if ctx.Err() != nil {
			status = store.StatusInterrupted
			runErr = fmt.Errorf("превышено время выполнения или сервис остановлен: %w", runErr)
		}
		logs.Printf("ошибка: %v", runErr)
		s.log.Error("бэкап не выполнен", "target", t.Name, "target_id", t.ID, "err", runErr)
		s.finish(backupID, nil, status, runErr.Error(), start, logs)
		return
	}

	if err := writeChecksumFile(res); err != nil {
		logs.Printf("предупреждение: не удалось записать .sha256: %v", err)
	}

	fin := time.Since(start)
	s.finish(backupID, &res, store.StatusOK, "", start, logs)
	s.log.Info("бэкап выполнен", "target", t.Name, "target_id", t.ID, "size", res.SizeBytes, "duration", fin.Round(time.Second).String())
	s.applyRetention(t)
}

func (s *Service) finish(backupID int64, res *backup.Result, status, errMsg string, start time.Time, logs *backup.LogBuffer) {
	fin := time.Now().UTC()
	b := &store.Backup{
		ID:         backupID,
		FinishedAt: &fin,
		Status:     status,
		DurationMS: fin.Sub(start).Milliseconds(),
		Error:      errMsg,
		LogTail:    logs.Tail(4000),
	}
	if res != nil {
		b.SizeBytes = res.SizeBytes
		b.SHA256 = res.SHA256
		b.Path = res.Path
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.st.FinishBackup(ctx, b); err != nil {
		s.log.Error("не удалось сохранить результат бэкапа", "backup_id", backupID, "err", err)
	}
}

// DeleteBackup removes a backup row and, optionally, its file.
func (s *Service) DeleteBackup(ctx context.Context, id int64, removeFile bool) error {
	b, err := s.st.GetBackup(ctx, id)
	if err != nil {
		return err
	}
	if b.Status == store.StatusRunning || b.Status == store.StatusQueued {
		return errors.New("нельзя удалить бэкап, который выполняется")
	}
	if removeFile {
		if err := removeBackupFiles(b); err != nil {
			return err
		}
	}
	return s.st.DeleteBackup(ctx, id)
}

// TestStored checks connectivity of a saved target.
func (s *Service) TestStored(ctx context.Context, id int64) error {
	t, err := s.st.GetTarget(ctx, id)
	if err != nil {
		return err
	}
	return backup.Test(ctx, t)
}

// TestInput checks connectivity of unsaved form values. id may be 0.
func (s *Service) TestInput(ctx context.Context, id int64, in TargetInput) error {
	var old *store.Target
	if id > 0 {
		old, _ = s.st.GetTarget(ctx, id)
	}
	t, err := s.targetFromInput(ctx, old, in)
	if err != nil {
		return err
	}
	return backup.Test(ctx, t)
}

// ---------------------------------------------------------------- retention

func (s *Service) applyRetention(t *store.Target) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	keep := t.RetentionKeep
	if keep <= 0 {
		keep = int(s.settingInt64(ctx, "default_retention", int64(s.cfg.DefaultRetention)))
	}
	if keep <= 0 {
		return
	}
	list, err := s.st.ListSuccessfulBackups(ctx, t.ID)
	if err != nil {
		s.log.Warn("не удалось применить retention", "target_id", t.ID, "err", err)
		return
	}
	for i := keep; i < len(list); i++ {
		b := list[i]
		if err := removeBackupFiles(b); err != nil {
			s.log.Warn("не удалось удалить старый бэкап", "backup_id", b.ID, "err", err)
			continue
		}
		if err := s.st.DeleteBackup(ctx, b.ID); err != nil {
			s.log.Warn("не удалось удалить запись бэкапа", "backup_id", b.ID, "err", err)
			continue
		}
		s.log.Info("старый бэкап удалён по retention", "target_id", t.ID, "backup_id", b.ID, "keep", keep)
	}
}

// ---------------------------------------------------------------- helpers

// IsRunning reports whether a backup is queued or executing for the target.
func (s *Service) IsRunning(targetID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[targetID]
}

// RunningTargets returns a set of targets with an active backup.
func (s *Service) RunningTargets() map[int64]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]bool, len(s.running))
	for k, v := range s.running {
		out[k] = v
	}
	return out
}

func (s *Service) setRunning(id int64, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v {
		s.running[id] = true
	} else {
		delete(s.running, id)
	}
}

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

func (s *Service) slug(t *store.Target) string {
	name := translit(strings.ToLower(t.Name))
	name = strings.Trim(slugRe.ReplaceAllString(name, "-"), "-")
	if name == "" {
		return fmt.Sprintf("target-%d", t.ID)
	}
	if len(name) > 40 {
		name = strings.Trim(name[:40], "-")
	}
	return fmt.Sprintf("%s-%d", name, t.ID)
}

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

func writeChecksumFile(res backup.Result) error {
	if res.Path == "" {
		return nil
	}
	line := fmt.Sprintf("%s  %s\n", res.SHA256, filepath.Base(res.Path))
	return os.WriteFile(res.Path+".sha256", []byte(line), 0o640)
}

func removeBackupFiles(b *store.Backup) error {
	if b.Path == "" {
		return nil
	}
	if err := os.Remove(b.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Remove(b.Path + ".sha256"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
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
