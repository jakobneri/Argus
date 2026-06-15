package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

type fakeHost struct {
	cpu                 float64
	memUsed, memTotal   uint64
	diskUsed, diskTotal uint64
	rx, tx              uint64
	cpuErr, memErr      error
	diskErr, netErr     error
	lastDiskPath        string
}

func (f *fakeHost) CPUPercent(_ context.Context) (float64, error) { return f.cpu, f.cpuErr }

func (f *fakeHost) Memory(_ context.Context) (uint64, uint64, error) {
	return f.memUsed, f.memTotal, f.memErr
}

func (f *fakeHost) Disk(_ context.Context, path string) (uint64, uint64, error) {
	f.lastDiskPath = path
	return f.diskUsed, f.diskTotal, f.diskErr
}

func (f *fakeHost) Net(_ context.Context) (uint64, uint64, error) { return f.rx, f.tx, f.netErr }

type fakeContainers struct {
	raws []rawContainerStats
	err  error
}

func (f *fakeContainers) Stats(_ context.Context) ([]rawContainerStats, error) {
	return f.raws, f.err
}

func newTestCollector(host hostSource, ctrs containerSource) *collector {
	return &collector{
		host:       host,
		containers: ctrs,
		diskPath:   "/",
		now:        func() time.Time { return time.Unix(1700000000, 0) },
	}
}

func statsSample(total, preTotal, sys, preSys uint64, cpus uint32, memUsage, memLimit uint64) container.StatsResponse {
	var s container.StatsResponse
	s.CPUStats.CPUUsage.TotalUsage = total
	s.CPUStats.SystemUsage = sys
	s.CPUStats.OnlineCPUs = cpus
	s.PreCPUStats.CPUUsage.TotalUsage = preTotal
	s.PreCPUStats.SystemUsage = preSys
	s.MemoryStats.Usage = memUsage
	s.MemoryStats.Limit = memLimit
	return s
}

func TestCollect(t *testing.T) {
	host := &fakeHost{
		cpu: 12.5, memUsed: 2 << 30, memTotal: 8 << 30,
		diskUsed: 10 << 30, diskTotal: 100 << 30, rx: 1000, tx: 500,
	}
	ctrs := &fakeContainers{raws: []rawContainerStats{
		{name: "web", stats: statsSample(200, 100, 2000, 1000, 2, 1<<20, 4<<20)},
	}}

	c := newTestCollector(host, ctrs)
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if snap.Host.CPUPercent != 12.5 || snap.Host.MemUsed != 2<<30 || snap.Host.MemTotal != 8<<30 {
		t.Errorf("host metrics not passed through: %+v", snap.Host)
	}
	if snap.Host.DiskMount != "/" || host.lastDiskPath != "/" {
		t.Errorf("disk mount = %q (queried %q), want /", snap.Host.DiskMount, host.lastDiskPath)
	}
	if snap.Host.NetRxBytes != 1000 || snap.Host.NetTxBytes != 500 {
		t.Errorf("net metrics = rx %d tx %d, want 1000/500", snap.Host.NetRxBytes, snap.Host.NetTxBytes)
	}
	if !snap.Timestamp.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("timestamp = %v, want injected clock", snap.Timestamp)
	}
	if len(snap.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(snap.Containers))
	}
	got := snap.Containers[0]
	// cpuDelta=100, sysDelta=1000, cpus=2 -> (100/1000)*2*100 = 20.0
	if got.Name != "web" || got.CPUPercent != 20.0 || got.MemUsed != 1<<20 || got.MemLimit != 4<<20 {
		t.Errorf("container metrics = %+v, want web/20.0/1MiB/4MiB", got)
	}
}

func TestCollectHostErrorIsFatal(t *testing.T) {
	cases := []struct {
		name string
		host *fakeHost
	}{
		{"cpu", &fakeHost{cpuErr: errors.New("no /proc/stat")}},
		{"mem", &fakeHost{memErr: errors.New("no meminfo")}},
		{"disk", &fakeHost{diskErr: errors.New("no statfs")}},
		{"net", &fakeHost{netErr: errors.New("no netdev")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCollector(tc.host, &fakeContainers{})
			if _, err := c.Collect(context.Background()); err == nil {
				t.Fatal("expected a fatal host error, got nil")
			}
		})
	}
}

func TestCollectDockerErrorIsNonFatal(t *testing.T) {
	c := newTestCollector(&fakeHost{cpu: 5}, &fakeContainers{err: errors.New("docker down")})
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect should swallow docker errors, got %v", err)
	}
	if len(snap.Containers) != 0 {
		t.Errorf("containers = %d, want 0 when docker is down", len(snap.Containers))
	}
	if snap.Host.CPUPercent != 5 {
		t.Errorf("host metrics should still be present, got %+v", snap.Host)
	}
}

func TestContainerCPUPercent(t *testing.T) {
	tests := []struct {
		name string
		in   container.StatsResponse
		want float64
	}{
		{"normal", statsSample(200, 100, 2000, 1000, 2, 0, 0), 20.0},
		{"single cpu", statsSample(150, 100, 1000, 900, 1, 0, 0), 50.0},
		{"zero system delta guards against divide by zero", statsSample(200, 100, 1000, 1000, 2, 0, 0), 0},
		{"negative cpu delta clamps to zero", statsSample(50, 100, 2000, 1000, 2, 0, 0), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containerCPUPercent(tt.in); got != tt.want {
				t.Errorf("containerCPUPercent = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestContainerCPUPercentFallsBackToPercpu(t *testing.T) {
	s := statsSample(200, 100, 2000, 1000, 0, 0, 0)
	s.CPUStats.CPUUsage.PercpuUsage = []uint64{1, 2, 3, 4} // 4 cores, OnlineCPUs unset
	if got := containerCPUPercent(s); got != 40.0 {
		t.Errorf("containerCPUPercent = %v, want 40.0 (4 cores via percpu)", got)
	}
}

func TestContainerMemUsedSubtractsCache(t *testing.T) {
	tests := []struct {
		name  string
		usage uint64
		stats map[string]uint64
		want  uint64
	}{
		{"no cache key", 1000, nil, 1000},
		{"cgroup v2 inactive_file", 1000, map[string]uint64{"inactive_file": 300}, 700},
		{"cgroup v1 total_inactive_file", 1000, map[string]uint64{"total_inactive_file": 250}, 750},
		{"cache larger than usage clamps to zero", 100, map[string]uint64{"inactive_file": 500}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s container.StatsResponse
			s.MemoryStats.Usage = tt.usage
			s.MemoryStats.Stats = tt.stats
			if got := containerMemUsed(s); got != tt.want {
				t.Errorf("containerMemUsed = %d, want %d", got, tt.want)
			}
		})
	}
}
