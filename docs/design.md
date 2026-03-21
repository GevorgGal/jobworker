# Design Document: Job Worker Service

## 1. Overview

A prototype job worker service that runs arbitrary Linux processes on the host machine via a gRPC API, targeting 64-bit Linux. The system consists of three components: a reusable **worker library** for job management, a **gRPC server** exposing the library over mTLS, and a **CLI client**. This targets the Level 4 requirements.

## 2. Scope

**In scope:** Start/stop/status/list/stream-output for arbitrary processes, mTLS authentication, role-based authorization derived from client certificates, efficient output streaming with concurrent client support.

**Out of scope:** cgroup-based resource control and guaranteed child process tree cleanup (Level 5). No persistence, high availability, or horizontal scaling — this is a single-node prototype. Out-of-scope items are noted as TODOs where relevant.

## 3. Architecture

```
┌───────────┐       gRPC + mTLS        ┌───────────────┐       func calls       ┌─────────────┐
│  CLI      │ ◄──────────────────────► │  gRPC Server  │ ◄────────────────────► │  Worker     │
│  Client   │                          │  (auth layer) │                        │  Library    │
└───────────┘                          └───────────────┘                        └─────────────┘
```

The **worker library** is a standalone Go package with no gRPC dependency — it manages process lifecycle and output buffering. The **gRPC server** wraps the library, handles mTLS termination, and enforces authorization via a gRPC unary/stream interceptor. The **CLI client** is a thin gRPC consumer that presents results to the user.

Jobs are stored in a `map[string]*Job` protected by `sync.RWMutex`. Each job manages its own output buffer and state synchronization independently — the map lock is never held during streaming or long operations.

_TODO: Add persistence layer so jobs survive server restarts._

_TODO: Graceful server shutdown — stop accepting new RPCs, drain active streams with a timeout, then exit._

### Project Layout

```
jobworker/
  cmd/server/       — gRPC server entrypoint
  cmd/jobctl/       — CLI client entrypoint
  pkg/worker/       — library (no gRPC dependency)
  pkg/auth/         — interceptor + role mapping
  proto/            — protobuf definitions
  certs/            — pre-generated certificates
```

## 4. API Design

### Protobuf Service

```protobuf
syntax = "proto3";
package jobworker.v1;

service JobWorker {
  rpc Start(StartRequest) returns (StartResponse);
  rpc Stop(StopRequest) returns (StopResponse);
  rpc GetStatus(GetStatusRequest) returns (GetStatusResponse);
  rpc ListJobs(ListJobsRequest) returns (ListJobsResponse);
  rpc StreamOutput(StreamOutputRequest) returns (stream StreamOutputResponse);
}

message StartRequest {
  string command = 1;
  repeated string args = 2;
}

message StartResponse {
  string job_id = 1;
}

message StopRequest {
  string job_id = 1;
}

message StopResponse {}

message GetStatusRequest {
  string job_id = 1;
}

message GetStatusResponse {
  string job_id = 1;
  JobStatus status = 2;
  int32 exit_code = 3;     // Only meaningful when status == JOB_STATUS_EXITED
  bool stopped_by_user = 4;
}

enum JobStatus {
  JOB_STATUS_UNSPECIFIED = 0;
  JOB_STATUS_RUNNING = 1;
  JOB_STATUS_EXITED = 2;
}

message ListJobsRequest {}

message ListJobsResponse {
  repeated JobSummary jobs = 1;
}

message JobSummary {
  string job_id = 1;
  JobStatus status = 2;
}

message StreamOutputRequest {
  string job_id = 1;
}

message StreamOutputResponse {
  bytes data = 1;
}
```

### Start Semantics

`Start` validates and execs the process synchronously. If exec fails (binary not found, permission denied), the RPC returns an error immediately — no job is created. Once the process is running, a UUID is assigned and returned. The process runs in the background; the client uses `GetStatus` or `StreamOutput` to follow progress. No shell is involved — `command` and `args` are passed directly to `exec`. Go's `exec.Command` performs PATH lookup by default, so both absolute paths and bare command names work.

### gRPC Error Mapping

| Scenario                      | Status Code           |
| ----------------------------- | --------------------- |
| Job not found                 | `NOT_FOUND`           |
| Unauthorized / unknown CN     | `PERMISSION_DENIED`   |
| Stop on already-exited job    | `FAILED_PRECONDITION` |
| Missing fields / exec failure | `INVALID_ARGUMENT`    |
| Internal error                | `INTERNAL`            |

## 5. Security

### TLS

TLS 1.3 only (`tls.Config{MinVersion: tls.VersionTLS13}`). Go's runtime fixes the TLS 1.3 cipher suites to AES-128-GCM, AES-256-GCM, and ChaCha20-Poly1305 — they are not configurable, and all are strong. We configure the certificate key algorithm (ECDSA P-256 with SHA-256 signatures) and the cert hierarchy.

### Certificate Hierarchy

A single self-signed CA issues all certificates:

- **CA cert** — trusted root used by both server and clients
- **Server cert** — presented by the gRPC server, signed by CA, with `localhost` SAN (sufficient for prototype; production would use proper hostnames)
- **Client certs** — presented by CLI clients, signed by the same CA

Certificates are pre-generated and committed to the repository for the prototype.

_TODO: Automate certificate generation; never commit secrets in production._

### mTLS Flow

The server sets `ClientAuth: tls.RequireAndVerifyClientCert` with the CA as the client CA pool. The client verifies the server certificate against the same CA. Both sides authenticate — no additional auth protocols on top of mTLS.

### Authorization

The client certificate's Common Name (CN) determines the role:

- CN `admin` → **admin** role: Start, Stop, GetStatus, ListJobs, StreamOutput
- CN `viewer` → **viewer** role: GetStatus, ListJobs, StreamOutput only
- Any other CN → rejected with `PERMISSION_DENIED`

Enforced in a gRPC unary/stream interceptor that extracts the CN from the peer's verified TLS certificate. Role mapping is hardcoded for the prototype. All authenticated users can see and interact with all jobs regardless of who started them.

_TODO: Externalize role mapping to configuration. Consider SAN extensions or SPIFFE IDs for identity. Add per-user job scoping using client certificate CN as owner._

## 6. Output Streaming

### Buffer Design

Each job has an append-only, in-memory byte buffer. Stdout and stderr are combined into a single stream — the spec says "output" (singular) and requires no assumptions about format. The buffer is unbounded for this prototype — a high-output process can exhaust server memory.

_TODO: Cap buffer size with backpressure or disk spill. Add TTL-based eviction for completed jobs._

### Notification Mechanism

A channel-based "close-and-replace" pattern notifies waiting readers without polling:

1. The buffer holds a `chan struct{}` ("notify channel").
2. On new data: close the current channel (waking all blocked readers), replace with a fresh one.
3. Readers `select` on both the notify channel and `ctx.Done()`, integrating cleanly with gRPC stream cancellation.

**Atomicity:** A mutex protects the buffer data and notify channel together. Readers must obtain both the data slice and the current notify channel reference under the same lock — otherwise a write between "check buffer" and "grab channel" causes missed notifications and an indefinite block.

### Stream Completion

When the process exits, the buffer is marked `done` and a final channel close wakes all readers. A reader that finds `done == true` with no unread data returns EOF.

Streaming a completed job is a valid use case: `StreamOutput` replays the full buffer then completes the stream normally.

### Reader Loop (Pseudocode)

```
offset = 0
loop:
  data, done, notifyCh = buffer.ReadFrom(offset)  // under lock
  if len(data) > 0:
    send data in ≤32KB chunks
    offset += len(data)
  if done and offset == buffer.Len():
    return EOF
  select:
    case <-notifyCh:   continue loop
    case <-ctx.Done(): return ctx.Err()
```

Output is sent as raw `bytes` — binary-safe, no encoding or framing assumptions. Chunk size is hardcoded at 32KB per gRPC message.

_TODO: Make chunk size configurable._

## 7. Job Lifecycle

### Start

1. Create `exec.Cmd` with `SysProcAttr{Setpgid: true}` (new process group).
2. Set `cmd.Stdout` and `cmd.Stderr` to the output buffer, which implements `io.Writer` — each `Write()` call appends data under the lock and fires the notification channel (Section 6).
3. Call `cmd.Start()` — if it fails, return error, no job created.
4. Assign UUID, store job, return ID to client.
5. A background goroutine calls `cmd.Wait()`, then marks the buffer as done with a final notification.

### Stop

1. Set `stopped_by_user = true` under lock before sending the signal — the background `Wait()` goroutine reads this flag when recording the final exit state, ensuring no race between stop and exit detection.
2. Send SIGTERM to the process group (`-pgid`).
3. Wait up to 5 seconds for exit.
4. If still running, send SIGKILL to the process group.

The Stop RPC blocks until the process exits or the SIGKILL timeout elapses (~5s). Stop is idempotent during the stop sequence: subsequent calls join the existing grace period. Calling Stop on an already-exited job returns `FAILED_PRECONDITION`. SIGKILL on an already-exited process is a harmless no-op (the kernel returns ESRCH).

This provides best-effort child process cleanup via process groups.

_TODO (L5): Use cgroups for guaranteed cleanup of the full process tree and resource control._

### States

Two states: **running** and **exited**. Exited jobs carry `exit_code` (from `ProcessState.ExitCode()` — Go returns -1 when killed by signal) and `stopped_by_user` (bool). The `stopped_by_user` flag is the reliable indicator of a Stop RPC — not the exit code.

## 8. CLI UX

```bash
# Start a job (everything after -- is the command + args)
$ jobctl start -- ls -la /tmp
Job started: a1b2c3d4-e5f6-7890-abcd-ef1234567890

# List all jobs
$ jobctl list
ID            STATUS
a1b2c3d4      RUNNING
f9e8d7c6      EXITED

# Check status
$ jobctl status a1b2c3d4
Status: RUNNING

# Stream output (follows until job exits or Ctrl+C)
$ jobctl logs a1b2c3d4
total 48
drwxrwxrwt 12 root root 4096 Mar 20 10:00 .
...

# Stream binary output to a file
$ jobctl logs a1b2c3d4 > output.bin

# Stop a job
$ jobctl stop a1b2c3d4
Job stopped.

# All commands require TLS certs and server address
$ jobctl --server-addr localhost:50051 \
         --ca-cert certs/ca.pem \
         --client-cert certs/admin.pem \
         --client-key certs/admin-key.pem \
         start -- sleep 60
```

The CLI writes raw bytes directly to stdout, enabling piping to files or other tools — no encoding transformation is applied. Job IDs can be specified as a prefix for convenience; the CLI resolves prefix matches client-side via `ListJobs`, and ambiguous prefixes (matching multiple jobs) return an error.

_TODO: Default cert paths and server address via environment variables or config file._
