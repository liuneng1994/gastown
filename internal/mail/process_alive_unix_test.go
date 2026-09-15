//go:build !windows

package mail

import "syscall"

func mailTestProcessAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
