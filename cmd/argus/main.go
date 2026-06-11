// Command argus is the unprivileged terminal bridge. It contains no logic
// beyond gRPC plumbing and rendering: in M0 it pings argusd over the Unix
// domain socket and shows the splash on success.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	argusv1 "github.com/jakobneri/argus/proto/argusv1"
	"github.com/jakobneri/argus/tui"
)

const defaultSocket = "/run/argus/argusd.sock"

func main() {
	socketPath := flag.String("socket", socketDefault(), "path to the argusd Unix domain socket (env: ARGUS_SOCKET)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*socketPath); err != nil {
		log.Error("argus failed", "error", err)
		os.Exit(1)
	}
}

func run(socketPath string) error {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("connect to argusd at %s: %w", socketPath, err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := argusv1.NewArgusClient(conn).Ping(ctx, &argusv1.Ping{}); err != nil {
		return fmt.Errorf("ping argusd at %s: %w", socketPath, err)
	}

	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		width = 0 // not a terminal: render the full splash
	}
	fmt.Println(tui.SplashForWidth(width))
	return nil
}

func socketDefault() string {
	if v := os.Getenv("ARGUS_SOCKET"); v != "" {
		return v
	}
	return defaultSocket
}
