//go:build linux || darwin || freebsd

package janitor

import "syscall"

// DiskFree devolve uma função que mede o espaço livre (para usuários comuns) em path.
func DiskFree(path string) func() (int64, error) {
	return func() (int64, error) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(path, &st); err != nil {
			return 0, err
		}
		return int64(st.Bavail) * int64(st.Bsize), nil
	}
}
