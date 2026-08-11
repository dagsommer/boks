package cli

import (
	"os"
	"strconv"
	"strings"
)

// hostMemoryMiB reads total physical memory from /proc/meminfo.
//
// MemTotal is what the kernel itself reports as installed, minus what the firmware keeps,
// which is the number a user comparing against `free` will see.
func hostMemoryMiB() (int, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		// MemTotal is in kibibytes.
		kib, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kib <= 0 {
			return 0, false
		}
		return int(kib / 1024), true
	}
	return 0, false
}
