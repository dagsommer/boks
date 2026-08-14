package network

import "errors"

// unexercisedOnWindows is the text Unexercised returns there.
//
// It lives in a file with no build constraint for the same reason windowsRootBundle does in
// internal/enforce: a claim this load-bearing should be readable by a test on the machine the
// tests actually run on. The Windows build is compiled in CI and executed nowhere, so a
// sentence that only exists behind `//go:build windows` is a sentence nothing checks.
func unexercisedOnWindows() error {
	return errors.New("no Ethernet frame has ever been observed crossing libkrun's virtio-net " +
		"device on Windows: krun.dll links with `--features blk,net` and exports " +
		"krun_add_net_unixstream, and a container has run in a microVM there through containerd " +
		"and the nerdbox shim, but the frame path itself is unexercised. If nothing connects to " +
		"the link socket the guest is not on this stack at all — it is on libkrun's TSI, where " +
		"the guest's 127.0.0.1 is the host's and no policy is enforced (see docs/windows.md)")
}
