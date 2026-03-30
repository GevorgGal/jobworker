// Package auth provides gRPC interceptors that enforce role-based access
// control using the client certificate's Common Name (CN) from mTLS.
package auth

import (
	"context"

	pb "github.com/GevorgGal/jobworker/proto/jobworker/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// role represents an authorization level derived from a client certificate CN.
type role int

const (
	roleUnknown role = iota // zero value; never mapped to a CN
	roleAdmin               // Start, Stop, GetStatus, StreamOutput
	roleViewer              // GetStatus, StreamOutput
)

// cnToRole maps client certificate Common Names to roles.
// TODO: Externalize to configuration.
var cnToRole = map[string]role{
	"admin":  roleAdmin,
	"viewer": roleViewer,
}

// viewerAllowed lists methods accessible to non-admin roles. Methods not in
// this set require admin. This defaults to deny — if a new RPC is added
// without updating this map, it is only accessible to admins.
var viewerAllowed = map[string]bool{
	pb.JobWorker_GetStatus_FullMethodName:    true,
	pb.JobWorker_StreamOutput_FullMethodName: true,
}

// errPermissionDenied is a single, generic error returned for all authorization
// failures. Using one message avoids leaking whether the failure was due to
// missing TLS, unknown CN, or insufficient role.
var errPermissionDenied = status.Error(codes.PermissionDenied, "permission denied")

// UnaryInterceptor returns a grpc.UnaryServerInterceptor that enforces
// role-based access control.
func UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		r, err := authenticate(ctx)
		if err != nil {
			return nil, err
		}
		if err := authorize(r, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor returns a grpc.StreamServerInterceptor that enforces
// role-based access control.
func StreamInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv any,
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		r, err := authenticate(ss.Context())
		if err != nil {
			return err
		}
		if err := authorize(r, info.FullMethod); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// authenticate extracts the client's role from the peer's TLS certificate.
// Returns a gRPC status error if the peer is missing, has no TLS info,
// or presents an unknown CN.
func authenticate(ctx context.Context) (role, error) {
	// Extract peer from gRPC context.
	p, ok := peer.FromContext(ctx)
	if !ok {
		return 0, errPermissionDenied
	}

	// Verify the peer connected over TLS.
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return 0, errPermissionDenied
	}

	// Ensure the client certificate was verified against our CA.
	chains := tlsInfo.State.VerifiedChains
	if len(chains) == 0 || len(chains[0]) == 0 {
		return 0, errPermissionDenied
	}

	// The leaf certificate's CN determines identity.
	cn := chains[0][0].Subject.CommonName

	// Map CN to a known role.
	r, known := cnToRole[cn]
	if !known {
		return 0, errPermissionDenied
	}

	return r, nil
}

// authorize checks whether the given role is allowed to call the method.
func authorize(r role, method string) error {
	// Non-admin roles can only access explicitly allowed methods.
	if r == roleAdmin || viewerAllowed[method] {
		return nil
	}

	return errPermissionDenied
}
