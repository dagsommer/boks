//go:build windows

package secret

func shell() string     { return "cmd" }
func shellFlag() string { return "/C" }
