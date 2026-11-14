package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vbackuper/internal/service"
	"vbackuper/internal/store"
)

// ---------------------------------------------------------------- DTOs

type projectPayload struct {
	ID            int64            `json:"id"`
	Name          string           `json:"name"`
	Schedule      string           `json:"schedule"`
	Enabled       *bool            `json:"enabled"`
	RetentionKeep *int             `json:"retention_keep"`
	Dir           *store.DirConfig `json:"dir"`
	DBType        string           `json:"db_type"`
	Database      *store.DBConfig  `json:"database"`
}

type dbDTO struct {
	Type        string `json:"type,omitempty"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Database    string `json:"database"`
	User        string `json:"user"`
	SSLMode     string `json:"sslmode,omitempty"`
	PasswordSet bool   `json:"password_set"`
}

type projectFileDTO struct {
	Role      string `json:"role"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type projectBackupDTO struct {
	ID             int64            `json:"id"`
	ProjectID      int64            `json:"project_id"`
	ProjectName    string           `json:"project,omitempty"`
	Status         string           `json:"status"`
	StartedAt      time.Time        `json:"started_at"`
	FinishedAt     *time.Time       `json:"finished_at,omitempty"`
	SizeBytes      int64            `json:"size_bytes"`
	DurationMS     int64            `json:"duration_ms"`
	Error          string           `json:"error,omitempty"`
	LogTail        string           `json:"log_tail,omitempty"`
	RunDir         string           `json:"run_dir,omitempty"`
	ManifestSHA256 string           `json:"manifest_sha256,omitempty"`
	Files          []projectFileDTO `json:"files,omitempty"`
}

type projectDTO struct {
	ID            int64             `json:"id"`
	Name          string            `json:"name"`
	Schedule      string            `json:"schedule"`
	Enabled       bool              `json:"enabled"`
	RetentionKeep int               `json:"retention_keep"`
	Running       bool              `json:"running"`
	Dir           *store.DirConfig  `json:"dir,omitempty"`
	Database      *dbDTO            `json:"database,omitempty"`
	NextRun       *time.Time        `json:"next_run,omitempty"`
	LastBackup    *projectBackupDTO `json:"last_backup,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

func projectBackupDTOFrom(b *store.ProjectBackup, files []*store.ProjectBackupFile, withLog bool) projectBackupDTO {
	d := projectBackupDTO{
		ID:             b.ID,
		ProjectID:      b.ProjectID,
		ProjectName:    b.ProjectName,
		Status:         b.Status,
		StartedAt:      b.StartedAt,
		FinishedAt:     b.FinishedAt,
		SizeBytes:      b.SizeBytes,
		DurationMS:     b.DurationMS,
		Error:          b.Error,
		RunDir:         b.RunDir,
		ManifestSHA256: b.ManifestSHA256,
	}
	if withLog {
		d.LogTail = b.LogTail
	}
	for _, f := range files {
		d.Files = append(d.Files, projectFileDTO{
			Role:      f.Role,
			Name:      f.Name,
			SizeBytes: f.SizeBytes,
			SHA256:    f.SHA256,
		})
	}
	return d
}

func (w *Web) projectDTO(project *store.Project, last *store.ProjectBackup) projectDTO {
	d := projectDTO{
		ID:            project.ID,
		Name:          project.Name,
		Schedule:      project.Schedule,
		Enabled:       project.Enabled,
		RetentionKeep: project.RetentionKeep,
		Running:       w.svc.IsProjectRunning(project.ID),
		NextRun:       w.sched.NextRun(project.ID),
		CreatedAt:     project.CreatedAt,
		UpdatedAt:     project.UpdatedAt,
	}
	if cfg, err := project.Dir(); err == nil {
		d.Dir = cfg
	}
	if cfg, err := project.DB(); err == nil && cfg != nil {
		d.Database = &dbDTO{
			Type:        project.DBType,
			Host:        cfg.Host,
			Port:        cfg.Port,
			Database:    cfg.Database,
			User:        cfg.User,
			SSLMode:     cfg.SSLMode,
			PasswordSet: cfg.Password != "",
		}
	}
	if last != nil {
		v := projectBackupDTOFrom(last, nil, false)
		d.LastBackup = &v
	}
	return d
}

func payloadToProjectInput(p projectPayload) service.ProjectInput {
	in := service.ProjectInput{
		Name:     strings.TrimSpace(p.Name),
		Schedule: strings.TrimSpace(p.Schedule),
		Enabled:  true,
	}
	if p.Enabled != nil {
		in.Enabled = *p.Enabled
	}
	if p.RetentionKeep != nil {
		in.RetentionKeep = *p.RetentionKeep
	}
	if p.Dir != nil {
		in.DirEnabled = true
		in.DirPath = strings.TrimSpace(p.Dir.Path)
		in.DirExclude = p.Dir.Exclude
	}
	if p.Database != nil {
		in.DBEnabled = true
		in.DBType = strings.TrimSpace(p.DBType)
		in.DBHost = strings.TrimSpace(p.Database.Host)
		in.DBPort = p.Database.Port
		in.DBName = strings.TrimSpace(p.Database.Database)
		in.DBUser = strings.TrimSpace(p.Database.User)
		in.DBPassword = p.Database.Password
		in.DBSSLMode = strings.TrimSpace(p.Database.SSLMode)
	}
	return in
}

// ---------------------------------------------------------------- projects

func (w *Web) apiProjectsList(resp http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projects, err := w.st.ListProjects(ctx)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	out := make([]projectDTO, 0, len(projects))
	for _, project := range projects {
		last, err := w.st.LastProjectBackup(ctx, project.ID)
		if err != nil {
			w.log.Warn("не удалось получить последний прогон проекта", "project_id", project.ID, "err", err)
		}
		out = append(out, w.projectDTO(project, last))
	}
	writeJSON(resp, http.StatusOK, map[string]any{"projects": out})
}

func (w *Web) apiProjectCreate(resp http.ResponseWriter, r *http.Request) {
	var p projectPayload
	if err := decodeJSON(r, &p); err != nil {
		writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	project, err := w.svc.CreateProject(r.Context(), payloadToProjectInput(p))
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	if err := w.sched.Sync(project); err != nil {
		w.log.Warn("расписание проекта не применено", "project_id", project.ID, "err", err)
	}
	writeJSON(resp, http.StatusCreated, w.projectDTO(project, nil))
}

func (w *Web) apiProjectGet(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	project, err := w.st.GetProject(r.Context(), id)
	if err != nil {
		w.apiFail(resp, r, err)
		return
	}
	last, _ := w.st.LastProjectBackup(r.Context(), id)
	writeJSON(resp, http.StatusOK, w.projectDTO(project, last))
}

func (w *Web) apiProjectUpdate(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	var p projectPayload
	if err := decodeJSON(r, &p); err != nil {
		writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	project, err := w.svc.UpdateProject(r.Context(), id, payloadToProjectInput(p))
	if err != nil {
		w.apiFail(resp, r, err)
		return
	}
	if err := w.sched.Sync(project); err != nil {
		w.log.Warn("расписание проекта не применено", "project_id", project.ID, "err", err)
	}
	last, _ := w.st.LastProjectBackup(r.Context(), id)
	writeJSON(resp, http.StatusOK, w.projectDTO(project, last))
}

func (w *Web) apiProjectDelete(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	removeFiles := r.URL.Query().Get("remove_files") != "false"
	if err := w.svc.DeleteProject(r.Context(), id, removeFiles); err != nil {
		w.apiFail(resp, r, err)
		return
	}
	w.sched.Remove(id)
	writeJSON(resp, http.StatusOK, map[string]any{"ok": true})
}

func (w *Web) apiProjectRun(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	backupID, err := w.svc.StartProjectBackup(r.Context(), id, "manual")
	if err != nil {
		w.apiFail(resp, r, err)
		return
	}
	writeJSON(resp, http.StatusAccepted, map[string]any{"id": backupID, "status": store.StatusQueued})
}

// apiProjectTest checks an unsaved form. The ID inside the body is optional.
func (w *Web) apiProjectTest(resp http.ResponseWriter, r *http.Request) {
	var p projectPayload
	if err := decodeJSON(r, &p); err != nil {
		writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	ctx, cancel := contextWithTimeout(r, 40*time.Second)
	defer cancel()
	writeJSON(resp, http.StatusOK, w.svc.TestProject(ctx, p.ID, payloadToProjectInput(p)))
}

// apiProjectTestStored checks a saved project, optionally with unsaved form values.
func (w *Web) apiProjectTestStored(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := contextWithTimeout(r, 40*time.Second)
	defer cancel()

	res := service.ProjectTestResult{}
	if body := readBody(r); len(body) > 0 {
		var p projectPayload
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
			return
		}
		res = w.svc.TestProject(ctx, id, payloadToProjectInput(p))
	} else {
		res = w.svc.TestStoredProject(ctx, id)
	}
	writeJSON(resp, http.StatusOK, res)
}

// ---------------------------------------------------------------- project runs

func (w *Web) apiProjectBackups(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	runs, err := w.st.ListProjectBackups(r.Context(), id, limit)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	out := make([]projectBackupDTO, 0, len(runs))
	for _, run := range runs {
		out = append(out, projectBackupDTOFrom(run, nil, false))
	}
	writeJSON(resp, http.StatusOK, map[string]any{"backups": out})
}

func (w *Web) apiProjectBackupsList(resp http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	runs, err := w.st.ListProjectBackups(r.Context(), projectID, limit)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	out := make([]projectBackupDTO, 0, len(runs))
	for _, run := range runs {
		out = append(out, projectBackupDTOFrom(run, nil, false))
	}
	writeJSON(resp, http.StatusOK, map[string]any{"backups": out})
}

func (w *Web) apiProjectBackupGet(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	run, err := w.st.GetProjectBackup(r.Context(), id)
	if err != nil {
		w.apiFail(resp, r, err)
		return
	}
	files, err := w.st.ListProjectBackupFiles(r.Context(), id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	writeJSON(resp, http.StatusOK, projectBackupDTOFrom(run, files, true))
}

func (w *Web) apiProjectBackupDelete(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	removeFiles := r.URL.Query().Get("remove_files") != "false"
	if err := w.svc.DeleteProjectBackup(r.Context(), id, removeFiles); err != nil {
		w.apiFail(resp, r, err)
		return
	}
	writeJSON(resp, http.StatusOK, map[string]any{"ok": true})
}
