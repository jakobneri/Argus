package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// maxLogLines bounds the in-memory log ring so a busy stream cannot grow the
// view without limit.
const maxLogLines = 2000

// LogSource selects which backends a log stream draws from. The values mirror
// the proto enum so the daemon can map them directly.
type LogSource int

const (
	// LogSourceAll streams from every backend.
	LogSourceAll LogSource = iota
	// LogSourceSystemd streams only from the systemd journal.
	LogSourceSystemd
	// LogSourceDocker streams only from Docker container logs.
	LogSourceDocker
)

func (s LogSource) String() string {
	switch s {
	case LogSourceSystemd:
		return "systemd"
	case LogSourceDocker:
		return "docker"
	default:
		return "all"
	}
}

// LogEntry is the rendering view of one unified log line.
type LogEntry struct {
	Timestamp time.Time
	Source    LogSource
	Origin    string
	Level     string
	Message   string
}

// LogRequest parameterises a log stream started from the TUI.
type LogRequest struct {
	Source LogSource
	Filter string
	Since  string
}

// LogStream starts streaming entries to emit until ctx is cancelled. The
// daemon injects an implementation that drives the logs manager in-process;
// the TUI stays free of domain logic.
type LogStream func(ctx context.Context, req LogRequest, emit func(LogEntry))

type (
	logEntryMsg struct {
		entry LogEntry
		gen   int
	}
	logClosedMsg struct{ gen int }
)

// logView is the scrolling, filterable Logs tab. It owns one in-flight stream;
// changing the filter or source cancels it and starts a fresh one, tagged with
// a generation so late messages from the old stream are ignored.
type logView struct {
	stream LogStream
	styles dashboardStyles

	vp        viewport.Model
	input     textinput.Model
	filtering bool
	ready     bool

	entries []LogEntry
	source  LogSource
	filter  string

	ch      chan LogEntry
	cancel  context.CancelFunc
	gen     int
	started bool

	width int
}

func newLogView(stream LogStream, styles dashboardStyles) *logView {
	ti := textinput.New()
	ti.Prompt = "filter (unit / container): "
	ti.CharLimit = 256
	return &logView{stream: stream, styles: styles, input: ti}
}

// start launches the stream the first time the Logs tab is shown.
func (lv *logView) start() tea.Cmd {
	if lv.started {
		return nil
	}
	lv.started = true
	return lv.restart()
}

// restart cancels any running stream and begins a new one for the current
// source and filter, clearing the view.
func (lv *logView) restart() tea.Cmd {
	lv.stop()
	lv.entries = nil
	lv.refreshContent()

	ctx, cancel := context.WithCancel(context.Background())
	lv.cancel = cancel
	lv.gen++
	gen := lv.gen
	ch := make(chan LogEntry, 256)
	lv.ch = ch

	req := LogRequest{Source: lv.source, Filter: lv.filter}
	stream := lv.stream
	go func() {
		stream(ctx, req, func(e LogEntry) {
			select {
			case ch <- e:
			case <-ctx.Done():
			}
		})
		close(ch)
	}()
	return waitForLog(ch, gen)
}

// stop cancels the running stream, if any. The goroutine drains and closes its
// channel once ctx is observed.
func (lv *logView) stop() {
	if lv.cancel != nil {
		lv.cancel()
		lv.cancel = nil
	}
}

func waitForLog(ch chan LogEntry, gen int) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return logClosedMsg{gen: gen}
		}
		return logEntryMsg{entry: e, gen: gen}
	}
}

func (lv *logView) setSize(width, height int) {
	lv.width = width
	h := max(height, 1)
	if !lv.ready {
		lv.vp = viewport.New(width, h)
		lv.ready = true
	} else {
		lv.vp.Width = width
		lv.vp.Height = h
	}
	lv.input.Width = max(width-30, 10)
	lv.refreshContent()
}

func (lv *logView) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case logEntryMsg:
		if msg.gen != lv.gen {
			return nil // stale entry from a superseded stream
		}
		lv.append(msg.entry)
		return waitForLog(lv.ch, lv.gen)
	case logClosedMsg:
		return nil
	case tea.KeyMsg:
		return lv.handleKey(msg)
	}
	var cmd tea.Cmd
	lv.vp, cmd = lv.vp.Update(msg)
	return cmd
}

func (lv *logView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if lv.filtering {
		switch msg.String() {
		case "enter":
			lv.filter = strings.TrimSpace(lv.input.Value())
			lv.filtering = false
			lv.input.Blur()
			return lv.restart()
		case "esc":
			lv.filtering = false
			lv.input.Blur()
			return nil
		default:
			var cmd tea.Cmd
			lv.input, cmd = lv.input.Update(msg)
			return cmd
		}
	}

	switch msg.String() {
	case "/":
		lv.filtering = true
		lv.input.SetValue(lv.filter)
		lv.input.Focus()
		return textinput.Blink
	case "1":
		return lv.setSource(LogSourceAll)
	case "2":
		return lv.setSource(LogSourceSystemd)
	case "3":
		return lv.setSource(LogSourceDocker)
	}

	var cmd tea.Cmd
	lv.vp, cmd = lv.vp.Update(msg)
	return cmd
}

func (lv *logView) setSource(s LogSource) tea.Cmd {
	if lv.source == s {
		return nil
	}
	lv.source = s
	return lv.restart()
}

func (lv *logView) append(e LogEntry) {
	lv.entries = append(lv.entries, e)
	if len(lv.entries) > maxLogLines {
		lv.entries = lv.entries[len(lv.entries)-maxLogLines:]
	}
	lv.refreshContent()
	lv.vp.GotoBottom()
}

func (lv *logView) refreshContent() {
	if !lv.ready {
		return
	}
	var b strings.Builder
	for i, e := range lv.entries {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(lv.renderEntry(e))
	}
	lv.vp.SetContent(b.String())
}

// plainPrefixWidth is the printable width of the per-line prefix (time, source
// tag, origin, level) used to budget the remaining space for the message.
const plainPrefixWidth = 8 + 1 + 1 + 1 + 18 + 1 + 7 + 1

func (lv *logView) renderEntry(e LogEntry) string {
	ts := "--:--:--"
	if !e.Timestamp.IsZero() {
		ts = e.Timestamp.Format("15:04:05")
	}
	origin := fmt.Sprintf("%-18s", truncate(e.Origin, 18))
	level := fmt.Sprintf("%-7s", truncate(e.Level, 7))

	prefix := lv.styles.hint.Render(ts) + " " +
		lv.styles.logSource.Render(sourceTag(e.Source)) + " " +
		lv.styles.hint.Render(origin) + " " +
		lv.levelStyle(e.Level).Render(level) + " "

	msg := e.Message
	if avail := lv.width - plainPrefixWidth; avail > 0 {
		msg = truncate(msg, avail)
	}
	return prefix + msg
}

func (lv *logView) levelStyle(level string) lipgloss.Style {
	switch level {
	case "emerg", "alert", "crit", "err", "stderr":
		return lv.styles.bad
	case "warning", "notice":
		return lv.styles.warn
	default:
		return lv.styles.hint
	}
}

func (lv *logView) view() string {
	if !lv.ready {
		return "loading…"
	}
	if lv.filtering {
		return lipgloss.JoinVertical(lipgloss.Left, lv.vp.View(), lv.input.View())
	}
	filter := lv.filter
	if filter == "" {
		filter = "(none)"
	}
	bar := lv.styles.statusBar.Render(fmt.Sprintf(
		"source: %s · filter: %s · / filter · 1 all · 2 systemd · 3 docker · ↑/↓ scroll",
		lv.source, filter,
	))
	return lipgloss.JoinVertical(lipgloss.Left, lv.vp.View(), bar)
}

func sourceTag(s LogSource) string {
	switch s {
	case LogSourceSystemd:
		return "S"
	case LogSourceDocker:
		return "D"
	default:
		return "?"
	}
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
