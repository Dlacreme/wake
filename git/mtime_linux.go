//go:build linux

package git

import "syscall"

func mtime(path string) float64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0
	}
	return float64(st.Mtim.Sec) + float64(st.Mtim.Nsec)*1e-9
}
