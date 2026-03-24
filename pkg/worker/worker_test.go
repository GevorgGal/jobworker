package worker

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// waitForDone drains the reader until EOF, with a timeout to prevent tests
// from hanging.
func waitForDone(t *testing.T, reader *OutputReader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, err := reader.Read(ctx)
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("waiting for done: %v", err)
		}
	}
}

func TestJobManager_StartAndGetStatus(t *testing.T) {
	m := NewJobManager()

	id, err := m.Start("echo", []string{"hello"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id == "" {
		t.Fatal("Start returned empty ID")
	}

	reader, _ := m.GetOutputReader(id)
	waitForDone(t, reader)

	status, exitCode, err := m.GetStatus(id)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if status != JobStatusExited {
		t.Fatalf("Status = %v, want EXITED", status)
	}
	if exitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", exitCode)
	}
}

func TestJobManager_StartInvalidCommand(t *testing.T) {
	m := NewJobManager()

	_, err := m.Start("/nonexistent/binary", nil)
	if err == nil {
		t.Fatal("expected error for invalid command")
	}
}

func TestJobManager_StartReturnsUniqueIDs(t *testing.T) {
	m := NewJobManager()

	id1, err := m.Start("true", nil)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	id2, err := m.Start("true", nil)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}

	if id1 == id2 {
		t.Fatalf("expected unique IDs, got %q twice", id1)
	}
}

func TestJobManager_Stop(t *testing.T) {
	m := NewJobManager()

	id, err := m.Start("sleep", []string{"60"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := m.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	reader, _ := m.GetOutputReader(id)
	waitForDone(t, reader)

	status, _, err := m.GetStatus(id)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if status != JobStatusStopped {
		t.Fatalf("Status = %v, want STOPPED", status)
	}
}

func TestJobManager_StopNotFound(t *testing.T) {
	m := NewJobManager()

	err := m.Stop("nonexistent-id")
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Stop: got %v, want ErrJobNotFound", err)
	}
}

func TestJobManager_GetStatusNotFound(t *testing.T) {
	m := NewJobManager()

	_, _, err := m.GetStatus("nonexistent-id")
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("GetStatus: got %v, want ErrJobNotFound", err)
	}
}

func TestJobManager_GetOutputReader(t *testing.T) {
	m := NewJobManager()

	id, err := m.Start("echo", []string{"hello"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	reader, err := m.GetOutputReader(id)
	if err != nil {
		t.Fatalf("GetOutputReader: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var collected []byte
	for {
		data, readErr := reader.Read(ctx)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			t.Fatalf("Read: %v", readErr)
		}
		collected = append(collected, data...)
	}

	if got := string(collected); got != "hello\n" {
		t.Fatalf("output = %q, want %q", got, "hello\n")
	}
}

func TestJobManager_GetOutputReaderNotFound(t *testing.T) {
	m := NewJobManager()

	_, err := m.GetOutputReader("nonexistent-id")
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("GetOutputReader: got %v, want ErrJobNotFound", err)
	}
}

func TestJobManager_FullLifecycle(t *testing.T) {
	m := NewJobManager()

	// Start.
	id, err := m.Start("sleep", []string{"60"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Verify running.
	status, _, err := m.GetStatus(id)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if status != JobStatusRunning {
		t.Fatalf("Status = %v, want RUNNING", status)
	}

	// Stop.
	if err := m.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Wait and verify stopped.
	reader, _ := m.GetOutputReader(id)
	waitForDone(t, reader)

	status, _, err = m.GetStatus(id)
	if err != nil {
		t.Fatalf("GetStatus after stop: %v", err)
	}
	if status != JobStatusStopped {
		t.Fatalf("Status = %v, want STOPPED", status)
	}

	// Stop again → ErrAlreadyStopped.
	err = m.Stop(id)
	if !errors.Is(err, ErrAlreadyStopped) {
		t.Fatalf("second Stop: got %v, want ErrAlreadyStopped", err)
	}
}

func TestJobManager_ConcurrentStarts(t *testing.T) {
	m := NewJobManager()

	const n = 10
	ids := make([]string, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			ids[i], errs[i] = m.Start("true", nil)
		})
	}
	wg.Wait()

	seen := make(map[string]bool)
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("Start %d: %v", i, errs[i])
		}
		if seen[ids[i]] {
			t.Fatalf("duplicate ID: %s", ids[i])
		}
		seen[ids[i]] = true
	}
}

func TestJobManager_ConcurrentStartAndStop(t *testing.T) {
	m := NewJobManager()

	const n = 5
	ids := make([]string, n)
	for i := range n {
		id, err := m.Start("sleep", []string{"60"})
		if err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
		ids[i] = id
	}

	// Stop all concurrently.
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			if err := m.Stop(id); err != nil {
				t.Errorf("Stop(%s): %v", id, err)
			}
		})
	}
	wg.Wait()

	// Verify all stopped.
	for _, id := range ids {
		reader, _ := m.GetOutputReader(id)
		waitForDone(t, reader)

		status, _, _ := m.GetStatus(id)
		if status != JobStatusStopped {
			t.Errorf("job %s: status = %v, want STOPPED", id, status)
		}
	}
}
