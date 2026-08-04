package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"syscall"
	"time"
)

const (
	// maxStream caps each of stdout and stderr. Past it the response reports
	// truncated: write to a file and read it back through /files instead.
	maxStream = 1 << 20 // 1 MiB

	defaultTimeout = 60 * time.Second

	// timeoutExitCode mirrors the shell convention for a command killed by
	// timeout(1), since the process has no exit status of its own.
	timeoutExitCode = 124
)

type executeRequest struct {
	Command        string `json:"command"`
	TimeoutSeconds *int   `json:"timeoutSeconds"`
}

type executeResponse struct {
	ExitCode       int    `json:"exitCode"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	DurationMillis int64  `json:"durationMillis"`
	Truncated      bool   `json:"truncated"`
}

func (s *server) execute(w http.ResponseWriter, r *http.Request) {
	var req executeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	if req.Command == "" {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}

	timeout := defaultTimeout
	if req.TimeoutSeconds != nil {
		switch {
		case *req.TimeoutSeconds < 0:
			writeError(w, http.StatusBadRequest, "timeoutSeconds must not be negative")
			return
		case *req.TimeoutSeconds == 0:
			timeout = 0 // run until it exits or the client disconnects
		default:
			timeout = time.Duration(*req.TimeoutSeconds) * time.Second
		}
	}

	select {
	case s.seat <- struct{}{}:
		defer func() { <-s.seat }()
	default:
		writeError(w, http.StatusConflict, "sandbox is busy: one command at a time")
		return
	}

	writeJSON(w, http.StatusOK, s.run(r.Context(), req.Command, timeout))
}

func (s *server) run(ctx context.Context, command string, timeout time.Duration) executeResponse {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var stdout, stderr capped
	stdout.limit, stderr.limit = maxStream, maxStream

	cmd := exec.CommandContext(ctx, s.shell, "-c", command)
	cmd.Dir = s.workspace
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Own a process group so a timeout takes the whole tree with it, not just
	// the shell, and give in-flight output a moment to drain after the kill.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second

	started := time.Now()
	err := cmd.Run()
	elapsed := time.Since(started)

	res := executeResponse{
		Stdout:         stdout.buf.String(),
		Stderr:         stderr.buf.String(),
		DurationMillis: elapsed.Milliseconds(),
		Truncated:      stdout.truncated || stderr.truncated,
	}

	var exitErr *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.ExitCode = timeoutExitCode
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case err != nil:
		res.ExitCode = -1
		res.Stderr += err.Error() + "\n"
	}

	return res
}

// capped collects up to limit bytes and records whether more were discarded.
type capped struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.limit - c.buf.Len(); room > 0 {
		if n > room {
			p, c.truncated = p[:room], true
		}
		c.buf.Write(p)
	} else if n > 0 {
		c.truncated = true
	}
	// Report every byte consumed: a full buffer truncates, it does not fail
	// the command with a short write.
	return n, nil
}
