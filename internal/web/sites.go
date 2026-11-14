package web

import (
	"archive/tar"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"vbackuper/internal/service"
	"vbackuper/internal/store"
)

// ---------------------------------------------------------------- pages

func (w *Web) sitesPage(resp http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sites, err := w.st.ListSites(ctx)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	d := sitesData{viewBase: w.base(r, "Сайты")}
	running := w.svc.RunningSites()
	for _, site := range sites {
		last, err := w.st.LastSiteBackup(ctx, site.ID)
		if err != nil {
			w.log.Warn("не удалось получить последний прогон сайта", "site_id", site.ID, "err", err)
		}
		row := w.siteRow(site, last, running[site.ID])
		if row.Running {
			d.HasRunning = true
		}
		d.Sites = append(d.Sites, row)
	}
	w.render(resp, "sites", d)
}

func (w *Web) siteNew(resp http.ResponseWriter, r *http.Request) {
	retention, _ := strconv.Atoi(w.svc.Settings(r.Context())["default_retention"])
	f := siteForm{
		Schedule:      "0 3 * * *",
		RetentionKeep: retention,
		Enabled:       true,
		DirEnabled:    true,
		DBType:        store.TypePostgres,
		DBPort:        "5432",
		DBSSLMode:     "prefer",
	}
	if roots := w.svc.ListSources(r.Context()); len(roots) > 0 {
		f.DirPath = roots[0]
	}
	w.render(resp, "site_form", w.siteFormData(r, "Новый сайт", true, 0, f))
}

func (w *Web) siteEdit(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	site, err := w.st.GetSite(r.Context(), id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	w.render(resp, "site_form", w.siteFormData(r, "Изменить сайт", false, id, siteFormFrom(site)))
}

func (w *Web) siteCreate(resp http.ResponseWriter, r *http.Request) {
	in, f := parseSiteForm(r)
	site, err := w.svc.CreateSite(r.Context(), in)
	if err != nil {
		d := w.siteFormData(r, "Новый сайт", true, 0, f)
		d.Error = err.Error()
		resp.WriteHeader(http.StatusBadRequest)
		w.render(resp, "site_form", d)
		return
	}
	if err := w.sched.SyncSite(site); err != nil {
		w.log.Warn("расписание сайта не применено", "site_id", site.ID, "err", err)
	}
	redirectWithFlash(resp, r, fmt.Sprintf("/sites/%d", site.ID), "сайт добавлен")
}

func (w *Web) siteUpdate(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	in, f := parseSiteForm(r)
	site, err := w.svc.UpdateSite(r.Context(), id, in)
	if err != nil {
		d := w.siteFormData(r, "Изменить сайт", false, id, f)
		d.Error = err.Error()
		resp.WriteHeader(http.StatusBadRequest)
		w.render(resp, "site_form", d)
		return
	}
	if err := w.sched.SyncSite(site); err != nil {
		w.log.Warn("расписание сайта не применено", "site_id", site.ID, "err", err)
	}
	redirectWithFlash(resp, r, fmt.Sprintf("/sites/%d", site.ID), "изменения сохранены")
}

func (w *Web) siteDelete(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	removeFiles := r.PostFormValue("delete_files") == "1"
	if err := w.svc.DeleteSite(r.Context(), id, removeFiles); err != nil {
		redirectWithError(resp, r, fmt.Sprintf("/sites/%d", id), err.Error())
		return
	}
	w.sched.RemoveSite(id)
	redirectWithFlash(resp, r, "/sites", "сайт удалён")
}

func (w *Web) siteRun(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	back := fmt.Sprintf("/sites/%d", id)
	if _, err := w.svc.StartSiteBackup(r.Context(), id, "manual"); err != nil {
		redirectWithError(resp, r, back, err.Error())
		return
	}
	redirectWithFlash(resp, r, back, "прогон сайта запущен")
}

func (w *Web) siteToggle(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	site, err := w.st.GetSite(ctx, id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	site.Enabled = !site.Enabled
	if err := w.st.UpdateSite(ctx, site); err != nil {
		w.fail(resp, r, err)
		return
	}
	if err := w.sched.SyncSite(site); err != nil {
		w.log.Warn("расписание сайта не применено", "site_id", site.ID, "err", err)
	}
	back := safeBack(r.PostFormValue("back"), "/sites")
	msg := "сайт выключен"
	if site.Enabled {
		msg = "сайт включён"
	}
	redirectWithFlash(resp, r, back, msg)
}

func (w *Web) siteTest(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := contextWithTimeout(r, 40*time.Second)
	defer cancel()

	res := w.svc.TestStoredSite(ctx, id)
	back := fmt.Sprintf("/sites/%d", id)
	if res.OK {
		redirectWithFlash(resp, r, back, siteTestSummary(res))
		return
	}
	redirectWithError(resp, r, back, siteTestSummary(res))
}

func (w *Web) siteDetail(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	site, err := w.st.GetSite(ctx, id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	runs, err := w.st.ListSiteBackups(ctx, id, 200)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	last, err := w.st.LastSiteBackup(ctx, id)
	if err != nil {
		w.log.Warn("не удалось получить последний прогон сайта", "site_id", id, "err", err)
	}

	d := siteDetailData{
		viewBase:   w.base(r, site.Name),
		Site:       w.siteInfo(site, last),
		HasRunning: w.svc.IsSiteRunning(id),
	}
	if err := w.sched.ValidateSchedule(site.Schedule); err != nil {
		d.ScheduleError = err.Error()
	}
	for _, run := range runs {
		files, err := w.st.ListSiteBackupFiles(ctx, run.ID)
		if err != nil {
			w.log.Warn("не удалось получить файлы прогона сайта", "backup_id", run.ID, "err", err)
		}
		v := w.siteBackupView(run, files)
		if v.Status == store.StatusRunning || v.Status == store.StatusQueued {
			d.HasRunning = true
		}
		d.Backups = append(d.Backups, v)
	}
	w.render(resp, "site_detail", d)
}

// ---------------------------------------------------------------- downloads

func (w *Web) siteBackupDownload(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	b, err := w.st.GetSiteBackup(ctx, id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	files, err := w.st.ListSiteBackupFiles(ctx, id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}

	if role := strings.TrimSpace(r.URL.Query().Get("file")); role != "" {
		for _, f := range files {
			if f.Role == role {
				w.serveSiteFile(resp, r, f)
				return
			}
		}
		http.Error(resp, "файл не найден в прогоне", http.StatusNotFound)
		return
	}

	if b.Status == store.StatusRunning || b.Status == store.StatusQueued {
		http.Error(resp, "прогон ещё выполняется", http.StatusConflict)
		return
	}
	if len(files) == 0 {
		http.Error(resp, "у прогона нет файлов", http.StatusNotFound)
		return
	}
	sortSiteFiles(files)
	for _, f := range files {
		if _, err := os.Stat(f.Path); err != nil {
			w.log.Error("файл прогона отсутствует", "path", f.Path, "err", err)
			http.Error(resp, "файл прогона не найден на диске: "+f.Name, http.StatusNotFound)
			return
		}
	}

	name := fmt.Sprintf("site-%d-backup-%d.tar", b.SiteID, b.ID)
	resp.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	resp.Header().Set("Content-Type", "application/x-tar")

	tw := tar.NewWriter(resp)
	defer tw.Close()
	for _, f := range files {
		fh, err := os.Open(f.Path)
		if err != nil {
			w.log.Error("не удалось открыть файл прогона", "path", f.Path, "err", err)
			return
		}
		fi, err := fh.Stat()
		if err != nil {
			fh.Close()
			w.log.Error("не удалось прочитать файл прогона", "path", f.Path, "err", err)
			return
		}
		hdr := &tar.Header{
			Name:     f.Name,
			Size:     fi.Size(),
			Mode:     int64(fi.Mode().Perm()),
			ModTime:  fi.ModTime(),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			fh.Close()
			w.log.Error("не удалось записать заголовок tar", "err", err)
			return
		}
		if _, err := io.Copy(tw, fh); err != nil {
			fh.Close()
			w.log.Error("не удалось записать файл в tar", "path", f.Path, "err", err)
			return
		}
		fh.Close()
	}
}

func (w *Web) serveSiteFile(resp http.ResponseWriter, r *http.Request, f *store.SiteBackupFile) {
	fh, err := os.Open(f.Path)
	if err != nil {
		w.log.Error("не удалось открыть файл прогона", "path", f.Path, "err", err)
		http.Error(resp, "файл не найден на диске", http.StatusNotFound)
		return
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	resp.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(f.Name)))
	resp.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(resp, r, filepath.Base(f.Name), fi.ModTime(), fh)
}

// sortSiteFiles puts the manifest first, then the directory and the database.
func sortSiteFiles(files []*store.SiteBackupFile) {
	rank := func(role string) int {
		switch role {
		case "manifest":
			return 0
		case "dir":
			return 1
		case "db":
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return rank(files[i].Role) < rank(files[j].Role) })
}

func (w *Web) siteBackupDelete(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	back := safeBack(r.PostFormValue("back"), "/backups")
	if err := w.svc.DeleteSiteBackup(r.Context(), id, true); err != nil {
		redirectWithError(resp, r, back, err.Error())
		return
	}
	redirectWithFlash(resp, r, back, "прогон сайта удалён")
}

// ---------------------------------------------------------------- forms

func (w *Web) siteFormData(r *http.Request, title string, isNew bool, id int64, f siteForm) siteFormData {
	d := siteFormData{
		viewBase: w.base(r, title),
		IsNew:    isNew,
		SiteID:   id,
		Form:     f,
	}
	if isNew {
		d.Action = "/sites"
	} else {
		d.Action = fmt.Sprintf("/sites/%d", id)
	}
	d.SourceRoots = w.svc.ListSources(r.Context())
	if f.Schedule != "" {
		if runs, err := w.sched.NextRuns(f.Schedule, 5); err == nil {
			d.NextRuns = runs
		}
	}
	return d
}

func siteFormFrom(site *store.Site) siteForm {
	f := siteForm{
		Name:          site.Name,
		Schedule:      site.Schedule,
		RetentionKeep: site.RetentionKeep,
		Enabled:       site.Enabled,
		DBType:        site.DBType,
	}
	if cfg, err := site.Dir(); err == nil && cfg != nil {
		f.DirEnabled = true
		f.DirPath = cfg.Path
		f.DirExclude = strings.Join(cfg.Exclude, "\n")
	}
	if cfg, err := site.DB(); err == nil && cfg != nil {
		f.DBEnabled = true
		f.DBHost = cfg.Host
		f.DBPort = strconv.Itoa(cfg.Port)
		f.DBName = cfg.Database
		f.DBUser = cfg.User
		f.DBSSLMode = cfg.SSLMode
		f.PasswordSet = cfg.Password != ""
	}
	if f.DBType == "" {
		f.DBType = store.TypePostgres
	}
	if f.DBPort == "" {
		if f.DBType == store.TypeMySQL {
			f.DBPort = "3306"
		} else {
			f.DBPort = "5432"
		}
	}
	if f.DBSSLMode == "" {
		f.DBSSLMode = "prefer"
	}
	return f
}

func parseSiteForm(r *http.Request) (service.SiteInput, siteForm) {
	f := siteForm{
		Name:          strings.TrimSpace(r.PostFormValue("name")),
		Schedule:      strings.TrimSpace(r.PostFormValue("schedule")),
		RetentionKeep: atoiDefault(r.PostFormValue("retention_keep"), 0),
		Enabled:       r.PostFormValue("enabled") == "1",
		DirEnabled:    r.PostFormValue("dir_enabled") == "1",
		DirPath:       strings.TrimSpace(r.PostFormValue("dir_path")),
		DirExclude:    strings.TrimSpace(r.PostFormValue("dir_exclude")),
		DBEnabled:     r.PostFormValue("db_enabled") == "1",
		DBType:        strings.TrimSpace(r.PostFormValue("db_type")),
		DBHost:        strings.TrimSpace(r.PostFormValue("db_host")),
		DBPort:        strings.TrimSpace(r.PostFormValue("db_port")),
		DBName:        strings.TrimSpace(r.PostFormValue("db_name")),
		DBUser:        strings.TrimSpace(r.PostFormValue("db_user")),
		DBSSLMode:     strings.TrimSpace(r.PostFormValue("db_sslmode")),
		PasswordSet:   r.PostFormValue("db_password") != "",
	}
	if f.DBType == "" {
		f.DBType = store.TypePostgres
	}
	in := service.SiteInput{
		Name:          f.Name,
		Schedule:      f.Schedule,
		Enabled:       f.Enabled,
		RetentionKeep: f.RetentionKeep,
		DirEnabled:    f.DirEnabled,
		DirPath:       f.DirPath,
		DirExclude:    []string{f.DirExclude},
		DBEnabled:     f.DBEnabled,
		DBType:        f.DBType,
		DBHost:        f.DBHost,
		DBPort:        atoiDefault(f.DBPort, 0),
		DBName:        f.DBName,
		DBUser:        f.DBUser,
		DBPassword:    r.PostFormValue("db_password"),
		DBSSLMode:     f.DBSSLMode,
	}
	return in, f
}

func siteTestSummary(res service.SiteTestResult) string {
	var parts []string
	if res.Dir != nil {
		parts = append(parts, "каталог: "+partSummary(*res.Dir))
	}
	if res.DB != nil {
		parts = append(parts, "база: "+partSummary(*res.DB))
	}
	if res.Error != "" {
		parts = append(parts, res.Error)
	}
	return strings.Join(parts, "; ")
}

func partSummary(p service.PartTestResult) string {
	if p.OK {
		if p.Info != "" {
			return "ок (" + p.Info + ")"
		}
		return "ок"
	}
	return "ошибка: " + p.Error
}
