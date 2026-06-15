package api_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/jakobneri/argus/internal/api"
	"github.com/jakobneri/argus/internal/containers"
	"github.com/jakobneri/argus/internal/logs"
	"github.com/jakobneri/argus/internal/metrics"
	"github.com/jakobneri/argus/internal/services"
	argusv1 "github.com/jakobneri/argus/proto/argusv1"
)

type fakeServiceManager struct {
	services []services.Service
	err      error
}

func (f *fakeServiceManager) List(_ context.Context) ([]services.Service, error) {
	return f.services, f.err
}

type fakeContainerManager struct {
	containers []containers.Container
	err        error
}

func (f *fakeContainerManager) List(_ context.Context) ([]containers.Container, error) {
	return f.containers, f.err
}

type fakeCollector struct {
	snap metrics.Snapshot
	err  error
}

func (f *fakeCollector) Collect(_ context.Context) (metrics.Snapshot, error) {
	return f.snap, f.err
}

// fakeLogManager emits canned entries, applying the Filter as an Origin match
// so tests can assert filter plumbing. It returns when ctx is cancelled or all
// entries are emitted.
type fakeLogManager struct {
	entries []logs.Entry
	block   bool
}

func (f *fakeLogManager) Stream(ctx context.Context, req logs.Request, emit func(logs.Entry) error) error {
	for _, e := range f.entries {
		if req.Filter != "" && e.Origin != req.Filter {
			continue
		}
		if err := emit(e); err != nil {
			return err
		}
	}
	if f.block {
		<-ctx.Done()
	}
	return nil
}

// testServer bundles the managers an API server needs for a test.
type testServer struct {
	svc     services.Manager
	ctr     containers.Manager
	metrics metrics.Collector
	logs    logs.Manager
}

// newTestClient serves the API over bufconn and returns a connected client.
func newTestClient(t *testing.T, ts testServer) argusv1.ArgusClient {
	t.Helper()

	if ts.svc == nil {
		ts.svc = &fakeServiceManager{}
	}
	if ts.ctr == nil {
		ts.ctr = &fakeContainerManager{}
	}
	if ts.metrics == nil {
		ts.metrics = &fakeCollector{}
	}
	if ts.logs == nil {
		ts.logs = &fakeLogManager{}
	}

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	argusv1.RegisterArgusServer(srv, api.NewServer(ts.svc, ts.ctr, ts.metrics, ts.logs, slog.New(slog.DiscardHandler)))

	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("serve: %v", err)
		}
	}()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return argusv1.NewArgusClient(conn)
}

func TestPing(t *testing.T) {
	client := newTestClient(t, testServer{})

	pong, err := client.Ping(context.Background(), &argusv1.PingRequest{})
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if pong == nil {
		t.Fatal("expected a PingResponse, got nil")
	}
}

func TestGetInventory(t *testing.T) {
	tests := []struct {
		name           string
		svc            *fakeServiceManager
		ctr            *fakeContainerManager
		wantErr        bool
		wantServices   int
		wantContainers int
		wantHint       bool
	}{
		{
			name: "services and containers",
			svc: &fakeServiceManager{services: []services.Service{
				{Name: "ssh.service", Description: "OpenSSH", LoadState: "loaded", ActiveState: "active", SubState: "running"},
			}},
			ctr: &fakeContainerManager{containers: []containers.Container{
				{Name: "web", Image: "nginx", State: "running", Status: "Up 1 hour"},
			}},
			wantServices:   1,
			wantContainers: 1,
		},
		{
			name:     "docker unreachable is non-fatal with hint",
			svc:      &fakeServiceManager{},
			ctr:      &fakeContainerManager{err: errors.New("docker daemon unreachable")},
			wantHint: true,
		},
		{
			name:    "systemd failure is an RPC error",
			svc:     &fakeServiceManager{err: errors.New("dbus broke")},
			ctr:     &fakeContainerManager{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient(t, testServer{svc: tt.svc, ctr: tt.ctr})

			resp, err := client.GetInventory(context.Background(), &argusv1.GetInventoryRequest{})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("get inventory: %v", err)
			}
			if got := len(resp.GetServices()); got != tt.wantServices {
				t.Errorf("services = %d, want %d", got, tt.wantServices)
			}
			if got := len(resp.GetContainers()); got != tt.wantContainers {
				t.Errorf("containers = %d, want %d", got, tt.wantContainers)
			}
			if tt.wantHint && resp.GetContainerError() == "" {
				t.Error("expected a container_error hint, got none")
			}
			if !tt.wantHint && resp.GetContainerError() != "" {
				t.Errorf("unexpected container_error: %q", resp.GetContainerError())
			}
		})
	}
}

func TestStreamMetrics(t *testing.T) {
	snap := metrics.Snapshot{
		Timestamp: time.Unix(1700000000, 0),
		Host: metrics.HostMetrics{
			CPUPercent: 42.5, MemUsed: 2 << 30, MemTotal: 8 << 30,
			DiskUsed: 10 << 30, DiskTotal: 100 << 30, DiskMount: "/",
			NetRxBytes: 1234, NetTxBytes: 567,
		},
		Containers: []metrics.ContainerMetrics{
			{Name: "web", CPUPercent: 12.0, MemUsed: 1 << 20, MemLimit: 4 << 20},
		},
	}
	client := newTestClient(t, testServer{metrics: &fakeCollector{snap: snap}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.StreamMetrics(ctx, &argusv1.StreamMetricsRequest{IntervalSeconds: 1})
	if err != nil {
		t.Fatalf("StreamMetrics: %v", err)
	}

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	host := resp.GetSnapshot().GetHost()
	if host.GetCpuPercent() != 42.5 || host.GetMemUsedBytes() != 2<<30 || host.GetDiskMount() != "/" {
		t.Errorf("host metrics = %+v", host)
	}
	if host.GetNetRxBytes() != 1234 || host.GetNetTxBytes() != 567 {
		t.Errorf("net metrics = rx %d tx %d", host.GetNetRxBytes(), host.GetNetTxBytes())
	}
	if got := resp.GetSnapshot().GetContainers(); len(got) != 1 || got[0].GetName() != "web" || got[0].GetCpuPercent() != 12.0 {
		t.Errorf("container metrics = %+v", got)
	}

	// Cancelling the client context must end the stream cleanly.
	cancel()
	for {
		if _, err := stream.Recv(); err != nil {
			break // stream ended as expected
		}
	}
}

func TestStreamMetricsCollectErrorIsRPCError(t *testing.T) {
	client := newTestClient(t, testServer{metrics: &fakeCollector{err: errors.New("no /proc")}})
	stream, err := client.StreamMetrics(context.Background(), &argusv1.StreamMetricsRequest{})
	if err != nil {
		t.Fatalf("StreamMetrics: %v", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("expected an RPC error when the collector fails, got nil")
	}
}

func TestStreamLogs(t *testing.T) {
	entries := []logs.Entry{
		{Timestamp: time.Unix(1700000000, 0), Source: logs.SourceSystemd, Origin: "ssh.service", Level: "info", Message: "login"},
		{Timestamp: time.Unix(1700000001, 0), Source: logs.SourceDocker, Origin: "web", Level: "stdout", Message: "GET /"},
	}

	t.Run("all entries", func(t *testing.T) {
		client := newTestClient(t, testServer{logs: &fakeLogManager{entries: entries}})
		got := drainLogs(t, client, &argusv1.StreamLogsRequest{Source: argusv1.LogSource_LOG_SOURCE_ALL})
		if len(got) != 2 {
			t.Fatalf("entries = %d, want 2", len(got))
		}
		if got[0].GetOrigin() != "ssh.service" || got[0].GetSource() != argusv1.LogSource_LOG_SOURCE_SYSTEMD {
			t.Errorf("entry[0] = %+v", got[0])
		}
		if got[1].GetSource() != argusv1.LogSource_LOG_SOURCE_DOCKER {
			t.Errorf("entry[1] source = %v, want docker", got[1].GetSource())
		}
	})

	t.Run("filter selects matching origin", func(t *testing.T) {
		client := newTestClient(t, testServer{logs: &fakeLogManager{entries: entries}})
		got := drainLogs(t, client, &argusv1.StreamLogsRequest{
			Source: argusv1.LogSource_LOG_SOURCE_SYSTEMD,
			Filter: "ssh.service",
		})
		if len(got) != 1 || got[0].GetOrigin() != "ssh.service" {
			t.Fatalf("filtered entries = %+v, want only ssh.service", got)
		}
		if got[0].GetMessage() != "login" {
			t.Errorf("message = %q, want login", got[0].GetMessage())
		}
	})
}

func drainLogs(t *testing.T, client argusv1.ArgusClient, req *argusv1.StreamLogsRequest) []*argusv1.LogEntry {
	t.Helper()
	stream, err := client.StreamLogs(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	var got []*argusv1.LogEntry
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got = append(got, resp.GetEntry())
	}
}
