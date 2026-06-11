// Package daemon wires the argusd process together: it owns the Unix domain
// socket listener and the gRPC server lifecycle.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"google.golang.org/grpc"

	"github.com/jakobneri/argus/internal/api"
	"github.com/jakobneri/argus/internal/containers"
	"github.com/jakobneri/argus/internal/services"
	argusv1 "github.com/jakobneri/argus/proto/argusv1"
	"github.com/jakobneri/argus/tui"
)

// Daemon is the privileged argusd core. It serves the read-only inventory
// API and hosts headless TUI sessions over the Attach stream.
type Daemon struct {
	socketPath string
	log        *slog.Logger
}

// New returns a Daemon that will listen on socketPath.
func New(socketPath string, log *slog.Logger) *Daemon {
	return &Daemon{socketPath: socketPath, log: log}
}

// Run serves the gRPC API on the Unix domain socket until ctx is cancelled.
// A stale socket file from a previous run is removed before listening; the
// socket file is removed again on shutdown.
func (d *Daemon) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(d.socketPath), 0o755); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	if err := os.Remove(d.socketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale socket %s: %w", d.socketPath, err)
	}

	lis, err := net.Listen("unix", d.socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", d.socketPath, err)
	}
	// Group members (argus group) must be able to connect.
	if err := os.Chmod(d.socketPath, 0o660); err != nil {
		return fmt.Errorf("chmod socket %s: %w", d.socketPath, err)
	}

	// The daemon renders headless TUI frames to a stream, not a TTY, so the
	// color profile cannot be autodetected and is forced instead.
	tui.ForceColors()

	srv := grpc.NewServer()
	argusv1.RegisterArgusServer(srv, api.NewServer(
		services.NewSystemdManager(),
		containers.NewDockerManager(),
		d.log,
	))

	errCh := make(chan error, 1)
	go func() {
		d.log.Info("argusd listening", "socket", d.socketPath)
		errCh <- srv.Serve(lis)
	}()

	select {
	case <-ctx.Done():
		d.log.Info("shutting down")
		srv.GracefulStop()
		<-errCh
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("serve grpc: %w", err)
		}
	}

	if err := os.Remove(d.socketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove socket %s: %w", d.socketPath, err)
	}
	return nil
}
