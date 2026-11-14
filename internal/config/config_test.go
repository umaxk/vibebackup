package config

import (
	"path/filepath"
	"testing"
)

func baseEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("VBC_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("VBC_BACKUP_DIR", filepath.Join(dir, "backups"))
	t.Setenv("VBC_SOURCE_ROOTS", filepath.Join(dir, "sources"))
	t.Setenv("VBC_ADMIN_PASSWORD", "test-password")
	t.Setenv("VBC_API_KEY", "")
	t.Setenv("VBC_UI_ENABLED", "")
}

func TestLoadDefaults(t *testing.T) {
	baseEnv(t)
	cfg, err := Load("test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.UIEnabled {
		t.Fatal("UIEnabled = false, want true by default")
	}
	if len(cfg.APIKeys) != 0 {
		t.Fatalf("APIKeys = %v, want none", cfg.APIKeys)
	}
}

func TestLoadRequiresAPIKeyInAPIOnlyMode(t *testing.T) {
	baseEnv(t)
	t.Setenv("VBC_UI_ENABLED", "false")
	if _, err := Load("test"); err == nil {
		t.Fatal("expected an error when the UI is disabled without an API key")
	}
}

func TestLoadParsesAPIKeys(t *testing.T) {
	baseEnv(t)
	t.Setenv("VBC_UI_ENABLED", "false")
	t.Setenv("VBC_API_KEY", " first-key-0123456789 , second-key-0123456789 ,")
	cfg, err := Load("test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UIEnabled {
		t.Fatal("UIEnabled = true, want false")
	}
	want := []string{"first-key-0123456789", "second-key-0123456789"}
	if len(cfg.APIKeys) != len(want) {
		t.Fatalf("APIKeys = %v, want %v", cfg.APIKeys, want)
	}
	for i := range want {
		if cfg.APIKeys[i] != want[i] {
			t.Fatalf("APIKeys = %v, want %v", cfg.APIKeys, want)
		}
	}
}

func TestLoadInvalidBoolFallsBackToDefault(t *testing.T) {
	baseEnv(t)
	t.Setenv("VBC_UI_ENABLED", "maybe")
	cfg, err := Load("test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.UIEnabled {
		t.Fatal("UIEnabled = false, want the default true")
	}
}
