package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"vbackuper/internal/config"
	"vbackuper/internal/scheduler"
	"vbackuper/internal/service"
	"vbackuper/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

var pages = []string{"login", "dashboard", "target_form", "target_detail", "sites", "site_form", "site_detail", "backups", "settings"}

const sessionCookie = "vbc_session"

// Web serves the HTML UI and the JSON API.
type Web struct {
	cfg     *config.Config
	st      *store.Store
	svc     *service.Service
	sched   *scheduler.Scheduler
	log     *slog.Logger
	tmpl    map[string]*template.Template
	limiter *loginLimiter
}

// New parses templates and returns the web layer.
func New(cfg *config.Config, st *store.Store, svc *service.Service, sched *scheduler.Scheduler, log *slog.Logger) (*Web, error) {
	w := &Web{
		cfg:     cfg,
		st:      st,
		svc:     svc,
		sched:   sched,
		log:     log,
		tmpl:    map[string]*template.Template{},
		limiter: newLoginLimiter(10, 10*time.Minute),
	}
	for _, page := range pages {
		t, err := template.New("layout").Funcs(templateFuncs()).ParseFS(templateFS,
			"templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("шаблон %s: %w", page, err)
		}
		w.tmpl[page] = t
	}
	return w, nil
}

// Routes builds the HTTP handler tree.
func (w *Web) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", w.healthz)

	if w.cfg.UIEnabled {
		mux.HandleFunc("GET /favicon.ico", func(resp http.ResponseWriter, r *http.Request) {
			resp.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("GET /login", w.loginPage)
		mux.HandleFunc("POST /login", w.loginSubmit)
		mux.HandleFunc("POST /logout", w.logout)
		mux.HandleFunc("POST /api/login", w.apiLogin)
		mux.Handle("GET /static/", http.FileServer(http.FS(staticFS)))

		mux.HandleFunc("GET /{$}", w.dashboard)
		mux.HandleFunc("GET /targets/new", w.targetNew)
		mux.HandleFunc("POST /targets", w.targetCreate)
		mux.HandleFunc("GET /targets/{id}", w.targetDetail)
		mux.HandleFunc("GET /targets/{id}/edit", w.targetEdit)
		mux.HandleFunc("POST /targets/{id}", w.targetUpdate)
		mux.HandleFunc("POST /targets/{id}/delete", w.targetDelete)
		mux.HandleFunc("POST /targets/{id}/run", w.targetRun)
		mux.HandleFunc("POST /targets/{id}/toggle", w.targetToggle)
		mux.HandleFunc("POST /targets/{id}/test", w.targetTest)
		mux.HandleFunc("GET /sites", w.sitesPage)
		mux.HandleFunc("GET /sites/new", w.siteNew)
		mux.HandleFunc("POST /sites", w.siteCreate)
		mux.HandleFunc("GET /sites/{id}", w.siteDetail)
		mux.HandleFunc("GET /sites/{id}/edit", w.siteEdit)
		mux.HandleFunc("POST /sites/{id}", w.siteUpdate)
		mux.HandleFunc("POST /sites/{id}/delete", w.siteDelete)
		mux.HandleFunc("POST /sites/{id}/run", w.siteRun)
		mux.HandleFunc("POST /sites/{id}/toggle", w.siteToggle)
		mux.HandleFunc("POST /sites/{id}/test", w.siteTest)
		mux.HandleFunc("GET /site-backups/{id}/download", w.siteBackupDownload)
		mux.HandleFunc("POST /site-backups/{id}/delete", w.siteBackupDelete)
		mux.HandleFunc("GET /backups", w.backupsPage)
		mux.HandleFunc("GET /backups/{id}/download", w.backupDownload)
		mux.HandleFunc("POST /backups/{id}/delete", w.backupDelete)
		mux.HandleFunc("GET /settings", w.settingsPage)
		mux.HandleFunc("POST /settings", w.settingsSave)
	}

	mux.HandleFunc("GET /api/targets", w.apiTargetsList)
	mux.HandleFunc("POST /api/targets", w.apiTargetCreate)
	mux.HandleFunc("GET /api/targets/{id}", w.apiTargetGet)
	mux.HandleFunc("PUT /api/targets/{id}", w.apiTargetUpdate)
	mux.HandleFunc("DELETE /api/targets/{id}", w.apiTargetDelete)
	mux.HandleFunc("POST /api/targets/{id}/test", w.apiTargetTest)
	mux.HandleFunc("POST /api/targets/{id}/run", w.apiTargetRun)
	mux.HandleFunc("GET /api/targets/{id}/backups", w.apiTargetBackups)
	mux.HandleFunc("GET /api/backups", w.apiBackupsList)
	mux.HandleFunc("GET /api/backups/{id}", w.apiBackupGet)
	mux.HandleFunc("DELETE /api/backups/{id}", w.apiBackupDelete)
	mux.HandleFunc("GET /api/backups/{id}/download", w.backupDownload)
	mux.HandleFunc("GET /api/sites", w.apiSitesList)
	mux.HandleFunc("POST /api/sites", w.apiSiteCreate)
	mux.HandleFunc("POST /api/sites/test", w.apiSiteTest)
	mux.HandleFunc("GET /api/sites/{id}", w.apiSiteGet)
	mux.HandleFunc("PUT /api/sites/{id}", w.apiSiteUpdate)
	mux.HandleFunc("DELETE /api/sites/{id}", w.apiSiteDelete)
	mux.HandleFunc("POST /api/sites/{id}/test", w.apiSiteTestStored)
	mux.HandleFunc("POST /api/sites/{id}/run", w.apiSiteRun)
	mux.HandleFunc("GET /api/sites/{id}/backups", w.apiSiteBackups)
	mux.HandleFunc("GET /api/site-backups", w.apiSiteBackupsList)
	mux.HandleFunc("GET /api/site-backups/{id}", w.apiSiteBackupGet)
	mux.HandleFunc("DELETE /api/site-backups/{id}", w.apiSiteBackupDelete)
	mux.HandleFunc("GET /api/site-backups/{id}/download", w.siteBackupDownload)
	mux.HandleFunc("GET /api/options/sources", w.apiSources)
	mux.HandleFunc("POST /api/cron/preview", w.apiCronPreview)
	mux.HandleFunc("POST /api/test", w.apiTest)
	mux.HandleFunc("GET /api/settings", w.apiSettingsGet)
	mux.HandleFunc("PUT /api/settings", w.apiSettingsUpdate)
	mux.HandleFunc("GET /api/system", w.apiSystem)

	return w.middleware(mux)
}

func (w *Web) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, r *http.Request) {
		resp.Header().Set("X-Frame-Options", "DENY")
		resp.Header().Set("X-Content-Type-Options", "nosniff")
		resp.Header().Set("Referrer-Policy", "same-origin")

		// In API-only mode anything that is not the API or the health probe
		// does not exist, even if a handler were registered by mistake.
		if !w.cfg.UIEnabled && r.URL.Path != "/healthz" && !strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(resp, r)
			return
		}

		if w.isPublic(r.URL.Path) {
			next.ServeHTTP(resp, r)
			return
		}
		if !w.authenticated(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				if w.cfg.UIEnabled {
					resp.Header().Set("WWW-Authenticate", `Basic realm="vbackuper"`)
				}
				writeJSONError(resp, http.StatusUnauthorized, "требуется авторизация")
				return
			}
			http.Redirect(resp, r, "/login", http.StatusSeeOther)
			return
		}
		if !w.originOK(r) {
			http.Error(resp, "некорректный Origin запроса", http.StatusForbidden)
			return
		}
		next.ServeHTTP(resp, r)
	})
}

func (w *Web) isPublic(path string) bool {
	if path == "/healthz" {
		return true
	}
	if !w.cfg.UIEnabled {
		return false
	}
	switch path {
	case "/login", "/api/login", "/favicon.ico":
		return true
	}
	return strings.HasPrefix(path, "/static/")
}

func (w *Web) originOK(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (w *Web) healthz(resp http.ResponseWriter, r *http.Request) {
	writeJSON(resp, http.StatusOK, map[string]any{"status": "ok", "version": w.cfg.Version})
}

// ---------------------------------------------------------------- auth

func (w *Web) authenticated(r *http.Request) bool {
	if w.apiKeyValid(r) {
		return true
	}
	// In API-only mode the key is the single accepted credential.
	if !w.cfg.UIEnabled {
		return false
	}
	if user, pass, ok := r.BasicAuth(); ok && w.checkCredentials(user, pass) {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return w.validSession(c.Value)
}

// apiKeyValid checks the X-API-Key header or an Authorization: Bearer token.
// The key is never accepted from the query string: it would leak into logs.
func (w *Web) apiKeyValid(r *http.Request) bool {
	if len(w.cfg.APIKeys) == 0 {
		return false
	}
	key := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if key == "" {
		if auth := r.Header.Get("Authorization"); len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
			key = strings.TrimSpace(auth[7:])
		}
	}
	if key == "" {
		return false
	}
	valid := false
	for _, candidate := range w.cfg.APIKeys {
		if subtle.ConstantTimeCompare([]byte(key), []byte(candidate)) == 1 {
			valid = true
		}
	}
	return valid
}

func (w *Web) checkCredentials(user, pass string) bool {
	uOK := subtle.ConstantTimeCompare([]byte(user), []byte(w.cfg.AdminUser)) == 1
	pOK := subtle.ConstantTimeCompare([]byte(pass), []byte(w.cfg.AdminPassword)) == 1
	return uOK && pOK
}

func (w *Web) sign(exp int64) string {
	mac := hmac.New(sha256.New, w.cfg.Secret)
	fmt.Fprintf(mac, "v1|%d", exp)
	return hex.EncodeToString(mac.Sum(nil))
}

func (w *Web) newSessionValue() string {
	exp := time.Now().Add(w.cfg.SessionTTL).Unix()
	return fmt.Sprintf("%d.%s", exp, w.sign(exp))
}

func (w *Web) validSession(value string) bool {
	expRaw, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(w.sign(exp)), []byte(sig))
}

func (w *Web) setSessionCookie(resp http.ResponseWriter, r *http.Request) {
	http.SetCookie(resp, &http.Cookie{
		Name:     sessionCookie,
		Value:    w.newSessionValue(),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		Expires:  time.Now().Add(w.cfg.SessionTTL),
	})
}

func (w *Web) clearSessionCookie(resp http.ResponseWriter, r *http.Request) {
	http.SetCookie(resp, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   -1,
	})
}

func (w *Web) loginPage(resp http.ResponseWriter, r *http.Request) {
	if w.authenticated(r) {
		http.Redirect(resp, r, "/", http.StatusSeeOther)
		return
	}
	data := loginData{viewBase: w.base(r, "Вход")}
	data.Authed = false
	data.Next = safeNext(r.URL.Query().Get("next"))
	w.render(resp, "login", data)
}

func (w *Web) loginSubmit(resp http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !w.limiter.Allow(ip) {
		redirectWithError(resp, r, "/login", "слишком много неудачных попыток входа, попробуйте позже")
		return
	}
	user := r.PostFormValue("username")
	pass := r.PostFormValue("password")
	if !w.checkCredentials(user, pass) {
		w.limiter.Fail(ip)
		w.log.Warn("неудачная попытка входа", "ip", ip, "user", user)
		redirectWithError(resp, r, "/login", "неверный логин или пароль")
		return
	}
	w.limiter.Reset(ip)
	w.setSessionCookie(resp, r)
	next := safeNext(r.PostFormValue("next"))
	if next == "" {
		next = "/"
	}
	http.Redirect(resp, r, next, http.StatusSeeOther)
}

func (w *Web) logout(resp http.ResponseWriter, r *http.Request) {
	w.clearSessionCookie(resp, r)
	http.Redirect(resp, r, "/login", http.StatusSeeOther)
}

// ---------------------------------------------------------------- helpers

func (w *Web) base(r *http.Request, title string) viewBase {
	q := r.URL.Query()
	return viewBase{
		Title:   title,
		Flash:   q.Get("flash"),
		Error:   q.Get("err"),
		Version: w.cfg.Version,
		Authed:  true,
	}
}

func (w *Web) render(resp http.ResponseWriter, page string, data any) {
	t, ok := w.tmpl[page]
	if !ok {
		http.Error(resp, "шаблон не найден", http.StatusInternalServerError)
		return
	}
	resp.Header().Set("Content-Type", "text/html; charset=utf-8")
	resp.Header().Set("Cache-Control", "no-store")
	if err := t.ExecuteTemplate(resp, "layout", data); err != nil {
		w.log.Error("рендер шаблона", "page", page, "err", err)
	}
}

func (w *Web) fail(resp http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(resp, "не найдено", http.StatusNotFound)
		return
	}
	w.log.Error("http-ошибка", "path", r.URL.Path, "err", err)
	http.Error(resp, "внутренняя ошибка, подробности в логах сервиса", http.StatusInternalServerError)
}

func redirectWithFlash(resp http.ResponseWriter, r *http.Request, path, msg string) {
	redirectWithQuery(resp, r, path, "flash", msg)
}

func redirectWithError(resp http.ResponseWriter, r *http.Request, path, msg string) {
	redirectWithQuery(resp, r, path, "err", msg)
}

func redirectWithQuery(resp http.ResponseWriter, r *http.Request, path, key, msg string) {
	q := url.Values{}
	if msg != "" {
		q.Set(key, msg)
	}
	target := path
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	http.Redirect(resp, r, target, http.StatusSeeOther)
}

func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return ""
	}
	return next
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func parseID(r *http.Request) (int64, error) {
	raw := r.PathValue("id")
	if raw == "" {
		return 0, errors.New("не указан идентификатор")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("некорректный идентификатор %q", raw)
	}
	return id, nil
}

// loginLimiter throttles failed logins per IP.
type loginLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{max: max, window: window, fails: map[string][]time.Time{}}
}

func (l *loginLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-l.window)
	kept := l.fails[ip][:0]
	for _, t := range l.fails[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.fails[ip] = kept
	return len(kept) < l.max
}

func (l *loginLimiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[ip] = append(l.fails[ip], time.Now())
}

func (l *loginLimiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}
