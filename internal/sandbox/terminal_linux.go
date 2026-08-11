package sandbox

import "golang.org/x/sys/unix"

// The ioctl numbers for reading and writing terminal settings differ between Linux and the
// BSDs, which is the only part of raw-mode handling that is not portable.
const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)
