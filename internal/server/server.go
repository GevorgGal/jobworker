// Package server implements the JobWorker gRPC service, bridging
// proto types to the worker library.
package server

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GevorgGal/jobworker/pkg/worker"
	pb "github.com/GevorgGal/jobworker/proto/jobworker/v1"
)

// Manager defines the job operations the server depends on.
// *worker.JobManager implements this interface.
type Manager interface {
	Start(command string, args []string) (string, error)
	Stop(id string) error
	GetStatus(id string) (worker.JobStatus, int, error)
	GetOutputReader(id string) (*worker.OutputReader, error)
}

// Server implements the pb.JobWorkerServer gRPC interface.
type Server struct {
	pb.UnimplementedJobWorkerServer
	manager Manager
	logger  *slog.Logger
}

// New creates a Server that delegates job operations to the given Manager.
func New(manager Manager, logger *slog.Logger) *Server {
	return &Server{manager: manager, logger: logger}
}

// Start launches a new process and returns its job ID.
func (s *Server) Start(ctx context.Context, req *pb.StartRequest) (*pb.StartResponse, error) {
	if req.GetCommand() == "" {
		return nil, status.Error(codes.InvalidArgument, "command is required")
	}

	id, err := s.manager.Start(req.GetCommand(), req.GetArgs())
	if err != nil {
		if errors.Is(err, worker.ErrInvalidCommand) {
			return nil, status.Errorf(codes.InvalidArgument, "command not found or not executable: %s", req.GetCommand())
		}
		s.logger.Error("failed to start job", "command", req.GetCommand(), "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	// args intentionally omitted — may contain sensitive values
	s.logger.Info("job started", "job_id", id, "command", req.GetCommand())

	return &pb.StartResponse{JobId: id}, nil
}

// Stop terminates a running job.
func (s *Server) Stop(ctx context.Context, req *pb.StopRequest) (*pb.StopResponse, error) {
	if req.GetJobId() == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}

	logger := s.logger.With("op", "stop", "job_id", req.GetJobId())

	if err := s.manager.Stop(req.GetJobId()); err != nil {
		return nil, mapError(logger.With("error", err), err)
	}

	logger.Info("job stopped")
	return &pb.StopResponse{}, nil
}

// GetStatus returns the current state and exit code of a job.
func (s *Server) GetStatus(ctx context.Context, req *pb.GetStatusRequest) (*pb.GetStatusResponse, error) {
	if req.GetJobId() == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}

	logger := s.logger.With("op", "get_status", "job_id", req.GetJobId())

	jobStatus, exitCode, err := s.manager.GetStatus(req.GetJobId())
	if err != nil {
		return nil, mapError(logger.With("error", err), err)
	}

	return &pb.GetStatusResponse{
		JobId:    req.GetJobId(),
		Status:   mapStatus(jobStatus),
		ExitCode: int32(exitCode),
	}, nil
}

// StreamOutput streams the job's combined stdout/stderr from the beginning.
// The stream completes when the job exits and all output has been delivered,
// or when the client disconnects.
func (s *Server) StreamOutput(req *pb.StreamOutputRequest, stream pb.JobWorker_StreamOutputServer) error {
	if req.GetJobId() == "" {
		return status.Error(codes.InvalidArgument, "job_id is required")
	}

	logger := s.logger.With("op", "stream_output", "job_id", req.GetJobId())

	reader, err := s.manager.GetOutputReader(req.GetJobId())
	if err != nil {
		return mapError(logger.With("error", err), err)
	}

	logger.Debug("streaming started")

	ctx := stream.Context()
	for {
		data, err := reader.Read(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				logger.Debug("stream completed")
				return nil
			}
			if ctx.Err() != nil {
				logger.Debug("stream ended", "error", err)
				return err
			}
			logger.Error("unexpected stream error", "error", err)
			return status.Error(codes.Internal, "internal error")
		}

		if err := stream.Send(&pb.StreamOutputResponse{Data: data}); err != nil {
			logger.Debug("stream send failed", "error", err)
			return err
		}
	}
}

// mapError translates worker library errors to gRPC status codes.
// Only unexpected errors are logged; expected errors (not found, already stopped)
// are communicated via gRPC status codes without additional logging.
func mapError(logger *slog.Logger, err error) error {
	switch {
	case errors.Is(err, worker.ErrJobNotFound):
		return status.Error(codes.NotFound, "job not found")
	case errors.Is(err, worker.ErrAlreadyStopped):
		return status.Error(codes.FailedPrecondition, "job has already terminated")
	default:
		logger.Error("request failed")
		return status.Error(codes.Internal, "internal error")
	}
}

// mapStatus converts a worker.JobStatus to the proto enum.
func mapStatus(js worker.JobStatus) pb.JobStatus {
	switch js {
	case worker.JobStatusRunning:
		return pb.JobStatus_JOB_STATUS_RUNNING
	case worker.JobStatusExited:
		return pb.JobStatus_JOB_STATUS_EXITED
	case worker.JobStatusStopped:
		return pb.JobStatus_JOB_STATUS_STOPPED
	default:
		return pb.JobStatus_JOB_STATUS_UNSPECIFIED
	}
}
