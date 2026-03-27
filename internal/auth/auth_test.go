package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	pb "github.com/GevorgGal/jobworker/proto/jobworker/v1"
)

// peerContext returns a context carrying a peer with a verified client
// certificate using the given CN. This simulates what gRPC provides
// after a successful mTLS handshake.
func peerContext(cn string) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				VerifiedChains: [][]*x509.Certificate{
					{{Subject: pkix.Name{CommonName: cn}}},
				},
			},
		},
	})
}

// ---------------------------------------------------------------------------
// authorize — covers the full CN × method matrix and all error paths.
// ---------------------------------------------------------------------------

func TestAuthorize(t *testing.T) {
	tests := []struct {
		name   string
		ctx    context.Context
		method string
		want   codes.Code
	}{
		// Admin — allowed on everything.
		{"admin start", peerContext("admin"), pb.JobWorker_Start_FullMethodName, codes.OK},
		{"admin stop", peerContext("admin"), pb.JobWorker_Stop_FullMethodName, codes.OK},
		{"admin get status", peerContext("admin"), pb.JobWorker_GetStatus_FullMethodName, codes.OK},
		{"admin stream output", peerContext("admin"), pb.JobWorker_StreamOutput_FullMethodName, codes.OK},

		// Viewer — read-only.
		{"viewer get status", peerContext("viewer"), pb.JobWorker_GetStatus_FullMethodName, codes.OK},
		{"viewer stream output", peerContext("viewer"), pb.JobWorker_StreamOutput_FullMethodName, codes.OK},
		{"viewer start denied", peerContext("viewer"), pb.JobWorker_Start_FullMethodName, codes.PermissionDenied},
		{"viewer stop denied", peerContext("viewer"), pb.JobWorker_Stop_FullMethodName, codes.PermissionDenied},

		// Default-deny: unknown methods require admin.
		{"admin unknown method allowed", peerContext("admin"), "/jobworker.v1.JobWorker/UnknownRPC", codes.OK},
		{"viewer unknown method denied", peerContext("viewer"), "/jobworker.v1.JobWorker/UnknownRPC", codes.PermissionDenied},

		// Unknown CN.
		{"unknown CN", peerContext("intruder"), pb.JobWorker_GetStatus_FullMethodName, codes.PermissionDenied},

		// Missing or malformed peer info.
		{"no peer", context.Background(), pb.JobWorker_GetStatus_FullMethodName, codes.PermissionDenied},
		{"no TLS info", peer.NewContext(context.Background(), &peer.Peer{}), pb.JobWorker_GetStatus_FullMethodName, codes.PermissionDenied},
		{
			"no verified chains",
			peer.NewContext(context.Background(), &peer.Peer{
				AuthInfo: credentials.TLSInfo{
					State: tls.ConnectionState{VerifiedChains: nil},
				},
			}),
			pb.JobWorker_GetStatus_FullMethodName,
			codes.PermissionDenied,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := authorize(tt.ctx, tt.method)
			got := codes.OK
			if err != nil {
				got = status.Code(err)
			}
			if got != tt.want {
				t.Fatalf("authorize() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Interceptor wiring — verify the interceptors call authorize and gate
// the handler. One allow + one deny case each is sufficient; the full
// matrix is covered by TestAuthorize above.
// ---------------------------------------------------------------------------

func TestUnaryInterceptor(t *testing.T) {
	interceptor := UnaryInterceptor()
	handler := func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	}

	// Allowed.
	resp, err := interceptor(
		peerContext("admin"), nil,
		&grpc.UnaryServerInfo{FullMethod: pb.JobWorker_Start_FullMethodName},
		handler,
	)
	if err != nil {
		t.Fatalf("admin Start: unexpected error: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("handler response = %v, want %q", resp, "ok")
	}

	// Denied.
	_, err = interceptor(
		peerContext("viewer"), nil,
		&grpc.UnaryServerInfo{FullMethod: pb.JobWorker_Start_FullMethodName},
		handler,
	)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer Start: got %v, want PermissionDenied", err)
	}
}

// mockStream implements grpc.ServerStream for testing — only Context() is called.
type mockStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (m *mockStream) Context() context.Context { return m.ctx }

func TestStreamInterceptor(t *testing.T) {
	interceptor := StreamInterceptor()
	called := false
	handler := func(srv any, stream grpc.ServerStream) error {
		called = true
		return nil
	}

	// Allowed.
	err := interceptor(
		nil, &mockStream{ctx: peerContext("admin")},
		&grpc.StreamServerInfo{FullMethod: pb.JobWorker_StreamOutput_FullMethodName},
		handler,
	)
	if err != nil {
		t.Fatalf("admin StreamOutput: unexpected error: %v", err)
	}
	if !called {
		t.Fatal("handler was not called")
	}

	// Denied.
	called = false
	err = interceptor(
		nil, &mockStream{ctx: peerContext("viewer")},
		&grpc.StreamServerInfo{FullMethod: pb.JobWorker_Stop_FullMethodName},
		handler,
	)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer Stop: got %v, want PermissionDenied", err)
	}
	if called {
		t.Fatal("handler should not have been called on denied request")
	}
}
