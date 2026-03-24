package worker

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestJobStatus_String(t *testing.T) {
	tests := []struct {
		status JobStatus
		want   string
	}{
		{JobStatusRunning, "RUNNING"},
		{JobStatusExited, "EXITED"},
		{JobStatusStopped, "STOPPED"},
		{JobStatus(99), "UNKNOWN"},
	}

	for _, tt := range tests {
		if got := tt.status.String(); got != tt.want {
			t.Errorf("JobStatus(%d).String() = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func Test_startJob_Success(t *testing.T) {
	job, err := startJob("test-1", "echo", []string{"hello"})
	if err != nil {
		t.Fatalf("startJob: unexpected error: %v", err)
	}

	if job.ID() != "test-1" {
		t.Fatalf("ID() = %q, want %q", job.ID(), "test-1")
	}
	if job.Output() == nil {
		t.Fatal("Output() returned nil")
	}

	<-job.Done()
}

func Test_startJob_InvalidCommand(t *testing.T) {
	_, err := startJob("test-1", "/nonexistent/binary", nil)
	if err == nil {
		t.Fatal("expected error for invalid command")
	}
}

func TestJob_NaturalExit(t *testing.T) {
	job, err := startJob("test-1", "echo", []string{"hello"})
	if err != nil {
		t.Fatalf("startJob: %v", err)
	}

	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for job to exit")
	}

	status, exitCode := job.StatusInfo()
	if status != JobStatusExited {
		t.Fatalf("status = %v, want EXITED", status)
	}
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}
}

func TestJob_NonZeroExitCode(t *testing.T) {
	job, err := startJob("test-1", "sh", []string{"-c", "exit 42"})
	if err != nil {
		t.Fatalf("startJob: %v", err)
	}

	<-job.Done()

	status, exitCode := job.StatusInfo()
	if status != JobStatusExited {
		t.Fatalf("status = %v, want EXITED", status)
	}
	if exitCode != 42 {
		t.Fatalf("exitCode = %d, want 42", exitCode)
	}
}

func TestJob_Stop(t *testing.T) {
	job, err := startJob("test-1", "sleep", []string{"60"})
	if err != nil {
		t.Fatalf("startJob: %v", err)
	}

	if err := job.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stopped job to finish")
	}

	status, exitCode := job.StatusInfo()
	if status != JobStatusStopped {
		t.Fatalf("status = %v, want STOPPED", status)
	}
	if exitCode != -1 {
		t.Fatalf("exitCode = %d, want -1", exitCode)
	}
}

func TestJob_StopAlreadyExited(t *testing.T) {
	job, err := startJob("test-1", "true", nil)
	if err != nil {
		t.Fatalf("startJob: %v", err)
	}

	<-job.Done()

	err = job.Stop()
	if !errors.Is(err, ErrAlreadyStopped) {
		t.Fatalf("Stop on exited job: got %v, want ErrAlreadyStopped", err)
	}
}

func TestJob_DoubleStop(t *testing.T) {
	job, err := startJob("test-1", "sleep", []string{"60"})
	if err != nil {
		t.Fatalf("startJob: %v", err)
	}

	if err := job.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}

	<-job.Done()

	err = job.Stop()
	if !errors.Is(err, ErrAlreadyStopped) {
		t.Fatalf("second Stop: got %v, want ErrAlreadyStopped", err)
	}
}

func TestJob_ConcurrentStop(t *testing.T) {
	job, err := startJob("test-1", "sleep", []string{"60"})
	if err != nil {
		t.Fatalf("startJob: %v", err)
	}

	var wg sync.WaitGroup
	results := make(chan error, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- job.Stop()
		}()
	}
	wg.Wait()
	close(results)

	<-job.Done()

	var succeeded, alreadyStopped int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrAlreadyStopped):
			alreadyStopped++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if succeeded == 0 {
		t.Fatal("expected at least one successful Stop")
	}
	if succeeded+alreadyStopped != 2 {
		t.Fatalf("unexpected result distribution: %d succeeded, %d alreadyStopped", succeeded, alreadyStopped)
	}
}

func TestJob_StatusWhileRunning(t *testing.T) {
	job, err := startJob("test-1", "sleep", []string{"60"})
	if err != nil {
		t.Fatalf("startJob: %v", err)
	}

	status, _ := job.StatusInfo()
	if status != JobStatusRunning {
		t.Fatalf("status = %v, want RUNNING", status)
	}

	_ = job.Stop()
	<-job.Done()
}

func TestJob_CapturesStdout(t *testing.T) {
	job, err := startJob("test-1", "echo", []string{"hello world"})
	if err != nil {
		t.Fatalf("startJob: %v", err)
	}

	<-job.Done()

	data, done, _ := job.Output().ReadFrom(0)
	if !done {
		t.Fatal("expected buffer done after job exit")
	}

	if got := string(data); got != "hello world\n" {
		t.Fatalf("stdout = %q, want %q", got, "hello world\n")
	}
}

func TestJob_CapturesStderr(t *testing.T) {
	job, err := startJob("test-1", "sh", []string{"-c", "echo error >&2"})
	if err != nil {
		t.Fatalf("startJob: %v", err)
	}

	<-job.Done()

	data, done, _ := job.Output().ReadFrom(0)
	if !done {
		t.Fatal("expected buffer done after job exit")
	}

	if got := string(data); got != "error\n" {
		t.Fatalf("stderr = %q, want %q", got, "error\n")
	}
}

func TestJob_BufferMarkedDoneOnExit(t *testing.T) {
	job, err := startJob("test-1", "true", nil)
	if err != nil {
		t.Fatalf("startJob: %v", err)
	}

	<-job.Done()

	_, done, _ := job.Output().ReadFrom(0)
	if !done {
		t.Fatal("expected output buffer done after job exit")
	}
}
