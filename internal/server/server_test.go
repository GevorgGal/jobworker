package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"net"
	"strings"
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
// Test helpers
// ---------------------------------------------------------------------------

// testEnv holds a connected gRPC client and the underlying JobManager.
type testEnv struct {
	client  pb.JobWorkerClient
	manager *worker.JobManager
}

// newTestEnv creates an in-process gRPC server and client over bufconn
// with no authentication — for testing server logic in isolation.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return setupEnv(t, nil, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// newTestEnvWithAuth creates an in-process gRPC server and client with
// auth interceptors. The given CN is injected into the peer's TLS state
// via fake transport credentials so the interceptor sees it.
func newTestEnvWithAuth(t *testing.T, cn string) *testEnv {
	t.Helper()
	creds := &fakeCreds{cn: cn}
	return setupEnv(t,
		[]grpc.ServerOption{
			grpc.Creds(creds),
			grpc.UnaryInterceptor(auth.UnaryInterceptor()),
			grpc.StreamInterceptor(auth.StreamInterceptor()),
		},
		grpc.WithTransportCredentials(creds),
	)
}

func setupEnv(t *testing.T, serverOpts []grpc.ServerOption, dialOpts ...grpc.DialOption) *testEnv {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	mgr := worker.NewJobManager()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := grpc.NewServer(serverOpts...)
	pb.RegisterJobWorkerServer(srv, server.New(mgr, logger))

	go func() { _ = srv.Serve(lis) }()

	dialOpts = append(dialOpts, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}))

	conn, err := grpc.NewClient("passthrough:///bufnet", dialOpts...)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}

	t.Cleanup(func() {
		conn.Close()
		srv.Stop()
	})

	return &testEnv{
		client:  pb.NewJobWorkerClient(conn),
		manager: mgr,
	}
}

// fakeCreds implements credentials.TransportCredentials to inject a fixed
// TLS peer identity into every connection. The auth interceptor sees the CN
// without a real TLS handshake.
type fakeCreds struct {
	cn string
}

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

// collectOutput streams all output from a job and returns it. Blocks until
// the stream completes (job exits) or a 5-second timeout fires.
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

// awaitJobExit blocks until the job has fully terminated using the
// manager's WaitForExit — independent of streaming correctness.
func awaitJobExit(t *testing.T, env *testEnv, jobID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := env.manager.WaitForExit(ctx, jobID); err != nil {
		t.Fatalf("awaitJobExit(%s): %v", jobID, err)
	}
}

// ---------------------------------------------------------------------------
// Start
// ---------------------------------------------------------------------------

func TestServer_Start(t *testing.T) {
	env := newTestEnv(t)

	resp, err := env.client.Start(t.Context(), &pb.StartRequest{
		Command: "echo",
		Args:    []string{"hello"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if resp.GetJobId() == "" {
		t.Fatal("Start returned empty job ID")
	}
}

func TestServer_StartErrors(t *testing.T) {
	env := newTestEnv(t)
	ctx := t.Context()

	tests := []struct {
		name string
		req  *pb.StartRequest
		code codes.Code
	}{
		{"empty command", &pb.StartRequest{}, codes.InvalidArgument},
		{"invalid binary", &pb.StartRequest{Command: "/nonexistent/binary"}, codes.InvalidArgument},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := env.client.Start(ctx, tt.req)
			if got := status.Code(err); got != tt.code {
				t.Fatalf("Start: got %v, want %v", got, tt.code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Stop
// ---------------------------------------------------------------------------

func TestServer_Stop(t *testing.T) {
	env := newTestEnv(t)
	ctx := t.Context()

	resp, err := env.client.Start(ctx, &pb.StartRequest{
		Command: "sleep",
		Args:    []string{"60"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := resp.GetJobId()

	if _, err := env.client.Stop(ctx, &pb.StopRequest{JobId: id}); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	awaitJobExit(t, env, id)

	st, err := env.client.GetStatus(ctx, &pb.GetStatusRequest{JobId: id})
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.GetStatus() != pb.JobStatus_JOB_STATUS_STOPPED {
		t.Fatalf("status = %v, want STOPPED", st.GetStatus())
	}
}

func TestServer_StopAlreadyExited(t *testing.T) {
	env := newTestEnv(t)
	ctx := t.Context()

	resp, err := env.client.Start(ctx, &pb.StartRequest{Command: "true"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	awaitJobExit(t, env, resp.GetJobId())

	_, err = env.client.Stop(ctx, &pb.StopRequest{JobId: resp.GetJobId()})
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("Stop on exited job: got %v, want FailedPrecondition", got)
	}
}

func TestServer_StopErrors(t *testing.T) {
	env := newTestEnv(t)
	ctx := t.Context()

	tests := []struct {
		name  string
		jobID string
		code  codes.Code
	}{
		{"empty job_id", "", codes.InvalidArgument},
		{"not found", "nonexistent", codes.NotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := env.client.Stop(ctx, &pb.StopRequest{JobId: tt.jobID})
			if got := status.Code(err); got != tt.code {
				t.Fatalf("Stop: got %v, want %v", got, tt.code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GetStatus
// ---------------------------------------------------------------------------

func TestServer_GetStatusLifecycle(t *testing.T) {
	env := newTestEnv(t)
	ctx := t.Context()

	resp, err := env.client.Start(ctx, &pb.StartRequest{
		Command: "sleep",
		Args:    []string{"60"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := resp.GetJobId()

	// Running.
	st, err := env.client.GetStatus(ctx, &pb.GetStatusRequest{JobId: id})
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.GetJobId() != id {
		t.Fatalf("job_id = %q, want %q", st.GetJobId(), id)
	}
	if st.GetStatus() != pb.JobStatus_JOB_STATUS_RUNNING {
		t.Fatalf("status = %v, want RUNNING", st.GetStatus())
	}

	// Stop and verify transition.
	if _, err := env.client.Stop(ctx, &pb.StopRequest{JobId: id}); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	awaitJobExit(t, env, id)

	st, err = env.client.GetStatus(ctx, &pb.GetStatusRequest{JobId: id})
	if err != nil {
		t.Fatalf("GetStatus after stop: %v", err)
	}
	if st.GetStatus() != pb.JobStatus_JOB_STATUS_STOPPED {
		t.Fatalf("status = %v, want STOPPED", st.GetStatus())
	}
	if st.GetExitCode() != -1 {
		t.Fatalf("exit_code = %d, want -1", st.GetExitCode())
	}
}

func TestServer_GetStatusNaturalExit(t *testing.T) {
	env := newTestEnv(t)
	ctx := t.Context()

	resp, err := env.client.Start(ctx, &pb.StartRequest{
		Command: "sh",
		Args:    []string{"-c", "exit 42"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	awaitJobExit(t, env, resp.GetJobId())

	st, err := env.client.GetStatus(ctx, &pb.GetStatusRequest{JobId: resp.GetJobId()})
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.GetStatus() != pb.JobStatus_JOB_STATUS_EXITED {
		t.Fatalf("status = %v, want EXITED", st.GetStatus())
	}
	if st.GetExitCode() != 42 {
		t.Fatalf("exit_code = %d, want 42", st.GetExitCode())
	}
}

func TestServer_GetStatusErrors(t *testing.T) {
	env := newTestEnv(t)
	ctx := t.Context()

	tests := []struct {
		name  string
		jobID string
		code  codes.Code
	}{
		{"empty job_id", "", codes.InvalidArgument},
		{"not found", "nonexistent", codes.NotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := env.client.GetStatus(ctx, &pb.GetStatusRequest{JobId: tt.jobID})
			if got := status.Code(err); got != tt.code {
				t.Fatalf("GetStatus: got %v, want %v", got, tt.code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// StreamOutput
// ---------------------------------------------------------------------------

func TestServer_StreamOutput(t *testing.T) {
	env := newTestEnv(t)

	resp, err := env.client.Start(t.Context(), &pb.StartRequest{
		Command: "echo",
		Args:    []string{"hello world"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	out := collectOutput(t, env.client, resp.GetJobId())
	if got := string(out); got != "hello world\n" {
		t.Fatalf("output = %q, want %q", got, "hello world\n")
	}

	// collectOutput drains until EOF, so the job has exited. Verify clean exit.
	st, err := env.client.GetStatus(t.Context(), &pb.GetStatusRequest{JobId: resp.GetJobId()})
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.GetStatus() != pb.JobStatus_JOB_STATUS_EXITED {
		t.Fatalf("status = %v, want EXITED", st.GetStatus())
	}
	if st.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d, want 0", st.GetExitCode())
	}
}

func TestServer_StreamOutputLive(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// "exec sleep" replaces the shell process with sleep, eliminating
	// any race between echo completing and sleep starting.
	resp, err := env.client.Start(ctx, &pb.StartRequest{
		Command: "sh",
		Args:    []string{"-c", "echo live; exec sleep 60"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := resp.GetJobId()
	t.Cleanup(func() { _ = env.manager.Stop(id) })

	stream, err := env.client.StreamOutput(ctx, &pb.StreamOutputRequest{JobId: id})
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}

	// First chunk arrives while the process is still running. Use Contains
	// rather than exact match — write granularity depends on shell flushing.
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if got := string(msg.GetData()); !strings.Contains(got, "live") {
		t.Fatalf("first chunk = %q, want it to contain %q", got, "live")
	}

	// Confirm the process hasn't exited — proves streaming is live.
	st, err := env.client.GetStatus(ctx, &pb.GetStatusRequest{JobId: id})
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.GetStatus() != pb.JobStatus_JOB_STATUS_RUNNING {
		t.Fatalf("expected RUNNING, got %v", st.GetStatus())
	}
}

func TestServer_StreamOutputReplay(t *testing.T) {
	env := newTestEnv(t)

	resp, err := env.client.Start(t.Context(), &pb.StartRequest{
		Command: "echo",
		Args:    []string{"replay"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := resp.GetJobId()

	// Wait for job to complete.
	awaitJobExit(t, env, id)

	// A new stream after completion should replay all output.
	out := collectOutput(t, env.client, id)
	if got := string(out); got != "replay\n" {
		t.Fatalf("replayed output = %q, want %q", got, "replay\n")
	}
}

func TestServer_StreamOutputConcurrentReaders(t *testing.T) {
	env := newTestEnv(t)

	resp, err := env.client.Start(t.Context(), &pb.StartRequest{
		Command: "sh",
		Args:    []string{"-c", "for i in 1 2 3 4 5; do echo line$i; sleep 0.05; done"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := resp.GetJobId()

	// Exact match is safe here: each echo is a separate write syscall that
	// the buffer captures atomically, and the sleep ensures writes don't coalesce.
	want := "line1\nline2\nline3\nline4\nline5\n"

	const numReaders = 3
	var wg sync.WaitGroup
	for range numReaders {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			stream, err := env.client.StreamOutput(ctx, &pb.StreamOutputRequest{JobId: id})
			if err != nil {
				t.Errorf("StreamOutput: %v", err)
				return
			}

			var out []byte
			for {
				msg, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Errorf("Recv: %v", err)
					return
				}
				out = append(out, msg.GetData()...)
			}

			if got := string(out); got != want {
				t.Errorf("reader got %q, want %q", got, want)
			}
		})
	}
	wg.Wait()
}

func TestServer_StreamOutputEmpty(t *testing.T) {
	env := newTestEnv(t)

	resp, err := env.client.Start(t.Context(), &pb.StartRequest{Command: "true"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	out := collectOutput(t, env.client, resp.GetJobId())
	if len(out) != 0 {
		t.Fatalf("expected empty output, got %q", out)
	}
}

func TestServer_StreamOutputStderr(t *testing.T) {
	env := newTestEnv(t)

	resp, err := env.client.Start(t.Context(), &pb.StartRequest{
		Command: "sh",
		Args:    []string{"-c", "echo out; echo err >&2"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Use Contains — stdout/stderr interleaving order is not guaranteed.
	out := collectOutput(t, env.client, resp.GetJobId())
	got := string(out)
	if !strings.Contains(got, "out") || !strings.Contains(got, "err") {
		t.Fatalf("output = %q, want both stdout and stderr", got)
	}
}

func TestServer_StreamOutputBinary(t *testing.T) {
	env := newTestEnv(t)

	// Octal escapes are universally supported by printf, unlike \x hex escapes.
	resp, err := env.client.Start(t.Context(), &pb.StartRequest{
		Command: "printf",
		Args:    []string{`\000\001\002\377\376`},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	out := collectOutput(t, env.client, resp.GetJobId())
	want := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}
	if !bytes.Equal(out, want) {
		t.Fatalf("binary output = %x, want %x", out, want)
	}
}

func TestServer_StreamOutputClientCancel(t *testing.T) {
	env := newTestEnv(t)

	resp, err := env.client.Start(t.Context(), &pb.StartRequest{
		Command: "sh",
		Args:    []string{"-c", "while true; do echo x; sleep 0.1; done"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := resp.GetJobId()
	t.Cleanup(func() { _ = env.manager.Stop(id) })

	ctx, cancel := context.WithCancel(t.Context())
	stream, err := env.client.StreamOutput(ctx, &pb.StreamOutputRequest{JobId: id})
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

func TestServer_StreamOutputErrors(t *testing.T) {
	env := newTestEnv(t)
	ctx := t.Context()

	tests := []struct {
		name  string
		jobID string
		code  codes.Code
	}{
		{"empty job_id", "", codes.InvalidArgument},
		{"not found", "nonexistent", codes.NotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream, err := env.client.StreamOutput(ctx, &pb.StreamOutputRequest{JobId: tt.jobID})
			// For server-streaming RPCs, the error may surface on Recv.
			if err == nil {
				_, err = stream.Recv()
			}
			if got := status.Code(err); got != tt.code {
				t.Fatalf("StreamOutput: got %v, want %v", got, tt.code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Authorization integration — verifies interceptors are wired correctly.
// The full CN × method matrix is covered in auth_test.go; here we confirm
// end-to-end gating through a real gRPC stack.
// ---------------------------------------------------------------------------

func TestServer_AuthAdminAllowed(t *testing.T) {
	env := newTestEnvWithAuth(t, "admin")
	ctx := t.Context()

	resp, err := env.client.Start(ctx, &pb.StartRequest{Command: "true"})
	if err != nil {
		t.Fatalf("admin Start: %v", err)
	}

	awaitJobExit(t, env, resp.GetJobId())

	if _, err := env.client.GetStatus(ctx, &pb.GetStatusRequest{JobId: resp.GetJobId()}); err != nil {
		t.Fatalf("admin GetStatus: %v", err)
	}

	// Stop returns FailedPrecondition (already exited), which proves
	// the request passed through the auth interceptor to the handler.
	_, err = env.client.Stop(ctx, &pb.StopRequest{JobId: resp.GetJobId()})
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("admin Stop: got %v, want FailedPrecondition", got)
	}
}

func TestServer_AuthViewerBlocked(t *testing.T) {
	env := newTestEnvWithAuth(t, "viewer")
	ctx := t.Context()

	_, err := env.client.Start(ctx, &pb.StartRequest{Command: "echo", Args: []string{"hi"}})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("viewer Start: got %v, want PermissionDenied", got)
	}

	_, err = env.client.Stop(ctx, &pb.StopRequest{JobId: "any"})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("viewer Stop: got %v, want PermissionDenied", got)
	}
}

func TestServer_AuthViewerAllowed(t *testing.T) {
	env := newTestEnvWithAuth(t, "viewer")
	ctx := t.Context()

	// Create a job via the manager — viewer cannot create jobs through the API.
	id, err := env.manager.Start("echo", []string{"hello"})
	if err != nil {
		t.Fatalf("manager.Start: %v", err)
	}

	// Viewer can call GetStatus.
	_, err = env.client.GetStatus(ctx, &pb.GetStatusRequest{JobId: id})
	if err != nil {
		t.Fatalf("viewer GetStatus: %v", err)
	}

	// Viewer can call StreamOutput.
	out := collectOutput(t, env.client, id)
	if got := string(out); got != "hello\n" {
		t.Fatalf("viewer output = %q, want %q", got, "hello\n")
	}
}

func TestServer_AuthUnknownCN(t *testing.T) {
	env := newTestEnvWithAuth(t, "intruder")

	_, err := env.client.GetStatus(t.Context(), &pb.GetStatusRequest{JobId: "any"})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("unknown CN: got %v, want PermissionDenied", got)
	}
}
