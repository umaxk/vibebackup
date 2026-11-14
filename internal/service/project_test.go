package service

import (
	"strings"
	"testing"

	"vbackuper/internal/config"
	"vbackuper/internal/store"
)

func TestProjectFromInputRequiresAPart(t *testing.T) {
	s := &Service{cfg: &config.Config{}}
	if _, err := s.projectFromInput(ProjectInput{Name: "project", Enabled: true}, nil); err == nil {
		t.Fatal("expected an error for a project without a directory and a database")
	}
}

func TestProjectFromInputDirOnly(t *testing.T) {
	dir := t.TempDir()
	s := &Service{cfg: &config.Config{SourceRoots: []string{dir}}}
	project, err := s.projectFromInput(ProjectInput{
		Name: "example.com", Enabled: true, DirEnabled: true, DirPath: dir,
		DirExclude: []string{"*.log, node_modules"},
	}, nil)
	if err != nil {
		t.Fatalf("projectFromInput: %v", err)
	}
	cfg, err := project.Dir()
	if err != nil || cfg == nil {
		t.Fatalf("Dir = %+v, %v", cfg, err)
	}
	if cfg.Path != dir || len(cfg.Exclude) != 2 {
		t.Fatalf("Dir = %+v", cfg)
	}
	if project.DBJSON != "" || project.DBType != "" {
		t.Fatalf("database part must be empty: %+v", project)
	}
}

func TestProjectFromInputRejectsPathOutsideRoots(t *testing.T) {
	s := &Service{cfg: &config.Config{SourceRoots: []string{t.TempDir()}}}
	_, err := s.projectFromInput(ProjectInput{
		Name: "project", Enabled: true, DirEnabled: true, DirPath: t.TempDir(),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "разрешённых каталогов") {
		t.Fatalf("err = %v, want a source-root error", err)
	}
}

func TestNormalizeProjectDBKeepsOldPassword(t *testing.T) {
	old := &store.Project{}
	if err := old.SetDB(store.TypePostgres, &store.DBConfig{
		Host: "h", Port: 5432, Database: "d", User: "u", Password: "secret",
	}); err != nil {
		t.Fatalf("SetDB: %v", err)
	}
	kind, cfg, err := normalizeProjectDB(ProjectInput{
		DBType: store.TypePostgres, DBHost: "h", DBName: "d", DBUser: "u",
	}, old)
	if err != nil {
		t.Fatalf("normalizeProjectDB: %v", err)
	}
	if kind != store.TypePostgres || cfg.Password != "secret" || cfg.SSLMode != "prefer" {
		t.Fatalf("kind = %q, cfg = %+v", kind, cfg)
	}
}

func TestNormalizeProjectDBRejectsUnknownType(t *testing.T) {
	if _, _, err := normalizeProjectDB(ProjectInput{DBType: "oracle"}, nil); err == nil {
		t.Fatal("expected an error for an unknown database type")
	}
}

func TestProjectSlug(t *testing.T) {
	s := &Service{}
	cases := []struct {
		name string
		want string
	}{
		{"example.com", "example-com-4"},
		{"Мой проект", "moy-proekt-4"},
		{"", "project-4"},
	}
	for _, c := range cases {
		if got := s.projectSlug(&store.Project{ID: 4, Name: c.name}); got != c.want {
			t.Errorf("projectSlug(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}
