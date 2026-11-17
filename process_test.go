package main

import (
	"context"
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func request(t *testing.T, srv *httptest.Server, method, target, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+target, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// kill removes a process from a cleanup, where the test's context is gone.
func kill(t *testing.T, srv *httptest.Server, id string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, srv.URL+"/processes/"+id, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE /processes/%s: %v", id, err)
	}
	_ = res.Body.Close()
}

func decode[T any](t *testing.T, res *http.Response) T {
	t.Helper()
	var out T
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func start(t *testing.T, srv *httptest.Server, body string) processInfo {
	t.Helper()
	res := request(t, srv, http.MethodPost, "/processes", body)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST /processes status = %d, want 201", res.StatusCode)
	}
	return decode[processInfo](t, res)
}

// readUntil long-polls a process's output until it contains want or the
// process has exited, and returns everything read.
func readUntil(t *testing.T, srv *httptest.Server, id, want string) (string, outputResponse) {
	t.Helper()
	var all strings.Builder
	var offset int64
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		res := request(t, srv, http.MethodGet, "/processes/"+id+"/output?wait=2&offset="+strconv.FormatInt(offset, 10), "")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET output status = %d, want 200", res.StatusCode)
		}
		out := decode[outputResponse](t, res)
		all.Write(out.Data)
		offset = out.NextOffset
		if strings.Contains(all.String(), want) || !out.Running {
			return all.String(), out
		}
	}
	t.Fatalf("timed out waiting for %q, got %q", want, all.String())
	return "", outputResponse{}
}

func TestProcesses(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	t.Run("runs in the background and reports its exit", func(t *testing.T) {
		p := start(t, srv, `{"command": "echo out; echo err >&2; exit 7"}`)
		if !p.Running && p.ExitCode == nil {
			t.Fatalf("process = %+v, want running or exited", p)
		}

		all, out := readUntil(t, srv, p.ID, "\x00never")
		if all != "out\nerr\n" {
			t.Fatalf("output = %q, want stdout and stderr interleaved", all)
		}
		if out.Running || out.ExitCode == nil || *out.ExitCode != 7 {
			t.Fatalf("output = %+v, want exited with 7", out)
		}

		got := decode[processInfo](t, request(t, srv, http.MethodGet, "/processes/"+p.ID, ""))
		if got.Running || got.ExitCode == nil || *got.ExitCode != 7 || got.EndedAt == nil {
			t.Fatalf("process = %+v, want exited with 7", got)
		}
	})

	t.Run("does not take the execute seat", func(t *testing.T) {
		p := start(t, srv, `{"command": "sleep 30"}`)
		t.Cleanup(func() { kill(t, srv, p.ID) })

		if status, out := post(t, srv, `{"command": "echo free"}`); status != http.StatusOK || out.Stdout != "free\n" {
			t.Fatalf("execute alongside a process: status = %d, stdout = %q", status, out.Stdout)
		}
	})

	t.Run("feeds stdin", func(t *testing.T) {
		p := start(t, srv, `{"command": "read line; echo got:$line"}`)
		if res := request(t, srv, http.MethodPost, "/processes/"+p.ID+"/input", "hello\n"); res.StatusCode != http.StatusNoContent {
			t.Fatalf("POST input status = %d, want 204", res.StatusCode)
		}
		if all, _ := readUntil(t, srv, p.ID, "got:hello"); !strings.Contains(all, "got:hello") {
			t.Fatalf("output = %q, want got:hello", all)
		}
	})

	t.Run("kills the process tree and forgets it", func(t *testing.T) {
		p := start(t, srv, `{"command": "sleep 30 & sleep 30"}`)
		if res := request(t, srv, http.MethodDelete, "/processes/"+p.ID, ""); res.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE status = %d, want 204", res.StatusCode)
		}
		if res := request(t, srv, http.MethodGet, "/processes/"+p.ID, ""); res.StatusCode != http.StatusNotFound {
			t.Fatalf("GET after DELETE status = %d, want 404", res.StatusCode)
		}
	})

	t.Run("lists processes", func(t *testing.T) {
		p := start(t, srv, `{"command": "true"}`)
		list := decode[struct{ Processes []processInfo }](t, request(t, srv, http.MethodGet, "/processes", ""))
		for _, got := range list.Processes {
			if got.ID == p.ID {
				return
			}
		}
		t.Fatalf("list = %+v, want it to include %s", list.Processes, p.ID)
	})

	t.Run("follows output as a raw stream", func(t *testing.T) {
		p := start(t, srv, `{"command": "echo one; sleep 0.2; echo two"}`)
		res := request(t, srv, http.MethodGet, "/processes/"+p.ID+"/output?follow=true", "")
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		if string(body) != "one\ntwo\n" {
			t.Fatalf("stream = %q, want both lines", body)
		}
	})

	t.Run("rejects bad requests", func(t *testing.T) {
		cases := []struct {
			method, target, body string
			want                 int
		}{
			{http.MethodPost, "/processes", `{}`, http.StatusBadRequest},
			{http.MethodPost, "/processes", `{"pty": true, "cols": 5000}`, http.StatusBadRequest},
			{http.MethodGet, "/processes/nope", "", http.StatusNotFound},
			{http.MethodGet, "/processes/nope/output", "", http.StatusNotFound},
			{http.MethodDelete, "/processes/nope", "", http.StatusNotFound},
		}
		for _, c := range cases {
			if res := request(t, srv, c.method, c.target, c.body); res.StatusCode != c.want {
				t.Errorf("%s %s status = %d, want %d", c.method, c.target, res.StatusCode, c.want)
			}
		}

		p := start(t, srv, `{"command": "true"}`)
		readUntil(t, srv, p.ID, "\x00never")
		if res := request(t, srv, http.MethodPost, "/processes/"+p.ID+"/input", "x"); res.StatusCode != http.StatusConflict {
			t.Errorf("input to an exited process status = %d, want 409", res.StatusCode)
		}
		if res := request(t, srv, http.MethodPut, "/processes/"+p.ID+"/size", `{"cols": 100, "rows": 40}`); res.StatusCode != http.StatusBadRequest {
			t.Errorf("resize without a terminal status = %d, want 400", res.StatusCode)
		}
	})
}

func TestTerminal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("terminals are only supported on linux")
	}
	t.Parallel()
	srv := newTestServer(t)

	p := start(t, srv, `{"pty": true, "cols": 120, "rows": 40}`)
	t.Cleanup(func() { kill(t, srv, p.ID) })
	if !p.PTY || p.Cols != 120 || p.Rows != 40 {
		t.Fatalf("process = %+v, want a 120x40 terminal", p)
	}

	// The shell sees a real tty of the requested size.
	request(t, srv, http.MethodPost, "/processes/"+p.ID+"/input", "test -t 0 && echo IS_TTY; stty size\n")
	if all, _ := readUntil(t, srv, p.ID, "40 120"); !strings.Contains(all, "IS_TTY") {
		t.Fatalf("output = %q, want IS_TTY and 40 120", all)
	}

	if res := request(t, srv, http.MethodPut, "/processes/"+p.ID+"/size", `{"cols": 100, "rows": 30}`); res.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT size status = %d, want 204", res.StatusCode)
	}
	request(t, srv, http.MethodPost, "/processes/"+p.ID+"/input", "stty size\n")
	readUntil(t, srv, p.ID, "30 100")

	request(t, srv, http.MethodPost, "/processes/"+p.ID+"/input", "exit 3\n")
	_, out := readUntil(t, srv, p.ID, "\x00never")
	if out.Running || out.ExitCode == nil || *out.ExitCode != 3 {
		t.Fatalf("output = %+v, want exited with 3", out)
	}
}

func TestExecuteStream(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	res := request(t, srv, http.MethodPost, "/execute", `{"command": "echo a; echo b >&2; printf '\\303\\251'; exit 2", "stream": true}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content type = %q, want application/x-ndjson", ct)
	}

	var stdout, stderr strings.Builder
	var exit *streamEvent
	scanner := bufio.NewScanner(res.Body)
	for scanner.Scan() {
		var ev streamEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatalf("decode %q: %v", scanner.Text(), err)
		}
		switch ev.Type {
		case "stdout":
			stdout.WriteString(ev.Data)
		case "stderr":
			stderr.WriteString(ev.Data)
		case "exit":
			exit = &ev
		default:
			t.Fatalf("unexpected event %+v", ev)
		}
	}

	if stdout.String() != "a\né" || stderr.String() != "b\n" {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	if exit == nil || exit.ExitCode == nil || *exit.ExitCode != 2 || exit.DurationMillis == nil {
		t.Fatalf("exit = %+v, want exit code 2 with a duration", exit)
	}
}

func TestEventWriterHoldsSplitRunes(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	w := &eventWriter{events: &events{w: rec, rc: http.NewResponseController(rec)}, kind: "stdout"}

	e := []byte("é") // two bytes
	_, _ = w.Write(append([]byte("x"), e[0]))
	_, _ = w.Write(e[1:])
	w.drain()

	var got strings.Builder
	for line := range strings.Lines(rec.Body.String()) {
		var ev streamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		got.WriteString(ev.Data)
	}
	if got.String() != "xé" {
		t.Fatalf("data = %q, want %q", got.String(), "xé")
	}
}

func TestOutputKeepsTheTail(t *testing.T) {
	t.Parallel()
	o := newOutput()
	_, _ = o.Write([]byte(strings.Repeat("a", maxOutput)))
	_, _ = o.Write([]byte("bc"))

	c := o.read(0)
	if c.From != 2 || c.Next != maxOutput+2 || len(c.Data) != maxOutput {
		t.Fatalf("read(0) = from %d, next %d, %d bytes; want from 2, next %d, %d bytes", c.From, c.Next, len(c.Data), maxOutput+2, maxOutput)
	}
	if c := o.read(maxOutput + 1); string(c.Data) != "c" {
		t.Fatalf("read at the end = %q, want %q", c.Data, "c")
	}
	if c := o.read(maxOutput + 10); len(c.Data) != 0 || c.From != maxOutput+2 {
		t.Fatalf("read past the end = %d bytes from %d, want none from %d", len(c.Data), c.From, maxOutput+2)
	}
}
