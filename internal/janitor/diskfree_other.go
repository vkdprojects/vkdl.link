//go:build !(linux || darwin || freebsd)

package janitor

// DiskFree não é suportado nesta plataforma: devolve nil (sem verificação de disco).
func DiskFree(path string) func() (int64, error) { return nil }
