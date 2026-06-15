package logs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// logsClient is the slice of the Docker SDK the log streamer needs.
type logsClient interface {
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
	ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error)
	Close() error
}

func defaultLogsConnect() (logsClient, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return cli, nil
}

// dockerStreamer streams logs from running containers via the Docker logs API.
type dockerStreamer struct {
	log     *slog.Logger
	connect func() (logsClient, error)
}

func (d *dockerStreamer) stream(ctx context.Context, req Request, out chan<- Entry) {
	cli, err := d.connect()
	if err != nil {
		// Non-fatal, mirroring the inventory RPC: Docker may not be running.
		d.log.Warn("docker logs: connect", "error", err)
		return
	}
	defer func() { _ = cli.Close() }()

	summaries, err := cli.ContainerList(ctx, container.ListOptions{All: false})
	if err != nil {
		d.log.Warn("docker logs: list containers", "error", err)
		return
	}

	var wg sync.WaitGroup
	for _, s := range summaries {
		name := trimSlash(firstName(s.Names))
		if req.Filter != "" && name != req.Filter {
			continue
		}
		wg.Add(1)
		go func(id, name string) {
			defer wg.Done()
			d.streamOne(ctx, cli, id, name, req, out)
		}(s.ID, name)
	}
	wg.Wait()
}

// streamOne follows one container's logs. TTY containers produce a raw stream;
// others produce Docker's multiplexed stdout/stderr framing.
func (d *dockerStreamer) streamOne(ctx context.Context, cli logsClient, id, name string, req Request, out chan<- Entry) {
	tty := false
	if info, err := cli.ContainerInspect(ctx, id); err == nil && info.Config != nil {
		tty = info.Config.Tty
	}

	opts := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: true,
		Tail:       "50",
		Since:      req.Since,
	}
	rc, err := cli.ContainerLogs(ctx, id, opts)
	if err != nil {
		d.log.Warn("docker logs: stream", "container", name, "error", err)
		return
	}
	defer func() { _ = rc.Close() }()

	if tty {
		d.scanRaw(ctx, rc, name, out)
		return
	}
	d.scanMultiplexed(ctx, rc, name, out)
}

// scanRaw reads a non-multiplexed (TTY) log stream line by line.
func (d *dockerStreamer) scanRaw(ctx context.Context, r io.Reader, name string, out chan<- Entry) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if !send(ctx, out, parseDockerLine(scanner.Bytes(), name, "stdout")) {
			return
		}
	}
}

// scanMultiplexed demultiplexes Docker's 8-byte-header stdout/stderr framing,
// buffering partial lines per stream so a line split across frames is kept
// whole.
func (d *dockerStreamer) scanMultiplexed(ctx context.Context, r io.Reader, name string, out chan<- Entry) {
	pending := map[byte][]byte{}
	for {
		frame, err := readFrame(r)
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				d.log.Warn("docker logs: read frame", "container", name, "error", err)
			}
			return
		}
		level := streamLevel(frame.stream)
		buf := append(pending[frame.stream], frame.data...)
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			if !send(ctx, out, parseDockerLine(buf[:i], name, level)) {
				return
			}
			buf = buf[i+1:]
		}
		pending[frame.stream] = append([]byte(nil), buf...)
	}
}

// logFrame is one demultiplexed Docker log frame.
type logFrame struct {
	stream byte // 1 = stdout, 2 = stderr
	data   []byte
}

// readFrame reads one Docker multiplexed frame: an 8-byte header (stream type
// in byte 0, big-endian payload size in bytes 4..8) followed by the payload.
func readFrame(r io.Reader) (logFrame, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return logFrame{}, fmt.Errorf("read frame header: %w", err)
	}
	size := binary.BigEndian.Uint32(hdr[4:8])
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return logFrame{}, fmt.Errorf("read frame payload: %w", err)
	}
	return logFrame{stream: hdr[0], data: data}, nil
}

func streamLevel(stream byte) string {
	if stream == 2 {
		return "stderr"
	}
	return "stdout"
}

// parseDockerLine turns one log line into an Entry. With Timestamps enabled
// Docker prefixes each line with an RFC3339Nano timestamp and a space.
func parseDockerLine(line []byte, origin, level string) Entry {
	s := strings.TrimRight(string(line), "\r\n")
	ts, msg := splitDockerTimestamp(s)
	return Entry{
		Timestamp: ts,
		Source:    SourceDocker,
		Origin:    origin,
		Level:     level,
		Message:   msg,
	}
}

func splitDockerTimestamp(s string) (time.Time, string) {
	i := strings.IndexByte(s, ' ')
	if i < 0 {
		return time.Time{}, s
	}
	t, err := time.Parse(time.RFC3339Nano, s[:i])
	if err != nil {
		return time.Time{}, s
	}
	return t, s[i+1:]
}

// send forwards e to out unless ctx is cancelled first; it reports whether the
// send succeeded so callers can stop on cancellation.
func send(ctx context.Context, out chan<- Entry, e Entry) bool {
	select {
	case out <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func trimSlash(name string) string {
	return strings.TrimPrefix(name, "/")
}
