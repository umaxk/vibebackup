package web

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vbackuper/internal/service"
	"vbackuper/internal/store"
)

func (w *Web) dashboard(resp http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	targets, err := w.st.ListTargets(ctx)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	d := dashboardData{viewBase: w.base(r, "Цели")}
	running := w.svc.RunningTargets()
	for _, t := range targets {
		last, err := w.st.LastBackup(ctx, t.ID)
		if err != nil {
			w.log.Warn("не удалось получить последний бэкап", "target_id", t.ID, "err", err)
		}
		row := w.targetRow(t, last, running[t.ID])
		if row.Running {
			d.HasRunning = true
		}
		d.Targets = append(d.Targets, row)
	}
	w.render(resp, "dashboard", d)
}

func (w *Web) targetNew(resp http.ResponseWriter, r *http.Request) {
	settings := w.svc.Settings(r.Context())
	retention, _ := strconv.Atoi(settings["default_retention"])
	f := targetForm{
		Type:          store.TypeDir,
		Schedule:      "0 3 * * *",
		RetentionKeep: retention,
		Enabled:       true,
		DBPort:        "5432",
		DBSSLMode:     "prefer",
	}
	if roots := w.svc.ListSources(r.Context()); len(roots) > 0 {
		f.DirPath = roots[0]
	}
	w.render(resp, "target_form", w.formData(r, "Новая цель", true, 0, f))
}

func (w *Web) targetEdit(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	t, err := w.st.GetTarget(r.Context(), id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	w.render(resp, "target_form", w.formData(r, "Изменить цель", false, id, formFromTarget(t)))
}

func (w *Web) targetCreate(resp http.ResponseWriter, r *http.Request) {
	in, f := parseTargetForm(r)
	t, err := w.svc.CreateTarget(r.Context(), in)
	if err != nil {
		d := w.formData(r, "Новая цель", true, 0, f)
		d.Error = err.Error()
		resp.WriteHeader(http.StatusBadRequest)
		w.render(resp, "target_form", d)
		return
	}
	if err := w.sched.Sync(t); err != nil {
		w.log.Warn("расписание не применено", "target_id", t.ID, "err", err)
	}
	redirectWithFlash(resp, r, fmt.Sprintf("/targets/%d", t.ID), "цель добавлена")
}

func (w *Web) targetUpdate(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	in, f := parseTargetForm(r)
	in.Enabled = r.PostFormValue("enabled") == "1"
	t, err := w.svc.UpdateTarget(r.Context(), id, in)
	if err != nil {
		d := w.formData(r, "Изменить цель", false, id, f)
		d.Error = err.Error()
		resp.WriteHeader(http.StatusBadRequest)
		w.render(resp, "target_form", d)
		return
	}
	if err := w.sched.Sync(t); err != nil {
		w.log.Warn("расписание не применено", "target_id", t.ID, "err", err)
	}
	redirectWithFlash(resp, r, fmt.Sprintf("/targets/%d", t.ID), "изменения сохранены")
}

func (w *Web) targetDelete(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	removeFiles := r.PostFormValue("delete_files") == "1"
	if err := w.svc.DeleteTarget(r.Context(), id, removeFiles); err != nil {
		redirectWithError(resp, r, fmt.Sprintf("/targets/%d", id), err.Error())
		return
	}
	w.sched.Remove(id)
	redirectWithFlash(resp, r, "/", "цель удалена")
}

func (w *Web) targetRun(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	back := fmt.Sprintf("/targets/%d", id)
	if _, err := w.svc.StartBackup(r.Context(), id, "manual"); err != nil {
		redirectWithError(resp, r, back, err.Error())
		return
	}
	redirectWithFlash(resp, r, back, "бэкап запущен")
}

func (w *Web) targetToggle(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	t, err := w.st.GetTarget(ctx, id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	t.Enabled = !t.Enabled
	if err := w.st.UpdateTarget(ctx, t); err != nil {
		w.fail(resp, r, err)
		return
	}
	if err := w.sched.Sync(t); err != nil {
		w.log.Warn("расписание не применено", "target_id", t.ID, "err", err)
	}
	back := safeBack(r.PostFormValue("back"), "/")
	msg := "цель выключена"
	if t.Enabled {
		msg = "цель включена"
	}
	redirectWithFlash(resp, r, back, msg)
}

func (w *Web) targetTest(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := contextWithTimeout(r, 40*time.Second)
	defer cancel()

	var testErr error
	if r.PostFormValue("name") != "" {
		in, _ := parseTargetForm(r)
		testErr = w.svc.TestInput(ctx, id, in)
	} else {
		testErr = w.svc.TestStored(ctx, id)
	}
	if testErr != nil {
		writeJSON(resp, http.StatusOK, map[string]any{"ok": false, "error": testErr.Error()})
		return
	}
	writeJSON(resp, http.StatusOK, map[string]any{"ok": true})
}

func (w *Web) targetDetail(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	t, err := w.st.GetTarget(ctx, id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	backups, err := w.st.ListTargetBackups(ctx, id, 200)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	d := detailData{
		viewBase:   w.base(r, t.Name),
		Target:     w.targetInfo(t),
		HasRunning: w.svc.IsRunning(id),
	}
	if err := w.sched.ValidateSchedule(t.Schedule); err != nil {
		d.ScheduleError = err.Error()
	}
	if last, err := w.st.LastBackup(ctx, id); err == nil && last != nil {
		v := w.backupView(last)
		d.Target.Last = &v
	}
	for _, b := range backups {
		d.Backups = append(d.Backups, w.backupView(b))
	}
	w.render(resp, "target_detail", d)
}

func (w *Web) backupsPage(resp http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	selected, _ := strconv.ParseInt(r.URL.Query().Get("target_id"), 10, 64)
	selectedSite, _ := strconv.ParseInt(r.URL.Query().Get("site_id"), 10, 64)
	backups, err := w.st.ListBackups(ctx, selected, 300)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	targets, err := w.st.ListTargets(ctx)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	sites, err := w.st.ListSites(ctx)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	runs, err := w.st.ListSiteBackups(ctx, selectedSite, 300)
	if err != nil {
		w.fail(resp, r, err)
		return
	}

	d := backupsData{
		viewBase:     w.base(r, "Бэкапы"),
		Selected:     selected,
		SelectedSite: selectedSite,
	}
	for _, t := range targets {
		d.Targets = append(d.Targets, targetRow{
			ID: t.ID, Name: t.Name, Type: t.Type, TypeLabel: typeLabel(t.Type), Enabled: t.Enabled,
		})
	}
	for _, site := range sites {
		d.Sites = append(d.Sites, siteRow{ID: site.ID, Name: site.Name, Enabled: site.Enabled})
	}
	for _, b := range backups {
		v := w.backupView(b)
		if v.Status == store.StatusRunning || v.Status == store.StatusQueued {
			d.HasRunning = true
		}
		d.Backups = append(d.Backups, v)
	}
	for _, run := range runs {
		v := w.siteBackupView(run, nil)
		if v.Status == store.StatusRunning || v.Status == store.StatusQueued {
			d.HasRunning = true
		}
		d.SiteRuns = append(d.SiteRuns, v)
	}
	w.render(resp, "backups", d)
}

func (w *Web) backupDownload(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	b, err := w.st.GetBackup(r.Context(), id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	if b.Path == "" {
		http.Error(resp, "файл бэкапа отсутствует", http.StatusNotFound)
		return
	}
	f, err := os.Open(b.Path)
	if err != nil {
		w.log.Error("не удалось открыть файл бэкапа", "path", b.Path, "err", err)
		http.Error(resp, "файл не найден на диске", http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	// Keep Cyrillic and other non-ASCII names safe in the header.
	resp.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(b.Path)))
	resp.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(resp, r, filepath.Base(b.Path), fi.ModTime(), f)
}

func (w *Web) backupDelete(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	back := safeBack(r.PostFormValue("back"), "/backups")
	if err := w.svc.DeleteBackup(r.Context(), id, true); err != nil {
		redirectWithError(resp, r, back, err.Error())
		return
	}
	redirectWithFlash(resp, r, back, "бэкап удалён")
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

func (w *Web) formData(r *http.Request, title string, isNew bool, id int64, f targetForm) formData {
	d := formData{
		viewBase:    w.base(r, title),
		IsNew:       isNew,
		TargetID:    id,
		Form:        f,
		PasswordSet: f.PasswordSet,
	}
	if isNew {
		d.Action = "/targets"
	} else {
		d.Action = fmt.Sprintf("/targets/%d", id)
	}
	d.SourceRoots = w.svc.ListSources(r.Context())
	if f.Schedule != "" {
		if runs, err := w.sched.NextRuns(f.Schedule, 5); err == nil {
			d.NextRuns = runs
		}
	}
	return d
}

func formFromTarget(t *store.Target) targetForm {
	f := targetForm{
		Name:          t.Name,
		Type:          t.Type,
		Schedule:      t.Schedule,
		RetentionKeep: t.RetentionKeep,
		Enabled:       t.Enabled,
	}
	switch t.Type {
	case store.TypeDir:
		if cfg, err := t.DirConfig(); err == nil {
			f.DirPath = cfg.Path
			f.DirExclude = strings.Join(cfg.Exclude, "\n")
		}
	case store.TypePostgres, store.TypeMySQL:
		if cfg, err := t.DBConfig(); err == nil {
			f.DBHost = cfg.Host
			f.DBPort = strconv.Itoa(cfg.Port)
			f.DBName = cfg.Database
			f.DBUser = cfg.User
			f.DBSSLMode = cfg.SSLMode
			f.PasswordSet = cfg.Password != ""
		}
	}
	return f
}

func parseTargetForm(r *http.Request) (service.TargetInput, targetForm) {
	f := targetForm{
		Name:          strings.TrimSpace(r.PostFormValue("name")),
		Type:          strings.TrimSpace(r.PostFormValue("type")),
		Schedule:      strings.TrimSpace(r.PostFormValue("schedule")),
		RetentionKeep: atoiDefault(r.PostFormValue("retention_keep"), 0),
		Enabled:       r.PostFormValue("enabled") == "1",
		DirPath:       strings.TrimSpace(r.PostFormValue("dir_path")),
		DirExclude:    strings.TrimSpace(r.PostFormValue("dir_exclude")),
		DBHost:        strings.TrimSpace(r.PostFormValue("db_host")),
		DBPort:        strings.TrimSpace(r.PostFormValue("db_port")),
		DBName:        strings.TrimSpace(r.PostFormValue("db_name")),
		DBUser:        strings.TrimSpace(r.PostFormValue("db_user")),
		DBSSLMode:     strings.TrimSpace(r.PostFormValue("db_sslmode")),
		PasswordSet:   r.PostFormValue("db_password") != "",
	}
	if f.Type == "" {
		f.Type = store.TypeDir
	}
	in := service.TargetInput{
		Type:          f.Type,
		Name:          f.Name,
		Schedule:      f.Schedule,
		Enabled:       f.Enabled,
		RetentionKeep: f.RetentionKeep,
		DirPath:       f.DirPath,
		DirExclude:    []string{f.DirExclude},
		DBHost:        f.DBHost,
		DBPort:        atoiDefault(f.DBPort, 0),
		DBName:        f.DBName,
		DBUser:        f.DBUser,
		DBPassword:    r.PostFormValue("db_password"),
		DBSSLMode:     f.DBSSLMode,
	}
	return in, f
}

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
