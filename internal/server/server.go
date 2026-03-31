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

// Server implements the pb.JobWorkerServer gRPC interface.
type Server struct {
	pb.UnimplementedJobWorkerServer
	manager *worker.JobManager
	logger  *slog.Logger
}

// New creates a Server that delegates job operations to the given JobManager.
func New(manager *worker.JobManager, logger *slog.Logger) *Server {
	return &Server{manager: manager, logger: logger}
}

// Start launches a new process and returns its job ID.
func (s *Server) Start(ctx context.Context, req *pb.StartRequest) (*pb.StartResponse, error) {
	// TODO: Pass ctx to manager methods to support request cancellation.
	if req.GetCommand() == "" {
		return nil, status.Error(codes.InvalidArgument, "command is required")
	}

	id, err := s.manager.Start(req.GetCommand(), req.GetArgs())
	if err != nil {
		// Start has method-specific error mapping; Stop/GetStatus use mapError.
		if errors.Is(err, worker.ErrInvalidCommand) {
			return nil, status.Errorf(codes.InvalidArgument, "command not found or not executable: %s", req.GetCommand())
		}
		// Unrecognized errors default to Internal to avoid leaking server details.
		s.logger.Error("failed to start job", "command", req.GetCommand(), "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	// args intentionally omitted — may contain sensitive values
	s.logger.Info("job started", "job_id", id, "command", req.GetCommand())

	return &pb.StartResponse{JobId: id}, nil
}

// Stop terminates a running job.
func (s *Server) Stop(ctx context.Context, req *pb.StopRequest) (*pb.StopResponse, error) {
	// TODO: Pass ctx to manager methods to support request cancellation.
	if req.GetJobId() == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}

	if err := s.manager.Stop(req.GetJobId()); err != nil {
		return nil, s.mapError("stop", req.GetJobId(), err)
	}

	s.logger.Info("job stopped", "job_id", req.GetJobId())

	return &pb.StopResponse{}, nil
}

// GetStatus returns the current state and exit code of a job.
func (s *Server) GetStatus(ctx context.Context, req *pb.GetStatusRequest) (*pb.GetStatusResponse, error) {
	// TODO: Pass ctx to manager methods to support request cancellation.
	if req.GetJobId() == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}

	jobStatus, exitCode, err := s.manager.GetStatus(req.GetJobId())
	if err != nil {
		return nil, s.mapError("get_status", req.GetJobId(), err)
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

	reader, err := s.manager.GetOutputReader(req.GetJobId())
	if err != nil {
		return s.mapError("stream_output", req.GetJobId(), err)
	}

	s.logger.Debug("streaming output", "job_id", req.GetJobId())

	ctx := stream.Context()
	for {
		// reader.Read returns at most maxReadSize (32KB) bytes per call,
		// which becomes one gRPC message. Chunking is handled by the buffer layer.
		data, err := reader.Read(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.logger.Debug("stream completed", "job_id", req.GetJobId())
				return nil
			}
			// Context cancellation (client disconnect) or deadline exceeded.
			// Return as-is; gRPC maps context errors to appropriate status codes.
			s.logger.Debug("stream ended", "job_id", req.GetJobId(), "reason", err)
			return err
		}

		if err := stream.Send(&pb.StreamOutputResponse{Data: data}); err != nil {
			s.logger.Debug("stream send failed", "job_id", req.GetJobId(), "error", err)
			return err
		}
	}
}

// mapError translates worker library errors to gRPC status codes.
// Unrecognized errors are logged and returned as Internal.
func (s *Server) mapError(op, jobID string, err error) error {
	switch {
	case errors.Is(err, worker.ErrJobNotFound):
		s.logger.Debug("job not found", "op", op, "job_id", jobID)
		return status.Error(codes.NotFound, "job not found")
	case errors.Is(err, worker.ErrAlreadyStopped):
		s.logger.Debug("job already terminated", "op", op, "job_id", jobID)
		return status.Error(codes.FailedPrecondition, "job has already terminated")
	default:
		s.logger.Error("unexpected error", "op", op, "job_id", jobID, "error", err)
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
