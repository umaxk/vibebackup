package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds every runtime setting of the service.
type Config struct {
	Addr             string
	DataDir          string
	BackupDir        string
	SourceRoots      []string
	AdminUser        string
	AdminPassword    string
	APIKeys          []string
	UIEnabled        bool
	Secret           []byte
	MaxConcurrent    int
	JobTimeout       time.Duration
	MinFreeMB        int64
	DefaultRetention int
	GraceMinutes     int
	SessionTTL       time.Duration
	Version          string
	Location         *time.Location
}

// Load reads configuration from the environment and prepares directories.
func Load(version string) (*Config, error) {
	loc := time.Local
	if tz := strings.TrimSpace(os.Getenv("VBC_TZ")); tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("VBC_TZ: %w", err)
		}
		loc = l
	}

	c := &Config{
		Addr:             env("VBC_ADDR", ":8787"),
		DataDir:          filepath.Clean(env("VBC_DATA_DIR", "/data")),
		BackupDir:        filepath.Clean(env("VBC_BACKUP_DIR", "/backups")),
		AdminUser:        env("VBC_ADMIN_USER", "admin"),
		AdminPassword:    os.Getenv("VBC_ADMIN_PASSWORD"),
		UIEnabled:        envBool("VBC_UI_ENABLED", true),
		MaxConcurrent:    envInt("VBC_MAX_CONCURRENT", 2),
		JobTimeout:       envDuration("VBC_JOB_TIMEOUT", 2*time.Hour),
		MinFreeMB:        int64(envInt("VBC_MIN_FREE_MB", 512)),
		DefaultRetention: envInt("VBC_DEFAULT_RETENTION", 7),
		GraceMinutes:     envInt("VBC_GRACE_MINUTES", 60),
		SessionTTL:       7 * 24 * time.Hour,
		Version:          version,
		Location:         loc,
	}

	for _, r := range strings.Split(env("VBC_SOURCE_ROOTS", "/sources"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			c.SourceRoots = append(c.SourceRoots, filepath.Clean(r))
		}
	}

	// Several keys may be listed at once so a key can be rotated without
	// restarting with a single value.
	for _, k := range strings.Split(os.Getenv("VBC_API_KEY"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			c.APIKeys = append(c.APIKeys, k)
		}
	}
	if !c.UIEnabled && len(c.APIKeys) == 0 {
		return nil, errors.New("VBC_UI_ENABLED=false требует непустого VBC_API_KEY, иначе сервис будет недоступен")
	}
	for _, k := range c.APIKeys {
		if len(k) < 16 {
			slog.Warn("ключ API короче 16 символов, используйте длинный случайный ключ", "length", len(k))
		}
	}

	if c.MaxConcurrent < 1 {
		c.MaxConcurrent = 1
	}
	if c.DefaultRetention < 0 {
		c.DefaultRetention = 0
	}
	if c.GraceMinutes < 0 {
		c.GraceMinutes = 0
	}

	if c.AdminPassword == "" {
		c.AdminPassword = "changeme"
		slog.Warn("VBC_ADMIN_PASSWORD не задан, используется пароль по умолчанию; задайте его в docker-compose")
	}

	for _, dir := range []string{c.DataDir, c.BackupDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("создание каталога %s: %w", dir, err)
		}
	}

	if len(c.Secret) == 0 {
		secret, err := loadOrCreateSecret(c.DataDir)
		if err != nil {
			return nil, err
		}
		c.Secret = secret
	}

	return c, nil
}

// loadOrCreateSecret keeps the cookie-signing key stable across restarts.
func loadOrCreateSecret(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, "secret.key")
	if b, err := os.ReadFile(path); err == nil {
		if decoded, derr := hex.DecodeString(strings.TrimSpace(string(b))); derr == nil && len(decoded) >= 16 {
			return decoded, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("генерация секрета: %w", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(buf)), 0o600); err != nil {
		return nil, fmt.Errorf("запись %s: %w", path, err)
	}
	return buf, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		slog.Warn("некорректное значение переменной окружения, использую значение по умолчанию", "key", key, "value", v, "default", def)
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		slog.Warn("некорректное булево значение, использую значение по умолчанию", "key", key, "value", v, "default", def)
		return def
	}
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		slog.Warn("некорректная длительность, использую значение по умолчанию", "key", key, "value", v, "default", def.String())
	}
	return def
}
