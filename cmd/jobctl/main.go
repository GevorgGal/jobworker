// Command jobctl is the CLI client for the JobWorker gRPC service.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc/status"

	"github.com/GevorgGal/jobworker/internal/client"
)

func main() {
	serverAddr := flag.String("server-addr", "localhost:50051", "gRPC server address")
	caCert := flag.String("ca-cert", "certs/ca.pem", "path to CA certificate")
	clientCert := flag.String("client-cert", "certs/admin.pem", "path to client certificate")
	clientKey := flag.String("client-key", "certs/admin-key.pem", "path to client private key")
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	c, err := client.Dial(*serverAddr, *caCert, *clientCert, *clientKey)
	if err != nil {
		fatal(err)
	}
	defer c.Close()

	switch args[0] {
	case "start":
		runStart(c, args[1:])
	case "stop":
		runStop(c, args[1:])
	case "status":
		runStatus(c, args[1:])
	case "logs":
		runLogs(c, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: jobctl [flags] <command>

Commands:
  start [--] <cmd> [args...]   Start a new job
  stop <job-id>                Stop a running job
  status <job-id>              Get job status
  logs <job-id>                Stream job output

Flags:
`)
	flag.PrintDefaults()
}

func runStart(c *client.Client, args []string) {
	// Strip optional -- separator.
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: jobctl start [--] <command> [args...]\n")
		os.Exit(1)
	}

	id, err := c.Start(context.Background(), args[0], args[1:])
	if err != nil {
		fatal(err)
	}
	fmt.Println(id)
}

func runStop(c *client.Client, args []string) {
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: jobctl stop <job-id>\n")
		os.Exit(1)
	}

	if err := c.Stop(context.Background(), args[0]); err != nil {
		fatal(err)
	}
}

func runStatus(c *client.Client, args []string) {
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: jobctl status <job-id>\n")
		os.Exit(1)
	}

	result, err := c.GetStatus(context.Background(), args[0])
	if err != nil {
		fatal(err)
	}

	fmt.Printf("Status: %s\n", result.Status)
	if result.Status != "RUNNING" {
		fmt.Printf("Exit Code: %d\n", result.ExitCode)
	}
}

func runLogs(c *client.Client, args []string) {
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: jobctl logs <job-id>\n")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := c.StreamOutput(ctx, args[0], os.Stdout); err != nil {
		if ctx.Err() != nil {
			return // Ctrl+C — exit silently
		}
		fatal(err)
	}
}

// fatal prints a clean error message to stderr and exits with code 1.
func fatal(err error) {
	if s, ok := status.FromError(err); ok {
		fmt.Fprintf(os.Stderr, "Error: %s\n", s.Message())
	} else {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	}
	os.Exit(1)
}
