package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSiteCRUDAndCascade(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	site := &Site{Name: "example.com", Schedule: "0 3 * * *", Enabled: true, RetentionKeep: 2}
	if err := site.SetDir(&DirConfig{Path: "/sources/site", Exclude: []string{"*.log"}}); err != nil {
		t.Fatalf("SetDir: %v", err)
	}
	if err := site.SetDB(TypePostgres, &DBConfig{Host: "postgres", Port: 5432, Database: "app", User: "backup", Password: "secret"}); err != nil {
		t.Fatalf("SetDB: %v", err)
	}
	if err := st.CreateSite(ctx, site); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if site.ID == 0 {
		t.Fatal("CreateSite did not fill the ID")
	}

	got, err := st.GetSite(ctx, site.ID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	dir, err := got.Dir()
	if err != nil || dir == nil || dir.Path != "/sources/site" || len(dir.Exclude) != 1 {
		t.Fatalf("Dir = %+v, %v", dir, err)
	}
	db, err := got.DB()
	if err != nil || db == nil || db.Database != "app" || db.Password != "secret" || got.DBType != TypePostgres {
		t.Fatalf("DB = %+v, %v (type %q)", db, err, got.DBType)
	}

	got.Name = "renamed"
	got.Enabled = false
	if err := st.UpdateSite(ctx, got); err != nil {
		t.Fatalf("UpdateSite: %v", err)
	}
	got, err = st.GetSite(ctx, site.ID)
	if err != nil {
		t.Fatalf("GetSite after update: %v", err)
	}
	if got.Name != "renamed" || got.Enabled {
		t.Fatalf("update not applied: %+v", got)
	}

	if list, err := st.ListSites(ctx); err != nil || len(list) != 1 {
		t.Fatalf("ListSites = %d, %v", len(list), err)
	}

	run := &SiteBackup{SiteID: site.ID, StartedAt: time.Now().UTC(), Status: StatusQueued}
	if err := st.CreateSiteBackup(ctx, run); err != nil {
		t.Fatalf("CreateSiteBackup: %v", err)
	}
	if err := st.MarkSiteBackupRunning(ctx, run.ID); err != nil {
		t.Fatalf("MarkSiteBackupRunning: %v", err)
	}
	files := []*SiteBackupFile{
		{BackupID: run.ID, Role: "db", Name: "db.dump", Path: "/backups/db.dump", SizeBytes: 10, SHA256: "aa"},
		{BackupID: run.ID, Role: "manifest", Name: "manifest.json", Path: "/backups/manifest.json", SizeBytes: 5, SHA256: "bb"},
	}
	for _, f := range files {
		if err := st.AddSiteBackupFile(ctx, f); err != nil {
			t.Fatalf("AddSiteBackupFile: %v", err)
		}
	}
	fin := time.Now().UTC()
	run.FinishedAt = &fin
	run.Status = StatusOK
	run.SizeBytes = 15
	run.RunDir = "/backups/sites/example-com-1/2026/11/run"
	run.ManifestSHA256 = "cc"
	if err := st.FinishSiteBackup(ctx, run); err != nil {
		t.Fatalf("FinishSiteBackup: %v", err)
	}

	last, err := st.LastSiteBackup(ctx, site.ID)
	if err != nil || last == nil || last.Status != StatusOK || last.SiteName != "renamed" {
		t.Fatalf("LastSiteBackup = %+v, %v", last, err)
	}
	if runs, err := st.ListSuccessfulSiteBackups(ctx, site.ID); err != nil || len(runs) != 1 {
		t.Fatalf("ListSuccessfulSiteBackups = %d, %v", len(runs), err)
	}
	if gotFiles, err := st.ListSiteBackupFiles(ctx, run.ID); err != nil || len(gotFiles) != 2 {
		t.Fatalf("ListSiteBackupFiles = %d, %v", len(gotFiles), err)
	}
	if all, err := st.ListSiteBackups(ctx, 0, 10); err != nil || len(all) != 1 {
		t.Fatalf("ListSiteBackups(all) = %d, %v", len(all), err)
	}

	if err := st.DeleteSite(ctx, site.ID); err != nil {
		t.Fatalf("DeleteSite: %v", err)
	}
	if _, err := st.GetSite(ctx, site.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSite after delete = %v, want ErrNotFound", err)
	}
	if _, err := st.GetSiteBackup(ctx, run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSiteBackup after delete = %v, want ErrNotFound", err)
	}
	if gotFiles, err := st.ListSiteBackupFiles(ctx, run.ID); err != nil || len(gotFiles) != 0 {
		t.Fatalf("files after cascade = %d, %v", len(gotFiles), err)
	}
}

func TestMarkSiteBackupsInterrupted(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	site := &Site{Name: "s", Enabled: true}
	if err := st.CreateSite(ctx, site); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	run := &SiteBackup{SiteID: site.ID, StartedAt: time.Now().UTC(), Status: StatusQueued}
	if err := st.CreateSiteBackup(ctx, run); err != nil {
		t.Fatalf("CreateSiteBackup: %v", err)
	}
	if err := st.MarkSiteBackupsInterrupted(ctx); err != nil {
		t.Fatalf("MarkSiteBackupsInterrupted: %v", err)
	}
	got, err := st.GetSiteBackup(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetSiteBackup: %v", err)
	}
	if got.Status != StatusInterrupted || got.FinishedAt == nil || got.Error == "" {
		t.Fatalf("interrupted run = %+v", got)
	}
}
