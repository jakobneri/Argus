// Package metrics collects host and per-container resource metrics. The data
// sources sit behind small interfaces so the collector can be tested against
// mocked backends. Implementations must be read-only.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
)

// defaultCPUWindow is how long the host CPU sample is taken over. A positive
// window makes the gopsutil reading self-contained (it samples /proc/stat
// twice around a sleep) instead of relying on package-global state, so any
// number of concurrent Collect callers stay correct.
const defaultCPUWindow = 750 * time.Millisecond

// statsConcurrency bounds how many container stats streams are read at once.
const statsConcurrency = 8

// HostMetrics is the whole-host resource usage at one instant.
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

// ContainerMetrics is the resource usage of one running container.
type ContainerMetrics struct {
	Name       string
	CPUPercent float64
	MemUsed    uint64
	MemLimit   uint64
}

// Snapshot is one point-in-time reading of host and container metrics.
type Snapshot struct {
	Timestamp  time.Time
	Host       HostMetrics
	Containers []ContainerMetrics
}

// Collector produces metrics snapshots. Each Collect call is self-contained:
// it does not carry CPU state between calls, so a single Collector instance is
// safe to share between the in-daemon TUI and the StreamMetrics RPC.
type Collector interface {
	Collect(ctx context.Context) (Snapshot, error)
}

// hostSource abstracts the host metric backend (gopsutil) for testing.
type hostSource interface {
	CPUPercent(ctx context.Context) (float64, error)
	Memory(ctx context.Context) (used, total uint64, err error)
	Disk(ctx context.Context, path string) (used, total uint64, err error)
	Net(ctx context.Context) (rx, tx uint64, err error)
}

// rawContainerStats is one container's decoded Docker stats. The collector
// turns it into a ContainerMetrics; keeping the raw form here lets the CPU%
// derivation be unit-tested against canned stats.
type rawContainerStats struct {
	name  string
	stats container.StatsResponse
}

// containerSource abstracts the Docker stats backend for testing.
type containerSource interface {
	Stats(ctx context.Context) ([]rawContainerStats, error)
}

// collector is the default Collector implementation.
type collector struct {
	host       hostSource
	containers containerSource
	diskPath   string
	now        func() time.Time
}

// NewCollector returns a Collector backed by gopsutil (host) and the Docker
// stats API (containers). The disk figures refer to the root mount.
func NewCollector() Collector {
	return &collector{
		host:       gopsutilHost{cpuWindow: defaultCPUWindow},
		containers: &dockerStats{connect: defaultStatsConnect},
		diskPath:   "/",
		now:        time.Now,
	}
}

// Collect builds one snapshot. A host-metric failure is fatal (the host is the
// primary signal); an unreachable Docker daemon is non-fatal and yields an
// empty container list, mirroring the inventory RPC.
func (c *collector) Collect(ctx context.Context) (Snapshot, error) {
	host, err := c.collectHost(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{Timestamp: c.now(), Host: host}

	raws, err := c.containers.Stats(ctx)
	if err != nil {
		// Non-fatal by design: Docker may not be installed/running.
		return snap, nil
	}
	for _, r := range raws {
		snap.Containers = append(snap.Containers, ContainerMetrics{
			Name:       r.name,
			CPUPercent: containerCPUPercent(r.stats),
			MemUsed:    containerMemUsed(r.stats),
			MemLimit:   r.stats.MemoryStats.Limit,
		})
	}
	return snap, nil
}

func (c *collector) collectHost(ctx context.Context) (HostMetrics, error) {
	cpuPct, err := c.host.CPUPercent(ctx)
	if err != nil {
		return HostMetrics{}, fmt.Errorf("host cpu: %w", err)
	}
	memUsed, memTotal, err := c.host.Memory(ctx)
	if err != nil {
		return HostMetrics{}, fmt.Errorf("host memory: %w", err)
	}
	diskUsed, diskTotal, err := c.host.Disk(ctx, c.diskPath)
	if err != nil {
		return HostMetrics{}, fmt.Errorf("host disk: %w", err)
	}
	rx, tx, err := c.host.Net(ctx)
	if err != nil {
		return HostMetrics{}, fmt.Errorf("host net: %w", err)
	}
	return HostMetrics{
		CPUPercent: cpuPct,
		MemUsed:    memUsed,
		MemTotal:   memTotal,
		DiskUsed:   diskUsed,
		DiskTotal:  diskTotal,
		DiskMount:  c.diskPath,
		NetRxBytes: rx,
		NetTxBytes: tx,
	}, nil
}

// containerCPUPercent derives CPU usage from the delta between the current and
// previous CPU readings carried in the stats sample, normalised to the number
// of online CPUs (this matches the `docker stats` formula).
func containerCPUPercent(s container.StatsResponse) float64 {
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	if sysDelta <= 0 || cpuDelta < 0 {
		return 0
	}
	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpus == 0 {
		cpus = 1
	}
	return (cpuDelta / sysDelta) * cpus * 100
}

// containerMemUsed returns the container's working-set memory: reported usage
// minus the reclaimable page cache, as `docker stats` does (the key differs
// between cgroup v2 and v1).
func containerMemUsed(s container.StatsResponse) uint64 {
	used := s.MemoryStats.Usage
	for _, key := range []string{"inactive_file", "total_inactive_file"} {
		if cache, ok := s.MemoryStats.Stats[key]; ok {
			if cache <= used {
				return used - cache
			}
			return 0
		}
	}
	return used
}

// gopsutilHost reads host metrics via gopsutil (pure Go on Linux).
type gopsutilHost struct {
	cpuWindow time.Duration
}

func (g gopsutilHost) CPUPercent(ctx context.Context) (float64, error) {
	pct, err := cpu.PercentWithContext(ctx, g.cpuWindow, false)
	if err != nil {
		return 0, fmt.Errorf("cpu percent: %w", err)
	}
	if len(pct) == 0 {
		return 0, nil
	}
	return pct[0], nil
}

func (g gopsutilHost) Memory(ctx context.Context) (uint64, uint64, error) {
	v, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("virtual memory: %w", err)
	}
	return v.Used, v.Total, nil
}

func (g gopsutilHost) Disk(ctx context.Context, path string) (uint64, uint64, error) {
	u, err := disk.UsageWithContext(ctx, path)
	if err != nil {
		return 0, 0, fmt.Errorf("disk usage %s: %w", path, err)
	}
	return u.Used, u.Total, nil
}

func (g gopsutilHost) Net(ctx context.Context) (uint64, uint64, error) {
	counters, err := net.IOCountersWithContext(ctx, false)
	if err != nil {
		return 0, 0, fmt.Errorf("net io counters: %w", err)
	}
	if len(counters) == 0 {
		return 0, 0, nil
	}
	return counters[0].BytesRecv, counters[0].BytesSent, nil
}

// statsClient is the slice of the Docker SDK the stats backend needs.
type statsClient interface {
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerStats(ctx context.Context, containerID string, stream bool) (container.StatsResponseReader, error)
	Close() error
}

func defaultStatsConnect() (statsClient, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return cli, nil
}

// dockerStats reads per-container stats via the Docker stats API.
type dockerStats struct {
	connect func() (statsClient, error)
}

// Stats returns delta-resolved stats for every running container. It reads two
// streaming frames per container so the second frame carries a valid
// PreCPUStats, which keeps CPU% correct without remembering state across
// snapshots. Containers whose stats cannot be read are skipped.
func (d *dockerStats) Stats(ctx context.Context) ([]rawContainerStats, error) {
	cli, err := d.connect()
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	summaries, err := cli.ContainerList(ctx, container.ListOptions{All: false})
	if err != nil {
		return nil, fmt.Errorf("docker: list containers: %w", err)
	}

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		out  = make([]rawContainerStats, 0, len(summaries))
		sema = make(chan struct{}, statsConcurrency)
	)
	for _, s := range summaries {
		if len(s.Names) == 0 {
			continue
		}
		s := s
		wg.Add(1)
		sema <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sema }()
			stats, err := readTwoFrames(ctx, cli, s.ID)
			if err != nil {
				return // skip this container; one bad stream must not fail all
			}
			mu.Lock()
			out = append(out, rawContainerStats{name: trimSlash(s.Names[0]), stats: stats})
			mu.Unlock()
		}()
	}
	wg.Wait()

	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// readTwoFrames reads up to two stats frames from a streaming stats response.
// In streaming mode each frame's PreCPUStats is the previous frame's CPUStats,
// so the second frame yields a correct CPU delta.
func readTwoFrames(ctx context.Context, cli statsClient, id string) (container.StatsResponse, error) {
	var s container.StatsResponse
	reader, err := cli.ContainerStats(ctx, id, true)
	if err != nil {
		return s, fmt.Errorf("docker: container stats %s: %w", id, err)
	}
	defer func() { _ = reader.Body.Close() }()

	dec := json.NewDecoder(reader.Body)
	for i := 0; i < 2; i++ {
		if err := dec.Decode(&s); err != nil {
			if i == 0 {
				return s, fmt.Errorf("docker: decode stats %s: %w", id, err)
			}
			break // one frame is enough to return; CPU% will simply read as 0
		}
	}
	return s, nil
}

func trimSlash(name string) string {
	return strings.TrimPrefix(name, "/")
}
