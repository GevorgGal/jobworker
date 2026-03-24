package worker

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Low-level buffer tests — exercise readFrom directly to verify the
// internal notification and data mechanics.
// ---------------------------------------------------------------------------

func TestOutputBuffer_WriteAndRead(t *testing.T) {
	tests := []struct {
		name   string
		writes []string
		offset int
		want   string
	}{
		{
			name:   "single write read from start",
			writes: []string{"hello"},
			offset: 0,
			want:   "hello",
		},
		{
			name:   "multiple writes accumulate",
			writes: []string{"hello", " ", "world"},
			offset: 0,
			want:   "hello world",
		},
		{
			name:   "read from middle offset",
			writes: []string{"hello world"},
			offset: 6,
			want:   "world",
		},
		{
			name:   "read at end returns nil",
			writes: []string{"hello"},
			offset: 5,
			want:   "",
		},
		{
			name:   "read beyond end returns nil",
			writes: []string{"hello"},
			offset: 100,
			want:   "",
		},
		{
			name:   "read from empty buffer",
			writes: nil,
			offset: 0,
			want:   "",
		},
		{
			name:   "negative offset clamps to zero",
			writes: []string{"hello"},
			offset: -1,
			want:   "hello",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := NewOutputBuffer()
			for _, w := range tt.writes {
				n, err := buf.Write([]byte(w))
				if err != nil {
					t.Fatalf("Write(%q): unexpected error: %v", w, err)
				}

				if n != len(w) {
					t.Fatalf("Write(%q): got n=%d, want %d", w, n, len(w))
				}
			}

			data, _, _ := buf.readFrom(tt.offset)

			if tt.want == "" && data != nil {
				t.Fatalf("readFrom(%d) = %q, want nil", tt.offset, data)
			}

			if got := string(data); got != tt.want {
				t.Fatalf("readFrom(%d) = %q, want %q", tt.offset, got, tt.want)
			}
		})
	}
}

func TestOutputBuffer_ReadFromReturnsCopy(t *testing.T) {
	buf := NewOutputBuffer()
	_, _ = buf.Write([]byte("hello"))

	data, _, _ := buf.readFrom(0)
	data[0] = 'X' // mutate returned slice

	original, _, _ := buf.readFrom(0)
	if string(original) != "hello" {
		t.Fatalf("internal data was mutated: got %q, want %q", original, "hello")
	}
}

func TestOutputBuffer_WriteAfterMarkDone(t *testing.T) {
	buf := NewOutputBuffer()
	buf.MarkDone()

	_, err := buf.Write([]byte("late data"))
	if !errors.Is(err, ErrBufferClosed) {
		t.Fatalf("Write after MarkDone: got %v, want ErrBufferClosed", err)
	}
}

func TestOutputBuffer_MarkDoneIdempotent(t *testing.T) {
	buf := NewOutputBuffer()
	buf.MarkDone()
	buf.MarkDone() // must not panic
}

func TestOutputBuffer_DoneFlag(t *testing.T) {
	buf := NewOutputBuffer()

	_, done, _ := buf.readFrom(0)
	if done {
		t.Fatal("expected done=false before MarkDone")
	}

	buf.MarkDone()

	_, done, _ = buf.readFrom(0)
	if !done {
		t.Fatal("expected done=true after MarkDone")
	}
}

func TestOutputBuffer_NotifyOnWrite(t *testing.T) {
	buf := NewOutputBuffer()
	_, _, ch := buf.readFrom(0)

	_, _ = buf.Write([]byte("data"))

	select {
	case <-ch:
		// expected: channel closed by write
	case <-time.After(time.Second):
		t.Fatal("notification channel not closed after Write")
	}
}

func TestOutputBuffer_NotifyOnMarkDone(t *testing.T) {
	buf := NewOutputBuffer()
	_, _, ch := buf.readFrom(0)

	buf.MarkDone()

	select {
	case <-ch:
		// expected: channel closed by MarkDone
	case <-time.After(time.Second):
		t.Fatal("notification channel not closed after MarkDone")
	}
}

func TestOutputBuffer_NotifyChannelReplaced(t *testing.T) {
	buf := NewOutputBuffer()
	_, _, ch1 := buf.readFrom(0)

	_, _ = buf.Write([]byte("data"))
	_, _, ch2 := buf.readFrom(0)

	// Old channel should be closed, new one should be open.
	select {
	case <-ch1:
	default:
		t.Fatal("old channel should be closed")
	}
	select {
	case <-ch2:
		t.Fatal("new channel should be open")
	default:
	}
}

func TestOutputBuffer_BinaryData(t *testing.T) {
	buf := NewOutputBuffer()

	input := []byte{0x00, 0xFF, 0x01, 0xFE, 0x00}
	_, _ = buf.Write(input)

	data, _, _ := buf.readFrom(0)
	if len(data) != len(input) {
		t.Fatalf("expected %d bytes, got %d", len(input), len(data))
	}

	for i := range input {
		if data[i] != input[i] {
			t.Fatalf("byte %d: expected 0x%02x, got 0x%02x", i, input[i], data[i])
		}
	}
}

// ---------------------------------------------------------------------------
// OutputReader tests — exercise the public consumer API.
// ---------------------------------------------------------------------------

func TestOutputReader_ReaderLoop(t *testing.T) {
	buf := NewOutputBuffer()
	reader := buf.NewReader()

	go func() {
		_, _ = buf.Write([]byte("chunk1"))
		_, _ = buf.Write([]byte("chunk2"))
		_, _ = buf.Write([]byte("chunk3"))
		buf.MarkDone()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var collected []byte
	for {
		data, err := reader.Read(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		collected = append(collected, data...)
	}

	if got := string(collected); got != "chunk1chunk2chunk3" {
		t.Fatalf("collected = %q, want %q", got, "chunk1chunk2chunk3")
	}
}

func TestOutputReader_ConcurrentReaders(t *testing.T) {
	buf := NewOutputBuffer()
	want := "abcdefghij"

	var wg sync.WaitGroup

	wg.Go(func() {
		for _, b := range []byte(want) {
			_, _ = buf.Write([]byte{b})
			time.Sleep(time.Millisecond)
		}
		buf.MarkDone()
	})

	const numReaders = 5
	for range numReaders {
		wg.Go(func() {
			reader := buf.NewReader()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			var collected []byte
			for {
				data, err := reader.Read(ctx)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Errorf("reader error: %v", err)
					return
				}
				collected = append(collected, data...)
			}

			if got := string(collected); got != want {
				t.Errorf("reader got %q, want %q", got, want)
			}
		})
	}

	wg.Wait()
}

func TestOutputReader_ReadFromCompletedBuffer(t *testing.T) {
	buf := NewOutputBuffer()
	_, _ = buf.Write([]byte("complete data"))
	buf.MarkDone()

	// Reader arrives after buffer is done — should get all data then EOF.
	reader := buf.NewReader()
	ctx := context.Background()

	data, err := reader.Read(ctx)
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if string(data) != "complete data" {
		t.Fatalf("got %q, want %q", string(data), "complete data")
	}

	// Second read — EOF.
	_, err = reader.Read(ctx)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("second Read: got %v, want io.EOF", err)
	}
}

func TestOutputReader_ContextCancellation(t *testing.T) {
	buf := NewOutputBuffer()
	reader := buf.NewReader()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := reader.Read(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
