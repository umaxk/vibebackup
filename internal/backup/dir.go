package backup

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"

	"vbackuper/internal/store"
)

func runDir(ctx context.Context, t *store.Target, destFile string, logs *LogBuffer) (Result, error) {
	cfg, err := t.DirConfig()
	if err != nil {
		return Result{}, err
	}
	base := filepath.Clean(cfg.Path)
	info, err := os.Stat(base)
	if err != nil {
		return Result{}, fmt.Errorf("каталог %s: %w", base, err)
	}
	if !info.IsDir() {
		return Result{}, fmt.Errorf("%s не является каталогом", base)
	}
	if strings.HasPrefix(destFile, base+string(os.PathSeparator)) {
		return Result{}, fmt.Errorf("каталог бэкапов не должен находиться внутри источника %s", base)
	}

	top := filepath.Base(base)
	if top == "." || top == string(os.PathSeparator) || top == "" {
		top = "root"
	}

	f, err := os.Create(destFile)
	if err != nil {
		return Result{}, fmt.Errorf("создание файла: %w", err)
	}
	h := newHasher()
	zw, err := zstd.NewWriter(io.MultiWriter(f, h), zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		f.Close()
		os.Remove(destFile)
		return Result{}, err
	}
	tw := tar.NewWriter(zw)

	logs.Printf("архивирую каталог %s", base)

	var (
		files int
		bytes int64
	)
	walkErr := filepath.WalkDir(base, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			logs.Printf("предупреждение: %v", walkErr)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		rel, err := filepath.Rel(base, p)
		if err != nil {
			return nil
		}
		if rel != "." && excluded(rel, cfg.Exclude) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			logs.Printf("пропуск %s: %v", rel, err)
			return nil
		}

		name := top
		if rel != "." {
			name = path.Join(top, filepath.ToSlash(rel))
		}

		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				logs.Printf("пропуск ссылки %s: %v", rel, err)
				return nil
			}
		}

		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			logs.Printf("пропуск %s: %v", rel, err)
			return nil
		}
		hdr.Name = name
		if fi.IsDir() {
			hdr.Name = name + "/"
		}
		hdr.Uid, hdr.Gid = ownerOf(fi)
		if !fi.Mode().IsRegular() {
			hdr.Size = 0
		}

		if fi.Mode().IsRegular() {
			// Open before writing the header so a permission error cannot
			// leave a dangling header in the archive.
			src, err := os.Open(p)
			if err != nil {
				logs.Printf("пропуск %s: %v", name, err)
				return nil
			}
			if err := tw.WriteHeader(hdr); err != nil {
				src.Close()
				return fmt.Errorf("запись заголовка %s: %w", name, err)
			}
			n, copyErr := io.Copy(tw, src)
			closeErr := src.Close()
			if copyErr != nil {
				return fmt.Errorf("чтение %s: %w", name, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("чтение %s: %w", name, closeErr)
			}
			if n != fi.Size() {
				return fmt.Errorf("%s изменился во время бэкапа (%d из %d байт)", name, n, fi.Size())
			}
			files++
			bytes += n
			if files%5000 == 0 {
				logs.Printf("обработано файлов: %d, %s", files, humanBytes(bytes))
			}
			return nil
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("запись заголовка %s: %w", name, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			logs.Printf("пропуск специального файла %s", name)
		}
		return nil
	})

	if walkErr != nil {
		tw.Close()
		zw.Close()
		f.Close()
		os.Remove(destFile)
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, walkErr
	}

	if err := tw.Close(); err != nil {
		zw.Close()
		f.Close()
		os.Remove(destFile)
		return Result{}, fmt.Errorf("завершение архива: %w", err)
	}
	if err := zw.Close(); err != nil {
		f.Close()
		os.Remove(destFile)
		return Result{}, fmt.Errorf("сжатие архива: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(destFile)
		return Result{}, fmt.Errorf("запись архива: %w", err)
	}

	res, err := resultFor(destFile, h)
	if err != nil {
		return Result{}, err
	}
	logs.Printf("готово: %d файлов, %s исходных данных, %s в архиве", files, humanBytes(bytes), humanBytes(res.SizeBytes))
	return res, nil
}

// excluded matches a slash-separated relative path against exclude patterns.
// Patterns are globs matched against the full relative path, against the base
// name and against every path segment, so "node_modules" and "*.log" both work
// at any depth.
func excluded(rel string, patterns []string) bool {
	rel = filepath.ToSlash(rel)
	base := path.Base(rel)
	segments := strings.Split(rel, "/")
	for _, raw := range patterns {
		p := strings.TrimSpace(filepath.ToSlash(raw))
		p = strings.TrimPrefix(p, "./")
		p = strings.TrimSuffix(p, "/")
		if p == "" {
			continue
		}
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
		if ok, _ := path.Match(p, base); ok {
			return true
		}
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
		for _, seg := range segments {
			if seg == p {
				return true
			}
			if ok, _ := path.Match(p, seg); ok {
				return true
			}
		}
	}
	return false
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Б", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"КБ", "МБ", "ГБ", "ТБ", "ПБ", "ЭБ"}
	if exp >= len(suffixes) {
		exp = len(suffixes) - 1
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), suffixes[exp])
}
