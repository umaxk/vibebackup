package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"vbackuper/internal/store"
)

func TestExcluded(t *testing.T) {
	patterns := []string{"node_modules", "*.log", "cache/", ".git"}
	cases := map[string]bool{
		"node_modules":              true,
		"sub/node_modules":          true,
		"sub/node_modules/pkg/a.js": true,
		"sub/app.log":               true,
		"app.log":                   true,
		"cache":                     true,
		"cache/data.bin":            true,
		"sub/cache/data.bin":        true,
		".git/config":               true,
		"sub/.git/config":           true,
		"keep.txt":                  false,
		"sub/keep.txt":              false,
		"logs.txt":                  false,
	}
	for rel, want := range cases {
		if got := excluded(rel, patterns); got != want {
			t.Errorf("excluded(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestRunDirArchive(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.txt"), "hello")
	mustWrite(t, filepath.Join(src, "sub", "keep.txt"), "keep")
	mustWrite(t, filepath.Join(src, "sub", "b.log"), "skip me")
	mustWrite(t, filepath.Join(src, "sub", "node_modules", "x.js"), "skip me too")
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	target := &store.Target{ID: 1, Type: store.TypeDir}
	if err := target.SetConfig(store.DirConfig{Path: src, Exclude: []string{"node_modules", "*.log"}}); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	logs := NewLogBuffer(4096)
	res, err := Run(context.Background(), target, dest, "sample-1", logs)
	if err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, logs.String())
	}
	if res.SizeBytes <= 0 {
		t.Fatalf("size = %d, want > 0", res.SizeBytes)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("archive not found: %v", err)
	}

	// The checksum must match the file on disk.
	f, err := os.Open(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != res.SHA256 {
		t.Fatalf("sha256 = %s, want %s", res.SHA256, got)
	}

	names := tarNames(t, res.Path)
	top := filepath.Base(filepath.Clean(src))
	want := []string{
		top + "/",
		top + "/a.txt",
		top + "/sub/",
		top + "/sub/keep.txt",
		top + "/empty/",
	}
	for _, w := range want {
		if !contains(names, w) {
			t.Errorf("archive is missing %q; entries: %v", w, names)
		}
	}
	for _, n := range names {
		if strings.Contains(n, "node_modules") || strings.HasSuffix(n, ".log") {
			t.Errorf("archive contains excluded entry %q", n)
		}
	}
}

func TestRunDirRejectsNestedDestination(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.txt"), "hello")
	target := &store.Target{ID: 1, Type: store.TypeDir}
	if err := target.SetConfig(store.DirConfig{Path: src}); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), target, filepath.Join(src, "backups"), "x-1", NewLogBuffer(1024))
	if err == nil {
		t.Fatal("expected an error when destination is inside the source")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:                      "0 Б",
		512:                    "512 Б",
		1024:                   "1.0 КБ",
		1536:                   "1.5 КБ",
		1024 * 1024:            "1.0 МБ",
		5 * 1024 * 1024 * 1024: "5.0 ГБ",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tarNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var out []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, hdr.Name)
	}
	return out
}

func contains(items []string, want string) bool {
	for _, it := range items {
		if it == want {
			return true
		}
	}
	return false
}
