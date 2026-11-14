package backup

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"

	"vbackuper/internal/store"
)

func pgPort(cfg store.DBConfig) int {
	if cfg.Port <= 0 {
		return 5432
	}
	return cfg.Port
}

// pgEnv builds the libpq environment (avoids putting the password into argv).
func pgEnv(cfg store.DBConfig) []string {
	ssl := cfg.SSLMode
	if ssl == "" {
		ssl = "prefer"
	}
	return append(os.Environ(),
		"PGPASSWORD="+cfg.Password,
		"PGSSLMODE="+ssl,
		"PGCONNECT_TIMEOUT=15",
	)
}

func runPostgres(ctx context.Context, t *store.Target, destFile string, logs *LogBuffer) (Result, error) {
	cfg, err := t.DBConfig()
	if err != nil {
		return Result{}, err
	}
	// Custom format is already compressed and restores with pg_restore.
	args := []string{
		"--format=custom",
		"--compress=6",
		"--no-password",
		"--host", cfg.Host,
		"--port", strconv.Itoa(pgPort(cfg)),
		"--username", cfg.User,
		"--dbname", cfg.Database,
	}
	logs.Printf("запускаю pg_dump для %s@%s:%d/%s", cfg.User, cfg.Host, pgPort(cfg), cfg.Database)
	return streamCommand(ctx, "pg_dump", args, pgEnv(cfg), destFile, logs)
}

func testPostgres(ctx context.Context, t *store.Target) error {
	cfg, err := t.DBConfig()
	if err != nil {
		return err
	}
	bin, err := exec.LookPath("psql")
	if err != nil {
		return fmt.Errorf("клиент psql не найден в контейнере: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin,
		"--no-password",
		"--host", cfg.Host,
		"--port", strconv.Itoa(pgPort(cfg)),
		"--username", cfg.User,
		"--dbname", cfg.Database,
		"--no-align", "--tuples-only",
		"--command", "SELECT 1",
	)
	cmd.Env = pgEnv(cfg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("psql: %w: %s", err, trimOutput(out))
	}
	return nil
}

// streamCommand runs bin with args and writes stdout straight to destFile while
// hashing the bytes on disk.
func streamCommand(ctx context.Context, bin string, args, env []string, destFile string, logs *LogBuffer) (Result, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return Result{}, fmt.Errorf("%s не найден в контейнере: %w", bin, err)
	}
	f, err := os.Create(destFile)
	if err != nil {
		return Result{}, fmt.Errorf("создание файла: %w", err)
	}
	h := sha256.New()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.Stdout = io.MultiWriter(f, h)
	cmd.Stderr = logs

	runErr := cmd.Run()
	closeErr := f.Close()
	if runErr == nil {
		runErr = closeErr
	}
	if runErr != nil {
		// A failed dump leaves a truncated file behind; drop it so the run
		// directory holds only artifacts that are actually recorded.
		_ = os.Remove(destFile)
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("%s завершился с ошибкой: %w%s", bin, runErr, logSuffix(logs))
	}
	res, err := resultFor(destFile, h)
	if err != nil {
		return Result{}, err
	}
	logs.Printf("%s завершён: %s", bin, humanBytes(res.SizeBytes))
	return res, nil
}

func trimOutput(b []byte) string {
	s := string(b)
	if len(s) > 600 {
		s = s[len(s)-600:]
	}
	return s
}
