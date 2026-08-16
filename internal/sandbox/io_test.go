package sandbox

import (
	"bytes"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/pkg/cio"
)

// TestIOCreatorCreatesEveryStreamItAnnounces is the check that would have caught the failure
// the first Homebrew install on macOS hit: `boks run .` in a terminal died with
//
//	containerd-shim: opening file ".../boks-exec-<id>-stderr" failed: no such file or directory
//
// The shim is handed three paths in ExecProcessRequest and opens the non-empty ones. Every
// path Boks puts in that request therefore has to exist by the time the request is sent —
// this asserts exactly that, against the real cio machinery and the real filesystem, rather
// than against Boks's intent.
//
// The terminal case is the one that broke. cio.NewFIFOSetInDir fills in all three paths
// unconditionally on unix, and cio's copyIO then declines to create the stderr FIFO when
// Terminal is set, so a caller that passes a stderr writer alongside a pty announces a file
// that was never made.
func TestIOCreatorCreatesEveryStreamItAnnounces(t *testing.T) {
	if runtime.GOOS == "windows" {
		// cio addresses Windows streams as named pipes under \\.\pipe, not as files on
		// disk, so "does this path exist" is not the same question there. Windows is
		// safe by construction anyway: its NewFIFOSetInDir leaves Stderr empty whenever
		// Terminal is set, so it cannot announce an uncreated stream.
		t.Skip("cio uses named pipes on Windows, which are not filesystem paths")
	}

	for _, tty := range []bool{false, true} {
		name := "piped"
		if tty {
			name = "tty"
		}
		t.Run(name, func(t *testing.T) {
			// An explicit FIFO directory keeps the test out of the machine's
			// /run/containerd/fifo, which needs a running containerd's permissions.
			// It changes nothing about which streams get created.
			opts := append(ioOpts(tty, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}),
				cio.WithFIFODir(t.TempDir()))
			streams, err := cio.NewCreator(opts...)("boks-exec-test")
			if err != nil {
				t.Fatalf("creating io: %v", err)
			}
			t.Cleanup(func() {
				streams.Cancel()
				_ = streams.Close()
			})

			cfg := streams.Config()
			for _, stream := range []struct{ name, path string }{
				{"stdin", cfg.Stdin},
				{"stdout", cfg.Stdout},
				{"stderr", cfg.Stderr},
			} {
				if stream.path == "" {
					continue
				}
				if _, err := os.Stat(stream.path); err != nil {
					t.Errorf("%s is announced to the shim as %q but does not exist: %v\n"+
						"The shim opens every non-empty path it is given, so this is "+
						"the \"containerd-shim: opening file ... no such file or "+
						"directory\" failure.", stream.name, stream.path, err)
				}
			}
		})
	}
}

// TestTTYIOLeavesStderrUnnamed states the rule directly, on every platform: a process with a
// pseudo-terminal has one stream, so Boks must not hand cio a stderr writer for it. Anything
// the guest writes to stderr comes back over the console, on stdout.
func TestTTYIOLeavesStderrUnnamed(t *testing.T) {
	var streams cio.Streams
	for _, opt := range ioOpts(true, strings.NewReader(""), io.Discard, io.Discard) {
		opt(&streams)
	}
	if !streams.Terminal {
		t.Error("a tty process should be created with cio.WithTerminal")
	}
	if streams.Stderr != nil {
		t.Error("a tty process must have no stderr stream: cio only blanks the stderr " +
			"path it sends to the shim when the writer is nil, and it never creates " +
			"the stderr FIFO for a terminal")
	}
	if streams.Stdout == nil {
		t.Error("a tty process still needs stdout: the console arrives on it")
	}
}

// TestPipedIOKeepsStderr guards the other half — that dropping stderr is confined to the
// terminal case, so a piped run keeps the separate stream it exists to provide.
func TestPipedIOKeepsStderr(t *testing.T) {
	var streams cio.Streams
	for _, opt := range ioOpts(false, strings.NewReader(""), io.Discard, io.Discard) {
		opt(&streams)
	}
	if streams.Terminal {
		t.Error("a piped process must not be created with cio.WithTerminal")
	}
	if streams.Stderr == nil {
		t.Error("a piped process must keep its own stderr stream")
	}
}
