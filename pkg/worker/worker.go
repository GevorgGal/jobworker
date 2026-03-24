// Package worker provides a reusable library for managing Linux processes
// as background jobs with output capture and streaming support.
package worker

import (
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

// GetOutputBuffer returns the output buffer for the given job ID.
func (m *JobManager) GetOutputBuffer(id string) (*OutputBuffer, error) {
	m.mu.RLock()
	job, ok := m.jobs[id]
	m.mu.RUnlock()

	if !ok {
		return nil, ErrJobNotFound
	}

	return job.Output(), nil
}
