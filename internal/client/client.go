// Package client provides a gRPC client wrapper for the JobWorker service.
package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pb "github.com/GevorgGal/jobworker/proto/jobworker/v1"
)

// StatusResult holds the response from a GetStatus call, decoupling
// callers from protobuf types.
type StatusResult struct {
	JobID    string
	Status   string
	ExitCode int
}

// Client wraps the JobWorker gRPC stub with typed methods and manages
// the underlying connection.
type Client struct {
	conn *grpc.ClientConn
	rpc  pb.JobWorkerClient
}

// Dial creates a Client connected to addr using mTLS. The caller must
// call Close when finished.
func Dial(addr, caPath, certPath, keyPath string) (*Client, error) {
	tlsCfg, err := buildTLSConfig(caPath, certPath, keyPath)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
	)
	if err != nil {
		return nil, fmt.Errorf("create client for %s: %w", addr, err)
	}

	return New(conn), nil
}

// New creates a Client from an existing connection. Use Dial for the
// common mTLS path.
func New(conn *grpc.ClientConn) *Client {
	return &Client{
		conn: conn,
		rpc:  pb.NewJobWorkerClient(conn),
	}
}

// Close closes the underlying gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Start launches a new job and returns its ID.
func (c *Client) Start(ctx context.Context, command string, args []string) (string, error) {
	resp, err := c.rpc.Start(ctx, &pb.StartRequest{
		Command: command,
		Args:    args,
	})
	if err != nil {
		return "", err
	}
	return resp.GetJobId(), nil
}

// Stop terminates a running job.
func (c *Client) Stop(ctx context.Context, jobID string) error {
	_, err := c.rpc.Stop(ctx, &pb.StopRequest{JobId: jobID})
	return err
}

// GetStatus returns the current state and exit code of a job.
func (c *Client) GetStatus(ctx context.Context, jobID string) (*StatusResult, error) {
	resp, err := c.rpc.GetStatus(ctx, &pb.GetStatusRequest{JobId: jobID})
	if err != nil {
		return nil, err
	}
	return &StatusResult{
		JobID:    resp.GetJobId(),
		Status:   statusString(resp.GetStatus()),
		ExitCode: int(resp.GetExitCode()),
	}, nil
}

// StreamOutput streams the job's output to w until the job exits or ctx
// is cancelled.
func (c *Client) StreamOutput(ctx context.Context, jobID string, w io.Writer) error {
	stream, err := c.rpc.StreamOutput(ctx, &pb.StreamOutputRequest{JobId: jobID})
	if err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := w.Write(msg.GetData()); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
	}
}

// buildTLSConfig creates a TLS 1.3 client configuration for mTLS.
func buildTLSConfig(caPath, certPath, keyPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load client keypair: %w", err)
	}

	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse CA certificate: no valid certificates found")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// statusString converts a proto JobStatus to a human-readable string.
func statusString(s pb.JobStatus) string {
	switch s {
	case pb.JobStatus_JOB_STATUS_RUNNING:
		return "RUNNING"
	case pb.JobStatus_JOB_STATUS_EXITED:
		return "EXITED"
	case pb.JobStatus_JOB_STATUS_STOPPED:
		return "STOPPED"
	default:
		return "UNKNOWN"
	}
}
