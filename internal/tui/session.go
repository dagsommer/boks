package tui

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// sessionCmd runs an interactive command with the terminal handed to it. When the command
// fails straight away it holds the screen until Enter, because the dashboard redraws over
// whatever the command printed — and why a sandbox would not start is exactly what someone
// needs to read. A session that ran for a while and ended on a non-zero status (a shell
// whose last command failed) just returns: that status is the user's, not news.
type sessionCmd struct {
	cmd    *exec.Cmd
	what   string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// quickFailure is how soon a failed session counts as one that never got going.
const quickFailure = 5 * time.Second

func (s *sessionCmd) SetStdin(r io.Reader)  { s.stdin = r }
func (s *sessionCmd) SetStdout(w io.Writer) { s.stdout = w }
func (s *sessionCmd) SetStderr(w io.Writer) { s.stderr = w }

func (s *sessionCmd) Run() error {
	s.cmd.Stdin, s.cmd.Stdout, s.cmd.Stderr = s.stdin, s.stdout, s.stderr
	started := time.Now()
	err := s.cmd.Run()
	if err != nil && time.Since(started) < quickFailure && s.stdin != nil && s.stderr != nil {
		fmt.Fprintf(s.stderr, "\n%s: %v\nPress Enter to return to the dashboard.", s.what, err)
		_, _ = bufio.NewReader(s.stdin).ReadString('\n')
	}
	return err
}

// session hands the terminal to cmd and returns to the dashboard when it exits.
func session(cmd *exec.Cmd, what string) tea.Cmd {
	return tea.Exec(&sessionCmd{cmd: cmd, what: what}, func(err error) tea.Msg {
		return execDoneMsg{what: what, err: err}
	})
}
