# Taskflow Job Orchestrator (Go)

A minimal, production-ready job orchestration service written in Go.
The system accepts jobs over HTTP, processes them asynchronously using goroutines and channels, and exposes operational endpoints for health, readiness, and lifecycle management.
The focus of this project is **concurrency correctness**, **infrastructure readiness**, and **clean maintainable design**.

---

## Features

- **Asynchronous job execution** using goroutines and channels
- **Thread-safe job store** (mutex-protected)
- **Job lifecycle management** (created → started → completed/failed)
- **Job cancellation support** with context-based cancellation
- **Health and readiness endpoints** for Kubernetes/orchestrator integration
- **Graceful shutdown** handling (SIGINT/SIGTERM)
- **Race-condition safe** (validated with `go test -race`)

---

## Project Structure

**| File | Purpose |**

| **main.go** | Server initialization, HTTP routing, signal handling, graceful shutdown |
| **handlers.go** | HTTP request handlers, input validation, JSON response formatting |
| **service.go** | Job queue management, worker goroutines, concurrent state management, job execution logic |
| **service_test.go** | Unit tests for job lifecycle, concurrent submissions, cancellation, and edge cases |
| **handlers_test.go** | HTTP endpoint tests and response validation |
| **go.mod** | Go module definition and dependencies |

---

## Prerequisites

- **Go 1.20 or newer**

---

## Build

```bash
go build -o taskflow-infra
```

## Run

```bash
./taskflow-infra
```

The service runs on **http://localhost:8000**

## Run Tests

Use verbose race-enabled tests to validate concurrency:

```bash
go test -race -v ./...
```

---

## API Reference

### Create Job

**POST** `/jobs`

Creates a new asynchronous job and returns its ID and status.

**Request:**
```json
{
  "type": "email"
}
```

**Response (200 OK):**
```json
{
  "id": "123",
  "status": "pending"
}
```

**Status Codes:**
- `200 OK` - Job created successfully
- `400 Bad Request` - Invalid request payload
- `503 Service Unavailable` - Service not ready or queue is full

---

### List Jobs

**GET** `/jobs`

Returns all jobs with their current status.

**Response (200 OK):**
```json
[
  {
    "id": "123",
    "status": "completed",
    "type": "email"
  },
  {
    "id": "124",
    "status": "pending",
    "type": "email"
  }
]
```

---

### Get Job

**GET** `/jobs/{id}`

Retrieves details of a specific job.

**Response (200 OK):**
```json
{
  "id": "123",
  "status": "completed",
  "type": "email"
}
```

**Status Codes:**
- `200 OK` - Job found
- `404 Not Found` - Job does not exist

---

### Cancel Job

**DELETE** `/jobs/{id}`

Cancels a pending or running job.

**Behavior:**
- Cancels pending jobs immediately
- Signals running jobs to stop via context cancellation
- Completed jobs cannot be canceled

**Status Codes:**
- `200 OK` - Cancellation request accepted
- `400 Bad Request` - Job is already completed
- `404 Not Found` - Job does not exist

---

### Health Check

**GET** `/health`

Simple liveness probe indicating the service is alive.

**Response (200 OK):**
```json
{
  "status": "alive"
}
```

Always returns `200 OK` if the service is running.

---

### Readiness Check

**GET** `/ready`

Indicates whether the service is ready to accept new work.

**Response (200 OK):**
```json
{
  "ready": true
}
```

**Response (503 Service Unavailable):**
```json
{
  "ready": false,
  "reason": "service is shutting down"
}
```

**Returns:**
- `200 OK` - Service can accept new work
- `503 Service Unavailable` - Service not ready (queue full, shutting down, etc.)

---

## Deployment

### Build Linux Binary

```bash
GOOS=linux GOARCH=amd64 go build -o taskflow-infra
```

### Copy to EC2

```bash
scp -i key.pem taskflow-infra ec2-user@<EC2_IP>:/home/ec2-user/
```

### Run Service

**Manual:**

```bash
./taskflow-infra
```

### Verify

```bash
curl http://<EC2_IP>:8000/health
```

---

## Design Notes

### Concurrency Model

**Job Processing Pipeline:**
- Jobs are submitted via HTTP and placed into a buffered channel
- Worker goroutines consume from the channel and process jobs asynchronously
- A mutex-protected map stores job state to ensure thread-safety
- All concurrent operations are validated using Go's race detector

**Key Components:**
- **Buffered Channel** — Bounded queue for backpressure
- **Worker Pool** — Fixed number of concurrent workers
- **Mutex Lock** — Protects shared job map from concurrent access
- **Context** — Enables graceful cancellation of jobs

### Job Lifecycle

Jobs transition through well-defined states:

```
created → started → completed
              ↓
            failed
```

** State | Description :**

| **created** | Job accepted but not yet processed |
| **started** | Worker has acquired the job and is executing |
| **completed** | Job finished successfully |
| **failed** | Job encountered an error during execution |

### Job Cancellation

Cancellation is implemented using Go's `context.Context`:

- When a cancel request is received, the context is canceled
- Running workers listen on `ctx.Done()` and stop execution gracefully
- In-flight operations can check `ctx.Err()` to detect cancellation
- Pending jobs are removed from the queue immediately

**Timer Fix (Latest Update):**
The service uses properly managed timers with `time.Timer` to avoid resource leaks. Each stage of job execution uses a timer that is explicitly stopped when the context is done.

### Storage

- **In-Memory Map** — Jobs stored in a `map[string]*Job` protected by `sync.RWMutex`
- **No Persistence** — State is lost on restart (suitable for stateless deployments)
- **Thread-Safe Access** — All map operations are protected by locks

### Graceful Shutdown

When the service receives `SIGINT` or `SIGTERM`:

1. Server stops accepting new HTTP requests
2. Existing in-queue jobs are allowed to complete
3. Running jobs receive cancellation signals via context
4. Service waits for workers to finish before exiting
5. Timeout (configurable) prevents indefinite hangs

### Readiness and Liveness Probes

**Liveness (`/health`):**
- Simple check that the service process is alive
- Always returns 200 OK
- Used by orchestrators to detect dead processes

**Readiness (`/ready`):**
- Checks if the service can accept new work
- Returns 503 if queue is full or shutting down
- Used by load balancers to route traffic only to ready replicas

### Backpressure Management

The job queue is bounded to prevent unbounded memory growth:

- Queue capacity is fixed at initialization
- New requests are rejected (503) when the queue is full
- Clients must implement retry logic with exponential backoff
- This prevents cascading failures in distributed systems

### Worker Goroutines

Each worker:
- Continuously polls the job channel
- Executes job logic with a timeout
- Updates job status atomically
- Handles errors gracefully without crashing
- Exits cleanly on context cancellation

---

## Testing

The test suite validates critical aspects of the orchestrator:

### Test Coverage

**Lifecycle Tests:**
- Job creation and status transitions
- Concurrent job submissions without race conditions
- Job retrieval and enumeration

**Concurrency Tests:**
- Multiple workers processing jobs simultaneously
- Mutex protection under high contention
- No data corruption under stress
- Validated with Go race detector

**Cancellation Tests:**
- Pending jobs are canceled immediately
- Running jobs receive cancellation signals
- Completed jobs cannot be canceled
- Resources are cleaned up properly

**Edge Cases:**
- Empty job list
- Non-existent job lookups
- Duplicate job IDs
- Queue overflow scenarios

### Run Tests

Execute all tests with race detection enabled:

```bash
go test -race -v ./...
```

**Test Flags Explained:**
- `-race` — Detects data races (concurrent access issues)
- `-v` — Verbose output showing each test
- `./...` — Run all tests in the package

---

## Configuration

Environment variables and defaults:

```bash
# Port the service listens on (default: 8000)
export PORT=8000

# Number of worker goroutines (default: 10)
export WORKERS=10

# Job queue capacity (default: 100)
export QUEUE_SIZE=100

# Job execution timeout in seconds (default: 30)
export JOB_TIMEOUT=30

# Graceful shutdown timeout in seconds (default: 15)
export SHUTDOWN_TIMEOUT=15
```

---

## Performance Characteristics

- **Throughput** — Limited by worker count and job complexity
- **Latency** — Job submission is O(1); job execution is job-dependent
- **Memory** — Proportional to queue size + number of in-flight jobs
- **Concurrency Safety** — Validated with `go test -race` on every build

---

## Troubleshooting

### Service Won't Start

Check port availability:
```bash
lsof -i :8000
```

Or bind to a different port:
```bash
PORT=8001 ./taskflow-infra
```

### High Memory Usage

Reduce queue size or worker count:
```bash
QUEUE_SIZE=50 WORKERS=5 ./taskflow-infra
```

### Jobs Not Processing

Verify workers are running:
```bash
curl http://localhost:8000/ready
```

Check service logs for errors.

### Race Detector Warnings

Run tests to identify data races:
```bash
go test -race -v ./...
```

Fix the issue before deploying to production.

---

## License

[Add your license here]
