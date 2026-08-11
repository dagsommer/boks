package cli

import "golang.org/x/sys/unix"

// hostMemoryMiB reads total physical memory from the kernel.
func hostMemoryMiB() (int, bool) {
	bytes, err := unix.SysctlUint64("hw.memsize")
	if err != nil || bytes == 0 {
		return 0, false
	}
	return int(bytes / (1 << 20)), true
}
