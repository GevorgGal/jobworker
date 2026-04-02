# Job Worker Service

A prototype job worker service that runs arbitrary Linux processes on the host machine via a gRPC API. Consists of three components: a reusable **worker library**, a **gRPC server** with mTLS and role-based authorization, and a **CLI client**.

See [docs/design.md](docs/design.md) for the full design document.

## Prerequisites

- Go 1.25+
- OpenSSL (only if regenerating certificates)

## Build

```bash
make build
```

This produces `bin/server` and `bin/jobctl`. Add `bin/` to your PATH or use the full path directly:

```bash
export PATH=$PWD/bin:$PATH
```

## Certificates

Pre-generated certificates are included in `certs/` for the prototype. To regenerate:

```bash
rm certs/*.pem
make certs
```

The certificate hierarchy (ECDSA P-256, TLS 1.3):
- **CA** — self-signed root
- **Server** — `localhost` SAN, used by `bin/server`
- **Admin client** (CN=admin) — full access: start, stop, status, logs
- **Viewer client** (CN=viewer) — read-only: status, logs

## Running

Start the server:

```bash
bin/server
```

The server listens on `localhost:50051` by default. All flags have sensible defaults:

```
--listen-addr   gRPC listen address    (default "localhost:50051")
--ca-cert       path to CA certificate (default "certs/ca.pem")
--server-cert   path to server cert    (default "certs/server.pem")
--server-key    path to server key     (default "certs/server-key.pem")
```

## CLI Usage

The CLI (`bin/jobctl`) connects using the admin certificate by default — zero flags needed for local use.

```bash
# Start a job
jobctl start -- ls -la /tmp
# a1b2c3d4-e5f6-7890-abcd-ef1234567890

# Check status
jobctl status a1b2c3d4-e5f6-7890-abcd-ef1234567890
# Status: RUNNING

# Stream output (follows until job exits or Ctrl+C)
jobctl logs a1b2c3d4-e5f6-7890-abcd-ef1234567890

# Stream binary output to a file
jobctl logs a1b2c3d4-e5f6-7890-abcd-ef1234567890 > output.bin

# Stop a job
jobctl stop a1b2c3d4-e5f6-7890-abcd-ef1234567890

# Pipe start into logs
jobctl start -- sleep 60 | xargs jobctl logs
```

Override defaults for non-standard setups:

```bash
jobctl --server-addr remote:50051 \
       --client-cert certs/viewer.pem \
       --client-key certs/viewer-key.pem \
       status a1b2c3d4-e5f6-7890-abcd-ef1234567890
```

## Testing

```bash
make test
```

Runs all tests with the race detector enabled.
