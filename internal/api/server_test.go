package api_test

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/jakobneri/argus/internal/api"
	argusv1 "github.com/jakobneri/argus/proto/argusv1"
)

func TestPing(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	argusv1.RegisterArgusServer(srv, api.NewServer())

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

	client := argusv1.NewArgusClient(conn)
	pong, err := client.Ping(context.Background(), &argusv1.Ping{})
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if pong == nil {
		t.Fatal("expected a Pong, got nil")
	}
}
