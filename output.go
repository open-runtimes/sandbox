package main

import "sync"

// maxOutput is how much of a process's output is retained. Readers address
// output by absolute byte offset, so a reader that falls further behind than
// this resumes at the oldest retained byte and sees the gap in the offsets.
const maxOutput = 1 << 20 // 1 MiB

// output is the retained tail of a background process's output, readable by
// offset while the process keeps writing.
type output struct {
	mu     sync.Mutex
	buf    []byte
	start  int64 // absolute offset of buf[0]
	closed bool
	// changed is closed and replaced on every write and on close, so a
	// waiting reader wakes without polling.
	changed chan struct{}
}

func newOutput() *output {
	return &output{changed: make(chan struct{})}
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.buf = append(o.buf, p...)
	if over := len(o.buf) - maxOutput; over > 0 {
		o.buf = append([]byte(nil), o.buf[over:]...)
		o.start += int64(over)
	}
	o.notify()
	return len(p), nil
}

// close marks the end of output: readers that have caught up stop waiting.
func (o *output) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	o.notify()
}

func (o *output) notify() {
	close(o.changed)
	o.changed = make(chan struct{})
}

// chunk is what a read at an offset returns: the bytes from From up to Next,
// whether the output has ended, and a channel closed on the next change.
type chunk struct {
	Data    []byte
	From    int64
	Next    int64
	Closed  bool
	Changed <-chan struct{}
}

// read returns the retained bytes at or after offset. An offset older than
// the retained tail starts at the tail; one past the end returns nothing.
func (o *output) read(offset int64) chunk {
	o.mu.Lock()
	defer o.mu.Unlock()

	end := o.start + int64(len(o.buf))
	from := min(max(offset, o.start), end)

	return chunk{
		Data:    append([]byte(nil), o.buf[from-o.start:]...),
		From:    from,
		Next:    end,
		Closed:  o.closed,
		Changed: o.changed,
	}
}
