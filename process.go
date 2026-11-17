package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	// maxProcesses bounds the processes a sandbox keeps, running or exited.
	// Exited ones are dropped oldest first to make room; past that, starting
	// another is 409.
	maxProcesses = 32

	// maxWait caps a long-poll on /output, so a read always comes back well
	// inside any proxy's idle timeout.
	maxWait = 30 * time.Second

	// maxInput caps one write to a process's stdin.
	maxInput = 1 << 20 // 1 MiB

	defaultCols, defaultRows = 80, 24
	maxTerminalSize          = 1000
)

// processes is the sandbox's set of background processes. Unlike /execute,
// these are not serialized: a dev server, a watcher and a terminal can all
// run while /execute is used for one-off commands.
type processes struct {
	mu    sync.Mutex
	next  int
	procs []*process
}

type process struct {
	id        string
	command   string
	pty       bool
	startedAt time.Time

	cmd      *exec.Cmd
	stdin    io.Writer // the stdin pipe, or the terminal for a pty
	terminal *os.File  // the controlling side of the pty, nil without one
	out      *output
	done     chan struct{}

	mu       sync.Mutex
	exitCode *int
	endedAt  *time.Time
	cols     int
	rows     int
}

type processInfo struct {
	ID        string     `json:"id"`
	Command   string     `json:"command"`
	PTY       bool       `json:"pty"`
	Running   bool       `json:"running"`
	ExitCode  *int       `json:"exitCode"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt"`
	Cols      int        `json:"cols,omitempty"`
	Rows      int        `json:"rows,omitempty"`
}

func (p *process) info() processInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	info := processInfo{
		ID:        p.id,
		Command:   p.command,
		PTY:       p.pty,
		Running:   p.exitCode == nil,
		ExitCode:  p.exitCode,
		StartedAt: p.startedAt,
		EndedAt:   p.endedAt,
	}
	if p.pty {
		info.Cols, info.Rows = p.cols, p.rows
	}
	return info
}

func (p *process) running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

type startRequest struct {
	Command string `json:"command"`
	PTY     bool   `json:"pty"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

func (s *server) startProcess(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	if req.Command == "" && !req.PTY {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}
	if req.Cols == 0 {
		req.Cols = defaultCols
	}
	if req.Rows == 0 {
		req.Rows = defaultRows
	}
	if !validSize(req.Cols, req.Rows) {
		writeError(w, http.StatusBadRequest, "cols and rows must be between 1 and 1000")
		return
	}

	id, ok := s.processes.reserve()
	if !ok {
		writeError(w, http.StatusConflict, "too many running processes")
		return
	}

	// The process outlives this request: keep its values, drop its cancel.
	p, err := s.spawn(context.WithoutCancel(r.Context()), id, req)
	if err != nil {
		s.processes.release(id)
		writeError(w, http.StatusInternalServerError, "start process: "+err.Error())
		return
	}
	s.processes.add(p)

	writeJSON(w, http.StatusCreated, p.info())
}

func (s *server) spawn(ctx context.Context, id string, req startRequest) (*process, error) {
	var cmd *exec.Cmd
	if req.Command == "" {
		// A bare terminal is an interactive shell: on a tty, with no -c, it
		// prompts and reads commands from the terminal.
		cmd = exec.CommandContext(ctx, s.shell)
	} else {
		cmd = exec.CommandContext(ctx, s.shell, "-c", req.Command)
	}
	cmd.Dir = s.workspace

	p := &process{
		id:        id,
		command:   req.Command,
		pty:       req.PTY,
		startedAt: time.Now().UTC(),
		cmd:       cmd,
		out:       newOutput(),
		done:      make(chan struct{}),
	}

	if req.PTY {
		return p, s.spawnTerminal(p, req.Cols, req.Rows)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	p.stdin = stdin
	// One writer for both streams: os/exec then copies them through a single
	// goroutine, and the output reads in the order it was written.
	cmd.Stdout = p.out
	cmd.Stderr = p.out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	go func() {
		p.exit(cmd.Wait())
		p.out.close()
	}()

	return p, nil
}

func (s *server) spawnTerminal(p *process, cols, rows int) error {
	terminal, tty, err := openPTY()
	if err != nil {
		return err
	}
	defer func() { _ = tty.Close() }() // the child holds its own copy

	if err := resizePTY(terminal, cols, rows); err != nil {
		_ = terminal.Close()
		return err
	}

	cmd := p.cmd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	if os.Getenv("TERM") == "" {
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	}
	// A new session whose controlling terminal is the pty (fd 0 in the
	// child), so job control, ^C and SIGWINCH reach the shell. The session
	// leader also leads its process group, which DELETE signals.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

	if err := cmd.Start(); err != nil {
		_ = terminal.Close()
		return err
	}

	p.terminal, p.stdin, p.cols, p.rows = terminal, terminal, cols, rows

	copied := make(chan struct{})
	go func() {
		// Reads end with EIO once every holder of the tty has closed it.
		_, _ = io.Copy(p.out, terminal)
		close(copied)
	}()

	go func() {
		err := cmd.Wait()
		// A child the shell left running can hold the tty open forever;
		// give trailing output a moment, then stop reading.
		select {
		case <-copied:
		case <-time.After(time.Second):
		}
		_ = terminal.Close()
		<-copied
		p.exit(err)
		p.out.close()
	}()

	return nil
}

// exit records how the process ended. A process killed by a signal reports
// 128 plus the signal number, the shell convention.
func (p *process) exit(err error) {
	code := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			code = 128 + int(status.Signal())
		}
	case err != nil:
		code = -1
	}

	ended := time.Now().UTC()
	p.mu.Lock()
	p.exitCode, p.endedAt = &code, &ended
	p.mu.Unlock()
	close(p.done)
}

func (s *server) listProcesses(w http.ResponseWriter, _ *http.Request) {
	procs := s.processes.all()
	infos := make([]processInfo, 0, len(procs))
	for _, p := range procs {
		infos = append(infos, p.info())
	}
	writeJSON(w, http.StatusOK, map[string]any{"processes": infos})
}

func (s *server) getProcess(w http.ResponseWriter, r *http.Request) {
	p := s.lookup(w, r)
	if p == nil {
		return
	}
	writeJSON(w, http.StatusOK, p.info())
}

type outputResponse struct {
	Data       []byte `json:"data"` // base64: terminal output need not be UTF-8
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	Running    bool   `json:"running"`
	ExitCode   *int   `json:"exitCode"`
}

// readOutput answers GET /processes/{id}/output.
//
// By default it is a long-poll: the output at or after ?offset as JSON,
// waiting up to ?wait seconds (max 30) for some if there is none yet. Read
// again from nextOffset to continue. An offset older than the retained 1 MiB
// tail comes back as a later offset than asked for.
//
// With ?follow=true it streams the raw bytes instead, from ?offset until the
// process exits or the client goes away.
func (s *server) readOutput(w http.ResponseWriter, r *http.Request) {
	p := s.lookup(w, r)
	if p == nil {
		return
	}

	query := r.URL.Query()
	offset, err := queryInt(query.Get("offset"))
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
		return
	}

	if query.Get("follow") == "true" {
		follow(w, r, p, offset)
		return
	}

	waitSeconds, err := queryInt(query.Get("wait"))
	if err != nil || waitSeconds < 0 {
		writeError(w, http.StatusBadRequest, "wait must be a non-negative integer")
		return
	}
	deadline := time.NewTimer(min(time.Duration(waitSeconds)*time.Second, maxWait))
	defer deadline.Stop()

	c := p.out.read(offset)
	for len(c.Data) == 0 && !c.Closed {
		select {
		case <-c.Changed:
			c = p.out.read(offset)
			continue
		case <-deadline.C:
		case <-r.Context().Done():
		}
		break
	}

	info := p.info()
	writeJSON(w, http.StatusOK, outputResponse{
		Data:       c.Data,
		Offset:     c.From,
		NextOffset: c.Next,
		Running:    info.Running && !c.Closed,
		ExitCode:   info.ExitCode,
	})
}

func follow(w http.ResponseWriter, r *http.Request, p *process, offset int64) {
	rc := http.NewResponseController(w)
	c := p.out.read(offset)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Output-Offset", strconv.FormatInt(c.From, 10))
	w.WriteHeader(http.StatusOK)

	for {
		if len(c.Data) > 0 {
			if _, err := w.Write(c.Data); err != nil {
				return
			}
		}
		_ = rc.Flush()
		if c.Closed {
			return
		}
		select {
		case <-c.Changed:
			c = p.out.read(c.Next)
		case <-r.Context().Done():
			return
		}
	}
}

// writeInput answers POST /processes/{id}/input: the raw body goes to the
// process's stdin, or to its terminal as typed keystrokes.
func (s *server) writeInput(w http.ResponseWriter, r *http.Request) {
	p := s.lookup(w, r)
	if p == nil {
		return
	}
	if !p.running() {
		writeError(w, http.StatusConflict, "process has exited")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInput))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "input is larger than 1 MiB")
		return
	}
	if _, err := p.stdin.Write(body); err != nil {
		writeError(w, http.StatusConflict, "process is not reading input: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type sizeRequest struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// resize answers PUT /processes/{id}/size for a terminal.
func (s *server) resize(w http.ResponseWriter, r *http.Request) {
	p := s.lookup(w, r)
	if p == nil {
		return
	}
	if !p.pty {
		writeError(w, http.StatusBadRequest, "process has no terminal")
		return
	}

	var req sizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	if !validSize(req.Cols, req.Rows) {
		writeError(w, http.StatusBadRequest, "cols and rows must be between 1 and 1000")
		return
	}
	if !p.running() {
		writeError(w, http.StatusConflict, "process has exited")
		return
	}
	if err := resizePTY(p.terminal, req.Cols, req.Rows); err != nil {
		writeError(w, http.StatusConflict, "resize terminal: "+err.Error())
		return
	}

	p.mu.Lock()
	p.cols, p.rows = req.Cols, req.Rows
	p.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// killProcess answers DELETE /processes/{id}: kill the process group if it
// is still running, wait for it, and forget it.
func (s *server) killProcess(w http.ResponseWriter, r *http.Request) {
	p := s.lookup(w, r)
	if p == nil {
		return
	}
	if p.running() {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			writeError(w, http.StatusInternalServerError, "process did not exit after SIGKILL")
			return
		}
	}
	s.processes.release(p.id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) lookup(w http.ResponseWriter, r *http.Request) *process {
	p := s.processes.get(r.PathValue("id"))
	if p == nil {
		writeError(w, http.StatusNotFound, "process not found")
	}
	return p
}

// reserve claims a slot and an id for a new process, dropping the oldest
// exited process if the sandbox is full.
func (ps *processes) reserve() (string, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if len(ps.procs) >= maxProcesses {
		i := slices.IndexFunc(ps.procs, func(p *process) bool { return p != nil && !p.running() })
		if i < 0 {
			return "", false
		}
		ps.procs = slices.Delete(ps.procs, i, i+1)
	}

	ps.next++
	id := strconv.Itoa(ps.next)
	// Hold the slot with a placeholder until the process has started. Its
	// done never closes, so it is never the one dropped to make room.
	ps.procs = append(ps.procs, &process{id: id, done: make(chan struct{})})
	return id, true
}

func (ps *processes) add(p *process) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for i, held := range ps.procs {
		if held.id == p.id {
			ps.procs[i] = p
			return
		}
	}
}

func (ps *processes) release(id string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.procs = slices.DeleteFunc(ps.procs, func(p *process) bool { return p.id == id })
}

func (ps *processes) get(id string) *process {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, p := range ps.procs {
		if p.id == id && p.cmd != nil {
			return p
		}
	}
	return nil
}

func (ps *processes) all() []*process {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(ps.procs), func(p *process) bool { return p.cmd == nil })
}

func validSize(cols, rows int) bool {
	return cols >= 1 && cols <= maxTerminalSize && rows >= 1 && rows <= maxTerminalSize
}

func queryInt(v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	return strconv.ParseInt(v, 10, 64)
}
