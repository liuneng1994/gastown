//go:build windows

package mail

func mailTestProcessAlive(pid int) bool {
	return false
}
