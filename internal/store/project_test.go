package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestProjectCRUDAndCascade(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	project := &Project{Name: "example.com", Schedule: "0 3 * * *", Enabled: true, RetentionKeep: 2}
	if err := project.SetDir(&DirConfig{Path: "/sources/project", Exclude: []string{"*.log"}}); err != nil {
		t.Fatalf("SetDir: %v", err)
	}
	if err := project.SetDB(TypePostgres, &DBConfig{Host: "postgres", Port: 5432, Database: "app", User: "backup", Password: "secret"}); err != nil {
		t.Fatalf("SetDB: %v", err)
	}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if project.ID == 0 {
		t.Fatal("CreateProject did not fill the ID")
	}

	got, err := st.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	dir, err := got.Dir()
	if err != nil || dir == nil || dir.Path != "/sources/project" || len(dir.Exclude) != 1 {
		t.Fatalf("Dir = %+v, %v", dir, err)
	}
	db, err := got.DB()
	if err != nil || db == nil || db.Database != "app" || db.Password != "secret" || got.DBType != TypePostgres {
		t.Fatalf("DB = %+v, %v (type %q)", db, err, got.DBType)
	}

	got.Name = "renamed"
	got.Enabled = false
	if err := st.UpdateProject(ctx, got); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	got, err = st.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("GetProject after update: %v", err)
	}
	if got.Name != "renamed" || got.Enabled {
		t.Fatalf("update not applied: %+v", got)
	}

	if list, err := st.ListProjects(ctx); err != nil || len(list) != 1 {
		t.Fatalf("ListProjects = %d, %v", len(list), err)
	}

	run := &ProjectBackup{ProjectID: project.ID, StartedAt: time.Now().UTC(), Status: StatusQueued}
	if err := st.CreateProjectBackup(ctx, run); err != nil {
		t.Fatalf("CreateProjectBackup: %v", err)
	}
	if err := st.MarkProjectBackupRunning(ctx, run.ID); err != nil {
		t.Fatalf("MarkProjectBackupRunning: %v", err)
	}
	files := []*ProjectBackupFile{
		{BackupID: run.ID, Role: "db", Name: "db.dump", Path: "/backups/db.dump", SizeBytes: 10, SHA256: "aa"},
		{BackupID: run.ID, Role: "manifest", Name: "manifest.json", Path: "/backups/manifest.json", SizeBytes: 5, SHA256: "bb"},
	}
	for _, f := range files {
		if err := st.AddProjectBackupFile(ctx, f); err != nil {
			t.Fatalf("AddProjectBackupFile: %v", err)
		}
	}
	fin := time.Now().UTC()
	run.FinishedAt = &fin
	run.Status = StatusOK
	run.SizeBytes = 15
	run.RunDir = "/backups/projects/example-com-1/2026/11/run"
	run.ManifestSHA256 = "cc"
	if err := st.FinishProjectBackup(ctx, run); err != nil {
		t.Fatalf("FinishProjectBackup: %v", err)
	}

	last, err := st.LastProjectBackup(ctx, project.ID)
	if err != nil || last == nil || last.Status != StatusOK || last.ProjectName != "renamed" {
		t.Fatalf("LastProjectBackup = %+v, %v", last, err)
	}
	if runs, err := st.ListSuccessfulProjectBackups(ctx, project.ID); err != nil || len(runs) != 1 {
		t.Fatalf("ListSuccessfulProjectBackups = %d, %v", len(runs), err)
	}
	if gotFiles, err := st.ListProjectBackupFiles(ctx, run.ID); err != nil || len(gotFiles) != 2 {
		t.Fatalf("ListProjectBackupFiles = %d, %v", len(gotFiles), err)
	}
	if all, err := st.ListProjectBackups(ctx, 0, 10); err != nil || len(all) != 1 {
		t.Fatalf("ListProjectBackups(all) = %d, %v", len(all), err)
	}

	if err := st.DeleteProject(ctx, project.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := st.GetProject(ctx, project.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProject after delete = %v, want ErrNotFound", err)
	}
	if _, err := st.GetProjectBackup(ctx, run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProjectBackup after delete = %v, want ErrNotFound", err)
	}
	if gotFiles, err := st.ListProjectBackupFiles(ctx, run.ID); err != nil || len(gotFiles) != 0 {
		t.Fatalf("files after cascade = %d, %v", len(gotFiles), err)
	}
}

func TestMarkProjectBackupsInterrupted(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	project := &Project{Name: "p", Enabled: true}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	run := &ProjectBackup{ProjectID: project.ID, StartedAt: time.Now().UTC(), Status: StatusQueued}
	if err := st.CreateProjectBackup(ctx, run); err != nil {
		t.Fatalf("CreateProjectBackup: %v", err)
	}
	if err := st.MarkProjectBackupsInterrupted(ctx); err != nil {
		t.Fatalf("MarkProjectBackupsInterrupted: %v", err)
	}
	got, err := st.GetProjectBackup(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetProjectBackup: %v", err)
	}
	if got.Status != StatusInterrupted || got.FinishedAt == nil || got.Error == "" {
		t.Fatalf("interrupted run = %+v", got)
	}
}
