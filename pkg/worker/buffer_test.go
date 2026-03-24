package worker

import (
	"errors"
	"sync"
	"testing"
	"time"
)

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

			data, _, _ := buf.ReadFrom(tt.offset)

			if tt.want == "" && data != nil {
				t.Fatalf("ReadFrom(%d) = %q, want nil", tt.offset, data)
			}

			if got := string(data); got != tt.want {
				t.Fatalf("ReadFrom(%d) = %q, want %q", tt.offset, got, tt.want)
			}
		})
	}
}

func TestOutputBuffer_ReadFromReturnsCopy(t *testing.T) {
	buf := NewOutputBuffer()
	_, _ = buf.Write([]byte("hello"))

	data, _, _ := buf.ReadFrom(0)
	data[0] = 'X' // mutate returned slice

	original, _, _ := buf.ReadFrom(0)
	if string(original) != "hello" {
		t.Fatalf("internal data was mutated: got %q, want %q", original, "hello")
	}
}

func TestOutputBuffer_Len(t *testing.T) {
	buf := NewOutputBuffer()
	if buf.Len() != 0 {
		t.Fatalf("empty buffer Len() = %d, want 0", buf.Len())
	}

	_, _ = buf.Write([]byte("hello"))
	if buf.Len() != 5 {
		t.Fatalf("after write Len() = %d, want 5", buf.Len())
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

	_, done, _ := buf.ReadFrom(0)
	if done {
		t.Fatal("expected done=false before MarkDone")
	}

	buf.MarkDone()

	_, done, _ = buf.ReadFrom(0)
	if !done {
		t.Fatal("expected done=true after MarkDone")
	}
}

func TestOutputBuffer_NotifyOnWrite(t *testing.T) {
	buf := NewOutputBuffer()
	_, _, ch := buf.ReadFrom(0)

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
	_, _, ch := buf.ReadFrom(0)

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
	_, _, ch1 := buf.ReadFrom(0)

	_, _ = buf.Write([]byte("data"))
	_, _, ch2 := buf.ReadFrom(0)

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

func TestOutputBuffer_ReaderLoop(t *testing.T) {
	buf := NewOutputBuffer()

	go func() {
		_, _ = buf.Write([]byte("chunk1"))
		_, _ = buf.Write([]byte("chunk2"))
		_, _ = buf.Write([]byte("chunk3"))
		buf.MarkDone()
	}()

	var collected []byte
	offset := 0
	for {
		data, done, ch := buf.ReadFrom(offset)
		if len(data) > 0 {
			collected = append(collected, data...)
			offset += len(data)
		}
		if done && len(data) == 0 {
			break
		}
		if len(data) == 0 {
			select {
			case <-ch:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for notification")
			}
		}
	}

	if got := string(collected); got != "chunk1chunk2chunk3" {
		t.Fatalf("collected = %q, want %q", got, "chunk1chunk2chunk3")
	}
}

func TestOutputBuffer_ConcurrentReaders(t *testing.T) {
	buf := NewOutputBuffer()
	want := "abcdefghij"

	go func() {
		for _, b := range []byte(want) {
			_, _ = buf.Write([]byte{b})
			time.Sleep(time.Millisecond)
		}
		buf.MarkDone()
	}()

	const numReaders = 5
	var wg sync.WaitGroup

	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			var collected []byte
			offset := 0

			for {
				data, done, ch := buf.ReadFrom(offset)
				if len(data) > 0 {
					collected = append(collected, data...)
					offset += len(data)
				}
				if done && len(data) == 0 {
					break
				}
				if len(data) == 0 {
					select {
					case <-ch:
					case <-time.After(5 * time.Second):
						t.Errorf("reader timed out at offset %d", offset)
						return
					}
				}
			}

			if got := string(collected); got != want {
				t.Errorf("reader got %q, want %q", got, want)
			}
		}()
	}

	wg.Wait()
}

func TestOutputBuffer_ReadFromCompletedBuffer(t *testing.T) {
	buf := NewOutputBuffer()
	_, _ = buf.Write([]byte("complete data"))
	buf.MarkDone()

	// Reader arrives after buffer is done — should get all data.
	data, done, _ := buf.ReadFrom(0)
	if !done {
		t.Fatal("expected done=true")
	}

	if string(data) != "complete data" {
		t.Fatalf("got %q, want %q", string(data), "complete data")
	}

	// Second read from end — EOF condition.
	data, done, _ = buf.ReadFrom(len("complete data"))
	if !done {
		t.Fatal("expected done=true at EOF")
	}

	if len(data) != 0 {
		t.Fatalf("expected empty data at EOF, got %q", string(data))
	}
}

func TestOutputBuffer_BinaryData(t *testing.T) {
	buf := NewOutputBuffer()

	input := []byte{0x00, 0xFF, 0x01, 0xFE, 0x00}
	_, _ = buf.Write(input)

	data, _, _ := buf.ReadFrom(0)
	if len(data) != len(input) {
		t.Fatalf("expected %d bytes, got %d", len(input), len(data))
	}

	for i := range input {
		if data[i] != input[i] {
			t.Fatalf("byte %d: expected 0x%02x, got 0x%02x", i, input[i], data[i])
		}
	}
}
