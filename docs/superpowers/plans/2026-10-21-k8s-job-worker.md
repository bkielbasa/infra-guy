# Phase 2: Isolated Kubernetes Job Worker Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run each Terraform step (`init`, `plan`, `apply`) inside an isolated one-shot Kubernetes `batch/v1` `Job` when `WORKER_MODE=k8s`, while preserving in-process execution (`WORKER_MODE=local` default) for local development.

**Architecture:** When `w.Dispatcher != nil`, `Worker.Tick` claims a job in Postgres and dispatches a Kubernetes `Job` (`--job-id <id>`) instead of running Terraform in the server process. The spawned `Job` pod runs `cmd/server --job-id <id>`, which invokes `Worker.ExecuteClaimed(ctx, jobID)` to load the claimed job, maintain heartbeats, clone the repo into an ephemeral `emptyDir`, talk to the server's HTTP state backend via `INTERNAL_BASE_URL`, advance the run, mark the job succeeded, and exit.

**Tech Stack:** Go 1.27, `pgx/v5`, standard library `net/http` + `crypto/tls` + `encoding/json` for the Kubernetes `batch/v1` `Job` REST API.

---

### Task 1: Queue `Get` & Worker `Dispatcher` / `ExecuteClaimed`

**Files:**
- Modify: `internal/infrastructure/postgres/queue.go`
- Modify: `internal/infrastructure/postgres/worker_queue.go`
- Modify: `internal/infrastructure/postgres/queue_test.go`
- Modify: `internal/application/worker/worker.go`
- Modify: `internal/application/worker/worker_test.go`

- [ ] **Step 1: Add `Get(ctx, jobID)` to `postgres.Queue` and `postgres.WorkerQueue`**
  - In `internal/infrastructure/postgres/queue.go`, add:
    ```go
    func (q *Queue) Get(ctx context.Context, jobID int64) (*Job, error) {
    	var job Job
    	err := q.pool.QueryRow(ctx, `
    		SELECT id, run_id, step_name, attempts
    		FROM jobs
    		WHERE id = $1 AND status = 'running'`, jobID).Scan(&job.ID, &job.RunID, &job.Step, &job.Attempts)
    	if errors.Is(err, pgx.ErrNoRows) {
    		return nil, nil
    	}
    	if err != nil {
    		return nil, err
    	}
    	return &job, nil
    }
    ```
  - In `internal/infrastructure/postgres/worker_queue.go`, add:
    ```go
    func (q WorkerQueue) Get(ctx context.Context, jobID int64) (*worker.Job, error) {
    	job, err := q.Queue.Get(ctx, jobID)
    	if job == nil {
    		return nil, err
    	}
    	out := toWorkerJob(*job)
    	return &out, err
    }
    ```
- [ ] **Step 2: Update `worker.Queue`, add `worker.Dispatcher`, and add `Worker.ExecuteClaimed`**
  - In `internal/application/worker/worker.go`:
    - Add `Get(ctx context.Context, jobID int64) (*Job, error)` to `Queue` interface.
    - Define:
      ```go
      type Dispatcher interface {
      	Dispatch(ctx context.Context, job Job) error
      }
      ```
    - Add `Dispatcher Dispatcher` field to `Worker`.
    - In `Worker.Tick(ctx)`:
      - After `job, err := w.Queue.Claim(ctx, w.WorkerID, w.Projects)`, if `w.Dispatcher != nil`:
        ```go
        if w.Dispatcher != nil {
        	if err := w.Dispatcher.Dispatch(ctx, *job); err != nil {
        		if ferr := w.Service.Fail(ctx, job.RunID, job.Step, "dispatch worker job: "+err.Error()); ferr != nil {
        			return ferr
        		}
        		return w.completeAndCleanup(ctx, *job)
        	}
        	return nil
        }
        ```
    - Add `ExecuteClaimed(ctx context.Context, jobID int64) error`:
      ```go
      func (w *Worker) ExecuteClaimed(ctx context.Context, jobID int64) error {
      	job, err := w.Queue.Get(ctx, jobID)
      	if err != nil {
      		return err
      	}
      	if job == nil {
      		return fmt.Errorf("job %d not found or not running", jobID)
      	}
      	_ = w.Queue.Heartbeat(ctx, job.ID)
      	hbCtx, cancel := context.WithCancel(ctx)
      	defer cancel()
      	go w.heartbeat(hbCtx, job.ID)
      	return w.runJob(ctx, *job)
      }
      ```
- [ ] **Step 3: Write unit tests in `worker_test.go` and `queue_test.go`**
  - Test `Tick` with `Dispatcher != nil` dispatches the claimed job without running `Executor.Run` in-process.
  - Test `Tick` when `Dispatcher.Dispatch` returns an error marks the run failed and completes the job.
  - Test `ExecuteClaimed` loads the claimed job via `Queue.Get`, runs `Executor.Run`, advances the run, and completes the job.
  - Run `go test ./internal/application/worker/... ./internal/infrastructure/postgres/...` and commit.

---

### Task 2: Kubernetes `batch/v1` Job Dispatcher (`internal/infrastructure/k8s`)

**Files:**
- Create: `internal/infrastructure/k8s/dispatcher.go`
- Create: `internal/infrastructure/k8s/dispatcher_test.go`

- [ ] **Step 1: Write unit tests in `internal/infrastructure/k8s/dispatcher_test.go`**
  - Using `httptest.NewServer`, verify `JobDispatcher.Dispatch(ctx, job)`:
    - Sends `POST /apis/batch/v1/namespaces/infra-guy/jobs` with `Authorization: Bearer <token>` and `Content-Type: application/json`.
    - Produces a `batch/v1` `Job` named `infra-guy-job-<id>-<attempts>` with `backoffLimit: 0`, `ttlSecondsAfterFinished: 300`, `activeDeadlineSeconds: int64(worker.StepTimeout(job.Step).Seconds()) + 60`.
    - Sets pod template `restartPolicy: "Never"`, `serviceAccountName`, annotations (`linkerd.io/inject: "disabled"`), container `worker` with `image`, `imagePullPolicy`, `args: ["--job-id", "<id>"]`, sorted `env`, `envFrom` (`secretRef`), and volumes (`work` `emptyDir` mounted at `/work`, plus `env-secret` secret mounted read-only at `/run/secrets/infra-guy` when `EnvSecretName` is set).
    - Treats HTTP `201 Created` and `409 Conflict` as success, and returns an error including status code and response body on `403 Forbidden` / `500 Internal Server Error`.
- [ ] **Step 2: Implement `internal/infrastructure/k8s/dispatcher.go`**
  - Implement `JobDispatcher` and `NewInClusterDispatcher(cfg Config) (*JobDispatcher, error)`.
  - Run `go test ./internal/infrastructure/k8s/...` and commit.

---

### Task 3: Server CLI `--job-id` Mode, `INTERNAL_BASE_URL`, & `WORKER_MODE=k8s` Wiring

**Files:**
- Modify: `cmd/server/main.go`
- Create: `cmd/server/main_test.go`

- [ ] **Step 1: Update `cmd/server/main.go` and add unit tests in `cmd/server/main_test.go`**
  - Support `--job-id <id>` CLI flag (and `WORKER_JOB_ID` env var fallback).
  - Support `INTERNAL_BASE_URL` env var override in `internalBaseURL(addr string)` (trimming trailing slash, falling back to `http://127.0.0.1:<port>`).
  - When `jobID > 0`:
    - Skip `postgres.Migrate(ctx, pool)` and execute `w.ExecuteClaimed(ctx, jobID)`, exiting on completion.
  - When `jobID == 0` and `WORKER_MODE == "k8s"`:
    - Construct `k8s.NewInClusterDispatcher` with `WORKER_IMAGE`, `WORKER_IMAGE_PULL_POLICY`, `WORKER_K8S_NAMESPACE`, `WORKER_SERVICE_ACCOUNT`, `WORKER_ENV_SECRET`, and pass-through environment variables (`DATABASE_URL`, `PUBLIC_BASE_URL`, `INTERNAL_BASE_URL`, `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY_FILE`, `GITHUB_APP_WEBHOOK_SECRET`, `GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET`, `WORK_DIR=/work`) plus optional `CONTEXT_ENCRYPTION_KEY` if set in environment.
    - Assign `w.Dispatcher = dispatcher`.
  - Run `go test ./...` and commit.
