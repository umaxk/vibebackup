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

// SiteInput is a transport-agnostic description of a site, used by the web form
// and the JSON API. At least one of DirEnabled/DBEnabled must be true.
type SiteInput struct {
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

// PartTestResult is the outcome of checking one part of a site.
type PartTestResult struct {
	OK    bool   `json:"ok"`
	Info  string `json:"info,omitempty"`
	Error string `json:"error,omitempty"`
}

// SiteTestResult checks the directory and the database independently.
type SiteTestResult struct {
	OK    bool            `json:"ok"`
	Dir   *PartTestResult `json:"dir,omitempty"`
	DB    *PartTestResult `json:"db,omitempty"`
	Error string          `json:"error,omitempty"`
}

// siteManifest is written next to the artifacts of every run.
type siteManifest struct {
	SiteID     int64              `json:"site_id"`
	SiteName   string             `json:"site_name"`
	BackupID   int64              `json:"backup_id"`
	StartedAt  time.Time          `json:"started_at"`
	FinishedAt time.Time          `json:"finished_at"`
	Files      []siteManifestFile `json:"files"`
	Errors     map[string]string  `json:"errors,omitempty"`
}

type siteManifestFile struct {
	Role      string `json:"role"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// ---------------------------------------------------------------- CRUD

// CreateSite validates and stores a new site.
func (s *Service) CreateSite(ctx context.Context, in SiteInput) (*store.Site, error) {
	site, err := s.siteFromInput(nil, in)
	if err != nil {
		return nil, err
	}
	if err := s.st.CreateSite(ctx, site); err != nil {
		return nil, err
	}
	return site, nil
}

// UpdateSite validates and saves an existing site.
func (s *Service) UpdateSite(ctx context.Context, id int64, in SiteInput) (*store.Site, error) {
	old, err := s.st.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	site, err := s.siteFromInput(old, in)
	if err != nil {
		return nil, err
	}
	site.ID = id
	site.CreatedAt = old.CreatedAt
	if err := s.st.UpdateSite(ctx, site); err != nil {
		return nil, err
	}
	return site, nil
}

// DeleteSite removes a site and, optionally, every run directory it owns.
func (s *Service) DeleteSite(ctx context.Context, id int64, removeFiles bool) error {
	if s.IsSiteRunning(id) {
		return ErrSiteBusy
	}
	site, err := s.st.GetSite(ctx, id)
	if err != nil {
		return err
	}
	runs, err := s.st.ListSiteBackups(ctx, id, 1_000_000)
	if err != nil {
		return err
	}
	if removeFiles {
		for _, run := range runs {
			if err := s.removeDirInside(run.RunDir); err != nil {
				s.log.Warn("не удалось удалить каталог прогона сайта", "site_id", id, "backup_id", run.ID, "err", err)
			}
		}
	}
	if err := s.st.DeleteSite(ctx, id); err != nil {
		return err
	}
	if removeFiles {
		s.removeSiteRoot(site)
	}
	return nil
}

func (s *Service) siteFromInput(old *store.Site, in SiteInput) (*store.Site, error) {
	site := &store.Site{
		Name:          strings.TrimSpace(in.Name),
		Schedule:      strings.TrimSpace(in.Schedule),
		Enabled:       in.Enabled,
		RetentionKeep: in.RetentionKeep,
	}
	if site.Name == "" {
		return nil, errors.New("название сайта обязательно")
	}
	if site.RetentionKeep < 0 {
		site.RetentionKeep = 0
	}
	if site.Schedule != "" {
		if _, err := cron.ParseStandard(site.Schedule); err != nil {
			return nil, fmt.Errorf("расписание %q: %w", site.Schedule, err)
		}
	}
	if !in.DirEnabled && !in.DBEnabled {
		return nil, errors.New("включите каталог и/или базу данных: сайт без частей не имеет смысла")
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
		if err := site.SetDir(&store.DirConfig{Path: p, Exclude: cleanList(in.DirExclude)}); err != nil {
			return nil, err
		}
	}

	if in.DBEnabled {
		kind, cfg, err := normalizeSiteDB(in, old)
		if err != nil {
			return nil, err
		}
		if err := site.SetDB(kind, cfg); err != nil {
			return nil, err
		}
	}

	return site, nil
}

func normalizeSiteDB(in SiteInput, old *store.Site) (string, *store.DBConfig, error) {
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

// TestSite checks enabled parts independently and reports each outcome. A nil
// ID means the form is not saved yet.
func (s *Service) TestSite(ctx context.Context, id int64, in SiteInput) SiteTestResult {
	var old *store.Site
	if id > 0 {
		old, _ = s.st.GetSite(ctx, id)
	}

	res := SiteTestResult{}
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
		kind, cfg, err := normalizeSiteDB(in, old)
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

// TestStoredSite checks a saved site.
func (s *Service) TestStoredSite(ctx context.Context, id int64) SiteTestResult {
	site, err := s.st.GetSite(ctx, id)
	if err != nil {
		return SiteTestResult{Error: err.Error()}
	}
	return s.TestSite(ctx, id, siteToInput(site))
}

func siteToInput(site *store.Site) SiteInput {
	in := SiteInput{
		Name:          site.Name,
		Schedule:      site.Schedule,
		Enabled:       site.Enabled,
		RetentionKeep: site.RetentionKeep,
	}
	if cfg, err := site.Dir(); err == nil && cfg != nil {
		in.DirEnabled = true
		in.DirPath = cfg.Path
		in.DirExclude = cfg.Exclude
	}
	if cfg, err := site.DB(); err == nil && cfg != nil {
		in.DBEnabled = true
		in.DBType = site.DBType
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

// StartSiteBackup reserves a slot for the site and starts the run in the
// background. It returns the ID of the created run row.
func (s *Service) StartSiteBackup(ctx context.Context, siteID int64, trigger string) (int64, error) {
	if _, err := s.st.GetSite(ctx, siteID); err != nil {
		return 0, err
	}

	s.mu.Lock()
	if s.runningSites[siteID] {
		s.mu.Unlock()
		return 0, ErrSiteBusy
	}
	s.runningSites[siteID] = true
	s.mu.Unlock()

	b := &store.SiteBackup{SiteID: siteID, StartedAt: time.Now().UTC(), Status: store.StatusQueued}
	if err := s.st.CreateSiteBackup(ctx, b); err != nil {
		s.setSiteRunning(siteID, false)
		return 0, err
	}
	s.wg.Add(1)
	go s.executeSite(siteID, b.ID, trigger)
	return b.ID, nil
}

func (s *Service) executeSite(siteID, backupID int64, trigger string) {
	defer s.wg.Done()
	defer s.setSiteRunning(siteID, false)

	logs := backup.NewLogBuffer(32 << 10)
	start := time.Now()
	logs.Printf("запуск сайта (%s)", triggerLabel(trigger))

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-s.rootCtx.Done():
		s.finishSite(backupID, store.StatusInterrupted, "сервис остановлен", "", "", 0, start, logs)
		return
	}

	ctx, cancel := context.WithTimeout(s.rootCtx, s.cfg.JobTimeout)
	defer cancel()

	if err := s.st.MarkSiteBackupRunning(context.Background(), backupID); err != nil {
		s.log.Error("не удалось обновить статус прогона сайта", "backup_id", backupID, "err", err)
	}

	site, err := s.st.GetSite(ctx, siteID)
	if err != nil {
		s.finishSite(backupID, store.StatusFailed, err.Error(), "", "", 0, start, logs)
		return
	}
	if err := s.checkFreeSpace(); err != nil {
		logs.Printf("ошибка: %v", err)
		s.finishSite(backupID, store.StatusFailed, err.Error(), "", "", 0, start, logs)
		return
	}

	local := start.In(s.cfg.Location)
	slug := s.siteSlug(site)
	runDir := filepath.Join(s.cfg.BackupDir, "sites", slug, local.Format("2006"), local.Format("01"),
		fmt.Sprintf("%s-b%d", local.Format("20060102T150405"), backupID))
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		err = fmt.Errorf("создание каталога прогона: %w", err)
		logs.Printf("ошибка: %v", err)
		s.finishSite(backupID, store.StatusFailed, err.Error(), "", "", 0, start, logs)
		return
	}

	var (
		artifacts  []siteManifestFile
		partFiles  []*store.SiteBackupFile
		partErrors = map[string]string{}
	)

	// Database first: it fixes the state early in the run, then the directory
	// is read after it, so restored files are never older than the database.
	if dbCfg, err := site.DB(); err != nil {
		partErrors["db"] = err.Error()
	} else if dbCfg != nil {
		logs.Printf("часть «база данных»: %s %s@%s:%d/%s", dbKindLabel(site.DBType), dbCfg.User, dbCfg.Host, dbPort(site.DBType, dbCfg.Port), dbCfg.Database)
		t := &store.Target{ID: site.ID, Type: site.DBType, Name: site.Name, Config: site.DBJSON}
		res, runErr := backup.Run(ctx, t, runDir, "db", logs)
		if runErr != nil {
			partErrors["db"] = runErr.Error()
			logs.Printf("ошибка базы данных: %v", runErr)
		} else {
			artifacts = append(artifacts, manifestFile("db", res))
			partFiles = append(partFiles, &store.SiteBackupFile{Role: "db", Name: filepath.Base(res.Path), Path: res.Path, SizeBytes: res.SizeBytes, SHA256: res.SHA256})
		}
	}

	if dirCfg, err := site.Dir(); err != nil {
		partErrors["dir"] = err.Error()
	} else if dirCfg != nil {
		logs.Printf("часть «каталог»: %s", dirCfg.Path)
		t := &store.Target{ID: site.ID, Type: store.TypeDir, Name: site.Name, Config: site.DirJSON}
		res, runErr := backup.Run(ctx, t, runDir, "files", logs)
		if runErr != nil {
			partErrors["dir"] = runErr.Error()
			logs.Printf("ошибка каталога: %v", runErr)
		} else {
			artifacts = append(artifacts, manifestFile("dir", res))
			partFiles = append(partFiles, &store.SiteBackupFile{Role: "dir", Name: filepath.Base(res.Path), Path: res.Path, SizeBytes: res.SizeBytes, SHA256: res.SHA256})
		}
	}

	fin := time.Now().UTC()
	manifest := siteManifest{
		SiteID:     site.ID,
		SiteName:   site.Name,
		BackupID:   backupID,
		StartedAt:  start.UTC(),
		FinishedAt: fin,
		Files:      artifacts,
		Errors:     partErrors,
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
			partFiles = append(partFiles, &store.SiteBackupFile{
				Role: "manifest", Name: "manifest.json", Path: manifestPath,
				SizeBytes: manifestSize, SHA256: manifestSHA,
			})
		}
	}

	var total int64
	for _, f := range partFiles {
		f.BackupID = backupID
		total += f.SizeBytes
		if err := s.st.AddSiteBackupFile(context.Background(), f); err != nil {
			s.log.Error("не удалось сохранить файл прогона сайта", "backup_id", backupID, "role", f.Role, "err", err)
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
	s.finishSite(backupID, status, errMsg, runDir, manifestSHA, total, start, logs)

	if status == store.StatusOK {
		s.log.Info("прогон сайта выполнен", "site", site.Name, "site_id", site.ID,
			"files", len(partFiles), "size", total, "duration", fin.Sub(start).Round(time.Second).String())
	} else {
		s.log.Error("прогон сайта завершён с ошибкой", "site", site.Name, "site_id", site.ID, "status", status, "err", errMsg)
	}
	s.applySiteRetention(site)
}

func (s *Service) finishSite(backupID int64, status, errMsg, runDir, manifestSHA string, size int64, start time.Time, logs *backup.LogBuffer) {
	fin := time.Now().UTC()
	b := &store.SiteBackup{
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
	if err := s.st.FinishSiteBackup(ctx, b); err != nil {
		s.log.Error("не удалось сохранить результат прогона сайта", "backup_id", backupID, "err", err)
	}
}

// DeleteSiteBackup removes a run row and, optionally, its whole directory.
func (s *Service) DeleteSiteBackup(ctx context.Context, id int64, removeFiles bool) error {
	b, err := s.st.GetSiteBackup(ctx, id)
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
	return s.st.DeleteSiteBackup(ctx, id)
}

// ---------------------------------------------------------------- retention

func (s *Service) applySiteRetention(site *store.Site) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	keep := site.RetentionKeep
	if keep <= 0 {
		keep = int(s.settingInt64(ctx, "default_retention", int64(s.cfg.DefaultRetention)))
	}
	if keep <= 0 {
		return
	}
	list, err := s.st.ListSuccessfulSiteBackups(ctx, site.ID)
	if err != nil {
		s.log.Warn("не удалось применить retention сайта", "site_id", site.ID, "err", err)
		return
	}
	for i := keep; i < len(list); i++ {
		b := list[i]
		if err := s.removeDirInside(b.RunDir); err != nil {
			s.log.Warn("не удалось удалить старый прогон сайта", "backup_id", b.ID, "err", err)
			continue
		}
		if err := s.st.DeleteSiteBackup(ctx, b.ID); err != nil {
			s.log.Warn("не удалось удалить запись прогона сайта", "backup_id", b.ID, "err", err)
			continue
		}
		s.log.Info("старый прогон сайта удалён по retention", "site_id", site.ID, "backup_id", b.ID, "keep", keep)
	}
}

// ---------------------------------------------------------------- helpers

// IsSiteRunning reports whether a run is queued or executing for the site.
func (s *Service) IsSiteRunning(siteID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runningSites[siteID]
}

// RunningSites returns a set of sites with an active run.
func (s *Service) RunningSites() map[int64]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]bool, len(s.runningSites))
	for k, v := range s.runningSites {
		out[k] = v
	}
	return out
}

func (s *Service) setSiteRunning(id int64, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v {
		s.runningSites[id] = true
	} else {
		delete(s.runningSites, id)
	}
}

func (s *Service) siteSlug(site *store.Site) string {
	name := translit(strings.ToLower(site.Name))
	name = strings.Trim(slugRe.ReplaceAllString(name, "-"), "-")
	if name == "" {
		return fmt.Sprintf("site-%d", site.ID)
	}
	if len(name) > 40 {
		name = strings.Trim(name[:40], "-")
	}
	return fmt.Sprintf("%s-%d", name, site.ID)
}

func (s *Service) removeSiteRoot(site *store.Site) {
	dir := filepath.Join(s.cfg.BackupDir, "sites", s.siteSlug(site))
	if err := s.removeDirInside(dir); err != nil {
		s.log.Warn("не удалось удалить каталог сайта", "dir", dir, "err", err)
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

func manifestFile(role string, res backup.Result) siteManifestFile {
	return siteManifestFile{
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
