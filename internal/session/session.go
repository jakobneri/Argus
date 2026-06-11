// Package session runs ephemeral, headless Bubble Tea sessions inside the
// daemon. M1 scope: exactly one ephemeral session per Attach stream — no
// detach/reattach, no persistence, no panes.
package session

import (
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"
)

// Session drives one headless Bubble Tea program. Input bytes are fed to the
// program as if typed on a terminal; rendered frames are written to output.
//
// Assumption (M1): headless operation uses tea.WithInput/tea.WithOutput on
// stream endpoints. Because the output is a stream and not a TTY, SIGWINCH
// autodetection cannot work; resizes are injected via Resize as
// tea.WindowSizeMsg.
type Session struct {
	prog    *tea.Program
	inWrite *io.PipeWriter
}

// New creates a session around model, rendering to output.
func New(model tea.Model, output io.Writer) *Session {
	inRead, inWrite := io.Pipe()
	prog := tea.NewProgram(
		model,
		tea.WithInput(inRead),
		tea.WithOutput(output),
		tea.WithAltScreen(),
	)
	return &Session{prog: prog, inWrite: inWrite}
}

// Run blocks until the program exits (e.g. the user pressed "q") or is
// killed via Close.
func (s *Session) Run() error {
	if _, err := s.prog.Run(); err != nil {
		return fmt.Errorf("run tui program: %w", err)
	}
	return nil
}

// Input feeds raw key bytes into the program.
func (s *Session) Input(b []byte) error {
	if _, err := s.inWrite.Write(b); err != nil {
		return fmt.Errorf("write input to tui program: %w", err)
	}
	return nil
}

// Resize injects a new terminal size into the program.
func (s *Session) Resize(cols, rows int) {
	s.prog.Send(tea.WindowSizeMsg{Width: cols, Height: rows})
}

// Close tears the session down: the program is killed and the input pipe is
// closed. Safe to call after the program has already exited.
func (s *Session) Close() {
	s.prog.Kill()
	_ = s.inWrite.Close()
}
