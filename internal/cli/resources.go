package cli

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

// Resource defaults follow sbx: a sandbox that is not told otherwise gets the machine, not a
// token slice of it. An agent build is the workload, and a two-vCPU default made every
// sandbox feel like a slow machine for no safety benefit — the VM boundary does not depend
// on how much of the host is behind it.

// autoMemoryCapMiB bounds the automatic memory size. Half of a large host is more than any
// agent needs, and memory handed to a VM is memory the host cannot use.
const autoMemoryCapMiB = 32 * 1024

// fallbackMemoryMiB is used when the host's memory cannot be read. It is deliberately
// modest: guessing high on an unknown machine is how you get a VM that will not boot.
const fallbackMemoryMiB = 2048

// autoCPUs is the vCPU count for -cpus 0.
func autoCPUs() int { return runtime.NumCPU() }

// autoMemoryMiB is half the host's memory, capped, for an unset -memory.
func autoMemoryMiB() int {
	total, ok := hostMemoryMiB()
	if !ok {
		return fallbackMemoryMiB
	}
	half := total / 2
	if half > autoMemoryCapMiB {
		return autoMemoryCapMiB
	}
	if half < fallbackMemoryMiB {
		// On a small host, half of it may be less than a usable guest. Ask for the
		// fallback and let the hypervisor refuse if the host really cannot spare it.
		return fallbackMemoryMiB
	}
	return half
}

// parseMemory turns a -memory argument into MiB.
//
// The units are binary and the suffix is what carries them: 8g is 8 GiB, 1024m is 1 GiB. A
// bare number is bytes, as it is for docker's --memory, which makes `-memory 2048` a value
// too small to boot rather than a silently different 2 GiB — so it is rejected with the
// spelling the user meant.
func parseMemory(text string) (int, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, fmt.Errorf("memory size is empty")
	}

	digits := trimmed
	multiplier := int64(1)
	unit := strings.ToLower(strings.TrimLeft(trimmed, "0123456789"))
	if unit != "" {
		digits = trimmed[:len(trimmed)-len(unit)]
		switch unit {
		case "b":
			multiplier = 1
		case "k", "kb", "kib":
			multiplier = 1 << 10
		case "m", "mb", "mib":
			multiplier = 1 << 20
		case "g", "gb", "gib":
			multiplier = 1 << 30
		case "t", "tb", "tib":
			multiplier = 1 << 40
		default:
			return 0, fmt.Errorf("memory size %q has an unknown unit %q; use k, m, g or t (binary)", text, unit)
		}
	}

	value, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("memory size %q is not a positive number with an optional k/m/g/t suffix", text)
	}

	bytes := value * multiplier
	mib := int(bytes / (1 << 20))
	if mib < 64 {
		return 0, fmt.Errorf("memory size %q is %d MiB, too small for a guest; "+
			"sizes without a suffix are bytes, so you may have meant %sm", text, mib, digits)
	}
	return mib, nil
}
