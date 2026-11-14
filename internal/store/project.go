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

// Project is a named backup source that can combine a directory and a database
// in a single scheduled run. At least one of the two parts is configured.
type Project struct {
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

// Dir decodes the directory configuration. It returns nil when the project has
// no directory part.
func (p *Project) Dir() (*DirConfig, error) {
	if strings.TrimSpace(p.DirJSON) == "" {
		return nil, nil
	}
	var c DirConfig
	if err := json.Unmarshal([]byte(p.DirJSON), &c); err != nil {
		return nil, fmt.Errorf("разбор конфигурации каталога проекта: %w", err)
	}
	return &c, nil
}

// DB decodes the database configuration. It returns nil when the project has
// no database part.
func (p *Project) DB() (*DBConfig, error) {
	if strings.TrimSpace(p.DBJSON) == "" {
		return nil, nil
	}
	var c DBConfig
	if err := json.Unmarshal([]byte(p.DBJSON), &c); err != nil {
		return nil, fmt.Errorf("разбор конфигурации базы данных проекта: %w", err)
	}
	return &c, nil
}

// SetDir stores the directory configuration; nil clears it.
func (p *Project) SetDir(c *DirConfig) error {
	if c == nil {
		p.DirJSON = ""
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	p.DirJSON = string(b)
	return nil
}

// SetDB stores the database type and configuration; nil clears it.
func (p *Project) SetDB(kind string, c *DBConfig) error {
	if c == nil {
		p.DBType = ""
		p.DBJSON = ""
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	p.DBType = kind
	p.DBJSON = string(b)
	return nil
}

// ProjectBackup is one run of a project: it owns a set of artifact files.
type ProjectBackup struct {
	ID             int64
	ProjectID      int64
	ProjectName    string
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

// ProjectBackupFile is one artifact of a project backup run.
type ProjectBackupFile struct {
	ID        int64
	BackupID  int64
	Role      string // dir | db | manifest
	Name      string
	Path      string
	SizeBytes int64
	SHA256    string
}

const projectCols = `id, name, schedule, enabled, retention_keep, dir_config, db_type, db_config, created_at, updated_at`

func scanProject(row scanner) (*Project, error) {
	var (
		p       Project
		enabled int
		created int64
		updated int64
	)
	if err := row.Scan(&p.ID, &p.Name, &p.Schedule, &enabled, &p.RetentionKeep,
		&p.DirJSON, &p.DBType, &p.DBJSON, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.Enabled = enabled != 0
	p.CreatedAt = time.Unix(0, created).UTC()
	p.UpdatedAt = time.Unix(0, updated).UTC()
	return &p, nil
}

// CreateProject inserts a new project and fills its ID.
func (s *Store) CreateProject(ctx context.Context, project *Project) error {
	now := time.Now().UTC()
	if project.CreatedAt.IsZero() {
		project.CreatedAt = now
	}
	project.UpdatedAt = now
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (name, schedule, enabled, retention_keep, dir_config, db_type, db_config, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		project.Name, project.Schedule, boolInt(project.Enabled), project.RetentionKeep,
		project.DirJSON, project.DBType, project.DBJSON, project.CreatedAt.UnixNano(), project.UpdatedAt.UnixNano())
	if err != nil {
		return err
	}
	project.ID, err = res.LastInsertId()
	return err
}

// UpdateProject saves all mutable fields of a project.
func (s *Store) UpdateProject(ctx context.Context, project *Project) error {
	project.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET name = ?, schedule = ?, enabled = ?, retention_keep = ?,
		 dir_config = ?, db_type = ?, db_config = ?, updated_at = ?
		 WHERE id = ?`,
		project.Name, project.Schedule, boolInt(project.Enabled), project.RetentionKeep,
		project.DirJSON, project.DBType, project.DBJSON, project.UpdatedAt.UnixNano(), project.ID)
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

// GetProject returns one project by ID.
func (s *Store) GetProject(ctx context.Context, id int64) (*Project, error) {
	return scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE id = ?`, id))
}

// ListProjects returns all projects ordered by creation.
func (s *Store) ListProjects(ctx context.Context) ([]*Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectCols+` FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Project
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, project)
	}
	return out, rows.Err()
}

// DeleteProject removes a project; its runs and file rows are removed by cascade.
func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
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

const projectBackupSelect = `SELECT b.id, b.site_id, b.started_at, b.finished_at, b.status, b.size_bytes,
	b.run_dir, b.manifest_sha256, b.duration_ms, b.error, b.log_tail, COALESCE(p.name, '')
	FROM project_backups b LEFT JOIN projects p ON p.id = b.site_id`

func scanProjectBackup(row scanner) (*ProjectBackup, error) {
	var (
		b      ProjectBackup
		start  int64
		finish sql.NullInt64
	)
	if err := row.Scan(&b.ID, &b.ProjectID, &start, &finish, &b.Status, &b.SizeBytes,
		&b.RunDir, &b.ManifestSHA256, &b.DurationMS, &b.Error, &b.LogTail, &b.ProjectName); err != nil {
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

// CreateProjectBackup inserts a queued project run and fills its ID.
func (s *Store) CreateProjectBackup(ctx context.Context, b *ProjectBackup) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO project_backups (site_id, started_at, status) VALUES (?, ?, ?)`,
		b.ProjectID, b.StartedAt.UTC().UnixNano(), b.Status)
	if err != nil {
		return err
	}
	b.ID, err = res.LastInsertId()
	return err
}

// MarkProjectBackupRunning flags a queued project run as running.
func (s *Store) MarkProjectBackupRunning(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE project_backups SET status = ? WHERE id = ?`, StatusRunning, id)
	return err
}

// FinishProjectBackup writes the final state of a project run.
func (s *Store) FinishProjectBackup(ctx context.Context, b *ProjectBackup) error {
	var finished any
	if b.FinishedAt != nil {
		finished = b.FinishedAt.UTC().UnixNano()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE project_backups SET finished_at = ?, status = ?, size_bytes = ?, run_dir = ?,
		 manifest_sha256 = ?, duration_ms = ?, error = ?, log_tail = ?
		 WHERE id = ?`,
		finished, b.Status, b.SizeBytes, b.RunDir, b.ManifestSHA256, b.DurationMS, b.Error, b.LogTail, b.ID)
	return err
}

// GetProjectBackup returns one project run by ID.
func (s *Store) GetProjectBackup(ctx context.Context, id int64) (*ProjectBackup, error) {
	return scanProjectBackup(s.db.QueryRowContext(ctx, projectBackupSelect+` WHERE b.id = ?`, id))
}

// ListProjectBackups returns project runs newest first. A zero projectID means all projects.
func (s *Store) ListProjectBackups(ctx context.Context, projectID int64, limit int) ([]*ProjectBackup, error) {
	if limit <= 0 {
		limit = 200
	}
	var (
		rows *sql.Rows
		err  error
	)
	if projectID > 0 {
		rows, err = s.db.QueryContext(ctx, projectBackupSelect+` WHERE b.site_id = ? ORDER BY b.started_at DESC LIMIT ?`, projectID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, projectBackupSelect+` ORDER BY b.started_at DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProjectBackup
	for rows.Next() {
		b, err := scanProjectBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListSuccessfulProjectBackups returns successful runs of a project, newest first.
func (s *Store) ListSuccessfulProjectBackups(ctx context.Context, projectID int64) ([]*ProjectBackup, error) {
	rows, err := s.db.QueryContext(ctx,
		projectBackupSelect+` WHERE b.site_id = ? AND b.status = ? ORDER BY b.started_at DESC`, projectID, StatusOK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProjectBackup
	for rows.Next() {
		b, err := scanProjectBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// LastProjectBackup returns the most recent run of a project, or (nil, nil).
func (s *Store) LastProjectBackup(ctx context.Context, projectID int64) (*ProjectBackup, error) {
	b, err := scanProjectBackup(s.db.QueryRowContext(ctx,
		projectBackupSelect+` WHERE b.site_id = ? ORDER BY b.started_at DESC LIMIT 1`, projectID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return b, err
}

// DeleteProjectBackup removes a run; its file rows are removed by cascade.
func (s *Store) DeleteProjectBackup(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM project_backups WHERE id = ?`, id)
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

// AddProjectBackupFile records one artifact of a project run.
func (s *Store) AddProjectBackupFile(ctx context.Context, f *ProjectBackupFile) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO project_backup_files (backup_id, role, name, path, size_bytes, sha256)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		f.BackupID, f.Role, f.Name, f.Path, f.SizeBytes, f.SHA256)
	if err != nil {
		return err
	}
	f.ID, err = res.LastInsertId()
	return err
}

// ListProjectBackupFiles returns the artifacts of a project run in insertion order.
func (s *Store) ListProjectBackupFiles(ctx context.Context, backupID int64) ([]*ProjectBackupFile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, backup_id, role, name, path, size_bytes, sha256
		 FROM project_backup_files WHERE backup_id = ? ORDER BY id`, backupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProjectBackupFile
	for rows.Next() {
		var f ProjectBackupFile
		if err := rows.Scan(&f.ID, &f.BackupID, &f.Role, &f.Name, &f.Path, &f.SizeBytes, &f.SHA256); err != nil {
			return nil, err
		}
		out = append(out, &f)
	}
	return out, rows.Err()
}

// MarkProjectBackupsInterrupted flags leftovers of a previous process as interrupted.
func (s *Store) MarkProjectBackupsInterrupted(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE project_backups SET status = ?, finished_at = ?, error = CASE WHEN error = '' THEN ? ELSE error END
		 WHERE status IN (?, ?)`,
		StatusInterrupted, time.Now().UTC().UnixNano(), "прервано при остановке сервиса", StatusQueued, StatusRunning)
	return err
}
