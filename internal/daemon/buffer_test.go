package daemon

import (
	"bytes"
	"sync"
)

// syncBuffer is a bytes.Buffer safe to read while Serve's goroutines write to it. The plain
// one is not, and the race detector is right to say so: Serve hands the same writer to
// containerd's output pump and writes its own lines to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
