// Package api implements the gRPC server side of the Argus control-plane API.
// Handlers contain no domain logic: they delegate to the module interfaces
// (services.Manager, containers.Manager) and translate to proto messages.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jakobneri/argus/internal/containers"
	"github.com/jakobneri/argus/internal/logs"
	"github.com/jakobneri/argus/internal/metrics"
	"github.com/jakobneri/argus/internal/services"
	"github.com/jakobneri/argus/internal/session"
	argusv1 "github.com/jakobneri/argus/proto/argusv1"
	"github.com/jakobneri/argus/tui"
)

// defaultMetricsInterval is used when StreamMetrics is called with
// interval_seconds == 0.
const defaultMetricsInterval = 3 * time.Second

// Server implements the argus.v1.Argus gRPC service.
type Server struct {
	argusv1.UnimplementedArgusServer

	services   services.Manager
	containers containers.Manager
	metrics    metrics.Collector
	logs       logs.Manager
	log        *slog.Logger
}

// NewServer returns a new API server backed by the given module managers.
func NewServer(svc services.Manager, ctr containers.Manager, met metrics.Collector, lg logs.Manager, log *slog.Logger) *Server {
	return &Server{services: svc, containers: ctr, metrics: met, logs: lg, log: log}
}

// Ping answers a liveness probe.
func (s *Server) Ping(_ context.Context, _ *argusv1.PingRequest) (*argusv1.PingResponse, error) {
	return &argusv1.PingResponse{}, nil
}

// GetInventory returns a snapshot of systemd services and Docker containers.
// An unreachable Docker daemon is non-fatal: containers stay empty and
// container_error carries the hint.
func (s *Server) GetInventory(ctx context.Context, _ *argusv1.GetInventoryRequest) (*argusv1.GetInventoryResponse, error) {
	inv, err := s.inventory(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get inventory: %v", err)
	}

	resp := &argusv1.GetInventoryResponse{
		Services:       make([]*argusv1.ServiceUnit, 0, len(inv.Services)),
		Containers:     make([]*argusv1.Container, 0, len(inv.Containers)),
		ContainerError: inv.ContainerHint,
	}
	for _, svc := range inv.Services {
		resp.Services = append(resp.Services, &argusv1.ServiceUnit{
			Name:        svc.Name,
			Description: svc.Description,
			LoadState:   svc.LoadState,
			ActiveState: svc.ActiveState,
			SubState:    svc.SubState,
		})
	}
	for _, ctr := range inv.Containers {
		resp.Containers = append(resp.Containers, &argusv1.Container{
			Name:   ctr.Name,
			Image:  ctr.Image,
			State:  ctr.State,
			Status: ctr.Status,
		})
	}
	return resp, nil
}

// inventory builds one read-only snapshot by calling the module interfaces.
// It is shared by the GetInventory RPC and the in-daemon TUI (architecture
// decision: the TUI calls the modules in-process, not via self-gRPC; the RPC
// wraps the same interfaces as the external API surface).
func (s *Server) inventory(ctx context.Context) (tui.Snapshot, error) {
	var snap tui.Snapshot

	svcs, err := s.services.List(ctx)
	if err != nil {
		return snap, fmt.Errorf("list services: %w", err)
	}
	for _, svc := range svcs {
		snap.Services = append(snap.Services, tui.ServiceRow(svc))
	}

	ctrs, err := s.containers.List(ctx)
	if err != nil {
		// Non-fatal by design: Docker may simply not be installed/running.
		snap.ContainerHint = fmt.Sprintf("containers unavailable: %v", err)
		return snap, nil
	}
	for _, ctr := range ctrs {
		snap.Containers = append(snap.Containers, tui.ContainerRow(ctr))
	}
	return snap, nil
}

// StreamMetrics emits a host + per-container metrics snapshot on the requested
// interval until the client disconnects. The stream ends cleanly on disconnect
// (the ticker is stopped and the collect context is the stream context, so no
// goroutine is left behind).
func (s *Server) StreamMetrics(req *argusv1.StreamMetricsRequest, stream argusv1.Argus_StreamMetricsServer) error {
	interval := time.Duration(req.GetIntervalSeconds()) * time.Second
	if interval <= 0 {
		interval = defaultMetricsInterval
	}
	ctx := stream.Context()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		snap, err := s.metrics.Collect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // client disconnected mid-collect
			}
			return status.Errorf(codes.Internal, "collect metrics: %v", err)
		}
		if err := stream.Send(metricsSnapshotToProto(snap)); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return status.Errorf(codes.Internal, "send metrics: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// StreamLogs streams unified log entries until the client disconnects. The
// logs manager owns the backend lifecycles, so a disconnect (which cancels the
// stream context) tears the journalctl process and Docker readers down.
func (s *Server) StreamLogs(req *argusv1.StreamLogsRequest, stream argusv1.Argus_StreamLogsServer) error {
	ctx := stream.Context()
	lreq := logs.Request{
		Source: logSourceFromProto(req.GetSource()),
		Filter: req.GetFilter(),
		Since:  req.GetSince(),
	}
	err := s.logs.Stream(ctx, lreq, func(e logs.Entry) error {
		if sendErr := stream.Send(&argusv1.StreamLogsResponse{Entry: logEntryToProto(e)}); sendErr != nil {
			return fmt.Errorf("send log entry: %w", sendErr)
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		return status.Errorf(codes.Internal, "stream logs: %v", err)
	}
	return nil
}

// metricsFetch adapts the metrics collector to the dashboard's MetricsFetch.
func (s *Server) metricsFetch(ctx context.Context) (tui.MetricsSnapshot, error) {
	snap, err := s.metrics.Collect(ctx)
	if err != nil {
		return tui.MetricsSnapshot{}, fmt.Errorf("collect metrics: %w", err)
	}
	return metricsSnapshotToTUI(snap), nil
}

// logStream adapts the logs manager to the dashboard's LogStream. Stream errors
// are non-fatal for the in-daemon TUI: they are logged, and the view simply
// stops receiving new entries.
func (s *Server) logStream(ctx context.Context, req tui.LogRequest, emit func(tui.LogEntry)) {
	lreq := logs.Request{
		Source: logSourceFromTUI(req.Source),
		Filter: req.Filter,
		Since:  req.Since,
	}
	err := s.logs.Stream(ctx, lreq, func(e logs.Entry) error {
		emit(logEntryToTUI(e))
		return nil
	})
	if err != nil && ctx.Err() == nil {
		s.log.Warn("tui log stream ended", "error", err)
	}
}

// Attach runs one ephemeral headless dashboard session over the stream.
// The session ends when the TUI program quits ("q") or the stream closes;
// either way the daemon keeps running and accepts new Attach calls.
func (s *Server) Attach(stream argusv1.Argus_AttachServer) error {
	sess := session.New(tui.NewDashboard(tui.Deps{
		Inventory: s.inventory,
		Metrics:   s.metricsFetch,
		Logs:      s.logStream,
	}), &streamWriter{stream: stream})
	defer sess.Close()

	s.log.Info("attach session started")

	// Receive loop: feeds input and resize events into the session. It ends
	// when the client disconnects (killing the session) or, after the program
	// quit on its own, when the handler returns and gRPC cancels the stream.
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				sess.Close()
				return
			}
			switch ev := req.GetEvent().(type) {
			case *argusv1.AttachRequest_Input:
				if err := sess.Input(ev.Input); err != nil {
					sess.Close()
					return
				}
			case *argusv1.AttachRequest_Resize:
				sess.Resize(int(ev.Resize.GetCols()), int(ev.Resize.GetRows()))
			}
		}
	}()

	err := sess.Run()
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		s.log.Error("attach session failed", "error", err)
		return status.Errorf(codes.Internal, "attach session: %v", err)
	}
	s.log.Info("attach session ended")
	return nil
}

// streamWriter adapts the Attach send side to io.Writer for the TUI renderer.
// Bubble Tea flushes frames from a single renderer goroutine, so Send is
// never called concurrently.
type streamWriter struct {
	stream argusv1.Argus_AttachServer
}

func (w *streamWriter) Write(p []byte) (int, error) {
	if err := w.stream.Send(&argusv1.AttachResponse{Output: p}); err != nil {
		return 0, fmt.Errorf("send output frame: %w", err)
	}
	return len(p), nil
}
