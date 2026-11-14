package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Site is a named backup source that can combine a directory and a database in
// a single scheduled run. At least one of the two parts is configured.
type Site struct {
	ID            int64
	Name          string
	Schedule      string
	Enabled       bool
	RetentionKeep int

	DirJSON string
	DBType  string
	DBJSON  string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Dir decodes the directory configuration. It returns nil when the site has no
// directory part.
func (s *Site) Dir() (*DirConfig, error) {
	if strings.TrimSpace(s.DirJSON) == "" {
		return nil, nil
	}
	var c DirConfig
	if err := json.Unmarshal([]byte(s.DirJSON), &c); err != nil {
		return nil, fmt.Errorf("разбор конфигурации каталога сайта: %w", err)
	}
	return &c, nil
}

// DB decodes the database configuration. It returns nil when the site has no
// database part.
func (s *Site) DB() (*DBConfig, error) {
	if strings.TrimSpace(s.DBJSON) == "" {
		return nil, nil
	}
	var c DBConfig
	if err := json.Unmarshal([]byte(s.DBJSON), &c); err != nil {
		return nil, fmt.Errorf("разбор конфигурации базы данных сайта: %w", err)
	}
	return &c, nil
}

// SetDir stores the directory configuration; nil clears it.
func (s *Site) SetDir(c *DirConfig) error {
	if c == nil {
		s.DirJSON = ""
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	s.DirJSON = string(b)
	return nil
}

// SetDB stores the database type and configuration; nil clears it.
func (s *Site) SetDB(kind string, c *DBConfig) error {
	if c == nil {
		s.DBType = ""
		s.DBJSON = ""
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	s.DBType = kind
	s.DBJSON = string(b)
	return nil
}

// SiteBackup is one run of a site: it owns a set of artifact files.
type SiteBackup struct {
	ID             int64
	SiteID         int64
	SiteName       string
	StartedAt      time.Time
	FinishedAt     *time.Time
	Status         string
	SizeBytes      int64
	RunDir         string
	ManifestSHA256 string
	DurationMS     int64
	Error          string
	LogTail        string
}

// SiteBackupFile is one artifact of a site backup run.
type SiteBackupFile struct {
	ID        int64
	BackupID  int64
	Role      string // dir | db | manifest
	Name      string
	Path      string
	SizeBytes int64
	SHA256    string
}

const siteCols = `id, name, schedule, enabled, retention_keep, dir_config, db_type, db_config, created_at, updated_at`

func scanSite(row scanner) (*Site, error) {
	var (
		s       Site
		enabled int
		created int64
		updated int64
	)
	if err := row.Scan(&s.ID, &s.Name, &s.Schedule, &enabled, &s.RetentionKeep,
		&s.DirJSON, &s.DBType, &s.DBJSON, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s.Enabled = enabled != 0
	s.CreatedAt = time.Unix(0, created).UTC()
	s.UpdatedAt = time.Unix(0, updated).UTC()
	return &s, nil
}

// CreateSite inserts a new site and fills its ID.
func (s *Store) CreateSite(ctx context.Context, site *Site) error {
	now := time.Now().UTC()
	if site.CreatedAt.IsZero() {
		site.CreatedAt = now
	}
	site.UpdatedAt = now
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sites (name, schedule, enabled, retention_keep, dir_config, db_type, db_config, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		site.Name, site.Schedule, boolInt(site.Enabled), site.RetentionKeep,
		site.DirJSON, site.DBType, site.DBJSON, site.CreatedAt.UnixNano(), site.UpdatedAt.UnixNano())
	if err != nil {
		return err
	}
	site.ID, err = res.LastInsertId()
	return err
}

// UpdateSite saves all mutable fields of a site.
func (s *Store) UpdateSite(ctx context.Context, site *Site) error {
	site.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE sites SET name = ?, schedule = ?, enabled = ?, retention_keep = ?,
		 dir_config = ?, db_type = ?, db_config = ?, updated_at = ?
		 WHERE id = ?`,
		site.Name, site.Schedule, boolInt(site.Enabled), site.RetentionKeep,
		site.DirJSON, site.DBType, site.DBJSON, site.UpdatedAt.UnixNano(), site.ID)
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

// GetSite returns one site by ID.
func (s *Store) GetSite(ctx context.Context, id int64) (*Site, error) {
	return scanSite(s.db.QueryRowContext(ctx, `SELECT `+siteCols+` FROM sites WHERE id = ?`, id))
}

// ListSites returns all sites ordered by creation.
func (s *Store) ListSites(ctx context.Context) ([]*Site, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+siteCols+` FROM sites ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Site
	for rows.Next() {
		site, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, site)
	}
	return out, rows.Err()
}

// DeleteSite removes a site; its runs and file rows are removed by cascade.
func (s *Store) DeleteSite(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sites WHERE id = ?`, id)
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

const siteBackupSelect = `SELECT b.id, b.site_id, b.started_at, b.finished_at, b.status, b.size_bytes,
	b.run_dir, b.manifest_sha256, b.duration_ms, b.error, b.log_tail, COALESCE(s.name, '')
	FROM site_backups b LEFT JOIN sites s ON s.id = b.site_id`

func scanSiteBackup(row scanner) (*SiteBackup, error) {
	var (
		b      SiteBackup
		start  int64
		finish sql.NullInt64
	)
	if err := row.Scan(&b.ID, &b.SiteID, &start, &finish, &b.Status, &b.SizeBytes,
		&b.RunDir, &b.ManifestSHA256, &b.DurationMS, &b.Error, &b.LogTail, &b.SiteName); err != nil {
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

// CreateSiteBackup inserts a queued site run and fills its ID.
func (s *Store) CreateSiteBackup(ctx context.Context, b *SiteBackup) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO site_backups (site_id, started_at, status) VALUES (?, ?, ?)`,
		b.SiteID, b.StartedAt.UTC().UnixNano(), b.Status)
	if err != nil {
		return err
	}
	b.ID, err = res.LastInsertId()
	return err
}

// MarkSiteBackupRunning flags a queued site run as running.
func (s *Store) MarkSiteBackupRunning(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE site_backups SET status = ? WHERE id = ?`, StatusRunning, id)
	return err
}

// FinishSiteBackup writes the final state of a site run.
func (s *Store) FinishSiteBackup(ctx context.Context, b *SiteBackup) error {
	var finished any
	if b.FinishedAt != nil {
		finished = b.FinishedAt.UTC().UnixNano()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE site_backups SET finished_at = ?, status = ?, size_bytes = ?, run_dir = ?,
		 manifest_sha256 = ?, duration_ms = ?, error = ?, log_tail = ?
		 WHERE id = ?`,
		finished, b.Status, b.SizeBytes, b.RunDir, b.ManifestSHA256, b.DurationMS, b.Error, b.LogTail, b.ID)
	return err
}

// GetSiteBackup returns one site run by ID.
func (s *Store) GetSiteBackup(ctx context.Context, id int64) (*SiteBackup, error) {
	return scanSiteBackup(s.db.QueryRowContext(ctx, siteBackupSelect+` WHERE b.id = ?`, id))
}

// ListSiteBackups returns site runs newest first. A zero siteID means all sites.
func (s *Store) ListSiteBackups(ctx context.Context, siteID int64, limit int) ([]*SiteBackup, error) {
	if limit <= 0 {
		limit = 200
	}
	var (
		rows *sql.Rows
		err  error
	)
	if siteID > 0 {
		rows, err = s.db.QueryContext(ctx, siteBackupSelect+` WHERE b.site_id = ? ORDER BY b.started_at DESC LIMIT ?`, siteID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, siteBackupSelect+` ORDER BY b.started_at DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SiteBackup
	for rows.Next() {
		b, err := scanSiteBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListSuccessfulSiteBackups returns successful runs of a site, newest first.
func (s *Store) ListSuccessfulSiteBackups(ctx context.Context, siteID int64) ([]*SiteBackup, error) {
	rows, err := s.db.QueryContext(ctx,
		siteBackupSelect+` WHERE b.site_id = ? AND b.status = ? ORDER BY b.started_at DESC`, siteID, StatusOK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SiteBackup
	for rows.Next() {
		b, err := scanSiteBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// LastSiteBackup returns the most recent run of a site, or (nil, nil).
func (s *Store) LastSiteBackup(ctx context.Context, siteID int64) (*SiteBackup, error) {
	b, err := scanSiteBackup(s.db.QueryRowContext(ctx,
		siteBackupSelect+` WHERE b.site_id = ? ORDER BY b.started_at DESC LIMIT 1`, siteID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return b, err
}

// DeleteSiteBackup removes a run; its file rows are removed by cascade.
func (s *Store) DeleteSiteBackup(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM site_backups WHERE id = ?`, id)
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

// AddSiteBackupFile records one artifact of a site run.
func (s *Store) AddSiteBackupFile(ctx context.Context, f *SiteBackupFile) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO site_backup_files (backup_id, role, name, path, size_bytes, sha256)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		f.BackupID, f.Role, f.Name, f.Path, f.SizeBytes, f.SHA256)
	if err != nil {
		return err
	}
	f.ID, err = res.LastInsertId()
	return err
}

// ListSiteBackupFiles returns the artifacts of a site run in insertion order.
func (s *Store) ListSiteBackupFiles(ctx context.Context, backupID int64) ([]*SiteBackupFile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, backup_id, role, name, path, size_bytes, sha256
		 FROM site_backup_files WHERE backup_id = ? ORDER BY id`, backupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SiteBackupFile
	for rows.Next() {
		var f SiteBackupFile
		if err := rows.Scan(&f.ID, &f.BackupID, &f.Role, &f.Name, &f.Path, &f.SizeBytes, &f.SHA256); err != nil {
			return nil, err
		}
		out = append(out, &f)
	}
	return out, rows.Err()
}

// MarkSiteBackupsInterrupted flags leftovers of a previous process as interrupted.
func (s *Store) MarkSiteBackupsInterrupted(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE site_backups SET status = ?, finished_at = ?, error = CASE WHEN error = '' THEN ? ELSE error END
		 WHERE status IN (?, ?)`,
		StatusInterrupted, time.Now().UTC().UnixNano(), "прервано при остановке сервиса", StatusQueued, StatusRunning)
	return err
}
