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

func (w *Web) projectsPage(resp http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projects, err := w.st.ListProjects(ctx)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	d := projectsData{viewBase: w.base(r, "Проекты")}
	running := w.svc.RunningProjects()
	for _, project := range projects {
		last, err := w.st.LastProjectBackup(ctx, project.ID)
		if err != nil {
			w.log.Warn("не удалось получить последний прогон проекта", "project_id", project.ID, "err", err)
		}
		row := w.projectRow(project, last, running[project.ID])
		if row.Running {
			d.HasRunning = true
		}
		d.Projects = append(d.Projects, row)
	}
	w.render(resp, "projects", d)
}

func (w *Web) projectNew(resp http.ResponseWriter, r *http.Request) {
	retention, _ := strconv.Atoi(w.svc.Settings(r.Context())["default_retention"])
	f := projectForm{
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
	w.render(resp, "project_form", w.projectFormData(r, "Новый проект", true, 0, f))
}

func (w *Web) projectEdit(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	project, err := w.st.GetProject(r.Context(), id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	w.render(resp, "project_form", w.projectFormData(r, "Изменить проект", false, id, projectFormFrom(project)))
}

func (w *Web) projectCreate(resp http.ResponseWriter, r *http.Request) {
	in, f := parseProjectForm(r)
	project, err := w.svc.CreateProject(r.Context(), in)
	if err != nil {
		d := w.projectFormData(r, "Новый проект", true, 0, f)
		d.Error = err.Error()
		resp.WriteHeader(http.StatusBadRequest)
		w.render(resp, "project_form", d)
		return
	}
	if err := w.sched.Sync(project); err != nil {
		w.log.Warn("расписание проекта не применено", "project_id", project.ID, "err", err)
	}
	redirectWithFlash(resp, r, fmt.Sprintf("/projects/%d", project.ID), "проект добавлен")
}

func (w *Web) projectUpdate(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	in, f := parseProjectForm(r)
	project, err := w.svc.UpdateProject(r.Context(), id, in)
	if err != nil {
		d := w.projectFormData(r, "Изменить проект", false, id, f)
		d.Error = err.Error()
		resp.WriteHeader(http.StatusBadRequest)
		w.render(resp, "project_form", d)
		return
	}
	if err := w.sched.Sync(project); err != nil {
		w.log.Warn("расписание проекта не применено", "project_id", project.ID, "err", err)
	}
	redirectWithFlash(resp, r, fmt.Sprintf("/projects/%d", project.ID), "изменения сохранены")
}

func (w *Web) projectDelete(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	removeFiles := r.PostFormValue("delete_files") == "1"
	if err := w.svc.DeleteProject(r.Context(), id, removeFiles); err != nil {
		redirectWithError(resp, r, fmt.Sprintf("/projects/%d", id), err.Error())
		return
	}
	w.sched.Remove(id)
	redirectWithFlash(resp, r, "/projects", "проект удалён")
}

func (w *Web) projectRun(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	back := fmt.Sprintf("/projects/%d", id)
	if _, err := w.svc.StartProjectBackup(r.Context(), id, "manual"); err != nil {
		redirectWithError(resp, r, back, err.Error())
		return
	}
	redirectWithFlash(resp, r, back, "прогон проекта запущен")
}

func (w *Web) projectToggle(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	project, err := w.st.GetProject(ctx, id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	project.Enabled = !project.Enabled
	if err := w.st.UpdateProject(ctx, project); err != nil {
		w.fail(resp, r, err)
		return
	}
	if err := w.sched.Sync(project); err != nil {
		w.log.Warn("расписание проекта не применено", "project_id", project.ID, "err", err)
	}
	back := safeBack(r.PostFormValue("back"), "/projects")
	msg := "проект выключен"
	if project.Enabled {
		msg = "проект включён"
	}
	redirectWithFlash(resp, r, back, msg)
}

func (w *Web) projectTest(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeJSONError(resp, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := contextWithTimeout(r, 40*time.Second)
	defer cancel()

	res := w.svc.TestStoredProject(ctx, id)
	back := fmt.Sprintf("/projects/%d", id)
	if res.OK {
		redirectWithFlash(resp, r, back, projectTestSummary(res))
		return
	}
	redirectWithError(resp, r, back, projectTestSummary(res))
}

func (w *Web) projectDetail(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	project, err := w.st.GetProject(ctx, id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	runs, err := w.st.ListProjectBackups(ctx, id, 200)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	last, err := w.st.LastProjectBackup(ctx, id)
	if err != nil {
		w.log.Warn("не удалось получить последний прогон проекта", "project_id", id, "err", err)
	}

	d := projectDetailData{
		viewBase:   w.base(r, project.Name),
		Project:    w.projectInfo(project, last),
		HasRunning: w.svc.IsProjectRunning(id),
	}
	if err := w.sched.ValidateSchedule(project.Schedule); err != nil {
		d.ScheduleError = err.Error()
	}
	for _, run := range runs {
		files, err := w.st.ListProjectBackupFiles(ctx, run.ID)
		if err != nil {
			w.log.Warn("не удалось получить файлы прогона проекта", "backup_id", run.ID, "err", err)
		}
		v := w.projectBackupView(run, files)
		if v.Status == store.StatusRunning || v.Status == store.StatusQueued {
			d.HasRunning = true
		}
		d.Backups = append(d.Backups, v)
	}
	w.render(resp, "project_detail", d)
}

// ---------------------------------------------------------------- downloads

func (w *Web) projectBackupDownload(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	b, err := w.st.GetProjectBackup(ctx, id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}
	files, err := w.st.ListProjectBackupFiles(ctx, id)
	if err != nil {
		w.fail(resp, r, err)
		return
	}

	if role := strings.TrimSpace(r.URL.Query().Get("file")); role != "" {
		for _, f := range files {
			if f.Role == role {
				w.serveProjectFile(resp, r, f)
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
	sortProjectFiles(files)
	for _, f := range files {
		if _, err := os.Stat(f.Path); err != nil {
			w.log.Error("файл прогона отсутствует", "path", f.Path, "err", err)
			http.Error(resp, "файл прогона не найден на диске: "+f.Name, http.StatusNotFound)
			return
		}
	}

	name := fmt.Sprintf("project-%d-backup-%d.tar", b.ProjectID, b.ID)
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

func (w *Web) serveProjectFile(resp http.ResponseWriter, r *http.Request, f *store.ProjectBackupFile) {
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

// sortProjectFiles puts the manifest first, then the directory and the database.
func sortProjectFiles(files []*store.ProjectBackupFile) {
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

func (w *Web) projectBackupDelete(resp http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}
	back := safeBack(r.PostFormValue("back"), "/backups")
	if err := w.svc.DeleteProjectBackup(r.Context(), id, true); err != nil {
		redirectWithError(resp, r, back, err.Error())
		return
	}
	redirectWithFlash(resp, r, back, "прогон проекта удалён")
}

// ---------------------------------------------------------------- forms

func (w *Web) projectFormData(r *http.Request, title string, isNew bool, id int64, f projectForm) projectFormData {
	d := projectFormData{
		viewBase:  w.base(r, title),
		IsNew:     isNew,
		ProjectID: id,
		Form:      f,
	}
	if isNew {
		d.Action = "/projects"
	} else {
		d.Action = fmt.Sprintf("/projects/%d", id)
	}
	d.SourceRoots = w.svc.ListSources(r.Context())
	if f.Schedule != "" {
		if runs, err := w.sched.NextRuns(f.Schedule, 5); err == nil {
			d.NextRuns = runs
		}
	}
	return d
}

func projectFormFrom(project *store.Project) projectForm {
	f := projectForm{
		Name:          project.Name,
		Schedule:      project.Schedule,
		RetentionKeep: project.RetentionKeep,
		Enabled:       project.Enabled,
		DBType:        project.DBType,
	}
	if cfg, err := project.Dir(); err == nil && cfg != nil {
		f.DirEnabled = true
		f.DirPath = cfg.Path
		f.DirExclude = strings.Join(cfg.Exclude, "\n")
	}
	if cfg, err := project.DB(); err == nil && cfg != nil {
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

func parseProjectForm(r *http.Request) (service.ProjectInput, projectForm) {
	f := projectForm{
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
	in := service.ProjectInput{
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

func projectTestSummary(res service.ProjectTestResult) string {
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
