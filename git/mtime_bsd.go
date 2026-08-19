//go:build darwin || freebsd || netbsd || openbsd

package git

import "syscall"

func mtime(path string) float64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0
	}
	return float64(st.Mtimespec.Sec) + float64(st.Mtimespec.Nsec)*1e-9
}
