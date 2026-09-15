//go:build windows

package beads

func beadsTestProcessAlive(pid int) bool {
	return false
}
