package worker

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// waitForBufferDone drains the buffer's notification channel until the buffer
// is marked done, with a timeout to prevent tests from hanging.
func waitForBufferDone(t *testing.T, buf *OutputBuffer) {
	t.Helper()
	for {
		_, done, ch := buf.ReadFrom(0)
		if done {
			return
		}
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for buffer done")
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

	buf, _ := m.GetOutputBuffer(id)
	waitForBufferDone(t, buf)

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

	buf, _ := m.GetOutputBuffer(id)
	waitForBufferDone(t, buf)

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

func TestJobManager_GetOutputBuffer(t *testing.T) {
	m := NewJobManager()

	id, err := m.Start("echo", []string{"hello"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	buf, err := m.GetOutputBuffer(id)
	if err != nil {
		t.Fatalf("GetOutputBuffer: %v", err)
	}

	waitForBufferDone(t, buf)

	data, _, _ := buf.ReadFrom(0)
	if got := string(data); got != "hello\n" {
		t.Fatalf("output = %q, want %q", got, "hello\n")
	}
}

func TestJobManager_GetOutputBufferNotFound(t *testing.T) {
	m := NewJobManager()

	_, err := m.GetOutputBuffer("nonexistent-id")
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("GetOutputBuffer: got %v, want ErrJobNotFound", err)
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
	buf, _ := m.GetOutputBuffer(id)
	waitForBufferDone(t, buf)

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
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = m.Start("true", nil)
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool)
	for i := 0; i < n; i++ {
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
	for i := 0; i < n; i++ {
		id, err := m.Start("sleep", []string{"60"})
		if err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
		ids[i] = id
	}

	// Stop all concurrently.
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := m.Stop(id); err != nil {
				t.Errorf("Stop(%s): %v", id, err)
			}
		}(id)
	}
	wg.Wait()

	// Verify all stopped.
	for _, id := range ids {
		buf, _ := m.GetOutputBuffer(id)
		waitForBufferDone(t, buf)

		status, _, _ := m.GetStatus(id)
		if status != JobStatusStopped {
			t.Errorf("job %s: status = %v, want STOPPED", id, status)
		}
	}
}
