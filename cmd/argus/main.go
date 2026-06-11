// Command argus is the unprivileged terminal bridge. It contains no logic
// beyond gRPC plumbing and rendering: it attaches to the headless TUI hosted
// by argusd, forwards stdin bytes and terminal resizes upstream and writes
// received output bytes to the terminal.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	argusv1 "github.com/jakobneri/argus/proto/argusv1"
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) {
		return errors.New("stdin is not a terminal")
	}

	stream, err := argusv1.NewArgusClient(conn).Attach(ctx)
	if err != nil {
		return fmt.Errorf("attach to argusd at %s: %w", socketPath, err)
	}
	// gRPC streams forbid concurrent SendMsg; input and resize events are
	// sent from different goroutines, so serialize them.
	up := &sender{stream: stream}

	// Raw mode so every key byte reaches the daemon-side TUI unmangled.
	// Restored on every exit path below.
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return fmt.Errorf("set terminal raw mode: %w", err)
	}
	restore := func() {
		_ = term.Restore(stdinFd, oldState)
		// Defensive reset in case the stream died mid-session before the
		// daemon-side program could leave the alternate screen: exit alt
		// screen, show cursor.
		_, _ = fmt.Fprint(os.Stdout, "\x1b[?1049l\x1b[?25h")
	}
	defer restore()

	if err := sendSize(up, stdinFd); err != nil {
		return err
	}

	// Resize events: the daemon renders to a stream, not a TTY, so SIGWINCH
	// must be forwarded explicitly.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			_ = sendSize(up, stdinFd)
		}
	}()

	// SIGINT/SIGTERM tear the bridge down. In raw mode ctrl+c arrives as a
	// byte (0x03) and is forwarded to the TUI instead of raising SIGINT.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		<-sigs
		cancel()
	}()

	// Upstream: stdin bytes as input events.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				ev := &argusv1.AttachRequest{
					Event: &argusv1.AttachRequest_Input{Input: append([]byte(nil), buf[:n]...)},
				}
				if err := up.send(ev); err != nil {
					return
				}
			}
			if err != nil {
				up.closeSend()
				return
			}
		}
	}()

	// Downstream: rendered frames to stdout until the session ends.
	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil // clean end: program quit ("q") or local signal
			}
			return fmt.Errorf("attach stream: %w", err)
		}
		if _, err := os.Stdout.Write(resp.GetOutput()); err != nil {
			return fmt.Errorf("write to stdout: %w", err)
		}
	}
}

// sender serializes Send/CloseSend calls on the attach stream.
type sender struct {
	mu     sync.Mutex
	stream argusv1.Argus_AttachClient
}

func (s *sender) send(req *argusv1.AttachRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.stream.Send(req); err != nil {
		return fmt.Errorf("send attach event: %w", err)
	}
	return nil
}

func (s *sender) closeSend() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.stream.CloseSend()
}

// sendSize sends the current terminal size as a resize event.
func sendSize(up *sender, fd int) error {
	cols, rows, err := term.GetSize(fd)
	if err != nil {
		return fmt.Errorf("get terminal size: %w", err)
	}
	return up.send(&argusv1.AttachRequest{
		Event: &argusv1.AttachRequest_Resize{
			//nolint:gosec // terminal dimensions are small positive ints
			Resize: &argusv1.Resize{Cols: uint32(cols), Rows: uint32(rows)},
		},
	})
}

func socketDefault() string {
	if v := os.Getenv("ARGUS_SOCKET"); v != "" {
		return v
	}
	return defaultSocket
}
