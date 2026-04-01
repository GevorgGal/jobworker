// Package worker provides a reusable library for managing Linux processes
// as background jobs with output capture and streaming support.
package worker

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
)

// ErrJobNotFound is returned when an operation references a job ID
// that does not exist in the manager.
var ErrJobNotFound = errors.New("job not found")

// JobManager manages the lifecycle of multiple jobs.
type JobManager struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

// NewJobManager creates a new JobManager ready for use.
func NewJobManager() *JobManager {
	return &JobManager{
		jobs: make(map[string]*Job),
	}
}

// Start launches a new job with the given command and args.
func (m *JobManager) Start(command string, args []string) (string, error) {
	id := uuid.New().String()

	job, err := startJob(id, command, args)
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	m.jobs[id] = job
	m.mu.Unlock()

	return id, nil
}

// Stop terminates a running job by ID.
func (m *JobManager) Stop(id string) error {
	m.mu.RLock()
	job, ok := m.jobs[id]
	m.mu.RUnlock()

	if !ok {
		return ErrJobNotFound
	}

	return job.Stop()
}

// StopAll terminates all running jobs and waits for them to exit.
func (m *JobManager) StopAll() {
	m.mu.RLock()
	jobs := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.RUnlock()

	for _, j := range jobs {
		_ = j.Stop()
	}

	// Wait for all processes to fully exit so output buffers are marked
	// done before we return. This ensures StreamOutput RPCs see EOF.
	for _, j := range jobs {
		<-j.Done()
	}
}

// GetStatus returns the status and exit code for the given job ID.
func (m *JobManager) GetStatus(id string) (JobStatus, int, error) {
	m.mu.RLock()
	job, ok := m.jobs[id]
	m.mu.RUnlock()

	if !ok {
		return 0, 0, ErrJobNotFound
	}

	status, exitCode := job.StatusInfo()
	return status, exitCode, nil
}

// GetOutputReader returns a new OutputReader for the given job ID, starting
// from the beginning of the output buffer.
func (m *JobManager) GetOutputReader(id string) (*OutputReader, error) {
	m.mu.RLock()
	job, ok := m.jobs[id]
	m.mu.RUnlock()

	if !ok {
		return nil, ErrJobNotFound
	}

	return job.Output().NewReader(), nil
}

// WaitForExit blocks until the job has fully terminated or ctx is cancelled.
func (m *JobManager) WaitForExit(ctx context.Context, id string) error {
	m.mu.RLock()
	job, ok := m.jobs[id]
	m.mu.RUnlock()

	if !ok {
		return ErrJobNotFound
	}

	select {
	case <-job.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
