//go:build !darwin && !freebsd && !netbsd && !openbsd && !linux

package git

import "os"

func mtime(path string) float64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return float64(fi.ModTime().UnixNano()) * 1e-9
}
