package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"

	pb "github.com/GevorgGal/jobworker/proto/jobworker/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
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
// authenticate — TLS identity extraction and CN→role mapping.
// ---------------------------------------------------------------------------

func TestAuthenticate(t *testing.T) {
	tests := []struct {
		name     string
		ctx      context.Context
		wantRole role
		wantCode codes.Code
	}{
		{"admin CN", peerContext("admin"), roleAdmin, codes.OK},
		{"viewer CN", peerContext("viewer"), roleViewer, codes.OK},
		{"unknown CN", peerContext("intruder"), roleUnknown, codes.PermissionDenied},
		{"no peer", context.Background(), roleUnknown, codes.PermissionDenied},
		{"no TLS info", peer.NewContext(context.Background(), &peer.Peer{}), roleUnknown, codes.PermissionDenied},
		{
			"no verified chains",
			peer.NewContext(context.Background(), &peer.Peer{
				AuthInfo: credentials.TLSInfo{
					State: tls.ConnectionState{VerifiedChains: nil},
				},
			}),
			roleUnknown, codes.PermissionDenied,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := authenticate(tt.ctx)
			if tt.wantCode == codes.OK {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if r != tt.wantRole {
					t.Fatalf("role = %v, want %v", r, tt.wantRole)
				}
			} else {
				if status.Code(err) != tt.wantCode {
					t.Fatalf("error code = %v, want %v", status.Code(err), tt.wantCode)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// authorize — role × method permission checks. No TLS context needed.
// ---------------------------------------------------------------------------

func TestAuthorize(t *testing.T) {
	tests := []struct {
		name   string
		r      role
		method string
		want   codes.Code
	}{
		// Admin — allowed on everything.
		{"admin start", roleAdmin, pb.JobWorker_Start_FullMethodName, codes.OK},
		{"admin stop", roleAdmin, pb.JobWorker_Stop_FullMethodName, codes.OK},
		{"admin get status", roleAdmin, pb.JobWorker_GetStatus_FullMethodName, codes.OK},
		{"admin stream output", roleAdmin, pb.JobWorker_StreamOutput_FullMethodName, codes.OK},

		// Viewer — read-only.
		{"viewer get status", roleViewer, pb.JobWorker_GetStatus_FullMethodName, codes.OK},
		{"viewer stream output", roleViewer, pb.JobWorker_StreamOutput_FullMethodName, codes.OK},
		{"viewer start denied", roleViewer, pb.JobWorker_Start_FullMethodName, codes.PermissionDenied},
		{"viewer stop denied", roleViewer, pb.JobWorker_Stop_FullMethodName, codes.PermissionDenied},

		// Default-deny: unknown methods require admin.
		{"admin unknown method allowed", roleAdmin, "/jobworker.v1.JobWorker/UnknownRPC", codes.OK},
		{"viewer unknown method denied", roleViewer, "/jobworker.v1.JobWorker/UnknownRPC", codes.PermissionDenied},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := authorize(tt.r, tt.method)
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
// Interceptor wiring — verify the interceptors call authenticate+authorize
// and gate the handler. One allow + one deny case each is sufficient.
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
