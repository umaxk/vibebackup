package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"vbackuper/internal/backup"
	"vbackuper/internal/store"
)

// ProjectInput is a transport-agnostic description of a project, used by the
// web form and the JSON API. At least one of DirEnabled/DBEnabled must be true.
type ProjectInput struct {
	Name          string
	Schedule      string
	Enabled       bool
	RetentionKeep int

	DirEnabled bool
	DirPath    string
	DirExclude []string

	DBEnabled  bool
	DBType     string
	DBHost     string
	DBPort     int
	DBName     string
	DBUser     string
	DBPassword string
	DBSSLMode  string
}

// PartTestResult is the outcome of checking one part of a project.
type PartTestResult struct {
	OK    bool   `json:"ok"`
	Info  string `json:"info,omitempty"`
	Error string `json:"error,omitempty"`
}

// ProjectTestResult checks the directory and the database independently.
type ProjectTestResult struct {
	OK    bool            `json:"ok"`
	Dir   *PartTestResult `json:"dir,omitempty"`
	DB    *PartTestResult `json:"db,omitempty"`
	Error string          `json:"error,omitempty"`
}

// projectManifest is written next to the artifacts of every run.
type projectManifest struct {
	ProjectID   int64                 `json:"project_id"`
	ProjectName string                `json:"project_name"`
	BackupID    int64                 `json:"backup_id"`
	StartedAt   time.Time             `json:"started_at"`
	FinishedAt  time.Time             `json:"finished_at"`
	Files       []projectManifestFile `json:"files"`
	Errors      map[string]string     `json:"errors,omitempty"`
}

type projectManifestFile struct {
	Role      string `json:"role"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// ---------------------------------------------------------------- CRUD

// CreateProject validates and stores a new project.
func (s *Service) CreateProject(ctx context.Context, in ProjectInput) (*store.Project, error) {
	project, err := s.projectFromInput(in, nil)
	if err != nil {
		return nil, err
	}
	if err := s.st.CreateProject(ctx, project); err != nil {
		return nil, err
	}
	return project, nil
}

// UpdateProject validates and saves an existing project.
func (s *Service) UpdateProject(ctx context.Context, id int64, in ProjectInput) (*store.Project, error) {
	old, err := s.st.GetProject(ctx, id)
	if err != nil {
		return nil, err
	}
	project, err := s.projectFromInput(in, old)
	if err != nil {
		return nil, err
	}
	project.ID = id
	project.CreatedAt = old.CreatedAt
	if err := s.st.UpdateProject(ctx, project); err != nil {
		return nil, err
	}
	return project, nil
}

// DeleteProject removes a project and, optionally, every run directory it owns.
func (s *Service) DeleteProject(ctx context.Context, id int64, removeFiles bool) error {
	if s.IsProjectRunning(id) {
		return ErrProjectBusy
	}
	project, err := s.st.GetProject(ctx, id)
	if err != nil {
		return err
	}
	runs, err := s.st.ListProjectBackups(ctx, id, 1_000_000)
	if err != nil {
		return err
	}
	if removeFiles {
		for _, run := range runs {
			if err := s.removeDirInside(run.RunDir); err != nil {
				s.log.Warn("не удалось удалить каталог прогона проекта", "project_id", id, "backup_id", run.ID, "err", err)
			}
		}
	}
	if err := s.st.DeleteProject(ctx, id); err != nil {
		return err
	}
	if removeFiles {
		s.removeProjectRoot(project)
	}
	return nil
}

func (s *Service) projectFromInput(in ProjectInput, old *store.Project) (*store.Project, error) {
	project := &store.Project{
		Name:          strings.TrimSpace(in.Name),
		Schedule:      strings.TrimSpace(in.Schedule),
		Enabled:       in.Enabled,
		RetentionKeep: in.RetentionKeep,
	}
	if project.Name == "" {
		return nil, errors.New("название проекта обязательно")
	}
	if project.RetentionKeep < 0 {
		project.RetentionKeep = 0
	}
	if project.Schedule != "" {
		if _, err := cron.ParseStandard(project.Schedule); err != nil {
			return nil, fmt.Errorf("расписание %q: %w", project.Schedule, err)
		}
	}
	if !in.DirEnabled && !in.DBEnabled {
		return nil, errors.New("включите каталог и/или базу данных: проект без частей не имеет смысла")
	}

	if in.DirEnabled {
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
		if err := project.SetDir(&store.DirConfig{Path: p, Exclude: cleanList(in.DirExclude)}); err != nil {
			return nil, err
		}
	}

	if in.DBEnabled {
		kind, cfg, err := normalizeProjectDB(in, old)
		if err != nil {
			return nil, err
		}
		if err := project.SetDB(kind, cfg); err != nil {
			return nil, err
		}
	}

	return project, nil
}

func normalizeProjectDB(in ProjectInput, old *store.Project) (string, *store.DBConfig, error) {
	kind := strings.TrimSpace(in.DBType)
	if kind != store.TypePostgres && kind != store.TypeMySQL {
		return "", nil, errors.New("выберите тип базы данных: PostgreSQL или MySQL/MariaDB")
	}
	cfg := &store.DBConfig{
		Host:     strings.TrimSpace(in.DBHost),
		Port:     in.DBPort,
		Database: strings.TrimSpace(in.DBName),
		User:     strings.TrimSpace(in.DBUser),
		Password: in.DBPassword,
		SSLMode:  strings.TrimSpace(in.DBSSLMode),
	}
	if cfg.Host == "" {
		return "", nil, errors.New("хост базы данных обязателен")
	}
	if cfg.Port == 0 {
		if kind == store.TypePostgres {
			cfg.Port = 5432
		} else {
			cfg.Port = 3306
		}
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return "", nil, fmt.Errorf("некорректный порт: %d", cfg.Port)
	}
	if cfg.Database == "" {
		return "", nil, errors.New("имя базы данных обязательно")
	}
	if cfg.User == "" {
		return "", nil, errors.New("пользователь базы данных обязателен")
	}
	if cfg.Password == "" && old != nil && old.DBType == kind {
		if oldCfg, err := old.DB(); err == nil && oldCfg != nil {
			cfg.Password = oldCfg.Password
		}
	}
	if cfg.Password == "" {
		return "", nil, errors.New("пароль базы данных обязателен")
	}
	if kind == store.TypePostgres && cfg.SSLMode == "" {
		cfg.SSLMode = "prefer"
	}
	return kind, cfg, nil
}

// ---------------------------------------------------------------- tests

// TestProject checks enabled parts independently and reports each outcome. A nil
// ID means the form is not saved yet.
func (s *Service) TestProject(ctx context.Context, id int64, in ProjectInput) ProjectTestResult {
	var old *store.Project
	if id > 0 {
		old, _ = s.st.GetProject(ctx, id)
	}

	res := ProjectTestResult{}
	enabled := 0

	if in.DirEnabled {
		enabled++
		part := &PartTestResult{}
		p := filepath.Clean(strings.TrimSpace(in.DirPath))
		switch {
		case p == "" || p == ".":
			part.Error = "путь к каталогу обязателен"
		case s.checkDirPath(p) != nil:
			part.Error = s.checkDirPath(p).Error()
		default:
			n, err := backup.TestDir(ctx, store.DirConfig{Path: p, Exclude: cleanList(in.DirExclude)})
			if err != nil {
				part.Error = err.Error()
			} else {
				part.OK = true
				part.Info = fmt.Sprintf("%d элементов в корне", n)
			}
		}
		res.Dir = part
	}

	if in.DBEnabled {
		enabled++
		part := &PartTestResult{}
		kind, cfg, err := normalizeProjectDB(in, old)
		if err != nil {
			part.Error = err.Error()
		} else {
			info, err := backup.TestDB(ctx, kind, *cfg)
			if err != nil {
				part.Error = err.Error()
			} else {
				part.OK = true
				part.Info = info
			}
		}
		res.DB = part
	}

	if enabled == 0 {
		res.Error = "включите каталог и/или базу данных"
		return res
	}
	res.OK = (res.Dir == nil || res.Dir.OK) && (res.DB == nil || res.DB.OK)
	return res
}

// TestStoredProject checks a saved project.
func (s *Service) TestStoredProject(ctx context.Context, id int64) ProjectTestResult {
	project, err := s.st.GetProject(ctx, id)
	if err != nil {
		return ProjectTestResult{Error: err.Error()}
	}
	return s.TestProject(ctx, id, projectToInput(project))
}

func projectToInput(project *store.Project) ProjectInput {
	in := ProjectInput{
		Name:          project.Name,
		Schedule:      project.Schedule,
		Enabled:       project.Enabled,
		RetentionKeep: project.RetentionKeep,
	}
	if cfg, err := project.Dir(); err == nil && cfg != nil {
		in.DirEnabled = true
		in.DirPath = cfg.Path
		in.DirExclude = cfg.Exclude
	}
	if cfg, err := project.DB(); err == nil && cfg != nil {
		in.DBEnabled = true
		in.DBType = project.DBType
		in.DBHost = cfg.Host
		in.DBPort = cfg.Port
		in.DBName = cfg.Database
		in.DBUser = cfg.User
		in.DBPassword = cfg.Password
		in.DBSSLMode = cfg.SSLMode
	}
	return in
}

// ---------------------------------------------------------------- runs

// StartProjectBackup reserves a slot for the project and starts the run in the
// background. It returns the ID of the created run row.
func (s *Service) StartProjectBackup(ctx context.Context, projectID int64, trigger string) (int64, error) {
	if _, err := s.st.GetProject(ctx, projectID); err != nil {
		return 0, err
	}

	s.mu.Lock()
	if s.running[projectID] {
		s.mu.Unlock()
		return 0, ErrProjectBusy
	}
	s.running[projectID] = true
	s.mu.Unlock()

	b := &store.ProjectBackup{ProjectID: projectID, StartedAt: time.Now().UTC(), Status: store.StatusQueued}
	if err := s.st.CreateProjectBackup(ctx, b); err != nil {
		s.setProjectRunning(projectID, false)
		return 0, err
	}
	s.wg.Add(1)
	go s.executeProject(projectID, b.ID, trigger)
	return b.ID, nil
}

func (s *Service) executeProject(projectID, backupID int64, trigger string) {
	defer s.wg.Done()
	defer s.setProjectRunning(projectID, false)

	logs := backup.NewLogBuffer(32 << 10)
	start := time.Now()
	logs.Printf("запуск проекта (%s)", triggerLabel(trigger))

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-s.rootCtx.Done():
		s.finishProject(backupID, store.StatusInterrupted, "сервис остановлен", "", "", 0, start, logs)
		return
	}

	ctx, cancel := context.WithTimeout(s.rootCtx, s.cfg.JobTimeout)
	defer cancel()

	if err := s.st.MarkProjectBackupRunning(context.Background(), backupID); err != nil {
		s.log.Error("не удалось обновить статус прогона проекта", "backup_id", backupID, "err", err)
	}

	project, err := s.st.GetProject(ctx, projectID)
	if err != nil {
		s.finishProject(backupID, store.StatusFailed, err.Error(), "", "", 0, start, logs)
		return
	}
	if err := s.checkFreeSpace(); err != nil {
		logs.Printf("ошибка: %v", err)
		s.finishProject(backupID, store.StatusFailed, err.Error(), "", "", 0, start, logs)
		return
	}

	local := start.In(s.cfg.Location)
	slug := s.projectSlug(project)
	runDir := filepath.Join(s.cfg.BackupDir, "projects", slug, local.Format("2006"), local.Format("01"),
		fmt.Sprintf("%s-b%d", local.Format("20060102T150405"), backupID))
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		err = fmt.Errorf("создание каталога прогона: %w", err)
		logs.Printf("ошибка: %v", err)
		s.finishProject(backupID, store.StatusFailed, err.Error(), "", "", 0, start, logs)
		return
	}

	var (
		artifacts  []projectManifestFile
		partFiles  []*store.ProjectBackupFile
		partErrors = map[string]string{}
	)

	// Database first: it fixes the state early in the run, then the directory
	// is read after it, so restored files are never older than the database.
	if dbCfg, err := project.DB(); err != nil {
		partErrors["db"] = err.Error()
	} else if dbCfg != nil {
		logs.Printf("часть «база данных»: %s %s@%s:%d/%s", dbKindLabel(project.DBType), dbCfg.User, dbCfg.Host, dbPort(project.DBType, dbCfg.Port), dbCfg.Database)
		spec := backup.Spec{Type: project.DBType, DB: *dbCfg}
		res, runErr := backup.Run(ctx, spec, runDir, "db", logs)
		if runErr != nil {
			partErrors["db"] = runErr.Error()
			logs.Printf("ошибка базы данных: %v", runErr)
		} else {
			artifacts = append(artifacts, manifestFile("db", res))
			partFiles = append(partFiles, &store.ProjectBackupFile{Role: "db", Name: filepath.Base(res.Path), Path: res.Path, SizeBytes: res.SizeBytes, SHA256: res.SHA256})
		}
	}

	if dirCfg, err := project.Dir(); err != nil {
		partErrors["dir"] = err.Error()
	} else if dirCfg != nil {
		logs.Printf("часть «каталог»: %s", dirCfg.Path)
		spec := backup.Spec{Type: store.TypeDir, Dir: *dirCfg}
		res, runErr := backup.Run(ctx, spec, runDir, "files", logs)
		if runErr != nil {
			partErrors["dir"] = runErr.Error()
			logs.Printf("ошибка каталога: %v", runErr)
		} else {
			artifacts = append(artifacts, manifestFile("dir", res))
			partFiles = append(partFiles, &store.ProjectBackupFile{Role: "dir", Name: filepath.Base(res.Path), Path: res.Path, SizeBytes: res.SizeBytes, SHA256: res.SHA256})
		}
	}

	fin := time.Now().UTC()
	manifest := projectManifest{
		ProjectID:   project.ID,
		ProjectName: project.Name,
		BackupID:    backupID,
		StartedAt:   start.UTC(),
		FinishedAt:  fin,
		Files:       artifacts,
		Errors:      partErrors,
	}
	manifestSHA := ""
	manifestSize := int64(0)
	if raw, err := json.MarshalIndent(manifest, "", "  "); err != nil {
		partErrors["manifest"] = err.Error()
	} else {
		raw = append(raw, '\n')
		manifestPath := filepath.Join(runDir, "manifest.json")
		if err := os.WriteFile(manifestPath, raw, 0o640); err != nil {
			partErrors["manifest"] = fmt.Sprintf("запись manifest.json: %v", err)
		} else {
			sum := sha256.Sum256(raw)
			manifestSHA = hex.EncodeToString(sum[:])
			manifestSize = int64(len(raw))
			partFiles = append(partFiles, &store.ProjectBackupFile{
				Role: "manifest", Name: "manifest.json", Path: manifestPath,
				SizeBytes: manifestSize, SHA256: manifestSHA,
			})
		}
	}

	var total int64
	for _, f := range partFiles {
		f.BackupID = backupID
		total += f.SizeBytes
		if err := s.st.AddProjectBackupFile(context.Background(), f); err != nil {
			s.log.Error("не удалось сохранить файл прогона проекта", "backup_id", backupID, "role", f.Role, "err", err)
		}
	}

	status := store.StatusOK
	if len(partErrors) > 0 {
		status = store.StatusFailed
		if ctx.Err() != nil {
			status = store.StatusInterrupted
		}
	}
	errMsg := joinPartErrors(partErrors)
	if errMsg != "" {
		logs.Printf("прогон завершён с ошибками: %s", errMsg)
	}
	s.finishProject(backupID, status, errMsg, runDir, manifestSHA, total, start, logs)

	if status == store.StatusOK {
		s.log.Info("прогон проекта выполнен", "project", project.Name, "project_id", project.ID,
			"files", len(partFiles), "size", total, "duration", fin.Sub(start).Round(time.Second).String())
	} else {
		s.log.Error("прогон проекта завершён с ошибкой", "project", project.Name, "project_id", project.ID, "status", status, "err", errMsg)
	}
	s.applyProjectRetention(project)
}

func (s *Service) finishProject(backupID int64, status, errMsg, runDir, manifestSHA string, size int64, start time.Time, logs *backup.LogBuffer) {
	fin := time.Now().UTC()
	b := &store.ProjectBackup{
		ID:             backupID,
		FinishedAt:     &fin,
		Status:         status,
		SizeBytes:      size,
		RunDir:         runDir,
		ManifestSHA256: manifestSHA,
		DurationMS:     fin.Sub(start).Milliseconds(),
		Error:          errMsg,
		LogTail:        logs.Tail(8000),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.st.FinishProjectBackup(ctx, b); err != nil {
		s.log.Error("не удалось сохранить результат прогона проекта", "backup_id", backupID, "err", err)
	}
}

// DeleteProjectBackup removes a run row and, optionally, its whole directory.
func (s *Service) DeleteProjectBackup(ctx context.Context, id int64, removeFiles bool) error {
	b, err := s.st.GetProjectBackup(ctx, id)
	if err != nil {
		return err
	}
	if b.Status == store.StatusRunning || b.Status == store.StatusQueued {
		return errors.New("нельзя удалить прогон, который выполняется")
	}
	if removeFiles {
		if err := s.removeDirInside(b.RunDir); err != nil {
			return err
		}
	}
	return s.st.DeleteProjectBackup(ctx, id)
}

// ---------------------------------------------------------------- retention

func (s *Service) applyProjectRetention(project *store.Project) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	keep := project.RetentionKeep
	if keep <= 0 {
		keep = int(s.settingInt64(ctx, "default_retention", int64(s.cfg.DefaultRetention)))
	}
	if keep <= 0 {
		return
	}
	list, err := s.st.ListSuccessfulProjectBackups(ctx, project.ID)
	if err != nil {
		s.log.Warn("не удалось применить retention проекта", "project_id", project.ID, "err", err)
		return
	}
	for i := keep; i < len(list); i++ {
		b := list[i]
		if err := s.removeDirInside(b.RunDir); err != nil {
			s.log.Warn("не удалось удалить старый прогон проекта", "backup_id", b.ID, "err", err)
			continue
		}
		if err := s.st.DeleteProjectBackup(ctx, b.ID); err != nil {
			s.log.Warn("не удалось удалить запись прогона проекта", "backup_id", b.ID, "err", err)
			continue
		}
		s.log.Info("старый прогон проекта удалён по retention", "project_id", project.ID, "backup_id", b.ID, "keep", keep)
	}
}

// ---------------------------------------------------------------- helpers

// IsProjectRunning reports whether a run is queued or executing for the project.
func (s *Service) IsProjectRunning(projectID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[projectID]
}

// RunningProjects returns a set of projects with an active run.
func (s *Service) RunningProjects() map[int64]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]bool, len(s.running))
	for k, v := range s.running {
		out[k] = v
	}
	return out
}

func (s *Service) setProjectRunning(id int64, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v {
		s.running[id] = true
	} else {
		delete(s.running, id)
	}
}

func (s *Service) projectSlug(project *store.Project) string {
	name := translit(strings.ToLower(project.Name))
	name = strings.Trim(slugRe.ReplaceAllString(name, "-"), "-")
	if name == "" {
		return fmt.Sprintf("project-%d", project.ID)
	}
	if len(name) > 40 {
		name = strings.Trim(name[:40], "-")
	}
	return fmt.Sprintf("%s-%d", name, project.ID)
}

func (s *Service) removeProjectRoot(project *store.Project) {
	dir := filepath.Join(s.cfg.BackupDir, "projects", s.projectSlug(project))
	if err := s.removeDirInside(dir); err != nil {
		s.log.Warn("не удалось удалить каталог проекта", "dir", dir, "err", err)
	}
}

// removeDirInside deletes a directory only when it really lives under the
// configured backup root.
func (s *Service) removeDirInside(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	root := filepath.Clean(s.cfg.BackupDir)
	clean := filepath.Clean(dir)
	if clean == root || !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
		return fmt.Errorf("отказ удалять каталог вне %s: %s", root, dir)
	}
	if err := os.RemoveAll(clean); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func manifestFile(role string, res backup.Result) projectManifestFile {
	return projectManifestFile{
		Role:      role,
		Name:      filepath.Base(res.Path),
		SizeBytes: res.SizeBytes,
		SHA256:    res.SHA256,
	}
}

func joinPartErrors(m map[string]string) string {
	labels := map[string]string{"db": "база данных", "dir": "каталог", "manifest": "манифест"}
	var parts []string
	for _, role := range []string{"db", "dir", "manifest"} {
		if msg, ok := m[role]; ok && msg != "" {
			parts = append(parts, labels[role]+": "+msg)
		}
	}
	return strings.Join(parts, "; ")
}

func dbKindLabel(kind string) string {
	switch kind {
	case store.TypePostgres:
		return "PostgreSQL"
	case store.TypeMySQL:
		return "MySQL/MariaDB"
	default:
		return kind
	}
}

func dbPort(kind string, port int) int {
	if port > 0 {
		return port
	}
	if kind == store.TypePostgres {
		return 5432
	}
	return 3306
}
