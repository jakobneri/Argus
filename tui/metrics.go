package tui

import (
	"context"
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

// HostMetrics is the rendering view of whole-host resource usage.
type HostMetrics struct {
	CPUPercent float64
	MemUsed    uint64
	MemTotal   uint64
	DiskUsed   uint64
	DiskTotal  uint64
	DiskMount  string
	NetRxBytes uint64
	NetTxBytes uint64
}

// ContainerMetrics is the rendering view of one container's resource usage.
type ContainerMetrics struct {
	Name       string
	CPUPercent float64
	MemUsed    uint64
	MemLimit   uint64
}

// MetricsSnapshot is one metrics reading rendered by the dashboard.
type MetricsSnapshot struct {
	Host       HostMetrics
	Containers []ContainerMetrics
	// Valid is false before the first successful collection.
	Valid bool
}

// MetricsFetch produces a MetricsSnapshot. As with inventory, the daemon
// injects a fetcher that calls the metrics collector in-process; the TUI stays
// free of domain logic.
type MetricsFetch func(ctx context.Context) (MetricsSnapshot, error)

type metricsMsg MetricsSnapshot

// metricBar renders the host metric row: CPU / RAM / Disk / Net.
func (d *Dashboard) metricBar() string {
	if !d.metrics.Valid {
		if d.metricsErr != nil {
			return d.styles.errText.Render("metrics error: " + d.metricsErr.Error())
		}
		return d.styles.hint.Render("metrics: collecting…")
	}
	h := d.metrics.Host

	cpu := d.styles.metricLabel.Render("CPU ") + d.usageColor(h.CPUPercent).Render(fmt.Sprintf("%5.1f%%", h.CPUPercent))

	memPct := percent(h.MemUsed, h.MemTotal)
	mem := d.styles.metricLabel.Render("RAM ") + d.usageColor(memPct).Render(
		fmt.Sprintf("%s/%s", humanBytes(h.MemUsed), humanBytes(h.MemTotal)),
	)

	diskPct := percent(h.DiskUsed, h.DiskTotal)
	mount := h.DiskMount
	if mount == "" {
		mount = "/"
	}
	disk := d.styles.metricLabel.Render("DISK ") + d.usageColor(diskPct).Render(
		fmt.Sprintf("%s/%s", humanBytes(h.DiskUsed), humanBytes(h.DiskTotal)),
	) +
		d.styles.hint.Render(" ("+mount+")")

	net := d.styles.metricLabel.Render("NET ") + d.styles.statusBar.Render(
		fmt.Sprintf("↓%s ↑%s", humanBytes(h.NetRxBytes), humanBytes(h.NetTxBytes)),
	)

	sep := d.styles.hint.Render("   ")
	return cpu + sep + mem + sep + disk + sep + net
}

// usageColor maps a 0..100 usage to a green/yellow/red style.
func (d *Dashboard) usageColor(pct float64) lipgloss.Style {
	switch {
	case pct >= 90:
		return d.styles.bad
	case pct >= 70:
		return d.styles.warn
	default:
		return d.styles.good
	}
}

func percent(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// humanBytes formats a byte count with a binary (KiB/MiB/…) unit.
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
