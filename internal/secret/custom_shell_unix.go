//go:build !windows

package secret

func shell() string     { return "sh" }
func shellFlag() string { return "-c" }
