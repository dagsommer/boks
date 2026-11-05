package tui

import (
	"context"
	"io"

	tea "github.com/charmbracelet/bubbletea"
)

// Run shows the dashboard on the terminal behind in and out until the user quits.
func Run(ctx context.Context, src Source, info Info, in io.Reader, out io.Writer, opts ...Option) error {
	opts = append([]Option{WithContext(ctx)}, opts...)
	m := New(src, info, opts...)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithInput(in), tea.WithOutput(out), tea.WithContext(ctx))
	_, err := p.Run()
	if err == tea.ErrProgramKilled && ctx.Err() != nil {
		return nil
	}
	return err
}
