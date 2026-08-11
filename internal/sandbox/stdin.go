package sandbox

import (
	"context"
	"io"
	"sync"

	"github.com/containerd/containerd/v2/client"
)

// stdinWatcher reports when the host's input stream has run out.
//
// Closing the host end of the stdin FIFO is not enough: the shim keeps the guest process's
// stdin open until it is explicitly told to close it, so a guest reading until EOF — `tar
// -xf -`, `cat`, anything fed from a pipe — waits forever after the last byte arrives. The
// reader wrapper notices the end of the input and lets the caller send that close.
type stdinWatcher struct {
	reader io.Reader
	done   chan struct{}
	once   sync.Once
}

func watchStdin(r io.Reader) *stdinWatcher {
	if r == nil {
		return nil
	}
	return &stdinWatcher{reader: r, done: make(chan struct{})}
}

func (w *stdinWatcher) Read(p []byte) (int, error) {
	n, err := w.reader.Read(p)
	if err != nil {
		w.once.Do(func() { close(w.done) })
	}
	return n, err
}

// input returns what to hand to the IO creator, nil-safe so callers need no branch.
func (w *stdinWatcher) input() io.Reader {
	if w == nil {
		return nil
	}
	return w
}

// closeGuestStdin closes the guest's stdin once the host input is exhausted, and stops
// watching when the process exits. The returned function must be called to release the
// goroutine.
func (w *stdinWatcher) closeGuestStdin(ctx context.Context, p client.Process) func() {
	if w == nil {
		return func() {}
	}
	stop := make(chan struct{})
	go func() {
		select {
		case <-w.done:
			_ = p.CloseIO(ctx, client.WithStdinCloser)
		case <-stop:
		case <-ctx.Done():
		}
	}()
	return func() { close(stop) }
}
