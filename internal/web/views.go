package web

import (
	"fmt"
	"html/template"
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

type dbInfo struct {
	Host        string
	Port        int
	Database    string
	User        string
	SSLMode     string
	PasswordSet bool
}

type projectFileView struct {
	Role      string
	RoleLabel string
	Name      string
	SizeBytes int64
	SHA256    string
}

type projectBackupView struct {
	ID             int64
	ProjectID      int64
	ProjectName    string
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
	Files          []projectFileView
	HasDir         bool
	HasDB          bool
}

type projectRow struct {
	ID          int64
	Name        string
	Schedule    string
	Enabled     bool
	NextRun     *time.Time
	Last        *projectBackupView
	Running     bool
	DirPath     string
	DBType      string
	DBTypeLabel string
	DB          *dbInfo
}

type projectInfo struct {
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
	Last          *projectBackupView
}

type projectForm struct {
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

type projectsData struct {
	viewBase
	Projects   []projectRow
	HasRunning bool
}

type projectFormData struct {
	viewBase
	IsNew       bool
	Action      string
	ProjectID   int64
	Form        projectForm
	SourceRoots []string
	NextRuns    []time.Time
}

type projectDetailData struct {
	viewBase
	Project       projectInfo
	Backups       []projectBackupView
	HasRunning    bool
	ScheduleError string
}

type backupsData struct {
	viewBase
	ProjectRuns []projectBackupView
	Projects    []projectRow
	Selected    int64
	HasRunning  bool
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

func (w *Web) projectBackupView(b *store.ProjectBackup, files []*store.ProjectBackupFile) projectBackupView {
	v := projectBackupView{
		ID:             b.ID,
		ProjectID:      b.ProjectID,
		ProjectName:    b.ProjectName,
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
		v.Files = append(v.Files, projectFileView{
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

func (w *Web) projectRow(project *store.Project, last *store.ProjectBackup, running bool) projectRow {
	row := projectRow{
		ID:          project.ID,
		Name:        project.Name,
		Schedule:    project.Schedule,
		Enabled:     project.Enabled,
		NextRun:     w.sched.NextRun(project.ID),
		Running:     running,
		DBType:      project.DBType,
		DBTypeLabel: typeLabel(project.DBType),
	}
	if dir, err := project.Dir(); err == nil && dir != nil {
		row.DirPath = dir.Path
	}
	if db, err := project.DB(); err == nil && db != nil {
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
		v := w.projectBackupView(last, nil)
		row.Last = &v
	}
	return row
}

func (w *Web) projectInfo(project *store.Project, last *store.ProjectBackup) projectInfo {
	info := projectInfo{
		ID:            project.ID,
		Name:          project.Name,
		Schedule:      project.Schedule,
		Enabled:       project.Enabled,
		RetentionKeep: project.RetentionKeep,
		CreatedAt:     project.CreatedAt.In(w.cfg.Location),
		NextRun:       w.sched.NextRun(project.ID),
		DBType:        project.DBType,
		DBTypeLabel:   typeLabel(project.DBType),
	}
	if dir, err := project.Dir(); err == nil {
		info.Dir = dir
	}
	if db, err := project.DB(); err == nil && db != nil {
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
		v := w.projectBackupView(last, nil)
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
