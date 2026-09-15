//go:build !windows

package beads

import "syscall"

func beadsTestProcessAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
