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

type sitePayload struct {
	ID            int64            `json:"id"`
	Name          string           `json:"name"`
	Schedule      string           `json:"schedule"`
	Enabled       *bool            `json:"enabled"`
	RetentionKeep *int             `json:"retention_keep"`
	Dir           *store.DirConfig `json:"dir"`
	DBType        string           `json:"db_type"`
	Database      *store.DBConfig  `json:"database"`
}

type siteFileDTO struct {
	Role      string `json:"role"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type siteBackupDTO struct {
	ID             int64         `json:"id"`
	SiteID         int64         `json:"site_id"`
	SiteName       string        `json:"site,omitempty"`
	Status         string        `json:"status"`
	StartedAt      time.Time     `json:"started_at"`
	FinishedAt     *time.Time    `json:"finished_at,omitempty"`
	SizeBytes      int64         `json:"size_bytes"`
	DurationMS     int64         `json:"duration_ms"`
	Error          string        `json:"error,omitempty"`
	LogTail        string        `json:"log_tail,omitempty"`
	RunDir         string        `json:"run_dir,omitempty"`
	ManifestSHA256 string        `json:"manifest_sha256,omitempty"`
	Files          []siteFileDTO `json:"files,omitempty"`
}

type siteDTO struct {
	ID            int64            `json:"id"`
	Name          string           `json:"name"`
	Schedule      string           `json:"schedule"`
	Enabled       bool             `json:"enabled"`
	RetentionKeep int              `json:"retention_keep"`
	Running       bool             `json:"running"`
	Dir           *store.DirConfig `json:"dir,omitempty"`
	Database      *dbDTO           `json:"database,omitempty"`
	NextRun       *time.Time       `json:"next_run,omitempty"`
	LastBackup    *siteBackupDTO   `json:"last_backup,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

func siteBackupDTOFrom(b *store.SiteBackup, files []*store.SiteBackupFile, withLog bool) siteBackupDTO {
	d := siteBackupDTO{
		ID:             b.ID,
		SiteID:         b.SiteID,
		SiteName:       b.SiteName,
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
		d.Files = append(d.Files, siteFileDTO{
			Role:      f.Role,
			Name:      f.Name,
			SizeBytes: f.SizeBytes,
			SHA256:    f.SHA256,
		})
	}
	return d
}

func (w *Web) siteDTO(site *store.Site, last *store.SiteBackup) siteDTO {
	d := siteDTO{
		ID:            site.ID,
		Name:          site.Name,
		Schedule:      site.Schedule,
		Enabled:       site.Enabled,
		RetentionKeep: site.RetentionKeep,
		Running:       w.svc.IsSiteRunning(site.ID),
		NextRun:       w.sched.NextRunSite(site.ID),
		CreatedAt:     site.CreatedAt,
		UpdatedAt:     site.UpdatedAt,
	}
	if cfg, err := site.Dir(); err == nil {
		d.Dir = cfg
	}
	if cfg, err := site.DB(); err == nil && cfg != nil {
		d.Database = &dbDTO{
			Type:        site.DBType,
			Host:        cfg.Host,
			Port:        cfg.Port,
			Database:    cfg.Database,
			User:        cfg.User,
			SSLMode:     cfg.SSLMode,
			PasswordSet: cfg.Password != "",
		}
	}
	if last != nil {
		v := siteBackupDTOFrom(last, nil, false)
		d.LastBackup = &v
	}
	return d
}

func payloadToSiteInput(p sitePayload) service.SiteInput {
	in := service.SiteInput{
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

// ---------------------------------------------------------------- sites

func (w *Web) apiSitesList(resp http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sites, err := w.st.ListSites(ctx)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	out := make([]siteDTO, 0, len(sites))
	for _, site := range sites {
		last, err := w.st.LastSiteBackup(ctx, site.ID)
		if err != nil {
			w.log.Warn("не удалось получить последний прогон сайта", "site_id", site.ID, "err", err)
		}
		out = append(out, w.siteDTO(site, last))
	}
	writeJSON(resp, http.StatusOK, map[string]any{"sites": out})
}

func (w *Web) apiSiteCreate(resp http.ResponseWriter, r *http.Request) {
	var p sitePayload
	if err := decodeJSON(r, &p); err != nil {
		writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	site, err := w.svc.CreateSite(r.Context(), payloadToSiteInput(p))
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	if err := w.sched.SyncSite(site); err != nil {
		w.log.Warn("расписание сайта не применено", "site_id", site.ID, "err", err)
	}
	writeJSON(resp, http.StatusCreated, w.siteDTO(site, nil))
}

func (w *Web) apiSiteGet(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	site, err := w.st.GetSite(r.Context(), id)
	if err != nil {
		w.apiFail(resp, r, err)
		return
	}
	last, _ := w.st.LastSiteBackup(r.Context(), id)
	writeJSON(resp, http.StatusOK, w.siteDTO(site, last))
}

func (w *Web) apiSiteUpdate(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	var p sitePayload
	if err := decodeJSON(r, &p); err != nil {
		writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	site, err := w.svc.UpdateSite(r.Context(), id, payloadToSiteInput(p))
	if err != nil {
		w.apiFail(resp, r, err)
		return
	}
	if err := w.sched.SyncSite(site); err != nil {
		w.log.Warn("расписание сайта не применено", "site_id", site.ID, "err", err)
	}
	last, _ := w.st.LastSiteBackup(r.Context(), id)
	writeJSON(resp, http.StatusOK, w.siteDTO(site, last))
}

func (w *Web) apiSiteDelete(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	removeFiles := r.URL.Query().Get("remove_files") != "false"
	if err := w.svc.DeleteSite(r.Context(), id, removeFiles); err != nil {
		w.apiFail(resp, r, err)
		return
	}
	w.sched.RemoveSite(id)
	writeJSON(resp, http.StatusOK, map[string]any{"ok": true})
}

func (w *Web) apiSiteRun(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	backupID, err := w.svc.StartSiteBackup(r.Context(), id, "manual")
	if err != nil {
		w.apiFail(resp, r, err)
		return
	}
	writeJSON(resp, http.StatusAccepted, map[string]any{"id": backupID, "status": store.StatusQueued})
}

// apiSiteTest checks an unsaved form. The ID inside the body is optional.
func (w *Web) apiSiteTest(resp http.ResponseWriter, r *http.Request) {
	var p sitePayload
	if err := decodeJSON(r, &p); err != nil {
		writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	ctx, cancel := contextWithTimeout(r, 40*time.Second)
	defer cancel()
	writeJSON(resp, http.StatusOK, w.svc.TestSite(ctx, p.ID, payloadToSiteInput(p)))
}

// apiSiteTestStored checks a saved site, optionally with unsaved form values.
func (w *Web) apiSiteTestStored(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := contextWithTimeout(r, 40*time.Second)
	defer cancel()

	res := service.SiteTestResult{}
	if body := readBody(r); len(body) > 0 {
		var p sitePayload
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSONError(resp, http.StatusBadRequest, "некорректный JSON: "+err.Error())
			return
		}
		res = w.svc.TestSite(ctx, id, payloadToSiteInput(p))
	} else {
		res = w.svc.TestStoredSite(ctx, id)
	}
	writeJSON(resp, http.StatusOK, res)
}

// ---------------------------------------------------------------- site runs

func (w *Web) apiSiteBackups(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	runs, err := w.st.ListSiteBackups(r.Context(), id, limit)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	out := make([]siteBackupDTO, 0, len(runs))
	for _, run := range runs {
		out = append(out, siteBackupDTOFrom(run, nil, false))
	}
	writeJSON(resp, http.StatusOK, map[string]any{"backups": out})
}

func (w *Web) apiSiteBackupsList(resp http.ResponseWriter, r *http.Request) {
	siteID, _ := strconv.ParseInt(r.URL.Query().Get("site_id"), 10, 64)
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	runs, err := w.st.ListSiteBackups(r.Context(), siteID, limit)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	out := make([]siteBackupDTO, 0, len(runs))
	for _, run := range runs {
		out = append(out, siteBackupDTOFrom(run, nil, false))
	}
	writeJSON(resp, http.StatusOK, map[string]any{"backups": out})
}

func (w *Web) apiSiteBackupGet(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	run, err := w.st.GetSiteBackup(r.Context(), id)
	if err != nil {
		w.apiFail(resp, r, err)
		return
	}
	files, err := w.st.ListSiteBackupFiles(r.Context(), id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	writeJSON(resp, http.StatusOK, siteBackupDTOFrom(run, files, true))
}

func (w *Web) apiSiteBackupDelete(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	removeFiles := r.URL.Query().Get("remove_files") != "false"
	if err := w.svc.DeleteSiteBackup(r.Context(), id, removeFiles); err != nil {
		w.apiFail(resp, r, err)
		return
	}
	writeJSON(resp, http.StatusOK, map[string]any{"ok": true})
}
