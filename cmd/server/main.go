// Command server runs the JobWorker gRPC service with mTLS authentication
// and role-based authorization.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/GevorgGal/jobworker/internal/auth"
	"github.com/GevorgGal/jobworker/internal/server"
	"github.com/GevorgGal/jobworker/pkg/worker"
	pb "github.com/GevorgGal/jobworker/proto/jobworker/v1"
)

// shutdownTimeout is the maximum time to wait for active RPCs to complete
// during graceful shutdown before forcing a stop.
const shutdownTimeout = 10 * time.Second

func main() {
	listenAddr := flag.String("listen-addr", "localhost:50051", "gRPC listen address")
	caCert := flag.String("ca-cert", "certs/ca.pem", "path to CA certificate")
	serverCert := flag.String("server-cert", "certs/server.pem", "path to server certificate")
	serverKey := flag.String("server-key", "certs/server-key.pem", "path to server private key")
	flag.Parse()

	logger := slog.Default()

	tlsCfg, err := buildTLSConfig(*caCert, *serverCert, *serverKey)
	if err != nil {
		logger.Error("TLS setup failed", "error", err)
		os.Exit(1)
	}

	lis, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		logger.Error("failed to listen", "addr", *listenAddr, "error", err)
		os.Exit(1)
	}

	manager := worker.NewJobManager()

	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.UnaryInterceptor(auth.UnaryInterceptor()),
		grpc.StreamInterceptor(auth.StreamInterceptor()),
	)
	pb.RegisterJobWorkerServer(srv, server.New(manager, logger))

	// Register signal handler before Serve to avoid missing signals
	// delivered between Serve starting and the goroutine being scheduled.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	var forced atomic.Bool

	go func() {
		<-sigCh
		logger.Info("shutting down...")

		// Stop all running jobs first — this marks output buffers as done,
		// causing active StreamOutput RPCs to receive EOF and complete
		// naturally, allowing GracefulStop to drain quickly.
		manager.StopAll()

		// Watchdog: force-stop on second signal or timeout.
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)

		go func() {
			select {
			case <-sigCh:
				logger.Warn("second signal received, forcing shutdown")
				forced.Store(true)
				srv.Stop()
			case <-ctx.Done():
				if ctx.Err() == context.DeadlineExceeded {
					logger.Warn("graceful shutdown timed out, forcing stop")
					forced.Store(true)
					srv.Stop()
				}
			}
		}()

		srv.GracefulStop()
		cancel() // Disarms the watchdog.
	}()

	logger.Info("listening", "addr", *listenAddr, "ca", *caCert, "cert", *serverCert)
	if err := srv.Serve(lis); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}

	// Serve returns nil after GracefulStop or Stop.
	if forced.Load() {
		os.Exit(1)
	}
	logger.Info("server stopped")
}

// buildTLSConfig creates a TLS 1.3 mTLS configuration. The server
// presents its own certificate and requires clients to present a
// certificate signed by the given CA.
func buildTLSConfig(caPath, certPath, keyPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
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
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}
