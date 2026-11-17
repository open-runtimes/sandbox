package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"
)

// streamEvent is one line of a streamed /execute: output as it arrives, then
// exactly one exit event.
type streamEvent struct {
	Type           string `json:"type"` // stdout, stderr or exit
	Data           string `json:"data,omitempty"`
	ExitCode       *int   `json:"exitCode,omitempty"`
	DurationMillis *int64 `json:"durationMillis,omitempty"`
}

// stream runs command and writes NDJSON events as its output arrives. The
// status is committed before the command starts, so a failure to start it is
// reported as stderr followed by an exit event, never as an HTTP error.
func (s *server) stream(ctx context.Context, w http.ResponseWriter, command string, timeout time.Duration) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	out := &events{w: w, rc: http.NewResponseController(w)}
	out.flush()

	stdout := &eventWriter{events: out, kind: "stdout"}
	stderr := &eventWriter{events: out, kind: "stderr"}

	res := s.run(ctx, command, timeout, stdout, stderr)
	stdout.drain()
	stderr.drain()
	if res.Stderr != "" {
		out.send(streamEvent{Type: "stderr", Data: res.Stderr})
	}
	out.send(streamEvent{Type: "exit", ExitCode: &res.ExitCode, DurationMillis: &res.DurationMillis})
}

// events serializes NDJSON lines from both output streams onto one response.
type events struct {
	mu sync.Mutex
	w  io.Writer
	rc *http.ResponseController
}

func (e *events) send(ev streamEvent) {
	line, _ := json.Marshal(ev)
	line = append(line, '\n')

	e.mu.Lock()
	defer e.mu.Unlock()
	// A gone client is not the command's problem: it keeps running until it
	// exits or its context (the request's) is cancelled.
	_, _ = e.w.Write(line)
	e.flush()
}

func (e *events) flush() {
	_ = e.rc.Flush()
}

// eventWriter turns one output stream into events. JSON strings must be valid
// UTF-8, so a multi-byte character split across two writes is held back until
// the rest of it arrives rather than mangled.
type eventWriter struct {
	events  *events
	kind    string
	pending []byte
}

func (e *eventWriter) Write(p []byte) (int, error) {
	n := len(p)
	buf := append(e.pending, p...)

	cut := len(buf)
	// Look back at most UTFMax-1 bytes for the start of an incomplete rune.
	for i := len(buf) - 1; i >= 0 && i >= len(buf)-(utf8.UTFMax-1); i-- {
		if utf8.RuneStart(buf[i]) {
			if !utf8.FullRune(buf[i:]) {
				cut = i
			}
			break
		}
	}

	e.pending = append([]byte(nil), buf[cut:]...)
	if cut > 0 {
		e.events.send(streamEvent{Type: e.kind, Data: string(buf[:cut])})
	}
	return n, nil
}

// drain sends whatever was held back once the stream has ended.
func (e *eventWriter) drain() {
	if len(e.pending) > 0 {
		e.events.send(streamEvent{Type: e.kind, Data: string(e.pending)})
		e.pending = nil
	}
}
