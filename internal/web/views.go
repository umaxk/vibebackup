package web

import (
	"fmt"
	"html/template"
	"path/filepath"
	"strings"
	"time"

	"vbackuper/internal/store"
)

type viewBase struct {
	Title   string
	Flash   string
	Error   string
	Version string
	Authed  bool
}

type loginData struct {
	viewBase
	Next string
}

type backupView struct {
	ID          int64
	TargetID    int64
	TargetName  string
	Status      string
	StatusLabel string
	StatusClass string
	StartedAt   time.Time
	FinishedAt  *time.Time
	SizeBytes   int64
	DurationMS  int64
	ErrorText   string
	Log         string
	SHA256      string
	Path        string
	FileName    string
}

type targetRow struct {
	ID        int64
	Name      string
	Type      string
	TypeLabel string
	Schedule  string
	Enabled   bool
	NextRun   *time.Time
	Last      *backupView
	Running   bool
}

type dbInfo struct {
	Host        string
	Port        int
	Database    string
	User        string
	SSLMode     string
	PasswordSet bool
}

type targetInfo struct {
	ID            int64
	Name          string
	Type          string
	TypeLabel     string
	Schedule      string
	Enabled       bool
	RetentionKeep int
	CreatedAt     time.Time
	NextRun       *time.Time
	Dir           *store.DirConfig
	DB            *dbInfo
	Last          *backupView
}

type targetForm struct {
	Name          string
	Type          string
	Schedule      string
	RetentionKeep int
	Enabled       bool
	DirPath       string
	DirExclude    string
	DBHost        string
	DBPort        string
	DBName        string
	DBUser        string
	DBSSLMode     string
	PasswordSet   bool
}

type dashboardData struct {
	viewBase
	Targets    []targetRow
	HasRunning bool
}

type formData struct {
	viewBase
	IsNew       bool
	Action      string
	TargetID    int64
	Form        targetForm
	SourceRoots []string
	PasswordSet bool
	NextRuns    []time.Time
}

type detailData struct {
	viewBase
	Target        targetInfo
	Backups       []backupView
	HasRunning    bool
	ScheduleError string
}

type backupsData struct {
	viewBase
	Backups      []backupView
	SiteRuns     []siteBackupView
	Targets      []targetRow
	Sites        []siteRow
	Selected     int64
	SelectedSite int64
	HasRunning   bool
}

type settingsData struct {
	viewBase
	Settings      map[string]string
	BackupDir     string
	DataDir       string
	SourceRoots   []string
	MaxConcurrent int
	JobTimeout    string
	AdminUser     string
}

type siteFileView struct {
	Role      string
	RoleLabel string
	Name      string
	SizeBytes int64
	SHA256    string
}

type siteBackupView struct {
	ID             int64
	SiteID         int64
	SiteName       string
	Status         string
	StatusLabel    string
	StatusClass    string
	StartedAt      time.Time
	FinishedAt     *time.Time
	SizeBytes      int64
	DurationMS     int64
	ErrorText      string
	Log            string
	RunDir         string
	ManifestSHA256 string
	Files          []siteFileView
	HasDir         bool
	HasDB          bool
}

type siteRow struct {
	ID          int64
	Name        string
	Schedule    string
	Enabled     bool
	NextRun     *time.Time
	Last        *siteBackupView
	Running     bool
	DirPath     string
	DBType      string
	DBTypeLabel string
	DB          *dbInfo
}

type siteInfo struct {
	ID            int64
	Name          string
	Schedule      string
	Enabled       bool
	RetentionKeep int
	CreatedAt     time.Time
	NextRun       *time.Time
	Dir           *store.DirConfig
	DBType        string
	DBTypeLabel   string
	DB            *dbInfo
	Last          *siteBackupView
}

type siteForm struct {
	Name          string
	Schedule      string
	RetentionKeep int
	Enabled       bool
	DirEnabled    bool
	DirPath       string
	DirExclude    string
	DBEnabled     bool
	DBType        string
	DBHost        string
	DBPort        string
	DBName        string
	DBUser        string
	DBSSLMode     string
	PasswordSet   bool
}

type sitesData struct {
	viewBase
	Sites      []siteRow
	HasRunning bool
}

type siteFormData struct {
	viewBase
	IsNew       bool
	Action      string
	SiteID      int64
	Form        siteForm
	SourceRoots []string
	NextRuns    []time.Time
}

type siteDetailData struct {
	viewBase
	Site          siteInfo
	Backups       []siteBackupView
	HasRunning    bool
	ScheduleError string
}

func (w *Web) backupView(b *store.Backup) backupView {
	v := backupView{
		ID:          b.ID,
		TargetID:    b.TargetID,
		TargetName:  b.TargetName,
		Status:      b.Status,
		StatusLabel: statusLabel(b.Status),
		StatusClass: statusClass(b.Status),
		StartedAt:   b.StartedAt.In(w.cfg.Location),
		SizeBytes:   b.SizeBytes,
		DurationMS:  b.DurationMS,
		ErrorText:   b.Error,
		Log:         b.LogTail,
		SHA256:      b.SHA256,
		Path:        b.Path,
	}
	if b.FinishedAt != nil {
		t := b.FinishedAt.In(w.cfg.Location)
		v.FinishedAt = &t
	}
	if b.Path != "" {
		v.FileName = filepath.Base(b.Path)
	}
	return v
}

func (w *Web) targetRow(t *store.Target, last *store.Backup, running bool) targetRow {
	row := targetRow{
		ID:        t.ID,
		Name:      t.Name,
		Type:      t.Type,
		TypeLabel: typeLabel(t.Type),
		Schedule:  t.Schedule,
		Enabled:   t.Enabled,
		NextRun:   w.sched.NextRun(t.ID),
		Running:   running,
	}
	if last != nil {
		v := w.backupView(last)
		row.Last = &v
	}
	return row
}

func (w *Web) targetInfo(t *store.Target) targetInfo {
	info := targetInfo{
		ID:            t.ID,
		Name:          t.Name,
		Type:          t.Type,
		TypeLabel:     typeLabel(t.Type),
		Schedule:      t.Schedule,
		Enabled:       t.Enabled,
		RetentionKeep: t.RetentionKeep,
		CreatedAt:     t.CreatedAt.In(w.cfg.Location),
		NextRun:       w.sched.NextRun(t.ID),
	}
	switch t.Type {
	case store.TypeDir:
		if cfg, err := t.DirConfig(); err == nil {
			info.Dir = &cfg
		}
	case store.TypePostgres, store.TypeMySQL:
		if cfg, err := t.DBConfig(); err == nil {
			info.DB = &dbInfo{
				Host:        cfg.Host,
				Port:        cfg.Port,
				Database:    cfg.Database,
				User:        cfg.User,
				SSLMode:     cfg.SSLMode,
				PasswordSet: cfg.Password != "",
			}
		}
	}
	return info
}

func (w *Web) siteBackupView(b *store.SiteBackup, files []*store.SiteBackupFile) siteBackupView {
	v := siteBackupView{
		ID:             b.ID,
		SiteID:         b.SiteID,
		SiteName:       b.SiteName,
		Status:         b.Status,
		StatusLabel:    statusLabel(b.Status),
		StatusClass:    statusClass(b.Status),
		StartedAt:      b.StartedAt.In(w.cfg.Location),
		SizeBytes:      b.SizeBytes,
		DurationMS:     b.DurationMS,
		ErrorText:      b.Error,
		Log:            b.LogTail,
		RunDir:         b.RunDir,
		ManifestSHA256: b.ManifestSHA256,
	}
	if b.FinishedAt != nil {
		t := b.FinishedAt.In(w.cfg.Location)
		v.FinishedAt = &t
	}
	for _, f := range files {
		v.Files = append(v.Files, siteFileView{
			Role:      f.Role,
			RoleLabel: roleLabel(f.Role),
			Name:      f.Name,
			SizeBytes: f.SizeBytes,
			SHA256:    f.SHA256,
		})
		switch f.Role {
		case "dir":
			v.HasDir = true
		case "db":
			v.HasDB = true
		}
	}
	return v
}

func (w *Web) siteRow(site *store.Site, last *store.SiteBackup, running bool) siteRow {
	row := siteRow{
		ID:          site.ID,
		Name:        site.Name,
		Schedule:    site.Schedule,
		Enabled:     site.Enabled,
		NextRun:     w.sched.NextRunSite(site.ID),
		Running:     running,
		DBType:      site.DBType,
		DBTypeLabel: typeLabel(site.DBType),
	}
	if dir, err := site.Dir(); err == nil && dir != nil {
		row.DirPath = dir.Path
	}
	if db, err := site.DB(); err == nil && db != nil {
		row.DB = &dbInfo{
			Host:        db.Host,
			Port:        db.Port,
			Database:    db.Database,
			User:        db.User,
			SSLMode:     db.SSLMode,
			PasswordSet: db.Password != "",
		}
	}
	if last != nil {
		v := w.siteBackupView(last, nil)
		row.Last = &v
	}
	return row
}

func (w *Web) siteInfo(site *store.Site, last *store.SiteBackup) siteInfo {
	info := siteInfo{
		ID:            site.ID,
		Name:          site.Name,
		Schedule:      site.Schedule,
		Enabled:       site.Enabled,
		RetentionKeep: site.RetentionKeep,
		CreatedAt:     site.CreatedAt.In(w.cfg.Location),
		NextRun:       w.sched.NextRunSite(site.ID),
		DBType:        site.DBType,
		DBTypeLabel:   typeLabel(site.DBType),
	}
	if dir, err := site.Dir(); err == nil {
		info.Dir = dir
	}
	if db, err := site.DB(); err == nil && db != nil {
		info.DB = &dbInfo{
			Host:        db.Host,
			Port:        db.Port,
			Database:    db.Database,
			User:        db.User,
			SSLMode:     db.SSLMode,
			PasswordSet: db.Password != "",
		}
	}
	if last != nil {
		v := w.siteBackupView(last, nil)
		info.Last = &v
	}
	return info
}

func roleLabel(role string) string {
	switch role {
	case "dir":
		return "каталог"
	case "db":
		return "база данных"
	case "manifest":
		return "манифест"
	default:
		return role
	}
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"fmtBytes":    humanSize,
		"fmtTime":     func(t time.Time) string { return t.Format("2006-01-02 15:04:05") },
		"fmtTimeP":    fmtTimeP,
		"fmtDuration": fmtDuration,
		"shortSHA":    shortSHA,
		"join":        strings.Join,
	}
}

func fmtTimeP(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Format("02.01.2006 15:04")
}

func fmtDuration(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%d мс", ms)
	case d < time.Minute:
		return fmt.Sprintf("%.1f с", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%d мин %d с", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%d ч %d мин", int(d.Hours()), int(d.Minutes())%60)
	}
}

func humanSize(n int64) string {
	if n <= 0 {
		return "—"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Б", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"КБ", "МБ", "ГБ", "ТБ", "ПБ", "ЭБ"}
	if exp >= len(suffixes) {
		exp = len(suffixes) - 1
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), suffixes[exp])
}

func shortSHA(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func typeLabel(t string) string {
	switch t {
	case store.TypeDir:
		return "каталог"
	case store.TypePostgres:
		return "PostgreSQL"
	case store.TypeMySQL:
		return "MySQL/MariaDB"
	default:
		return t
	}
}

func statusLabel(s string) string {
	switch s {
	case store.StatusQueued:
		return "в очереди"
	case store.StatusRunning:
		return "выполняется"
	case store.StatusOK:
		return "успешно"
	case store.StatusFailed:
		return "ошибка"
	case store.StatusInterrupted:
		return "прервано"
	default:
		return s
	}
}

func statusClass(s string) string {
	switch s {
	case store.StatusOK:
		return "ok"
	case store.StatusFailed:
		return "err"
	case store.StatusInterrupted:
		return "warn"
	default:
		return "run"
	}
}
