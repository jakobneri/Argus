package api

import (
	"time"

	"github.com/jakobneri/argus/internal/logs"
	"github.com/jakobneri/argus/internal/metrics"
	argusv1 "github.com/jakobneri/argus/proto/argusv1"
	"github.com/jakobneri/argus/tui"
)

// unixNano returns the Unix-nanosecond timestamp, or 0 for the zero time so a
// missing timestamp does not surface as a large negative number.
func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// metricsSnapshotToProto maps a collector snapshot to the streamed response.
func metricsSnapshotToProto(snap metrics.Snapshot) *argusv1.StreamMetricsResponse {
	ms := &argusv1.MetricsSnapshot{
		TimestampUnixNano: unixNano(snap.Timestamp),
		Host: &argusv1.HostMetrics{
			CpuPercent:     snap.Host.CPUPercent,
			MemUsedBytes:   snap.Host.MemUsed,
			MemTotalBytes:  snap.Host.MemTotal,
			DiskUsedBytes:  snap.Host.DiskUsed,
			DiskTotalBytes: snap.Host.DiskTotal,
			DiskMount:      snap.Host.DiskMount,
			NetRxBytes:     snap.Host.NetRxBytes,
			NetTxBytes:     snap.Host.NetTxBytes,
		},
	}
	for _, c := range snap.Containers {
		ms.Containers = append(ms.Containers, &argusv1.ContainerMetrics{
			Name:          c.Name,
			CpuPercent:    c.CPUPercent,
			MemUsedBytes:  c.MemUsed,
			MemLimitBytes: c.MemLimit,
		})
	}
	return &argusv1.StreamMetricsResponse{Snapshot: ms}
}

// metricsSnapshotToTUI maps a collector snapshot to the dashboard view type.
func metricsSnapshotToTUI(snap metrics.Snapshot) tui.MetricsSnapshot {
	out := tui.MetricsSnapshot{
		Valid: true,
		Host: tui.HostMetrics{
			CPUPercent: snap.Host.CPUPercent,
			MemUsed:    snap.Host.MemUsed,
			MemTotal:   snap.Host.MemTotal,
			DiskUsed:   snap.Host.DiskUsed,
			DiskTotal:  snap.Host.DiskTotal,
			DiskMount:  snap.Host.DiskMount,
			NetRxBytes: snap.Host.NetRxBytes,
			NetTxBytes: snap.Host.NetTxBytes,
		},
	}
	for _, c := range snap.Containers {
		out.Containers = append(out.Containers, tui.ContainerMetrics{
			Name:       c.Name,
			CPUPercent: c.CPUPercent,
			MemUsed:    c.MemUsed,
			MemLimit:   c.MemLimit,
		})
	}
	return out
}

// logEntryToProto maps a unified log entry to the streamed response payload.
func logEntryToProto(e logs.Entry) *argusv1.LogEntry {
	return &argusv1.LogEntry{
		TimestampUnixNano: unixNano(e.Timestamp),
		Source:            logSourceToProto(e.Source),
		Origin:            e.Origin,
		Level:             e.Level,
		Message:           e.Message,
	}
}

// logEntryToTUI maps a unified log entry to the dashboard view type.
func logEntryToTUI(e logs.Entry) tui.LogEntry {
	return tui.LogEntry{
		Timestamp: e.Timestamp,
		Source:    logSourceToTUI(e.Source),
		Origin:    e.Origin,
		Level:     e.Level,
		Message:   e.Message,
	}
}

func logSourceFromProto(s argusv1.LogSource) logs.Source {
	switch s {
	case argusv1.LogSource_LOG_SOURCE_SYSTEMD:
		return logs.SourceSystemd
	case argusv1.LogSource_LOG_SOURCE_DOCKER:
		return logs.SourceDocker
	default: // unspecified or all
		return logs.SourceAll
	}
}

func logSourceToProto(s logs.Source) argusv1.LogSource {
	switch s {
	case logs.SourceSystemd:
		return argusv1.LogSource_LOG_SOURCE_SYSTEMD
	case logs.SourceDocker:
		return argusv1.LogSource_LOG_SOURCE_DOCKER
	default:
		return argusv1.LogSource_LOG_SOURCE_ALL
	}
}

func logSourceFromTUI(s tui.LogSource) logs.Source {
	switch s {
	case tui.LogSourceSystemd:
		return logs.SourceSystemd
	case tui.LogSourceDocker:
		return logs.SourceDocker
	default:
		return logs.SourceAll
	}
}

func logSourceToTUI(s logs.Source) tui.LogSource {
	switch s {
	case logs.SourceSystemd:
		return tui.LogSourceSystemd
	case logs.SourceDocker:
		return tui.LogSourceDocker
	default:
		return tui.LogSourceAll
	}
}
