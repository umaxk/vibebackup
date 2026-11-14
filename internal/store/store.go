package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Backup statuses.
const (
	StatusQueued      = "queued"
	StatusRunning     = "running"
	StatusOK          = "ok"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
)

// Database types used by project database parts.
const (
	TypeDir      = "dir"
	TypePostgres = "postgres"
	TypeMySQL    = "mysql"
)

// The column site_id in project_backups keeps its historical name so that the
// one-time table rename from site_backups does not have to rewrite data.
const schema = `
CREATE TABLE IF NOT EXISTS projects (
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

CREATE TABLE IF NOT EXISTS project_backups (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	site_id         INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
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

CREATE TABLE IF NOT EXISTS project_backup_files (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	backup_id  INTEGER NOT NULL REFERENCES project_backups(id) ON DELETE CASCADE,
	role       TEXT NOT NULL,
	name       TEXT NOT NULL,
	path       TEXT NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	sha256     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_project_backups_project ON project_backups(site_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_project_backups_started ON project_backups(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_project_backup_files ON project_backup_files(backup_id);

CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// DirConfig is the configuration of a project directory part.
type DirConfig struct {
	Path    string   `json:"path"`
	Exclude []string `json:"exclude,omitempty"`
}

// DBConfig is the configuration of a project database part.
type DBConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
	SSLMode  string `json:"sslmode,omitempty"`
}

// Store is the SQLite persistence layer.
type Store struct {
	db *sql.DB
}

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("подключение к базе: %w", err)
	}
	if err := migrateSiteTables(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("миграция таблиц сайтов: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("миграция схемы: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

type scanner interface {
	Scan(dest ...any) error
}

// ---------------------------------------------------------------- settings

// GetSetting returns a setting value and whether it is set.
func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSetting upserts a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Settings returns all settings.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
