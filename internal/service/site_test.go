package service

import (
	"strings"
	"testing"

	"vbackuper/internal/config"
	"vbackuper/internal/store"
)

func TestSiteFromInputRequiresAPart(t *testing.T) {
	s := &Service{cfg: &config.Config{}}
	if _, err := s.siteFromInput(nil, SiteInput{Name: "site", Enabled: true}); err == nil {
		t.Fatal("expected an error for a site without a directory and a database")
	}
}

func TestSiteFromInputDirOnly(t *testing.T) {
	dir := t.TempDir()
	s := &Service{cfg: &config.Config{SourceRoots: []string{dir}}}
	site, err := s.siteFromInput(nil, SiteInput{
		Name: "example.com", Enabled: true, DirEnabled: true, DirPath: dir,
		DirExclude: []string{"*.log, node_modules"},
	})
	if err != nil {
		t.Fatalf("siteFromInput: %v", err)
	}
	cfg, err := site.Dir()
	if err != nil || cfg == nil {
		t.Fatalf("Dir = %+v, %v", cfg, err)
	}
	if cfg.Path != dir || len(cfg.Exclude) != 2 {
		t.Fatalf("Dir = %+v", cfg)
	}
	if site.DBJSON != "" || site.DBType != "" {
		t.Fatalf("database part must be empty: %+v", site)
	}
}

func TestSiteFromInputRejectsPathOutsideRoots(t *testing.T) {
	s := &Service{cfg: &config.Config{SourceRoots: []string{t.TempDir()}}}
	_, err := s.siteFromInput(nil, SiteInput{
		Name: "site", Enabled: true, DirEnabled: true, DirPath: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "разрешённых каталогов") {
		t.Fatalf("err = %v, want a source-root error", err)
	}
}

func TestNormalizeSiteDBKeepsOldPassword(t *testing.T) {
	old := &store.Site{}
	if err := old.SetDB(store.TypePostgres, &store.DBConfig{
		Host: "h", Port: 5432, Database: "d", User: "u", Password: "secret",
	}); err != nil {
		t.Fatalf("SetDB: %v", err)
	}
	kind, cfg, err := normalizeSiteDB(SiteInput{
		DBType: store.TypePostgres, DBHost: "h", DBName: "d", DBUser: "u",
	}, old)
	if err != nil {
		t.Fatalf("normalizeSiteDB: %v", err)
	}
	if kind != store.TypePostgres || cfg.Password != "secret" || cfg.SSLMode != "prefer" {
		t.Fatalf("kind = %q, cfg = %+v", kind, cfg)
	}
}

func TestNormalizeSiteDBRejectsUnknownType(t *testing.T) {
	if _, _, err := normalizeSiteDB(SiteInput{DBType: "oracle"}, nil); err == nil {
		t.Fatal("expected an error for an unknown database type")
	}
}

func TestSiteSlug(t *testing.T) {
	s := &Service{}
	cases := []struct {
		name string
		want string
	}{
		{"example.com", "example-com-4"},
		{"Мой сайт", "moy-sayt-4"},
		{"", "site-4"},
	}
	for _, c := range cases {
		if got := s.siteSlug(&store.Site{ID: 4, Name: c.name}); got != c.want {
			t.Errorf("siteSlug(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}
