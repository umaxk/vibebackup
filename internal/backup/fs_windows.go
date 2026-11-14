//go:build windows

package backup

import (
	"math"
	"os"
)

// FreeBytes is a no-op stub on Windows (the service runs in Linux containers).
func FreeBytes(path string) (int64, error) { return math.MaxInt64, nil }

func ownerOf(fi os.FileInfo) (int, int) { return 0, 0 }
