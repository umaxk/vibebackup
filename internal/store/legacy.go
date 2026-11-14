package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// migrateSiteTables renames the tables of the former "sites" entity to
// "projects". Data is preserved; the operation is safe to repeat and is a
// no-op on a fresh database or after a successful migration.
func migrateSiteTables(db *sql.DB) error {
	pairs := [][2]string{
		{"sites", "projects"},
		{"site_backups", "project_backups"},
		{"site_backup_files", "project_backup_files"},
	}
	for _, pair := range pairs {
		oldExists, err := tableExistsDB(context.Background(), db, pair[0])
		if err != nil {
			return err
		}
		if !oldExists {
			continue
		}
		newExists, err := tableExistsDB(context.Background(), db, pair[1])
		if err != nil {
			return err
		}
		if newExists {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE ` + pair[0] + ` RENAME TO ` + pair[1]); err != nil {
			return fmt.Errorf("переименование %s в %s: %w", pair[0], pair[1], err)
		}
	}
	// The renamed tables keep their old index names; the new schema creates
	// indexes under new names, so the old ones are removed to avoid duplicates.
	for _, idx := range []string{"idx_site_backups_site", "idx_site_backups_started", "idx_site_backup_files"} {
		if _, err := db.Exec(`DROP INDEX IF EXISTS ` + idx); err != nil {
			return err
		}
	}
	return nil
}

func tableExistsDB(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var found string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) tableExists(ctx context.Context, name string) (bool, error) {
	return tableExistsDB(ctx, s.db, name)
}

// RemoveLegacyTargets irreversibly deletes the former "targets" entity: its
// backup rows, the backup files and directories it owned, and finally both
// tables. Only paths strictly inside backupDir are touched. The migration is a
// no-op when the legacy tables are already gone.
func (s *Store) RemoveLegacyTargets(ctx context.Context, backupDir string) error {
	hasTargets, err := s.tableExists(ctx, "targets")
	if err != nil {
		return err
	}
	if !hasTargets {
		return nil
	}

	type legacyTarget struct {
		id   int64
		name string
	}
	var targets []legacyTarget
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM targets ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t legacyTarget
		if err := rows.Scan(&t.id, &t.name); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	root := ""
	if strings.TrimSpace(backupDir) != "" {
		if abs, err := filepath.Abs(backupDir); err == nil {
			root = filepath.Clean(abs)
		}
	}

	var firstErr error
	keepErr := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if root != "" {
		hasBackups, err := s.tableExists(ctx, "backups")
		if err != nil {
			return err
		}
		if hasBackups {
			paths, err := s.backupPaths(ctx)
			if err != nil {
				return err
			}
			for _, p := range paths {
				keepErr(removeFileInside(root, p))
			}
		}
		for _, t := range targets {
			dir := filepath.Join(root, legacyTargetSlug(t.name, t.id))
			if !pathInside(root, dir) {
				continue
			}
			if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
				keepErr(err)
			}
		}
	}

	if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS backups`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_backups_target`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_backups_started`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS targets`); err != nil {
		return err
	}
	if err := s.SetSetting(ctx, "legacy_targets_removed", time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return firstErr
}

func (s *Store) backupPaths(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM backups WHERE path <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// removeFileInside removes a file, its .sha256 sibling, and an empty parent
// directory, but only when the path is strictly inside root.
func removeFileInside(root, path string) error {
	clean := filepath.Clean(path)
	if !pathInside(root, clean) {
		return nil
	}
	var firstErr error
	for _, p := range []string{clean, clean + ".sha256"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			firstErr = err
		}
	}
	// Remove now-empty month/year/target directories, stopping at root.
	dir := filepath.Dir(clean)
	for pathInside(root, dir) {
		if err := os.Remove(dir); err != nil {
			break
		}
		dir = filepath.Dir(dir)
	}
	return firstErr
}

func pathInside(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

var legacySlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// legacyTargetSlug reproduces the directory name that the removed target
// implementation used under the backup root.
func legacyTargetSlug(name string, id int64) string {
	slug := legacyTranslit(strings.ToLower(name))
	slug = strings.Trim(legacySlugRe.ReplaceAllString(slug, "-"), "-")
	if slug == "" {
		return fmt.Sprintf("target-%d", id)
	}
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	return fmt.Sprintf("%s-%d", slug, id)
}

var legacyTranslitTable = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "h", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "sch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}

func legacyTranslit(s string) string {
	var b strings.Builder
	for _, r := range s {
		if rep, ok := legacyTranslitTable[r]; ok {
			b.WriteString(rep)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
