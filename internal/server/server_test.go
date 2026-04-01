package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/GevorgGal/jobworker/internal/auth"
	"github.com/GevorgGal/jobworker/internal/server"
	"github.com/GevorgGal/jobworker/pkg/worker"
	pb "github.com/GevorgGal/jobworker/proto/jobworker/v1"
)

// ---------------------------------------------------------------------------
// Mock manager
// ---------------------------------------------------------------------------

// mockManager implements server.Manager with configurable function fields.
// Unconfigured methods panic with a descriptive message — this catches
// unexpected interactions immediately rather than returning silent errors.
type mockManager struct {
	startFn     func(string, []string) (string, error)
	stopFn      func(string) error
	statusFn    func(string) (worker.JobStatus, int, error)
	outReaderFn func(string) (*worker.OutputReader, error)
}

func (m *mockManager) Start(cmd string, args []string) (string, error) {
	return m.startFn(cmd, args)
}

func (m *mockManager) Stop(id string) error {
	return m.stopFn(id)
}

func (m *mockManager) GetStatus(id string) (worker.JobStatus, int, error) {
	return m.statusFn(id)
}

func (m *mockManager) GetOutputReader(id string) (*worker.OutputReader, error) {
	return m.outReaderFn(id)
}

// defaultMock returns a mock where every method panics. Tests override
// only the methods they exercise — if an unexpected method is called
// (e.g., validation was accidentally removed), the panic catches it.
func defaultMock() *mockManager {
	return &mockManager{
		startFn:     func(string, []string) (string, error) { panic("unexpected Start call") },
		stopFn:      func(string) error { panic("unexpected Stop call") },
		statusFn:    func(string) (worker.JobStatus, int, error) { panic("unexpected GetStatus call") },
		outReaderFn: func(string) (*worker.OutputReader, error) { panic("unexpected GetOutputReader call") },
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newClient creates an in-process gRPC server and client over bufconn
// with no authentication — for testing server logic in isolation.
func newClient(t *testing.T, mgr server.Manager) pb.JobWorkerClient {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := grpc.NewServer()
	pb.RegisterJobWorkerServer(srv, server.New(mgr, logger))
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}

	t.Cleanup(func() {
		conn.Close()
		srv.Stop()
	})

	return pb.NewJobWorkerClient(conn)
}

// newAuthClient creates an in-process gRPC server and client with auth
// interceptors. The given CN is injected into the peer's TLS state
// via fake transport credentials so the interceptor sees it.
func newAuthClient(t *testing.T, mgr server.Manager, cn string) pb.JobWorkerClient {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	creds := &fakeCreds{cn: cn}

	srv := grpc.NewServer(
		grpc.Creds(creds),
		grpc.UnaryInterceptor(auth.UnaryInterceptor()),
		grpc.StreamInterceptor(auth.StreamInterceptor()),
	)
	pb.RegisterJobWorkerServer(srv, server.New(mgr, logger))
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(creds),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}

	t.Cleanup(func() {
		conn.Close()
		srv.Stop()
	})

	return pb.NewJobWorkerClient(conn)
}

// completedReader creates an OutputReader backed by a buffer that already
// contains all its data and is marked done — no real process needed.
func completedReader(data []byte) *worker.OutputReader {
	buf := worker.NewOutputBuffer()
	if len(data) > 0 {
		buf.Write(data)
	}
	buf.MarkDone()
	return buf.NewReader()
}

// collectOutput streams all output from a job and returns it. Blocks until
// the stream completes (EOF) or a 5-second timeout fires.
func collectOutput(t *testing.T, client pb.JobWorkerClient, jobID string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	stream, err := client.StreamOutput(ctx, &pb.StreamOutputRequest{JobId: jobID})
	if err != nil {
		t.Fatalf("StreamOutput(%s): %v", jobID, err)
	}

	var out []byte
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("stream.Recv: %v", err)
		}
		out = append(out, msg.GetData()...)
	}
}

// fakeCreds implements credentials.TransportCredentials to inject a fixed
// CN into every connection's TLS state without a real TLS handshake.
type fakeCreds struct{ cn string }

func (c *fakeCreds) ClientHandshake(_ context.Context, _ string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return conn, c.tlsInfo(), nil
}

func (c *fakeCreds) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return conn, c.tlsInfo(), nil
}

func (c *fakeCreds) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls"}
}

func (c *fakeCreds) Clone() credentials.TransportCredentials {
	cp := *c
	return &cp
}

// Deprecated: Required by the TransportCredentials interface.
func (c *fakeCreds) OverrideServerName(string) error { return nil }

func (c *fakeCreds) tlsInfo() credentials.TLSInfo {
	return credentials.TLSInfo{
		State: tls.ConnectionState{
			VerifiedChains: [][]*x509.Certificate{
				{{Subject: pkix.Name{CommonName: c.cn}}},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Start
// ---------------------------------------------------------------------------

func TestServer_Start(t *testing.T) {
	tests := []struct {
		name   string
		req    *pb.StartRequest
		setup  func(m *mockManager)
		code   codes.Code
		wantID string
	}{
		{
			name: "success",
			req:  &pb.StartRequest{Command: "echo", Args: []string{"hello"}},
			setup: func(m *mockManager) {
				m.startFn = func(string, []string) (string, error) { return "job-123", nil }
			},
			code:   codes.OK,
			wantID: "job-123",
		},
		{
			name: "empty command",
			req:  &pb.StartRequest{},
			code: codes.InvalidArgument,
		},
		{
			name: "invalid command",
			req:  &pb.StartRequest{Command: "/nonexistent"},
			setup: func(m *mockManager) {
				m.startFn = func(string, []string) (string, error) { return "", worker.ErrInvalidCommand }
			},
			code: codes.InvalidArgument,
		},
		{
			name: "internal error",
			req:  &pb.StartRequest{Command: "echo"},
			setup: func(m *mockManager) {
				m.startFn = func(string, []string) (string, error) { return "", errors.New("unexpected") }
			},
			code: codes.Internal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := defaultMock()
			if tt.setup != nil {
				tt.setup(mock)
			}
			client := newClient(t, mock)

			resp, err := client.Start(t.Context(), tt.req)
			if got := status.Code(err); got != tt.code {
				t.Fatalf("code = %v, want %v", got, tt.code)
			}
			if tt.code == codes.OK && resp.GetJobId() != tt.wantID {
				t.Fatalf("job_id = %q, want %q", resp.GetJobId(), tt.wantID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Stop
// ---------------------------------------------------------------------------

func TestServer_Stop(t *testing.T) {
	tests := []struct {
		name  string
		jobID string
		setup func(m *mockManager)
		code  codes.Code
	}{
		{
			name:  "success",
			jobID: "job-1",
			setup: func(m *mockManager) {
				m.stopFn = func(string) error { return nil }
			},
			code: codes.OK,
		},
		{
			name:  "empty job_id",
			jobID: "",
			code:  codes.InvalidArgument,
		},
		{
			name:  "not found",
			jobID: "nonexistent",
			setup: func(m *mockManager) {
				m.stopFn = func(string) error { return worker.ErrJobNotFound }
			},
			code: codes.NotFound,
		},
		{
			name:  "already stopped",
			jobID: "job-1",
			setup: func(m *mockManager) {
				m.stopFn = func(string) error { return worker.ErrAlreadyStopped }
			},
			code: codes.FailedPrecondition,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := defaultMock()
			if tt.setup != nil {
				tt.setup(mock)
			}
			client := newClient(t, mock)

			_, err := client.Stop(t.Context(), &pb.StopRequest{JobId: tt.jobID})
			if got := status.Code(err); got != tt.code {
				t.Fatalf("code = %v, want %v", got, tt.code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GetStatus
// ---------------------------------------------------------------------------

func TestServer_GetStatus(t *testing.T) {
	tests := []struct {
		name       string
		jobID      string
		setup      func(m *mockManager)
		code       codes.Code
		wantStatus pb.JobStatus
		wantExit   int32
	}{
		{
			name:  "running",
			jobID: "job-1",
			setup: func(m *mockManager) {
				m.statusFn = func(string) (worker.JobStatus, int, error) {
					return worker.JobStatusRunning, 0, nil
				}
			},
			code:       codes.OK,
			wantStatus: pb.JobStatus_JOB_STATUS_RUNNING,
		},
		{
			name:  "exited success",
			jobID: "job-1",
			setup: func(m *mockManager) {
				m.statusFn = func(string) (worker.JobStatus, int, error) {
					return worker.JobStatusExited, 0, nil
				}
			},
			code:       codes.OK,
			wantStatus: pb.JobStatus_JOB_STATUS_EXITED,
		},
		{
			name:  "exited with code",
			jobID: "job-1",
			setup: func(m *mockManager) {
				m.statusFn = func(string) (worker.JobStatus, int, error) {
					return worker.JobStatusExited, 42, nil
				}
			},
			code:       codes.OK,
			wantStatus: pb.JobStatus_JOB_STATUS_EXITED,
			wantExit:   42,
		},
		{
			name:  "stopped",
			jobID: "job-1",
			setup: func(m *mockManager) {
				m.statusFn = func(string) (worker.JobStatus, int, error) {
					return worker.JobStatusStopped, -1, nil
				}
			},
			code:       codes.OK,
			wantStatus: pb.JobStatus_JOB_STATUS_STOPPED,
			wantExit:   -1,
		},
		{
			name:  "empty job_id",
			jobID: "",
			code:  codes.InvalidArgument,
		},
		{
			name:  "not found",
			jobID: "nonexistent",
			setup: func(m *mockManager) {
				m.statusFn = func(string) (worker.JobStatus, int, error) {
					return 0, 0, worker.ErrJobNotFound
				}
			},
			code: codes.NotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := defaultMock()
			if tt.setup != nil {
				tt.setup(mock)
			}
			client := newClient(t, mock)

			resp, err := client.GetStatus(t.Context(), &pb.GetStatusRequest{JobId: tt.jobID})
			if got := status.Code(err); got != tt.code {
				t.Fatalf("code = %v, want %v", got, tt.code)
			}
			if tt.code != codes.OK {
				return
			}
			if resp.GetStatus() != tt.wantStatus {
				t.Fatalf("status = %v, want %v", resp.GetStatus(), tt.wantStatus)
			}
			if resp.GetExitCode() != tt.wantExit {
				t.Fatalf("exit_code = %d, want %d", resp.GetExitCode(), tt.wantExit)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// StreamOutput
// ---------------------------------------------------------------------------

func TestServer_StreamOutput(t *testing.T) {
	tests := []struct {
		name  string
		jobID string
		setup func(m *mockManager)
		code  codes.Code
		want  []byte
	}{
		{
			name:  "complete output",
			jobID: "job-1",
			setup: func(m *mockManager) {
				m.outReaderFn = func(string) (*worker.OutputReader, error) {
					return completedReader([]byte("hello world\n")), nil
				}
			},
			code: codes.OK,
			want: []byte("hello world\n"),
		},
		{
			name:  "empty output",
			jobID: "job-1",
			setup: func(m *mockManager) {
				m.outReaderFn = func(string) (*worker.OutputReader, error) {
					return completedReader(nil), nil
				}
			},
			code: codes.OK,
		},
		{
			name:  "binary data preserved",
			jobID: "job-1",
			setup: func(m *mockManager) {
				m.outReaderFn = func(string) (*worker.OutputReader, error) {
					return completedReader([]byte{0x00, 0x01, 0x02, 0xff, 0xfe}), nil
				}
			},
			code: codes.OK,
			want: []byte{0x00, 0x01, 0x02, 0xff, 0xfe},
		},
		{
			name:  "empty job_id",
			jobID: "",
			code:  codes.InvalidArgument,
		},
		{
			name:  "not found",
			jobID: "nonexistent",
			setup: func(m *mockManager) {
				m.outReaderFn = func(string) (*worker.OutputReader, error) {
					return nil, worker.ErrJobNotFound
				}
			},
			code: codes.NotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := defaultMock()
			if tt.setup != nil {
				tt.setup(mock)
			}
			client := newClient(t, mock)

			// Error cases: for server-streaming RPCs, the error may surface on Recv.
			if tt.code != codes.OK {
				stream, err := client.StreamOutput(t.Context(), &pb.StreamOutputRequest{JobId: tt.jobID})
				if err == nil {
					_, err = stream.Recv()
				}
				if got := status.Code(err); got != tt.code {
					t.Fatalf("code = %v, want %v", got, tt.code)
				}
				return
			}

			out := collectOutput(t, client, tt.jobID)
			if !bytes.Equal(out, tt.want) {
				t.Fatalf("output = %x, want %x", out, tt.want)
			}
		})
	}
}

func TestServer_StreamOutputLive(t *testing.T) {
	buf := worker.NewOutputBuffer()
	mock := defaultMock()
	mock.outReaderFn = func(string) (*worker.OutputReader, error) {
		return buf.NewReader(), nil
	}
	client := newClient(t, mock)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	stream, err := client.StreamOutput(ctx, &pb.StreamOutputRequest{JobId: "job-1"})
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}

	// Write after stream is open — proves live delivery without polling.
	buf.Write([]byte("live\n"))

	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if got := string(msg.GetData()); got != "live\n" {
		t.Fatalf("got %q, want %q", got, "live\n")
	}

	// Mark done — stream should complete with EOF.
	buf.MarkDone()

	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("expected EOF after MarkDone, got: %v", err)
	}
}

func TestServer_StreamOutputConcurrentReaders(t *testing.T) {
	buf := worker.NewOutputBuffer()
	mock := defaultMock()
	mock.outReaderFn = func(string) (*worker.OutputReader, error) {
		return buf.NewReader(), nil
	}
	client := newClient(t, mock)

	// Write in a goroutine to test live concurrent streaming.
	go func() {
		for _, line := range []string{"line1\n", "line2\n", "line3\n", "line4\n", "line5\n"} {
			buf.Write([]byte(line))
			time.Sleep(10 * time.Millisecond)
		}
		buf.MarkDone()
	}()

	want := "line1\nline2\nline3\nline4\nline5\n"

	const numReaders = 3
	var wg sync.WaitGroup
	for range numReaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := collectOutput(t, client, "job-1")
			if got := string(out); got != want {
				t.Errorf("reader got %q, want %q", got, want)
			}
		}()
	}
	wg.Wait()
}

func TestServer_StreamOutputClientCancel(t *testing.T) {
	// Buffer that never completes — simulates a long-running job.
	buf := worker.NewOutputBuffer()
	mock := defaultMock()
	mock.outReaderFn = func(string) (*worker.OutputReader, error) {
		return buf.NewReader(), nil
	}
	client := newClient(t, mock)

	// Write continuously so there's always data available.
	go func() {
		for {
			if _, err := buf.Write([]byte("x")); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	// Clean up: mark buffer done so the writer goroutine exits.
	t.Cleanup(func() { buf.MarkDone() })

	ctx, cancel := context.WithCancel(t.Context())
	stream, err := client.StreamOutput(ctx, &pb.StreamOutputRequest{JobId: "job-1"})
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}

	// Read at least one chunk to confirm streaming is active.
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("first Recv: %v", err)
	}

	// Cancel and confirm the stream terminates with Canceled.
	cancel()
	for {
		_, err := stream.Recv()
		if err != nil {
			if code := status.Code(err); code != codes.Canceled {
				t.Fatalf("expected Canceled, got: %v", err)
			}
			break
		}
	}
}

// ---------------------------------------------------------------------------
// Authorization — verifies interceptors are wired correctly end-to-end.
// The full CN × method matrix is covered in auth_test.go.
// ---------------------------------------------------------------------------

func TestServer_AuthAdminAllowed(t *testing.T) {
	mock := defaultMock()
	mock.startFn = func(string, []string) (string, error) { return "job-1", nil }
	mock.stopFn = func(string) error { return nil }
	mock.statusFn = func(string) (worker.JobStatus, int, error) { return worker.JobStatusRunning, 0, nil }
	mock.outReaderFn = func(string) (*worker.OutputReader, error) {
		return completedReader([]byte("output")), nil
	}
	client := newAuthClient(t, mock, "admin")
	ctx := t.Context()

	if _, err := client.Start(ctx, &pb.StartRequest{Command: "echo"}); err != nil {
		t.Fatalf("admin Start: %v", err)
	}
	if _, err := client.Stop(ctx, &pb.StopRequest{JobId: "job-1"}); err != nil {
		t.Fatalf("admin Stop: %v", err)
	}
	if _, err := client.GetStatus(ctx, &pb.GetStatusRequest{JobId: "job-1"}); err != nil {
		t.Fatalf("admin GetStatus: %v", err)
	}
	out := collectOutput(t, client, "job-1")
	if len(out) == 0 {
		t.Fatal("admin StreamOutput: got no output")
	}
}

func TestServer_AuthViewerBlocked(t *testing.T) {
	client := newAuthClient(t, defaultMock(), "viewer")
	ctx := t.Context()

	tests := []struct {
		name string
		call func() error
	}{
		{"Start", func() error {
			_, err := client.Start(ctx, &pb.StartRequest{Command: "echo"})
			return err
		}},
		{"Stop", func() error {
			_, err := client.Stop(ctx, &pb.StopRequest{JobId: "any"})
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := status.Code(tt.call()); got != codes.PermissionDenied {
				t.Fatalf("viewer %s: got %v, want PermissionDenied", tt.name, got)
			}
		})
	}
}

func TestServer_AuthViewerAllowed(t *testing.T) {
	mock := defaultMock()
	mock.statusFn = func(string) (worker.JobStatus, int, error) { return worker.JobStatusExited, 0, nil }
	mock.outReaderFn = func(string) (*worker.OutputReader, error) {
		return completedReader([]byte("hello\n")), nil
	}
	client := newAuthClient(t, mock, "viewer")
	ctx := t.Context()

	if _, err := client.GetStatus(ctx, &pb.GetStatusRequest{JobId: "job-1"}); err != nil {
		t.Fatalf("viewer GetStatus: %v", err)
	}
	out := collectOutput(t, client, "job-1")
	if got := string(out); got != "hello\n" {
		t.Fatalf("viewer output = %q, want %q", got, "hello\n")
	}
}

func TestServer_AuthUnknownCN(t *testing.T) {
	client := newAuthClient(t, defaultMock(), "intruder")

	_, err := client.GetStatus(t.Context(), &pb.GetStatusRequest{JobId: "any"})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("unknown CN: got %v, want PermissionDenied", got)
	}
}
