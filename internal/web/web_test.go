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
	run := &projectBackupView{
		ID: 9, ProjectID: 2, ProjectName: "example.com", Status: "ok", StatusLabel: "успешно",
		StatusClass: "ok", StartedAt: now, SizeBytes: 4096, DurationMS: 2500,
		RunDir: "/backups/projects/example-com-2/2026/11/20261114T030000-b9",
		Files: []projectFileView{
			{Role: "manifest", RoleLabel: "манифест", Name: "manifest.json", SizeBytes: 96},
			{Role: "dir", RoleLabel: "каталог", Name: "files.tar.zst", SizeBytes: 3000},
			{Role: "db", RoleLabel: "база данных", Name: "db.dump", SizeBytes: 1000},
		},
		HasDir: true, HasDB: true, Log: "готово",
	}
	row := projectRow{
		ID: 2, Name: "example.com", Schedule: "0 3 * * *", Enabled: true, NextRun: &now,
		Last: run, Running: true, DirPath: "/sources/project", DBType: "postgres",
		DBTypeLabel: "PostgreSQL",
		DB:          &dbInfo{Host: "postgres", Port: 5432, Database: "example", User: "backup", PasswordSet: true},
	}
	base := viewBase{Title: "Тест", Version: "test", Authed: true}

	datas := map[string]any{
		"login": loginData{viewBase: viewBase{Title: "Вход", Version: "test"}},
		"projects": projectsData{
			viewBase: base, Projects: []projectRow{row}, HasRunning: true,
		},
		"project_form": projectFormData{
			viewBase: base, IsNew: true, Action: "/projects",
			Form: projectForm{
				Name: "example.com", Schedule: "0 3 * * *", RetentionKeep: 7, Enabled: true,
				DirEnabled: true, DirPath: "/sources/project", DirExclude: "*.log",
				DBEnabled: true, DBType: "postgres", DBHost: "postgres", DBPort: "5432",
				DBName: "example", DBUser: "backup", DBSSLMode: "prefer", PasswordSet: true,
			},
			SourceRoots: []string{"/sources", "/sources/project"},
			NextRuns:    []time.Time{now, now.Add(time.Hour)},
		},
		"project_detail": projectDetailData{
			viewBase: base,
			Project: projectInfo{
				ID: 2, Name: "example.com", Schedule: "0 3 * * *", Enabled: true, RetentionKeep: 7,
				CreatedAt: now, NextRun: &now,
				Dir:    &store.DirConfig{Path: "/sources/project", Exclude: []string{"*.log"}},
				DBType: "postgres", DBTypeLabel: "PostgreSQL",
				DB:   &dbInfo{Host: "postgres", Port: 5432, Database: "example", User: "backup", PasswordSet: true},
				Last: run,
			},
			Backups: []projectBackupView{*run}, HasRunning: true,
		},
		"backups": backupsData{
			viewBase: base, ProjectRuns: []projectBackupView{*run},
			Projects: []projectRow{row}, Selected: 2, HasRunning: true,
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
		r := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
		c.setup(r)
		if got := w.authenticated(r); got != c.want {
			t.Errorf("%s: authenticated = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAPIOnlyModeRejectsOtherCredentials(t *testing.T) {
	w := &Web{cfg: testConfig(false, "key-one-0123456789")}

	r := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	r.SetBasicAuth("admin", "pw")
	if w.authenticated(r) {
		t.Fatal("Basic auth must not work in API-only mode")
	}

	r = httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: w.newSessionValue()})
	if w.authenticated(r) {
		t.Fatal("session cookie must not work in API-only mode")
	}

	r = httptest.NewRequest(http.MethodGet, "/api/projects", nil)
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
		key    string
		want   int
	}{
		{http.MethodGet, "/", "", http.StatusNotFound},
		{http.MethodGet, "/login", "", http.StatusNotFound},
		{http.MethodGet, "/projects", "", http.StatusNotFound},
		{http.MethodGet, "/projects/new", "", http.StatusNotFound},
		{http.MethodGet, "/sites", "", http.StatusNotFound},
		{http.MethodGet, "/site-backups/9/download", "", http.StatusNotFound},
		{http.MethodPost, "/login", "", http.StatusNotFound},
		{http.MethodGet, "/static/app.css", "", http.StatusNotFound},
		{http.MethodGet, "/favicon.ico", "", http.StatusNotFound},
		{http.MethodGet, "/healthz", "", http.StatusOK},
		{http.MethodGet, "/api/projects", "", http.StatusUnauthorized},
		{http.MethodGet, "/api/targets", "key-one-0123456789", http.StatusNotFound},
		{http.MethodGet, "/api/backups", "key-one-0123456789", http.StatusNotFound},
		{http.MethodPost, "/api/login", "", http.StatusUnauthorized},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(c.method, c.path, nil)
		if c.key != "" {
			r.Header.Set("X-API-Key", c.key)
		}
		h.ServeHTTP(rec, r)
		if rec.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
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

// TestLegacyUIPathsRedirect checks the temporary compatibility redirects of the
// former "sites" entity.
func TestLegacyUIPathsRedirect(t *testing.T) {
	w, err := New(testConfig(true), nil, nil, nil, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := w.Routes()

	cases := []struct {
		method   string
		path     string
		wantPath string
	}{
		{http.MethodGet, "/sites", "/projects"},
		{http.MethodGet, "/sites/7", "/projects/7"},
		{http.MethodGet, "/sites/7/edit", "/projects/7/edit"},
		{http.MethodGet, "/site-backups/9/download", "/project-backups/9/download"},
		{http.MethodGet, "/sites?x=1", "/projects?x=1"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(c.method, c.path, nil)
		r.SetBasicAuth("admin", "pw")
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusTemporaryRedirect {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, rec.Code, http.StatusTemporaryRedirect)
			continue
		}
		location := rec.Header().Get("Location")
		if location != c.wantPath {
			t.Errorf("%s %s -> %q, want %q", c.method, c.path, location, c.wantPath)
		}
	}
}
