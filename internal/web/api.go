package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
	projects, _ := w.st.ListProjects(r.Context())
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
		"projects":       len(projects),
	})
}

func (w *Web) apiFail(resp http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSONError(resp, http.StatusNotFound, "не найдено")
	case errors.Is(err, service.ErrProjectBusy):
		writeJSONError(resp, http.StatusConflict, err.Error())
	default:
		msg := err.Error()
		if strings.Contains(msg, "расписание") || strings.Contains(msg, "обязател") || strings.Contains(msg, "некорректн") ||
			strings.Contains(msg, "каталог") || strings.Contains(msg, "путь") || strings.Contains(msg, "не является") ||
			strings.Contains(msg, "включите") || strings.Contains(msg, "выберите") || strings.Contains(msg, "база данных") ||
			strings.Contains(msg, "пользователь") || strings.Contains(msg, "пароль") || strings.Contains(msg, "порт") {
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
