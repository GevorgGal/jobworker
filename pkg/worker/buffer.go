package worker

import (
	"errors"
	"sync"
)

// ErrBufferClosed is returned when Write is called on a buffer that has been
// marked done.
var ErrBufferClosed = errors.New("buffer is closed")

// OutputBuffer is an append-only, concurrency-safe byte buffer that captures
// process output (combined stdout/stderr). It uses a close-and-replace channel
// pattern to notify multiple concurrent readers without polling.
type OutputBuffer struct {
	mu       sync.Mutex
	data     []byte
	done     bool
	notifyCh chan struct{}
}

// NewOutputBuffer creates a new OutputBuffer ready for writing and reading.
func NewOutputBuffer() *OutputBuffer {
	return &OutputBuffer{
		notifyCh: make(chan struct{}),
	}
}

// Write appends p to the buffer and wakes all blocked readers.
// Implements io.Writer. Returns ErrBufferClosed after MarkDone.
func (b *OutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.done {
		return 0, ErrBufferClosed
	}

	b.data = append(b.data, p...)

	// Close-and-replace: wake all waiting readers, prepare for next wait.
	close(b.notifyCh)
	b.notifyCh = make(chan struct{})

	return len(p), nil
}

// ReadFrom returns buffered data from offset, completion status, and a
// notification channel. The caller must provide a non-negative offset;
// negative values are clamped to 0.
func (b *OutputBuffer) ReadFrom(offset int) (data []byte, done bool, notify <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if offset < 0 {
		offset = 0
	}

	if offset < len(b.data) {
		data = make([]byte, len(b.data)-offset)
		copy(data, b.data[offset:])
	}

	return data, b.done, b.notifyCh
}

// MarkDone signals that no more data will be written and wakes all waiting
// readers. Idempotent.
func (b *OutputBuffer) MarkDone() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.done {
		return
	}

	b.done = true
	close(b.notifyCh)
}

// Len returns the current size of the buffered data in bytes.
func (b *OutputBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.data)
}
