package logs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestParseJournalLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want Entry
		ok   bool
	}{
		{
			name: "string message with unit",
			line: `{"__REALTIME_TIMESTAMP":"1700000000123456","PRIORITY":"6","_SYSTEMD_UNIT":"ssh.service","SYSLOG_IDENTIFIER":"sshd","MESSAGE":"Accepted publickey for root"}`,
			want: Entry{
				Timestamp: time.UnixMicro(1700000000123456),
				Source:    SourceSystemd,
				Origin:    "ssh.service",
				Level:     "info",
				Message:   "Accepted publickey for root",
			},
			ok: true,
		},
		{
			name: "array message falls back to syslog identifier",
			line: `{"__REALTIME_TIMESTAMP":"1700000000000000","PRIORITY":"3","SYSLOG_IDENTIFIER":"kernel","MESSAGE":[72,105]}`,
			want: Entry{
				Timestamp: time.UnixMicro(1700000000000000),
				Source:    SourceSystemd,
				Origin:    "kernel",
				Level:     "err",
				Message:   "Hi",
			},
			ok: true,
		},
		{
			name: "missing priority defaults to info, missing timestamp is zero",
			line: `{"_SYSTEMD_UNIT":"cron.service","MESSAGE":"tick"}`,
			want: Entry{
				Timestamp: time.Time{},
				Source:    SourceSystemd,
				Origin:    "cron.service",
				Level:     "info",
				Message:   "tick",
			},
			ok: true,
		},
		{
			name: "malformed json is skipped",
			line: `not json`,
			ok:   false,
		},
		{
			name: "blank line is skipped",
			line: ``,
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseJournalLine([]byte(tt.line))
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseJournalLine = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPriorityName(t *testing.T) {
	cases := map[string]string{
		"0": "emerg", "3": "err", "6": "info", "7": "debug",
		"": "info", "9": "info", "x": "info",
	}
	for in, want := range cases {
		if got := priorityName(in); got != want {
			t.Errorf("priorityName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJournalArgs(t *testing.T) {
	got := journalArgs(Request{Filter: "ssh.service", Since: "-1h", Priority: "err"})
	want := []string{"--output=json", "--follow", "--no-pager", "--unit", "ssh.service", "--since", "-1h", "--priority", "err"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("journalArgs = %v, want %v", got, want)
	}
	// No filters -> just the base flags.
	if got := journalArgs(Request{}); !reflect.DeepEqual(got, []string{"--output=json", "--follow", "--no-pager"}) {
		t.Errorf("journalArgs (empty) = %v", got)
	}
}

func TestSplitDockerTimestamp(t *testing.T) {
	ts, msg := splitDockerTimestamp("2026-06-15T10:11:12.5Z hello world")
	if !ts.Equal(time.Date(2026, 6, 15, 10, 11, 12, 500000000, time.UTC)) {
		t.Errorf("timestamp = %v", ts)
	}
	if msg != "hello world" {
		t.Errorf("message = %q", msg)
	}
	// A line without a parseable timestamp is kept whole.
	if ts, msg := splitDockerTimestamp("plain line"); !ts.IsZero() || msg != "plain line" {
		t.Errorf("no-timestamp = %v / %q", ts, msg)
	}
}

func muxFrame(stream byte, payload string) []byte {
	frame := make([]byte, 8)
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[4:], uint32(len(payload)))
	return append(frame, payload...)
}

func TestScanMultiplexed(t *testing.T) {
	var stream []byte
	stream = append(stream, muxFrame(1, "2026-06-15T10:00:00Z out line\n")...)
	stream = append(stream, muxFrame(2, "2026-06-15T10:00:01Z err line\n")...)
	// A line split across two stdout frames must be reassembled.
	stream = append(stream, muxFrame(1, "2026-06-15T10:00:02Z split ")...)
	stream = append(stream, muxFrame(1, "tail\n")...)

	d := &dockerStreamer{log: discardLog()}
	out := make(chan Entry, 16)
	go func() {
		d.scanMultiplexed(context.Background(), bytes.NewReader(stream), "web", out)
		close(out)
	}()

	var got []Entry
	for e := range out {
		got = append(got, e)
	}
	want := []Entry{
		{Timestamp: time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC), Source: SourceDocker, Origin: "web", Level: "stdout", Message: "out line"},
		{Timestamp: time.Date(2026, 6, 15, 10, 0, 1, 0, time.UTC), Source: SourceDocker, Origin: "web", Level: "stderr", Message: "err line"},
		{Timestamp: time.Date(2026, 6, 15, 10, 0, 2, 0, time.UTC), Source: SourceDocker, Origin: "web", Level: "stdout", Message: "split tail"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scanMultiplexed entries =\n%+v\nwant\n%+v", got, want)
	}
}

// fakeStreamer is a sourceStreamer that emits canned entries and records that
// it ran.
type fakeStreamer struct {
	entries []Entry
	block   bool
	ran     bool
}

func (f *fakeStreamer) stream(ctx context.Context, _ Request, out chan<- Entry) {
	f.ran = true
	for _, e := range f.entries {
		select {
		case out <- e:
		case <-ctx.Done():
			return
		}
	}
	if f.block {
		<-ctx.Done()
	}
}

func collect(m *manager, req Request) ([]Entry, error) {
	var got []Entry
	err := m.Stream(context.Background(), req, func(e Entry) error {
		got = append(got, e)
		return nil
	})
	return got, err
}

func TestManagerMergesSources(t *testing.T) {
	sys := &fakeStreamer{entries: []Entry{{Origin: "ssh.service", Source: SourceSystemd}}}
	dock := &fakeStreamer{entries: []Entry{{Origin: "web", Source: SourceDocker}}}
	m := &manager{log: discardLog(), systemd: sys, docker: dock}

	got, err := collect(m, Request{Source: SourceAll})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2 (merged)", len(got))
	}
	if !sys.ran || !dock.ran {
		t.Errorf("both sources should run for SourceAll: sys=%v dock=%v", sys.ran, dock.ran)
	}
}

func TestManagerSourceSelection(t *testing.T) {
	tests := []struct {
		name      string
		source    Source
		wantSys   bool
		wantDock  bool
		wantCount int
	}{
		{"systemd only", SourceSystemd, true, false, 1},
		{"docker only", SourceDocker, false, true, 1},
		{"all", SourceAll, true, true, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sys := &fakeStreamer{entries: []Entry{{Source: SourceSystemd}}}
			dock := &fakeStreamer{entries: []Entry{{Source: SourceDocker}}}
			m := &manager{log: discardLog(), systemd: sys, docker: dock}

			got, err := collect(m, Request{Source: tt.source})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if len(got) != tt.wantCount {
				t.Errorf("entries = %d, want %d", len(got), tt.wantCount)
			}
			if sys.ran != tt.wantSys || dock.ran != tt.wantDock {
				t.Errorf("ran sys=%v dock=%v, want sys=%v dock=%v", sys.ran, dock.ran, tt.wantSys, tt.wantDock)
			}
		})
	}
}

func TestManagerStopsOnContextCancel(t *testing.T) {
	sys := &fakeStreamer{block: true}
	m := &manager{log: discardLog(), systemd: sys, docker: &fakeStreamer{block: true}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- m.Stream(ctx, Request{Source: SourceAll}, func(Entry) error { return nil })
	}()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Stream error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stream did not return after context cancel (goroutine leak)")
	}
}

func TestManagerStopsOnEmitError(t *testing.T) {
	sys := &fakeStreamer{entries: []Entry{{Origin: "a"}, {Origin: "b"}}, block: true}
	m := &manager{log: discardLog(), systemd: sys, docker: &fakeStreamer{block: true}}

	emitErr := errors.New("client gone")
	err := m.Stream(context.Background(), Request{Source: SourceSystemd}, func(Entry) error {
		return emitErr
	})
	if !errors.Is(err, emitErr) {
		t.Errorf("Stream error = %v, want %v", err, emitErr)
	}
}
