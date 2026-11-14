package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

const legacySiteSchema = `
CREATE TABLE sites (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	name           TEXT NOT NULL,
	schedule       TEXT NOT NULL DEFAULT '',
	enabled        INTEGER NOT NULL DEFAULT 1,
	retention_keep INTEGER NOT NULL DEFAULT 0,
	dir_config     TEXT NOT NULL DEFAULT '',
	db_type        TEXT NOT NULL DEFAULT '',
	db_config      TEXT NOT NULL DEFAULT '',
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL
);
CREATE TABLE site_backups (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	site_id         INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
	started_at      INTEGER NOT NULL,
	finished_at     INTEGER,
	status          TEXT NOT NULL,
	size_bytes      INTEGER NOT NULL DEFAULT 0,
	run_dir         TEXT NOT NULL DEFAULT '',
	manifest_sha256 TEXT NOT NULL DEFAULT '',
	duration_ms     INTEGER NOT NULL DEFAULT 0,
	error           TEXT NOT NULL DEFAULT '',
	log_tail        TEXT NOT NULL DEFAULT ''
);
CREATE TABLE site_backup_files (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	backup_id  INTEGER NOT NULL REFERENCES site_backups(id) ON DELETE CASCADE,
	role       TEXT NOT NULL,
	name       TEXT NOT NULL,
	path       TEXT NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	sha256     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_site_backups_site ON site_backups(site_id, started_at DESC);
CREATE INDEX idx_site_backups_started ON site_backups(started_at DESC);
CREATE INDEX idx_site_backup_files ON site_backup_files(backup_id);
`

const legacyTargetSchema = `
CREATE TABLE targets (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	type           TEXT NOT NULL,
	name           TEXT NOT NULL,
	config         TEXT NOT NULL DEFAULT '{}',
	schedule       TEXT NOT NULL DEFAULT '',
	enabled        INTEGER NOT NULL DEFAULT 1,
	retention_keep INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL
);
CREATE TABLE backups (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	target_id   INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
	started_at  INTEGER NOT NULL,
	finished_at INTEGER,
	status      TEXT NOT NULL,
	size_bytes  INTEGER NOT NULL DEFAULT 0,
	sha256      TEXT NOT NULL DEFAULT '',
	path        TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0,
	error       TEXT NOT NULL DEFAULT '',
	log_tail    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_backups_target ON backups(target_id, started_at DESC);
CREATE INDEX idx_backups_started ON backups(started_at DESC);
`

func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	return db
}

func TestOpenRenamesLegacySiteTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw := openRaw(t, path)
	if _, err := raw.Exec(legacySiteSchema); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO sites (id, name, enabled, created_at, updated_at) VALUES (5, 'old site', 1, 1, 1)`); err != nil {
		t.Fatalf("insert site: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO site_backups (id, site_id, started_at, status, run_dir) VALUES (7, 5, 1, 'ok', '/backups/sites/a/run')`); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO site_backup_files (backup_id, role, name, path) VALUES (7, 'dir', 'files.tar.zst', '/backups/sites/a/run/files.tar.zst')`); err != nil {
		t.Fatalf("insert file: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	project, err := st.GetProject(ctx, 5)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if project.Name != "old site" {
		t.Fatalf("project name = %q", project.Name)
	}
	run, err := st.GetProjectBackup(ctx, 7)
	if err != nil {
		t.Fatalf("GetProjectBackup: %v", err)
	}
	if run.ProjectID != 5 || run.ProjectName != "old site" {
		t.Fatalf("run = %+v", run)
	}
	files, err := st.ListProjectBackupFiles(ctx, 7)
	if err != nil || len(files) != 1 || files[0].Role != "dir" {
		t.Fatalf("files = %+v, %v", files, err)
	}
}

func TestRemoveLegacyTargets(t *testing.T) {
	dbDir := t.TempDir()
	backupDir := t.TempDir()
	path := filepath.Join(dbDir, "legacy.db")
	raw := openRaw(t, path)
	if _, err := raw.Exec(legacyTargetSchema); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO targets (id, type, name, created_at, updated_at) VALUES (1, 'dir', 'Фото', 1, 1), (2, 'dir', 'Empty', 1, 1)`); err != nil {
		t.Fatalf("insert targets: %v", err)
	}

	file := filepath.Join(backupDir, legacyTargetSlug("Фото", 1), "2026", "11", "run.tar.zst")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file+".sha256", []byte("sum"), 0o644); err != nil {
		t.Fatal(err)
	}
	emptyDir := filepath.Join(backupDir, legacyTargetSlug("Empty", 2), "2026", "11")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	keepDir := filepath.Join(backupDir, "sites", "keep-me")
	if err := os.MkdirAll(keepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO backups (target_id, started_at, status, path) VALUES (1, 1, 'ok', ?)`, file); err != nil {
		t.Fatalf("insert backup: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.RemoveLegacyTargets(ctx, backupDir); err != nil {
		t.Fatalf("RemoveLegacyTargets: %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("legacy backup file still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(filepath.Dir(file)))); !os.IsNotExist(err) {
		t.Fatalf("legacy target directory still exists: %v", err)
	}
	if _, err := os.Stat(emptyDir); !os.IsNotExist(err) {
		t.Fatalf("empty legacy target directory still exists: %v", err)
	}
	if _, err := os.Stat(keepDir); err != nil {
		t.Fatalf("unrelated directory was removed: %v", err)
	}
	for _, table := range []string{"targets", "backups"} {
		ok, err := st.tableExists(ctx, table)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("table %s still exists", table)
		}
	}
	if v, ok, err := st.GetSetting(ctx, "legacy_targets_removed"); err != nil || !ok || v == "" {
		t.Fatalf("migration marker = %q, %v, %v", v, ok, err)
	}
	if err := st.RemoveLegacyTargets(ctx, backupDir); err != nil {
		t.Fatalf("second RemoveLegacyTargets: %v", err)
	}
}
