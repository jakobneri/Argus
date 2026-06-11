package api_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/jakobneri/argus/internal/api"
	"github.com/jakobneri/argus/internal/containers"
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

// newTestClient serves the API over bufconn and returns a connected client.
func newTestClient(t *testing.T, svc services.Manager, ctr containers.Manager) argusv1.ArgusClient {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	argusv1.RegisterArgusServer(srv, api.NewServer(svc, ctr, slog.New(slog.DiscardHandler)))

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
	client := newTestClient(t, &fakeServiceManager{}, &fakeContainerManager{})

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
			client := newTestClient(t, tt.svc, tt.ctr)

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
