package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// refreshInterval is how often the dashboard polls for fresh inventory and
// metrics. Assumption: snapshot polling via tea.Tick is sufficient for the
// in-daemon TUI; the streaming RPCs are the external API surface.
const refreshInterval = 3 * time.Second

// fetchTimeout bounds a single inventory/metrics fetch so a hung backend
// cannot stall the dashboard forever.
const fetchTimeout = 5 * time.Second

// tab identifies the active top-level view.
type tab int

const (
	tabDashboard tab = iota
	tabLogs
)

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

// Deps bundles the in-process data sources the dashboard renders. The daemon
// builds these from the same module interfaces the RPCs wrap.
type Deps struct {
	Inventory Fetch
	Metrics   MetricsFetch
	Logs      LogStream
}

type (
	snapshotMsg    Snapshot
	fetchErrMsg    struct{ err error }
	metricsErrMsg  struct{ err error }
	refreshTickMsg time.Time

	dashboardStyles struct {
		title       lipgloss.Style
		hint        lipgloss.Style
		errText     lipgloss.Style
		good        lipgloss.Style
		bad         lipgloss.Style
		warn        lipgloss.Style
		statusBar   lipgloss.Style
		metricLabel lipgloss.Style
		logSource   lipgloss.Style
		tabActive   lipgloss.Style
		tabInactive lipgloss.Style
	}
)

func newDashboardStyles() dashboardStyles {
	return dashboardStyles{
		title:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("44")),
		hint:        lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		errText:     lipgloss.NewStyle().Foreground(lipgloss.Color("203")),
		good:        lipgloss.NewStyle().Foreground(lipgloss.Color("42")),
		bad:         lipgloss.NewStyle().Foreground(lipgloss.Color("203")),
		warn:        lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		statusBar:   lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		metricLabel: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("44")),
		logSource:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("69")),
		tabActive:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("229")).Background(lipgloss.Color("57")),
		tabInactive: lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
	}
}

// Dashboard is the top-level TUI model. It hosts two tabs: a read-only
// inventory + host metrics view, and a streaming, filterable log view. "tab"
// switches between them; "q"/ctrl+c/esc quits.
type Dashboard struct {
	fetch        Fetch
	fetchMetrics MetricsFetch
	styles       dashboardStyles

	tab  tab
	logs *logView

	services   table.Model
	containers table.Model
	focused    int // 0 = services, 1 = containers

	snapshot  Snapshot
	fetchErr  error
	updatedAt time.Time

	metrics    MetricsSnapshot
	metricsErr error

	width  int
	height int
}

// NewDashboard returns a Dashboard that refreshes itself from deps.
func NewDashboard(deps Deps) *Dashboard {
	styles := table.DefaultStyles()
	styles.Selected = styles.Selected.
		Foreground(lipgloss.Color("229")).
		Background(lipgloss.Color("57"))

	services := table.New(table.WithColumns(serviceColumns(80)), table.WithFocused(true))
	services.SetStyles(styles)
	containers := table.New(table.WithColumns(containerColumns(80)))
	containers.SetStyles(styles)

	ds := newDashboardStyles()
	return &Dashboard{
		fetch:        deps.Inventory,
		fetchMetrics: deps.Metrics,
		styles:       ds,
		logs:         newLogView(deps.Logs, ds),
		services:     services,
		containers:   containers,
	}
}

// Init starts the first inventory + metrics fetch and the refresh ticker. The
// log stream starts lazily the first time the Logs tab is shown.
func (d *Dashboard) Init() tea.Cmd {
	return tea.Batch(d.fetchCmd(), d.metricsCmd(), refreshTick())
}

// Update handles input, resizes, ticks, fetch results and log events.
func (d *Dashboard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return d.handleKey(msg)
	case tea.WindowSizeMsg:
		d.width, d.height = msg.Width, msg.Height
		d.layout()
		d.logs.setSize(msg.Width, d.logsHeight())
		return d, nil
	case refreshTickMsg:
		return d, tea.Batch(d.fetchCmd(), d.metricsCmd(), refreshTick())
	case snapshotMsg:
		d.snapshot = Snapshot(msg)
		d.fetchErr = nil
		d.updatedAt = time.Now()
		d.fillTables()
		return d, nil
	case fetchErrMsg:
		d.fetchErr = msg.err
		return d, nil
	case metricsMsg:
		d.metrics = MetricsSnapshot(msg)
		d.metrics.Valid = true
		d.metricsErr = nil
		return d, nil
	case metricsErrMsg:
		d.metricsErr = msg.err
		return d, nil
	case logEntryMsg, logClosedMsg:
		// Always delivered so the log stream keeps draining even when the
		// Dashboard tab is in front.
		return d, d.logs.update(msg)
	}

	// Forward anything else to the active view.
	if d.tab == tabLogs {
		return d, d.logs.update(msg)
	}
	var cmd tea.Cmd
	if d.focused == 0 {
		d.services, cmd = d.services.Update(msg)
	} else {
		d.containers, cmd = d.containers.Update(msg)
	}
	return d, cmd
}

func (d *Dashboard) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// ctrl+c always quits, even while typing a filter.
	if msg.String() == "ctrl+c" {
		return d, tea.Quit
	}
	// While typing a log filter, the log view consumes every other key.
	if d.tab == tabLogs && d.logs.filtering {
		return d, d.logs.update(msg)
	}

	switch msg.String() {
	case "q", "esc":
		return d, tea.Quit
	case "tab":
		return d, d.switchTab()
	}

	if d.tab == tabLogs {
		return d, d.logs.update(msg)
	}

	switch msg.String() {
	case "shift+tab":
		d.toggleFocus()
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

// switchTab flips between the two tabs, lazily starting the log stream the
// first time the Logs tab is shown.
func (d *Dashboard) switchTab() tea.Cmd {
	if d.tab == tabDashboard {
		d.tab = tabLogs
		return d.logs.start()
	}
	d.tab = tabDashboard
	return nil
}

// View renders the active tab.
func (d *Dashboard) View() string {
	if d.width == 0 {
		return "loading…"
	}

	header := d.styles.title.Render("argus") + d.styles.hint.Render("  ·  "+Tagline)
	tabs := d.tabBar()

	if d.tab == tabLogs {
		return lipgloss.JoinVertical(lipgloss.Left, header, tabs, "", d.logs.view())
	}

	servicesTitle := d.sectionTitle("Services (systemd)", d.focused == 0)
	containersTitle := d.sectionTitle("Containers (docker)", d.focused == 1)

	containersBody := d.containers.View()
	if d.snapshot.ContainerHint != "" {
		containersBody = d.styles.warn.Render("⚠ " + d.snapshot.ContainerHint)
	}

	status := fmt.Sprintf("q quit · tab logs · shift+tab pane · refresh %s", refreshInterval)
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
		tabs,
		"",
		d.metricBar(),
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

func (d *Dashboard) tabBar() string {
	return d.tabChip("Dashboard", d.tab == tabDashboard) + " " + d.tabChip("Logs", d.tab == tabLogs)
}

func (d *Dashboard) tabChip(label string, active bool) string {
	if active {
		return d.styles.tabActive.Render(" " + label + " ")
	}
	return d.styles.tabInactive.Render(" " + label + " ")
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

// logsHeight is the room left for the log viewport: total minus header, tab
// bar, a blank line and the bottom bar.
func (d *Dashboard) logsHeight() int {
	return max(d.height-4, 1)
}

// layout distributes the available terminal space between the two tables.
func (d *Dashboard) layout() {
	// Fixed lines: header(1) + tab bar(1) + blanks(4) + metric bar(1) +
	// section titles(2) + status bar(1); the table models consume the rest.
	avail := d.height - 11
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

// metricsCmd runs one metrics fetch off the update loop.
func (d *Dashboard) metricsCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		snap, err := d.fetchMetrics(ctx)
		if err != nil {
			return metricsErrMsg{err: err}
		}
		return metricsMsg(snap)
	}
}

func refreshTick() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return refreshTickMsg(t)
	})
}
