package sandbox

import "github.com/containerd/containerd/v2/pkg/cio"

// fifoDirOpts has nothing to choose on Windows, where cio carries stdio over named pipes
// rather than FIFOs in a directory. See fifodir_unix.go.
func fifoDirOpts() ([]cio.Opt, error) { return nil, nil }
