// Command argusd is the privileged Argus daemon. It owns all state and will
// later execute privileged actions; clients talk to it over a Unix domain
// socket via gRPC.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jakobneri/argus/internal/daemon"
)

const defaultSocket = "/run/argus/argusd.sock"

func main() {
	socketPath := flag.String("socket", socketDefault(), "path to the Unix domain socket (env: ARGUS_SOCKET)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d := daemon.New(*socketPath, log)
	if err := d.Run(ctx); err != nil {
		log.Error("argusd failed", "error", err)
		os.Exit(1)
	}
	log.Info("argusd stopped")
}

func socketDefault() string {
	if v := os.Getenv("ARGUS_SOCKET"); v != "" {
		return v
	}
	return defaultSocket
}
