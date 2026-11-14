package web

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vbackuper/internal/config"
	"vbackuper/internal/store"
)

// TestTemplatesRender parses and executes every page with representative data.
// It catches typos in field names that only appear at render time.
func TestTemplatesRender(t *testing.T) {
	cfg := &config.Config{Version: "test", Location: time.UTC, Secret: []byte("0123456789abcdef0123456789abcdef")}
	w, err := New(cfg, nil, nil, nil, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	now := time.Now().UTC()
	last := &backupView{
		ID: 5, TargetID: 1, TargetName: "Фото", Status: "ok", StatusLabel: "успешно",
		StatusClass: "ok", StartedAt: now, SizeBytes: 2048, DurationMS: 1500,
		FileName: "foto-1-20261114T030000.tar.zst", SHA256: strings.Repeat("a", 64),
		Path: "/backups/foto-1/2026/11/foto-1-20261114T030000.tar.zst", Log: "готово",
	}
	row := targetRow{
		ID: 1, Name: "Фото", Type: "dir", TypeLabel: "каталог", Schedule: "0 3 * * *",
		Enabled: true, NextRun: &now, Last: last,
	}
	base := viewBase{Title: "Тест", Version: "test", Authed: true}

	datas := map[string]any{
		"login": loginData{viewBase: viewBase{Title: "Вход", Version: "test"}},
		"dashboard": dashboardData{
			viewBase: base, Targets: []targetRow{row}, HasRunning: true,
		},
		"target_form": formData{
			viewBase: base, IsNew: true, Action: "/targets",
			Form: targetForm{
				Name: "Фото", Type: "dir", Schedule: "0 3 * * *", RetentionKeep: 7, Enabled: true,
				DirPath: "/sources/photo", DBPort: "5432", DBSSLMode: "prefer", PasswordSet: true,
			},
			SourceRoots: []string{"/sources", "/sources/photo"},
			NextRuns:    []time.Time{now, now.Add(time.Hour)},
		},
		"target_detail": detailData{
			viewBase: base,
			Target: targetInfo{
				ID: 1, Name: "Фото", Type: "dir", TypeLabel: "каталог", Schedule: "0 3 * * *",
				Enabled: true, RetentionKeep: 7, CreatedAt: now, NextRun: &now,
				Dir:  &store.DirConfig{Path: "/sources/photo", Exclude: []string{"*.tmp"}},
				Last: last,
			},
			Backups: []backupView{*last}, HasRunning: true,
		},
		"backups": backupsData{
			viewBase: base, Backups: []backupView{*last}, Targets: []targetRow{row}, Selected: 1, HasRunning: true,
		},
		"settings": settingsData{
			viewBase:  base,
			Settings:  map[string]string{"default_retention": "7", "grace_minutes": "60", "min_free_mb": "512"},
			BackupDir: "/backups", DataDir: "/data", SourceRoots: []string{"/sources"},
			MaxConcurrent: 2, JobTimeout: "2h0m0s", AdminUser: "admin",
		},
	}

	for page, data := range datas {
		tpl, ok := w.tmpl[page]
		if !ok {
			t.Fatalf("template %q not parsed", page)
		}
		var buf bytes.Buffer
		if err := tpl.ExecuteTemplate(&buf, "layout", data); err != nil {
			t.Errorf("render %s: %v", page, err)
			continue
		}
		if !strings.Contains(buf.String(), "vbackuper") {
			t.Errorf("render %s: unexpected output", page)
		}
	}
}

func testConfig(uiEnabled bool, keys ...string) *config.Config {
	return &config.Config{
		Version:       "test",
		Location:      time.UTC,
		Secret:        []byte("0123456789abcdef0123456789abcdef"),
		AdminUser:     "admin",
		AdminPassword: "pw",
		UIEnabled:     uiEnabled,
		APIKeys:       keys,
		SessionTTL:    time.Hour,
	}
}

func TestAPIKeyAuth(t *testing.T) {
	w := &Web{cfg: testConfig(true, "key-one-0123456789", "key-two-0123456789")}

	cases := []struct {
		name  string
		setup func(*http.Request)
		want  bool
	}{
		{"x-api-key", func(r *http.Request) { r.Header.Set("X-API-Key", "key-two-0123456789") }, true},
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer key-one-0123456789") }, true},
		{"wrong key", func(r *http.Request) { r.Header.Set("X-API-Key", "nope") }, false},
		{"no credentials", func(r *http.Request) {}, false},
		{"basic in ui mode", func(r *http.Request) { r.SetBasicAuth("admin", "pw") }, true},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/targets", nil)
		c.setup(r)
		if got := w.authenticated(r); got != c.want {
			t.Errorf("%s: authenticated = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAPIOnlyModeRejectsOtherCredentials(t *testing.T) {
	w := &Web{cfg: testConfig(false, "key-one-0123456789")}

	r := httptest.NewRequest(http.MethodGet, "/api/targets", nil)
	r.SetBasicAuth("admin", "pw")
	if w.authenticated(r) {
		t.Fatal("Basic auth must not work in API-only mode")
	}

	r = httptest.NewRequest(http.MethodGet, "/api/targets", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: w.newSessionValue()})
	if w.authenticated(r) {
		t.Fatal("session cookie must not work in API-only mode")
	}

	r = httptest.NewRequest(http.MethodGet, "/api/targets", nil)
	r.Header.Set("X-API-Key", "key-one-0123456789")
	if !w.authenticated(r) {
		t.Fatal("API key must work in API-only mode")
	}
}

func TestAPIOnlyModeHidesUI(t *testing.T) {
	w, err := New(testConfig(false, "key-one-0123456789"), nil, nil, nil, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := w.Routes()

	cases := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/login", http.StatusNotFound},
		{http.MethodGet, "/targets/new", http.StatusNotFound},
		{http.MethodPost, "/login", http.StatusNotFound},
		{http.MethodGet, "/static/app.css", http.StatusNotFound},
		{http.MethodGet, "/favicon.ico", http.StatusNotFound},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/api/targets", http.StatusUnauthorized},
		{http.MethodPost, "/api/login", http.StatusUnauthorized},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/targets", nil)
	r.Header.Set("X-API-Key", "wrong")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestUIEnabledKeepsLoginPublic(t *testing.T) {
	w, err := New(testConfig(true), nil, nil, nil, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login = %d, want %d", rec.Code, http.StatusOK)
	}
}
