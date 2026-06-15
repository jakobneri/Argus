// Package logs streams unified log entries from the systemd journal and from
// Docker. Each backend sits behind a small interface so the merging logic can
// be tested without spawning processes or talking to Docker. Everything here
// is read-only.
package logs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Source selects which backends a stream draws from.
type Source int

const (
	// SourceAll streams from every backend.
	SourceAll Source = iota
	// SourceSystemd streams only from the systemd journal.
	SourceSystemd
	// SourceDocker streams only from Docker container logs.
	SourceDocker
)

// Entry is one unified log line.
type Entry struct {
	Timestamp time.Time
	Source    Source
	// Origin is the systemd unit or the Docker container name.
	Origin string
	// Level is the journald severity ("info", "err", ...) or, for Docker, the
	// stream the line came from ("stdout"/"stderr").
	Level   string
	Message string
}

// Request parameterises a log stream.
type Request struct {
	Source Source
	// Filter is an optional systemd unit name and/or Docker container name.
	Filter string
	// Since is an optional start point passed through to the backends.
	Since string
	// Priority is an optional journald priority filter. It is not wired to the
	// StreamLogs RPC in M2 but the journald backend honours it.
	Priority string
}

// Manager streams log entries from the configured backends.
type Manager interface {
	// Stream feeds entries to emit until ctx is cancelled, the sources end, or
	// emit returns an error. It blocks for the lifetime of the stream. Because
	// a single goroutine drains the merged entries, emit is never called
	// concurrently (gRPC stream Send is not concurrency-safe).
	Stream(ctx context.Context, req Request, emit func(Entry) error) error
}

// sourceStreamer streams entries from one backend into out until ctx is done.
// It must select on ctx when sending so a slow or gone consumer cannot wedge
// it, and it owns the lifecycle of its underlying process/connection.
type sourceStreamer interface {
	stream(ctx context.Context, req Request, out chan<- Entry)
}

// manager merges the journald and Docker backends.
type manager struct {
	log     *slog.Logger
	systemd sourceStreamer
	docker  sourceStreamer
}

// NewManager returns a Manager backed by `journalctl -o json --follow` (no
// CGO) and the Docker logs API.
func NewManager(log *slog.Logger) Manager {
	return &manager{
		log:     log,
		systemd: &journaldStreamer{log: log},
		docker:  &dockerStreamer{log: log, connect: defaultLogsConnect},
	}
}

// Stream fans the selected backends into one channel and forwards entries to
// emit. Cancelling ctx (or emit returning an error) tears every backend down:
// streamCtx is cancelled, which kills the journalctl process and closes the
// Docker readers, so no goroutine is left behind.
func (m *manager) Stream(ctx context.Context, req Request, emit func(Entry) error) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch := make(chan Entry, 256)
	var wg sync.WaitGroup
	start := func(s sourceStreamer) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.stream(streamCtx, req, ch)
		}()
	}

	switch req.Source {
	case SourceSystemd:
		start(m.systemd)
	case SourceDocker:
		start(m.docker)
	default: // SourceAll
		start(m.systemd)
		start(m.docker)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("log stream: %w", ctx.Err())
		case entry, ok := <-ch:
			if !ok {
				return nil // all backends finished
			}
			if err := emit(entry); err != nil {
				return err
			}
		}
	}
}
