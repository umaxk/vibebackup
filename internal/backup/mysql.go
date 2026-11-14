package backup

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"vbackuper/internal/store"
)

var (
	binCache  sync.Once
	dumpBin   string
	clientBin string
)

func mysqlBins() (dump, client string) {
	binCache.Do(func() {
		dump = lookFirst("mysqldump", "mariadb-dump")
		client = lookFirst("mysql", "mariadb")
		if dump == "" {
			dump = "mysqldump"
		}
		if client == "" {
			client = "mysql"
		}
		dumpBin, clientBin = dump, client
	})
	return dumpBin, clientBin
}

func lookFirst(names ...string) string {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

var flagSupport sync.Map // key: bin+"|"+flag -> bool

// supportsFlag checks whether a dump client understands an option. MySQL and
// MariaDB differ here, so the flags are probed once per process.
func supportsFlag(bin, flag string) bool {
	key := bin + "|" + flag
	if v, ok := flagSupport.Load(key); ok {
		return v.(bool)
	}
	out, err := exec.Command(bin, "--help").CombinedOutput()
	ok := err == nil && strings.Contains(string(out), flag)
	flagSupport.Store(key, ok)
	return ok
}

func mysqlPort(cfg store.DBConfig) int {
	if cfg.Port <= 0 {
		return 3306
	}
	return cfg.Port
}

func mysqlEnv(cfg store.DBConfig) []string {
	return append(os.Environ(), "MYSQL_PWD="+cfg.Password)
}

func runMySQL(ctx context.Context, t *store.Target, destFile string, logs *LogBuffer) (Result, error) {
	cfg, err := t.DBConfig()
	if err != nil {
		return Result{}, err
	}
	dump, _ := mysqlBins()
	if _, err := exec.LookPath(dump); err != nil {
		return Result{}, fmt.Errorf("клиент mysqldump не найден в контейнере: %w", err)
	}

	args := []string{
		"--host=" + cfg.Host,
		"--port=" + strconv.Itoa(mysqlPort(cfg)),
		"--user=" + cfg.User,
		"--single-transaction",
		"--quick",
		"--routines",
		"--triggers",
		"--events",
		"--hex-blob",
		"--default-character-set=utf8mb4",
	}
	// MySQL 8 needs --no-tablespaces without the PROCESS privilege; MariaDB's
	// dump client does not know the flag.
	if supportsFlag(dump, "--no-tablespaces") {
		args = append(args, "--no-tablespaces")
	}
	if supportsFlag(dump, "--set-gtid-purged") {
		args = append(args, "--set-gtid-purged=OFF")
	}
	args = append(args, cfg.Database)

	logs.Printf("запускаю %s для %s@%s:%d/%s", dump, cfg.User, cfg.Host, mysqlPort(cfg), cfg.Database)

	f, err := os.Create(destFile)
	if err != nil {
		return Result{}, fmt.Errorf("создание файла: %w", err)
	}
	h := sha256.New()
	zw, err := zstd.NewWriter(io.MultiWriter(f, h), zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		f.Close()
		return Result{}, err
	}

	cmd := exec.CommandContext(ctx, dump, args...)
	cmd.Env = mysqlEnv(cfg)
	cmd.Stdout = zw
	cmd.Stderr = logs

	runErr := cmd.Run()
	zErr := zw.Close()
	fErr := f.Close()
	if runErr == nil {
		runErr = zErr
	}
	if runErr == nil {
		runErr = fErr
	}
	if runErr != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("%s завершился с ошибкой: %w%s", dump, runErr, logSuffix(logs))
	}

	res, err := resultFor(destFile, h)
	if err != nil {
		return Result{}, err
	}
	logs.Printf("%s завершён: %s", dump, humanBytes(res.SizeBytes))
	return res, nil
}

func testMySQL(ctx context.Context, t *store.Target) error {
	cfg, err := t.DBConfig()
	if err != nil {
		return err
	}
	_, client := mysqlBins()
	bin, err := exec.LookPath(client)
	if err != nil {
		return fmt.Errorf("клиент mysql не найден в контейнере: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin,
		"--host="+cfg.Host,
		"--port="+strconv.Itoa(mysqlPort(cfg)),
		"--user="+cfg.User,
		"--connect-timeout=10",
		"--execute=SELECT 1",
		cfg.Database,
	)
	cmd.Env = mysqlEnv(cfg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mysql: %w: %s", err, trimOutput(out))
	}
	return nil
}
