package worker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// JobStatus represents the current state of a job.
type JobStatus int

const (
	// JobStatusUnspecified is the zero value, never set on a real job.
	// It ensures callers who forget to check errors don't see a
	// misleading "Running" status.
	JobStatusUnspecified JobStatus = iota
	// JobStatusRunning indicates the process is still executing.
	JobStatusRunning
	// JobStatusExited indicates the process terminated on its own.
	JobStatusExited
	// JobStatusStopped indicates the process was terminated via Stop.
	JobStatusStopped
)

// String returns a human-readable representation of the job status.
func (s JobStatus) String() string {
	switch s {
	case JobStatusRunning:
		return "RUNNING"
	case JobStatusExited:
		return "EXITED"
	case JobStatusStopped:
		return "STOPPED"
	default:
		return "UNKNOWN"
	}
}

// ErrAlreadyStopped is returned when Stop is called on a job that has
// already terminated (either exited naturally or was previously stopped).
var ErrAlreadyStopped = errors.New("job is already stopped")

// ErrInvalidCommand is returned when the command cannot be started because
// it was not found, does not exist, or is not executable.
var ErrInvalidCommand = errors.New("invalid command")

// Job represents a managed OS process with output capture.
type Job struct {
	mu       sync.Mutex
	id       string
	cmd      *exec.Cmd
	status   JobStatus
	exitCode int
	stopping bool
	output   *OutputBuffer
	done     chan struct{}
}

// startJob launches the given command as a background process and returns a
// Job tracking it. Returns an error if the process fails to start.
func startJob(id, command string, args []string) (*Job, error) {
	output := NewOutputBuffer()

	cmd := exec.Command(command, args...)
	cmd.Stdout = output
	cmd.Stderr = output

	if err := cmd.Start(); err != nil {
		// Any Start failure means the process didn't launch — wrap uniformly.
		// The underlying cause is in the message for diagnostics but not in
		// the error chain, keeping exec internals out of the library's public API.
		return nil, fmt.Errorf("%w: %v", ErrInvalidCommand, err)
	}

	j := &Job{
		id:     id,
		cmd:    cmd,
		status: JobStatusRunning,
		output: output,
		done:   make(chan struct{}),
	}

	go j.wait()

	return j, nil
}

// Stop sends SIGKILL to the process.
func (j *Job) Stop() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.stopping || j.status != JobStatusRunning {
		return ErrAlreadyStopped
	}

	j.stopping = true

	// Kill may return os.ErrProcessDone if the process exited after we
	// checked status but before wait() acquired the lock. The stopping
	// flag is already set, so wait() will record JobStatusStopped.
	err := j.cmd.Process.Kill()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// StatusInfo returns the job's status and exit code atomically.
func (j *Job) StatusInfo() (JobStatus, int) {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.status, j.exitCode
}

// ID returns the job's unique identifier.
// id is immutable after creation; no lock needed.
func (j *Job) ID() string {
	return j.id
}

// Output returns the job's output buffer for reading.
// output is immutable after creation; no lock needed.
func (j *Job) Output() *OutputBuffer {
	return j.output
}

// Done returns a channel that is closed after the job has fully terminated.
func (j *Job) Done() <-chan struct{} {
	return j.done
}

// wait blocks until the process exits, records the exit code and final status.
func (j *Job) wait() {
	_ = j.cmd.Wait()

	j.mu.Lock()
	j.exitCode = j.cmd.ProcessState.ExitCode()
	if j.stopping {
		j.status = JobStatusStopped
	} else {
		j.status = JobStatusExited
	}
	j.mu.Unlock()

	// Mark buffer done after releasing the lock — avoids holding two
	// locks and ensures readers blocked on the buffer wake up promptly.
	j.output.MarkDone()
	close(j.done)
}
