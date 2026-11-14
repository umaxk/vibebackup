package backup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"vbackuper/internal/store"
)

// TestDir checks that a directory exists and is readable, returning the number
// of entries in its root.
func TestDir(ctx context.Context, cfg store.DirConfig) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	_ = ctx

	info, err := os.Stat(cfg.Path)
	if err != nil {
		return 0, fmt.Errorf("каталог %s: %w", cfg.Path, err)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s не является каталогом", cfg.Path)
	}
	entries, err := os.ReadDir(cfg.Path)
	if err != nil {
		return 0, fmt.Errorf("каталог %s недоступен для чтения: %w", cfg.Path, err)
	}
	return len(entries), nil
}

// TestDB connects to the exact database named in cfg and returns a short
// server description. Connecting to the database itself (not just the server)
// is what proves that the database is visible to the configured credentials.
func TestDB(ctx context.Context, kind string, cfg store.DBConfig) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	switch kind {
	case store.TypePostgres:
		return testPostgresInfo(ctx, cfg)
	case store.TypeMySQL:
		return testMySQLInfo(ctx, cfg)
	default:
		return "", fmt.Errorf("неизвестный тип базы данных: %q", kind)
	}
}

func testPostgresInfo(ctx context.Context, cfg store.DBConfig) (string, error) {
	bin, err := exec.LookPath("psql")
	if err != nil {
		return "", fmt.Errorf("клиент psql не найден в контейнере: %w", err)
	}
	port := pgPort(cfg)
	cmd := exec.CommandContext(ctx, bin,
		"--no-password",
		"--host", cfg.Host,
		"--port", strconv.Itoa(port),
		"--username", cfg.User,
		"--dbname", cfg.Database,
		"--no-align", "--tuples-only",
		"--command", "SELECT current_database() || ' ' || current_setting('server_version')",
	)
	cmd.Env = pgEnv(cfg)
	out, errOut, err := cmdOutput(cmd)
	if err != nil {
		return "", dbConnectError("PostgreSQL", cfg, port, joinOutput(out, errOut), err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	dbName, version := cfg.Database, ""
	if len(fields) > 0 {
		dbName = fields[0]
	}
	if len(fields) > 1 {
		version = fields[1]
	}
	if version == "" {
		version = "неизвестной версии"
	}
	return fmt.Sprintf("PostgreSQL %s, база %s доступна", version, dbName), nil
}

func testMySQLInfo(ctx context.Context, cfg store.DBConfig) (string, error) {
	_, client := mysqlBins()
	bin, err := exec.LookPath(client)
	if err != nil {
		return "", fmt.Errorf("клиент mysql не найден в контейнере: %w", err)
	}
	port := mysqlPort(cfg)
	cmd := exec.CommandContext(ctx, bin,
		"--host="+cfg.Host,
		"--port="+strconv.Itoa(port),
		"--user="+cfg.User,
		"--connect-timeout=10",
		"--batch",
		"--skip-column-names",
		"--execute=SELECT VERSION()",
		cfg.Database,
	)
	cmd.Env = mysqlEnv(cfg)
	out, errOut, err := cmdOutput(cmd)
	if err != nil {
		return "", dbConnectError("MySQL/MariaDB", cfg, port, joinOutput(out, errOut), err)
	}
	// Only stdout matters on success: clients print deprecation warnings to
	// stderr, which must not leak into the reported version.
	version := strings.TrimSpace(string(out))
	if version == "" {
		version = "неизвестной версии"
	}
	return fmt.Sprintf("MySQL/MariaDB %s, база %s доступна", version, cfg.Database), nil
}

// cmdOutput runs a client and returns stdout separately from stderr, so that
// informational warnings never mix with the parsed result.
func cmdOutput(cmd *exec.Cmd) (stdout, stderr []byte, err error) {
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	return out, errBuf.Bytes(), err
}

func joinOutput(stdout, stderr []byte) []byte {
	if len(stderr) == 0 {
		return stdout
	}
	out := make([]byte, 0, len(stdout)+len(stderr)+1)
	out = append(out, stdout...)
	if len(stdout) > 0 && stdout[len(stdout)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, stderr...)
	return out
}

// dbConnectError turns client output into an actionable message that separates
// an unreachable server, bad credentials and a missing database.
func dbConnectError(engine string, cfg store.DBConfig, port int, out []byte, err error) error {
	text := strings.TrimSpace(string(out))
	low := strings.ToLower(text)
	addr := fmt.Sprintf("%s:%d", cfg.Host, port)

	switch {
	case strings.Contains(low, "role ") && strings.Contains(low, "does not exist"):
		return fmt.Errorf("пользователь %q не найден на сервере %s", cfg.User, addr)
	case strings.Contains(low, "unknown database") ||
		(strings.Contains(low, "database ") && strings.Contains(low, "does not exist")):
		return fmt.Errorf("база %q не найдена или нет прав доступа", cfg.Database)
	case strings.Contains(low, "password authentication failed") ||
		strings.Contains(low, "access denied") ||
		strings.Contains(low, "authentication failed"):
		return fmt.Errorf("неверные учётные данные для базы %q", cfg.Database)
	case strings.Contains(low, "no pg_hba.conf entry"):
		return fmt.Errorf("сервер %s отклонил подключение (pg_hba.conf)", addr)
	case strings.Contains(low, "could not connect") ||
		strings.Contains(low, "connection refused") ||
		strings.Contains(low, "can't connect") ||
		strings.Contains(low, "no such host") ||
		strings.Contains(low, "server closed the connection") ||
		strings.Contains(low, "timeout"):
		return fmt.Errorf("сервер %s недоступен", addr)
	}

	if text != "" {
		return fmt.Errorf("%s: %s", engine, trimOutput(out))
	}
	return fmt.Errorf("%s: %w", engine, err)
}
