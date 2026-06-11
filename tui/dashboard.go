package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// refreshInterval is how often the dashboard polls for a fresh inventory.
// Assumption (M1): snapshot polling via tea.Tick is sufficient; no server
// push / streaming inventory.
const refreshInterval = 3 * time.Second

// fetchTimeout bounds a single inventory fetch so a hung backend cannot
// stall the dashboard forever.
const fetchTimeout = 5 * time.Second

// ServiceRow is the rendering view of one systemd service unit.
type ServiceRow struct {
	Name        string
	Description string
	LoadState   string
	ActiveState string
	SubState    string
}

// ContainerRow is the rendering view of one Docker container.
type ContainerRow struct {
	Name   string
	Image  string
	State  string
	Status string
}

// Snapshot is one read-only inventory snapshot rendered by the dashboard.
type Snapshot struct {
	Services []ServiceRow
	// Containers is empty when ContainerHint is set (e.g. Docker daemon
	// unreachable); the hint is shown instead.
	Containers    []ContainerRow
	ContainerHint string
}

// Fetch produces a Snapshot. The dashboard stays free of domain logic: the
// daemon injects a fetcher that calls the module interfaces in-process
// (architecture decision: no self-gRPC for the in-daemon TUI).
type Fetch func(ctx context.Context) (Snapshot, error)

type (
	snapshotMsg     Snapshot
	fetchErrMsg     struct{ err error }
	refreshTickMsg  time.Time
	dashboardStyles struct {
		title     lipgloss.Style
		hint      lipgloss.Style
		errText   lipgloss.Style
		good      lipgloss.Style
		bad       lipgloss.Style
		warn      lipgloss.Style
		statusBar lipgloss.Style
	}
)

func newDashboardStyles() dashboardStyles {
	return dashboardStyles{
		title:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("44")),
		hint:      lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		errText:   lipgloss.NewStyle().Foreground(lipgloss.Color("203")),
		good:      lipgloss.NewStyle().Foreground(lipgloss.Color("42")),
		bad:       lipgloss.NewStyle().Foreground(lipgloss.Color("203")),
		warn:      lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		statusBar: lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
	}
}

// Dashboard is the read-only M1 dashboard: a services table and a containers
// table, refreshed periodically. "q" (or ctrl+c, esc) quits.
type Dashboard struct {
	fetch  Fetch
	styles dashboardStyles

	services   table.Model
	containers table.Model
	focused    int // 0 = services, 1 = containers

	snapshot  Snapshot
	fetchErr  error
	updatedAt time.Time

	width  int
	height int
}

// NewDashboard returns a Dashboard that refreshes itself via fetch.
func NewDashboard(fetch Fetch) *Dashboard {
	styles := table.DefaultStyles()
	styles.Selected = styles.Selected.
		Foreground(lipgloss.Color("229")).
		Background(lipgloss.Color("57"))

	services := table.New(table.WithColumns(serviceColumns(80)), table.WithFocused(true))
	services.SetStyles(styles)
	containers := table.New(table.WithColumns(containerColumns(80)))
	containers.SetStyles(styles)

	return &Dashboard{
		fetch:      fetch,
		styles:     newDashboardStyles(),
		services:   services,
		containers: containers,
	}
}

// Init starts the first fetch and the refresh ticker.
func (d *Dashboard) Init() tea.Cmd {
	return tea.Batch(d.fetchCmd(), refreshTick())
}

// Update handles input, resizes, ticks and fetch results.
func (d *Dashboard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return d, tea.Quit
		case "tab":
			d.toggleFocus()
			return d, nil
		}
	case tea.WindowSizeMsg:
		d.width, d.height = msg.Width, msg.Height
		d.layout()
		return d, nil
	case refreshTickMsg:
		return d, tea.Batch(d.fetchCmd(), refreshTick())
	case snapshotMsg:
		d.snapshot = Snapshot(msg)
		d.fetchErr = nil
		d.updatedAt = time.Now()
		d.fillTables()
		return d, nil
	case fetchErrMsg:
		d.fetchErr = msg.err
		return d, nil
	}

	var cmd tea.Cmd
	if d.focused == 0 {
		d.services, cmd = d.services.Update(msg)
	} else {
		d.containers, cmd = d.containers.Update(msg)
	}
	return d, cmd
}

// View renders the full dashboard frame.
func (d *Dashboard) View() string {
	if d.width == 0 {
		return "loading…"
	}

	header := d.styles.title.Render("argus") + d.styles.hint.Render("  ·  "+Tagline)

	servicesTitle := d.sectionTitle("Services (systemd)", d.focused == 0)
	containersTitle := d.sectionTitle("Containers (docker)", d.focused == 1)

	containersBody := d.containers.View()
	if d.snapshot.ContainerHint != "" {
		containersBody = d.styles.warn.Render("⚠ " + d.snapshot.ContainerHint)
	}

	status := fmt.Sprintf("q quit · tab switch · refresh %s", refreshInterval)
	if !d.updatedAt.IsZero() {
		status += " · updated " + d.updatedAt.Format("15:04:05")
	}
	bar := d.styles.statusBar.Render(status)
	if d.fetchErr != nil {
		bar = d.styles.errText.Render("fetch error: " + d.fetchErr.Error())
	}

	return lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		"",
		servicesTitle,
		d.services.View(),
		"",
		containersTitle,
		containersBody,
		"",
		bar,
	)
}

func (d *Dashboard) sectionTitle(s string, focused bool) string {
	if focused {
		return d.styles.title.Render("▸ " + s)
	}
	return d.styles.hint.Render("  " + s)
}

func (d *Dashboard) toggleFocus() {
	d.focused = 1 - d.focused
	if d.focused == 0 {
		d.services.Focus()
		d.containers.Blur()
	} else {
		d.services.Blur()
		d.containers.Focus()
	}
}

// layout distributes the available terminal space between the two tables.
func (d *Dashboard) layout() {
	// Fixed lines: header(1) + blanks(3) + section titles(2) + status bar(1)
	// + table headers/borders consume rows inside the table models.
	avail := d.height - 7
	if avail < 6 {
		avail = 6
	}
	svcRows := avail * 2 / 3
	ctrRows := avail - svcRows
	if svcRows < 3 {
		svcRows = 3
	}
	if ctrRows < 3 {
		ctrRows = 3
	}

	d.services.SetColumns(serviceColumns(d.width))
	d.services.SetWidth(d.width)
	d.services.SetHeight(svcRows)

	d.containers.SetColumns(containerColumns(d.width))
	d.containers.SetWidth(d.width)
	d.containers.SetHeight(ctrRows)
}

func serviceColumns(width int) []table.Column {
	name := width * 30 / 100
	desc := width - name - 3*10 - 6
	if desc < 10 {
		desc = 10
	}
	return []table.Column{
		{Title: "Unit", Width: name},
		{Title: "Load", Width: 10},
		{Title: "Active", Width: 10},
		{Title: "Sub", Width: 10},
		{Title: "Description", Width: desc},
	}
}

func containerColumns(width int) []table.Column {
	name := width * 25 / 100
	image := width * 30 / 100
	status := width - name - image - 10 - 8
	if status < 10 {
		status = 10
	}
	return []table.Column{
		{Title: "Name", Width: name},
		{Title: "Image", Width: image},
		{Title: "State", Width: 10},
		{Title: "Status", Width: status},
	}
}

func (d *Dashboard) fillTables() {
	svcRows := make([]table.Row, 0, len(d.snapshot.Services))
	for _, s := range d.snapshot.Services {
		svcRows = append(svcRows, table.Row{
			s.Name, s.LoadState, d.colorActive(s.ActiveState), s.SubState, s.Description,
		})
	}
	d.services.SetRows(svcRows)

	ctrRows := make([]table.Row, 0, len(d.snapshot.Containers))
	for _, c := range d.snapshot.Containers {
		ctrRows = append(ctrRows, table.Row{
			c.Name, c.Image, d.colorContainerState(c.State), c.Status,
		})
	}
	d.containers.SetRows(ctrRows)
}

func (d *Dashboard) colorActive(state string) string {
	switch state {
	case "active":
		return d.styles.good.Render(state)
	case "failed":
		return d.styles.bad.Render(state)
	default:
		return d.styles.warn.Render(state)
	}
}

func (d *Dashboard) colorContainerState(state string) string {
	switch state {
	case "running":
		return d.styles.good.Render(state)
	case "exited", "dead":
		return d.styles.bad.Render(state)
	default:
		return d.styles.warn.Render(state)
	}
}

// fetchCmd runs one inventory fetch off the update loop.
func (d *Dashboard) fetchCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		snap, err := d.fetch(ctx)
		if err != nil {
			return fetchErrMsg{err: err}
		}
		return snapshotMsg(snap)
	}
}

func refreshTick() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return refreshTickMsg(t)
	})
}
