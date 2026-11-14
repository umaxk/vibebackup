package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vbackuper/internal/backup"
	"vbackuper/internal/service"
	"vbackuper/internal/store"
)

var startedAt = time.Now()

func writeJSON(resp http.ResponseWriter, status int, v any) {
	resp.Header().Set("Content-Type", "application/json; charset=utf-8")
	resp.WriteHeader(status)
	enc := json.NewEncoder(resp)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeJSONError(resp http.ResponseWriter, status int, msg string) {
	writeJSON(resp, status, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// ---------------------------------------------------------------- DTOs

type targetPayload struct {
	ID            int64            `json:"id"`
	Type          string           `json:"type"`
	Name          string           `json:"name"`
	Schedule      string           `json:"schedule"`
	Enabled       *bool            `json:"enabled"`
	RetentionKeep *int             `json:"retention_keep"`
	Dir           *store.DirConfig `json:"dir"`
	Database      *store.DBConfig  `json:"database"`
}

type dbDTO struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Database    string `json:"database"`
	User        string `json:"user"`
	SSLMode     string `json:"sslmode,omitempty"`
	PasswordSet bool   `json:"password_set"`
}

type backupDTO struct {
	ID         int64      `json:"id"`
	TargetID   int64      `json:"target_id"`
	TargetName string     `json:"target,omitempty"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	SizeBytes  int64      `json:"size_bytes"`
	SHA256     string     `json:"sha256,omitempty"`
	Path       string     `json:"path,omitempty"`
	DurationMS int64      `json:"duration_ms"`
	Error      string     `json:"error,omitempty"`
	LogTail    string     `json:"log_tail,omitempty"`
}

type targetDTO struct {
	ID            int64            `json:"id"`
	Type          string           `json:"type"`
	Name          string           `json:"name"`
	Schedule      string           `json:"schedule"`
	Enabled       bool             `json:"enabled"`
	RetentionKeep int              `json:"retention_keep"`
	Running       bool             `json:"running"`
	Dir           *store.DirConfig `json:"dir,omitempty"`
	Database      *dbDTO           `json:"database,omitempty"`
	NextRun       *time.Time       `json:"next_run,omitempty"`
	LastBackup    *backupDTO       `json:"last_backup,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

func backupDTOFrom(b *store.Backup, withLog bool) backupDTO {
	d := backupDTO{
		ID:         b.ID,
		TargetID:   b.TargetID,
		TargetName: b.TargetName,
		Status:     b.Status,
		StartedAt:  b.StartedAt,
		FinishedAt: b.FinishedAt,
		SizeBytes:  b.SizeBytes,
		SHA256:     b.SHA256,
		Path:       b.Path,
		DurationMS: b.DurationMS,
		Error:      b.Error,
	}
	if withLog {
		d.LogTail = b.LogTail
	}
	return d
}

func (w *Web) targetDTO(t *store.Target, last *store.Backup) targetDTO {
	d := targetDTO{
		ID:            t.ID,
		Type:          t.Type,
		Name:          t.Name,
		Schedule:      t.Schedule,
		Enabled:       t.Enabled,
		RetentionKeep: t.RetentionKeep,
		Running:       w.svc.IsRunning(t.ID),
		NextRun:       w.sched.NextRun(t.ID),
		CreatedAt:     t.CreatedAt,
		UpdatedAt:     t.UpdatedAt,
	}
	switch t.Type {
	case store.TypeDir:
		if cfg, err := t.DirConfig(); err == nil {
			d.Dir = &cfg
		}
	case store.TypePostgres, store.TypeMySQL:
		if cfg, err := t.DBConfig(); err == nil {
			d.Database = &dbDTO{
				Host:        cfg.Host,
				Port:        cfg.Port,
				Database:    cfg.Database,
				User:        cfg.User,
				SSLMode:     cfg.SSLMode,
				PasswordSet: cfg.Password != "",
			}
		}
	}
	if last != nil {
		v := backupDTOFrom(last, false)
		d.LastBackup = &v
	}
	return d
}

func payloadToInput(p targetPayload) service.TargetInput {
	in := service.TargetInput{
		Type:     strings.TrimSpace(p.Type),
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
		in.DirPath = strings.TrimSpace(p.Dir.Path)
		in.DirExclude = p.Dir.Exclude
	}
	if p.Database != nil {
		in.DBHost = strings.TrimSpace(p.Database.Host)
		in.DBPort = p.Database.Port
		in.DBName = strings.TrimSpace(p.Database.Database)
		in.DBUser = strings.TrimSpace(p.Database.User)
		in.DBPassword = p.Database.Password
		in.DBSSLMode = strings.TrimSpace(p.Database.SSLMode)
	}
	return in
}

// ---------------------------------------------------------------- auth

func (w *Web) apiLogin(resp http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !w.limiter.Allow(ip) {
		writeJSONError(resp, http.StatusTooManyRequests, "слишком много попыток входа, попробуйте позже")
		return
	}
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		_ = r.ParseForm()
		creds.Username = r.PostFormValue("username")
		creds.Password = r.PostFormValue("password")
	} else if err := decodeJSON(r, &creds); err != nil {
		writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	if !w.checkCredentials(creds.Username, creds.Password) {
		w.limiter.Fail(ip)
		writeJSONError(resp, http.StatusUnauthorized, "неверный логин или пароль")
		return
	}
	w.limiter.Reset(ip)
	w.setSessionCookie(resp, r)
	writeJSON(resp, http.StatusOK, map[string]any{"ok": true})
}

func (w *Web) apiLogout(resp http.ResponseWriter, r *http.Request) {
	w.clearSessionCookie(resp, r)
	writeJSON(resp, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- targets

func (w *Web) apiTargetsList(resp http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	targets, err := w.st.ListTargets(ctx)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	out := make([]targetDTO, 0, len(targets))
	for _, t := range targets {
		last, err := w.st.LastBackup(ctx, t.ID)
		if err != nil {
			w.log.Warn("не удалось получить последний бэкап", "target_id", t.ID, "err", err)
		}
		out = append(out, w.targetDTO(t, last))
	}
	writeJSON(resp, http.StatusOK, map[string]any{"targets": out})
}

func (w *Web) apiTargetCreate(resp http.ResponseWriter, r *http.Request) {
	var p targetPayload
	if err := decodeJSON(r, &p); err != nil {
		writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	t, err := w.svc.CreateTarget(r.Context(), payloadToInput(p))
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	if err := w.sched.Sync(t); err != nil {
		w.log.Warn("расписание не применено", "target_id", t.ID, "err", err)
	}
	writeJSON(resp, http.StatusCreated, w.targetDTO(t, nil))
}

func (w *Web) apiTargetGet(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	t, err := w.st.GetTarget(r.Context(), id)
	if err != nil {
		w.apiFail(resp, r, err)
		return
	}
	last, _ := w.st.LastBackup(r.Context(), id)
	writeJSON(resp, http.StatusOK, w.targetDTO(t, last))
}

func (w *Web) apiTargetUpdate(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	var p targetPayload
	if err := decodeJSON(r, &p); err != nil {
		writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	t, err := w.svc.UpdateTarget(r.Context(), id, payloadToInput(p))
	if err != nil {
		w.apiFail(resp, r, err)
		return
	}
	if err := w.sched.Sync(t); err != nil {
		w.log.Warn("расписание не применено", "target_id", t.ID, "err", err)
	}
	last, _ := w.st.LastBackup(r.Context(), id)
	writeJSON(resp, http.StatusOK, w.targetDTO(t, last))
}

func (w *Web) apiTargetDelete(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	removeFiles := r.URL.Query().Get("remove_files") != "false"
	if err := w.svc.DeleteTarget(r.Context(), id, removeFiles); err != nil {
		w.apiFail(resp, r, err)
		return
	}
	w.sched.Remove(id)
	writeJSON(resp, http.StatusOK, map[string]any{"ok": true})
}

func (w *Web) apiTargetTest(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := contextWithTimeout(r, 40*time.Second)
	defer cancel()

	var (
		testErr error
		p       targetPayload
		body    = readBody(r)
	)
	if len(body) > 0 {
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
			return
		}
		testErr = w.svc.TestInput(ctx, id, payloadToInput(p))
	} else {
		testErr = w.svc.TestStored(ctx, id)
	}
	w.testResponse(resp, testErr)
}

func (w *Web) apiTest(resp http.ResponseWriter, r *http.Request) {
	var p targetPayload
	if err := decodeJSON(r, &p); err != nil {
		writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	ctx, cancel := contextWithTimeout(r, 40*time.Second)
	defer cancel()
	w.testResponse(resp, w.svc.TestInput(ctx, p.ID, payloadToInput(p)))
}

func (w *Web) testResponse(resp http.ResponseWriter, err error) {
	if err != nil {
		writeJSON(resp, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(resp, http.StatusOK, map[string]any{"ok": true})
}

func (w *Web) apiTargetRun(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	backupID, err := w.svc.StartBackup(r.Context(), id, "manual")
	if err != nil {
		w.apiFail(resp, r, err)
		return
	}
	writeJSON(resp, http.StatusAccepted, map[string]any{"id": backupID, "status": store.StatusQueued})
}

// ---------------------------------------------------------------- backups

func (w *Web) apiTargetBackups(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	backups, err := w.st.ListTargetBackups(r.Context(), id, limit)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	out := make([]backupDTO, 0, len(backups))
	for _, b := range backups {
		out = append(out, backupDTOFrom(b, false))
	}
	writeJSON(resp, http.StatusOK, map[string]any{"backups": out})
}

func (w *Web) apiBackupsList(resp http.ResponseWriter, r *http.Request) {
	targetID, _ := strconv.ParseInt(r.URL.Query().Get("target_id"), 10, 64)
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	backups, err := w.st.ListBackups(r.Context(), targetID, limit)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	out := make([]backupDTO, 0, len(backups))
	for _, b := range backups {
		out = append(out, backupDTOFrom(b, false))
	}
	writeJSON(resp, http.StatusOK, map[string]any{"backups": out})
}

func (w *Web) apiBackupGet(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	b, err := w.st.GetBackup(r.Context(), id)
	if err != nil {
		w.apiFail(resp, r, err)
		return
	}
	writeJSON(resp, http.StatusOK, backupDTOFrom(b, true))
}

func (w *Web) apiBackupDelete(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	removeFile := r.URL.Query().Get("remove_file") != "false"
	if err := w.svc.DeleteBackup(r.Context(), id, removeFile); err != nil {
		w.apiFail(resp, r, err)
		return
	}
	writeJSON(resp, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- misc

func (w *Web) apiSources(resp http.ResponseWriter, r *http.Request) {
	writeJSON(resp, http.StatusOK, map[string]any{"sources": w.svc.ListSources(r.Context())})
}

func (w *Web) apiCronPreview(resp http.ResponseWriter, r *http.Request) {
	var body struct {
		Schedule string `json:"schedule"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Schedule) == "" {
		writeJSON(resp, http.StatusOK, map[string]any{"next": []time.Time{}})
		return
	}
	runs, err := w.sched.NextRuns(body.Schedule, 5)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(resp, http.StatusOK, map[string]any{"next": runs})
}

func (w *Web) apiSettingsGet(resp http.ResponseWriter, r *http.Request) {
	writeJSON(resp, http.StatusOK, map[string]any{
		"settings":       w.svc.Settings(r.Context()),
		"backup_dir":     w.cfg.BackupDir,
		"data_dir":       w.cfg.DataDir,
		"source_roots":   w.cfg.SourceRoots,
		"max_concurrent": w.cfg.MaxConcurrent,
		"job_timeout":    w.cfg.JobTimeout.String(),
	})
}

func (w *Web) apiSettingsUpdate(resp http.ResponseWriter, r *http.Request) {
	values := map[string]string{}
	if err := decodeJSON(r, &values); err != nil {
		writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	if err := w.svc.SaveSettings(r.Context(), values); err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	w.apiSettingsGet(resp, r)
}

func (w *Web) apiSystem(resp http.ResponseWriter, r *http.Request) {
	free, _ := backup.FreeBytes(w.cfg.BackupDir)
	targets, _ := w.st.ListTargets(r.Context())
	writeJSON(resp, http.StatusOK, map[string]any{
		"version":        w.cfg.Version,
		"uptime_seconds": int64(time.Since(startedAt).Seconds()),
		"timezone":       w.cfg.Location.String(),
		"backup_dir":     w.cfg.BackupDir,
		"data_dir":       w.cfg.DataDir,
		"source_roots":   w.cfg.SourceRoots,
		"max_concurrent": w.cfg.MaxConcurrent,
		"job_timeout":    w.cfg.JobTimeout.String(),
		"free_bytes":     free,
		"targets":        len(targets),
	})
}

func (w *Web) apiFail(resp http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSONError(resp, http.StatusNotFound, "не найдено")
	case errors.Is(err, service.ErrBusy):
		writeJSONError(resp, http.StatusConflict, err.Error())
	default:
		msg := err.Error()
		if strings.Contains(msg, "расписание") || strings.Contains(msg, "обязател") || strings.Contains(msg, "некорректн") ||
			strings.Contains(msg, "каталог") || strings.Contains(msg, "путь") || strings.Contains(msg, "не является") {
			writeJSONError(resp, http.StatusBadRequest, msg)
			return
		}
		w.log.Error("api-ошибка", "path", r.URL.Path, "err", err)
		writeJSONError(resp, http.StatusInternalServerError, "внутренняя ошибка: "+msg)
	}
}

func readBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil
	}
	return []byte(strings.TrimSpace(string(b)))
}
