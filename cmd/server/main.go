// Command server runs the JobWorker gRPC service with mTLS authentication
// and role-based authorization.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
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

	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.UnaryInterceptor(auth.UnaryInterceptor()),
		grpc.StreamInterceptor(auth.StreamInterceptor()),
	)
	pb.RegisterJobWorkerServer(srv, server.New(worker.NewJobManager(), logger))

	// Register signal handler before Serve to avoid missing signals
	// delivered between Serve starting and the goroutine being scheduled.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// TODO: Stop all running jobs before exiting. Currently child processes
	// are orphaned to init on server shutdown.
	go func() {
		<-sigCh
		logger.Info("shutting down...")

		timer := time.NewTimer(shutdownTimeout)
		defer timer.Stop()

		done := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(done)
		}()

		select {
		case <-done:
			logger.Info("graceful shutdown complete")
		case <-sigCh:
			logger.Warn("second signal received, forcing shutdown")
			srv.Stop()
		case <-timer.C:
			logger.Warn("graceful shutdown timed out, forcing stop")
			srv.Stop()
		}

		signal.Stop(sigCh)
	}()

	logger.Info("listening", "addr", *listenAddr)
	if err := srv.Serve(lis); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
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
		NextProtos:   []string{"h2"},
	}, nil
}
