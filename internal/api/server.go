// Package api implements the gRPC server side of the Argus control-plane API.
package api

import (
	"context"

	argusv1 "github.com/jakobneri/argus/proto/argusv1"
)

// Server implements the argus.v1.Argus gRPC service.
type Server struct {
	argusv1.UnimplementedArgusServer
}

// NewServer returns a new API server.
func NewServer() *Server {
	return &Server{}
}

// Ping answers a liveness probe.
func (s *Server) Ping(_ context.Context, _ *argusv1.Ping) (*argusv1.Pong, error) {
	return &argusv1.Pong{}, nil
}
