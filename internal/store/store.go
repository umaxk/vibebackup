package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// Target types.
const (
	TypeDir      = "dir"
	TypePostgres = "postgres"
	TypeMySQL    = "mysql"
)

const schema = `
CREATE TABLE IF NOT EXISTS targets (
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

CREATE TABLE IF NOT EXISTS backups (
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

CREATE INDEX IF NOT EXISTS idx_backups_target ON backups(target_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_backups_started ON backups(started_at DESC);

CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sites (
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

CREATE TABLE IF NOT EXISTS site_backups (
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

CREATE TABLE IF NOT EXISTS site_backup_files (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	backup_id  INTEGER NOT NULL REFERENCES site_backups(id) ON DELETE CASCADE,
	role       TEXT NOT NULL,
	name       TEXT NOT NULL,
	path       TEXT NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	sha256     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_site_backups_site ON site_backups(site_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_site_backups_started ON site_backups(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_site_backup_files ON site_backup_files(backup_id);
`

// Target is a configured backup source.
type Target struct {
	ID            int64
	Type          string
	Name          string
	Config        string
	Schedule      string
	Enabled       bool
	RetentionKeep int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// DirConfig is the configuration of a directory target.
type DirConfig struct {
	Path    string   `json:"path"`
	Exclude []string `json:"exclude,omitempty"`
}

// DBConfig is the configuration of a database target.
type DBConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
	SSLMode  string `json:"sslmode,omitempty"`
}

// DirConfig decodes the target config for directory targets.
func (t *Target) DirConfig() (DirConfig, error) {
	var c DirConfig
	if err := json.Unmarshal([]byte(t.Config), &c); err != nil {
		return c, fmt.Errorf("разбор конфигурации каталога: %w", err)
	}
	return c, nil
}

// DBConfig decodes the target config for database targets.
func (t *Target) DBConfig() (DBConfig, error) {
	var c DBConfig
	if err := json.Unmarshal([]byte(t.Config), &c); err != nil {
		return c, fmt.Errorf("разбор конфигурации базы данных: %w", err)
	}
	return c, nil
}

// SetConfig serialises v into the target config.
func (t *Target) SetConfig(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	t.Config = string(b)
	return nil
}

// Backup is a single backup run.
type Backup struct {
	ID         int64
	TargetID   int64
	StartedAt  time.Time
	FinishedAt *time.Time
	Status     string
	SizeBytes  int64
	SHA256     string
	Path       string
	DurationMS int64
	Error      string
	LogTail    string
	TargetName string
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
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("миграция схемы: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// ---------------------------------------------------------------- targets

const targetCols = `id, type, name, config, schedule, enabled, retention_keep, created_at, updated_at`

type scanner interface {
	Scan(dest ...any) error
}

func scanTarget(row scanner) (*Target, error) {
	var (
		t       Target
		enabled int
		created int64
		updated int64
	)
	if err := row.Scan(&t.ID, &t.Type, &t.Name, &t.Config, &t.Schedule, &enabled, &t.RetentionKeep, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.Enabled = enabled != 0
	t.CreatedAt = time.Unix(0, created).UTC()
	t.UpdatedAt = time.Unix(0, updated).UTC()
	return &t, nil
}

// CreateTarget inserts a new target and fills its ID.
func (s *Store) CreateTarget(ctx context.Context, t *Target) error {
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO targets (type, name, config, schedule, enabled, retention_keep, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Type, t.Name, t.Config, t.Schedule, boolInt(t.Enabled), t.RetentionKeep,
		t.CreatedAt.UnixNano(), t.UpdatedAt.UnixNano())
	if err != nil {
		return err
	}
	t.ID, err = res.LastInsertId()
	return err
}

// UpdateTarget saves all mutable fields of a target.
func (s *Store) UpdateTarget(ctx context.Context, t *Target) error {
	t.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE targets SET type = ?, name = ?, config = ?, schedule = ?, enabled = ?, retention_keep = ?, updated_at = ?
		 WHERE id = ?`,
		t.Type, t.Name, t.Config, t.Schedule, boolInt(t.Enabled), t.RetentionKeep, t.UpdatedAt.UnixNano(), t.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteTarget removes a target and its backup rows (files are handled by the caller).
func (s *Store) DeleteTarget(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM targets WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetTarget returns one target by ID.
func (s *Store) GetTarget(ctx context.Context, id int64) (*Target, error) {
	return scanTarget(s.db.QueryRowContext(ctx, `SELECT `+targetCols+` FROM targets WHERE id = ?`, id))
}

// ListTargets returns all targets ordered by creation.
func (s *Store) ListTargets(ctx context.Context) ([]*Target, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+targetCols+` FROM targets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- backups

const backupSelect = `SELECT b.id, b.target_id, b.started_at, b.finished_at, b.status, b.size_bytes, b.sha256,
	b.path, b.duration_ms, b.error, b.log_tail, COALESCE(t.name, '')
	FROM backups b LEFT JOIN targets t ON t.id = b.target_id`

func scanBackup(row scanner) (*Backup, error) {
	var (
		b      Backup
		start  int64
		finish sql.NullInt64
	)
	if err := row.Scan(&b.ID, &b.TargetID, &start, &finish, &b.Status, &b.SizeBytes, &b.SHA256,
		&b.Path, &b.DurationMS, &b.Error, &b.LogTail, &b.TargetName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	b.StartedAt = time.Unix(0, start).UTC()
	if finish.Valid {
		ts := time.Unix(0, finish.Int64).UTC()
		b.FinishedAt = &ts
	}
	return &b, nil
}

// CreateBackup inserts a queued backup row and fills its ID.
func (s *Store) CreateBackup(ctx context.Context, b *Backup) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO backups (target_id, started_at, status) VALUES (?, ?, ?)`,
		b.TargetID, b.StartedAt.UTC().UnixNano(), b.Status)
	if err != nil {
		return err
	}
	b.ID, err = res.LastInsertId()
	return err
}

// MarkBackupRunning flags a queued backup as running.
func (s *Store) MarkBackupRunning(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backups SET status = ? WHERE id = ?`, StatusRunning, id)
	return err
}

// FinishBackup writes the final state of a backup.
func (s *Store) FinishBackup(ctx context.Context, b *Backup) error {
	var finished any
	if b.FinishedAt != nil {
		finished = b.FinishedAt.UTC().UnixNano()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE backups SET finished_at = ?, status = ?, size_bytes = ?, sha256 = ?, path = ?, duration_ms = ?, error = ?, log_tail = ?
		 WHERE id = ?`,
		finished, b.Status, b.SizeBytes, b.SHA256, b.Path, b.DurationMS, b.Error, b.LogTail, b.ID)
	return err
}

// GetBackup returns one backup by ID.
func (s *Store) GetBackup(ctx context.Context, id int64) (*Backup, error) {
	return scanBackup(s.db.QueryRowContext(ctx, backupSelect+` WHERE b.id = ?`, id))
}

// ListBackups returns the newest backups across all targets.
func (s *Store) ListBackups(ctx context.Context, targetID int64, limit int) ([]*Backup, error) {
	if limit <= 0 {
		limit = 200
	}
	var (
		rows *sql.Rows
		err  error
	)
	if targetID > 0 {
		rows, err = s.db.QueryContext(ctx, backupSelect+` WHERE b.target_id = ? ORDER BY b.started_at DESC LIMIT ?`, targetID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, backupSelect+` ORDER BY b.started_at DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListTargetBackups returns the newest backups of one target.
func (s *Store) ListTargetBackups(ctx context.Context, targetID int64, limit int) ([]*Backup, error) {
	return s.ListBackups(ctx, targetID, limit)
}

// ListSuccessfulBackups returns successful backups of a target, newest first.
func (s *Store) ListSuccessfulBackups(ctx context.Context, targetID int64) ([]*Backup, error) {
	rows, err := s.db.QueryContext(ctx,
		backupSelect+` WHERE b.target_id = ? AND b.status = ? ORDER BY b.started_at DESC`, targetID, StatusOK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// LastBackup returns the most recent backup of a target, or (nil, nil) if none exists.
func (s *Store) LastBackup(ctx context.Context, targetID int64) (*Backup, error) {
	b, err := scanBackup(s.db.QueryRowContext(ctx,
		backupSelect+` WHERE b.target_id = ? ORDER BY b.started_at DESC LIMIT 1`, targetID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return b, err
}

// DeleteBackup removes a backup row.
func (s *Store) DeleteBackup(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM backups WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkInterrupted flags leftovers of a previous process as interrupted.
func (s *Store) MarkInterrupted(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE backups SET status = ?, finished_at = ?, error = CASE WHEN error = '' THEN ? ELSE error END
		 WHERE status IN (?, ?)`,
		StatusInterrupted, time.Now().UTC().UnixNano(), "прервано при остановке сервиса", StatusQueued, StatusRunning)
	return err
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
