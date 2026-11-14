package web

import (
	"net/http"
	"strconv"
	"strings"

	"vbackuper/internal/store"
)

func (w *Web) backupsPage(resp http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	selected, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	if selected == 0 {
		// The former "site" filter name stays accepted for one release.
		selected, _ = strconv.ParseInt(r.URL.Query().Get("site_id"), 10, 64)
	}

	runs, err := w.st.ListProjectBackups(ctx, selected, 300)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	projects, err := w.st.ListProjects(ctx)
	if err != nil {
		w.fail(resp, r, err)
		return
	}

	d := backupsData{
		viewBase: w.base(r, "Бэкапы"),
		Selected: selected,
	}
	for _, project := range projects {
		d.Projects = append(d.Projects, projectRow{
			ID: project.ID, Name: project.Name, Enabled: project.Enabled,
		})
	}
	for _, run := range runs {
		v := w.projectBackupView(run, nil)
		if v.Status == store.StatusRunning || v.Status == store.StatusQueued {
			d.HasRunning = true
		}
		d.ProjectRuns = append(d.ProjectRuns, v)
	}
	w.render(resp, "backups", d)
}

func (w *Web) settingsPage(resp http.ResponseWriter, r *http.Request) {
	d := settingsData{
		viewBase:      w.base(r, "Настройки"),
		Settings:      w.svc.Settings(r.Context()),
		BackupDir:     w.cfg.BackupDir,
		DataDir:       w.cfg.DataDir,
		SourceRoots:   w.cfg.SourceRoots,
		MaxConcurrent: w.cfg.MaxConcurrent,
		JobTimeout:    w.cfg.JobTimeout.String(),
		AdminUser:     w.cfg.AdminUser,
	}
	w.render(resp, "settings", d)
}

func (w *Web) settingsSave(resp http.ResponseWriter, r *http.Request) {
	values := map[string]string{
		"default_retention": r.PostFormValue("default_retention"),
		"grace_minutes":     r.PostFormValue("grace_minutes"),
		"min_free_mb":       r.PostFormValue("min_free_mb"),
	}
	if err := w.svc.SaveSettings(r.Context(), values); err != nil {
		redirectWithError(resp, r, "/settings", err.Error())
		return
	}
	redirectWithFlash(resp, r, "/settings", "настройки сохранены (окно догоняющих запусков применится после перезапуска)")
}

// ---------------------------------------------------------------- helpers

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func safeBack(v, def string) string {
	if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
		return v
	}
	return def
}
