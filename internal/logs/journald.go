package logs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"time"
)

// journaldBinary is the journalctl executable. Streaming the journal as JSON
// and following it (no CGO, no libsystemd binding) is the M2 decision for
// reading the journal; see README "Assumptions / decisions".
const journaldBinary = "journalctl"

// maxJournalLine bounds a single journal line; structured messages can be long.
const maxJournalLine = 1024 * 1024

// journaldStreamer streams the systemd journal by execing journalctl.
type journaldStreamer struct {
	log *slog.Logger
	// binary overrides the journalctl path in tests; empty means journaldBinary.
	binary string
}

func (j *journaldStreamer) stream(ctx context.Context, req Request, out chan<- Entry) {
	bin := j.binary
	if bin == "" {
		bin = journaldBinary
	}

	// exec.CommandContext sends SIGKILL when ctx is cancelled, so the follow
	// process is torn down on client disconnect.
	cmd := exec.CommandContext(ctx, bin, journalArgs(req)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		j.log.Warn("journald: stdout pipe", "error", err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		j.log.Warn("journald: stderr pipe", "error", err)
		return
	}
	if err := cmd.Start(); err != nil {
		// journalctl missing or not permitted: non-fatal, the Docker source
		// (if any) keeps streaming.
		j.log.Warn("journald: start journalctl", "error", err)
		return
	}

	// Drain stderr and log it, but never treat it as fatal.
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			j.log.Warn("journalctl stderr", "line", sc.Text())
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), maxJournalLine)
	for scanner.Scan() {
		entry, ok := parseJournalLine(scanner.Bytes())
		if !ok {
			continue
		}
		select {
		case out <- entry:
		case <-ctx.Done():
			_ = cmd.Wait()
			return
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) {
		j.log.Warn("journald: read", "error", err)
	}
	_ = cmd.Wait()
}

// journalArgs builds the journalctl invocation from the request filters.
func journalArgs(req Request) []string {
	args := []string{"--output=json", "--follow", "--no-pager"}
	if req.Filter != "" {
		args = append(args, "--unit", req.Filter)
	}
	if req.Since != "" {
		args = append(args, "--since", req.Since)
	}
	if req.Priority != "" {
		args = append(args, "--priority", req.Priority)
	}
	return args
}

// journalRecord is the subset of the journalctl JSON object we care about.
type journalRecord struct {
	Realtime string          `json:"__REALTIME_TIMESTAMP"`
	Message  json.RawMessage `json:"MESSAGE"`
	Priority string          `json:"PRIORITY"`
	Unit     string          `json:"_SYSTEMD_UNIT"`
	SyslogID string          `json:"SYSLOG_IDENTIFIER"`
}

// parseJournalLine parses one `journalctl -o json` line. It returns false for
// blank or malformed lines so the caller can skip them.
func parseJournalLine(line []byte) (Entry, bool) {
	var rec journalRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return Entry{}, false
	}
	origin := rec.Unit
	if origin == "" {
		origin = rec.SyslogID
	}
	return Entry{
		Timestamp: parseRealtime(rec.Realtime),
		Source:    SourceSystemd,
		Origin:    origin,
		Level:     priorityName(rec.Priority),
		Message:   decodeJournalMessage(rec.Message),
	}, true
}

// decodeJournalMessage handles both the common string form and the array-of-
// bytes form journald uses for non-UTF-8 messages.
func decodeJournalMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var nums []int
	if err := json.Unmarshal(raw, &nums); err == nil {
		b := make([]byte, len(nums))
		for i, n := range nums {
			b[i] = byte(n)
		}
		return string(b)
	}
	return string(raw)
}

// parseRealtime converts journald's __REALTIME_TIMESTAMP (microseconds since
// the Unix epoch, as a decimal string) to a time.Time.
func parseRealtime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	us, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMicro(us)
}

// priorityNames maps syslog priority numbers (0..7) to names.
var priorityNames = []string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

func priorityName(p string) string {
	n, err := strconv.Atoi(p)
	if err != nil || n < 0 || n >= len(priorityNames) {
		return "info"
	}
	return priorityNames[n]
}
