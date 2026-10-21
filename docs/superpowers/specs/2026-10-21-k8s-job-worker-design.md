# Phase 2 Design: Isolated Kubernetes Job Worker (Issue #5)

## Goal
Run each Terraform step (`init`, `plan`, `apply`) in an isolated one-shot Kubernetes `Job` (`batch/v1`) when deployed to Kubernetes (`WORKER_MODE=k8s`), while preserving the in-process worker execution loop (`WORKER_MODE=local`, default) for local development and tests.

## Architecture

### 1. Execution Flow in Kubernetes Mode (`WORKER_MODE=k8s`)
1. **Server Pod (`cmd/server` without `--job-id`):**
   - Runs the HTTP server and the background `Worker.Loop` ticker.
   - On each `Worker.Tick`:
     - Calls `Queue.Reap` to recover or fail stale jobs (`locked_at < now() - 2m`).
     - Calls `Queue.Claim` to atomically claim a pending job and set `projects.active_run_id`.
     - When `w.Dispatcher != nil`, instead of running Terraform in-process, calls `w.Dispatcher.Dispatch(ctx, *job)` to create a Kubernetes `batch/v1` `Job` named `infra-guy-job-<jobID>-<attempt>` with `args: ["--job-id", "<jobID>"]`.
     - If `Dispatch` fails, immediately fails the step on the run (`w.Service.Fail`) and completes/cleans up the job so the project lock is released and the error is visible in the Run UI.
2. **One-Shot Worker Pod (`cmd/server --job-id <id>`):**
   - Starts inside the Kubernetes `Job` pod with an isolated `emptyDir` mounted at `/work` (`WORK_DIR=/work`).
   - Connects to Postgres, builds the `Worker` dependencies, and invokes `w.ExecuteClaimed(ctx, jobID)`.
   - `ExecuteClaimed` loads the running job via `Queue.Get(ctx, jobID)`, sends an immediate `Queue.Heartbeat`, spawns the 20s heartbeat goroutine, and runs `w.runJob(ctx, *job)`:
     - Mints a short-lived GitHub App installation token.
     - Clones the repository commit into `/work/<runID>`.
     - Ensures the project's configured Terraform version binary via `terraform.Installer`.
     - Resolves and decrypts attached project contexts into `/work/<runID>/.home`.
     - Writes `~infraguy_backend_override.tf.json` pointing to `INTERNAL_BASE_URL` (e.g., `http://infra-guy:8080`), so Terraform's HTTP state backend communicates with the server pod over the cluster-internal Kubernetes Service.
     - Executes the Terraform step (`init`, `plan`, or `apply`), stores logs, `PlanFile`, `PlanText`, and `TerraformVersion` in Postgres via `w.Service.Advance`, marks the job `succeeded`, and exits the container with code `0`.

### 2. Lightweight Kubernetes Job Dispatcher (`internal/infrastructure/k8s`)
- Uses standard library `net/http` and `crypto/tls` against the in-cluster Kubernetes API (`https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT/apis/batch/v1/namespaces/{namespace}/jobs`) authenticated with the mounted ServiceAccount token (`/var/run/secrets/kubernetes.io/serviceaccount/token`) and CA bundle (`/var/run/secrets/kubernetes.io/serviceaccount/ca.crt`).
- Avoids adding heavy `k8s.io/client-go` dependencies.
- Configures the `batch/v1` `Job` with:
  - `backoffLimit: 0` (retries are managed deterministically by `postgres.Queue.Reap`).
  - `ttlSecondsAfterFinished: 300` (automatic cleanup of finished Job pods after 5 minutes).
  - `activeDeadlineSeconds: int64(StepTimeout(job.Step).Seconds()) + 60`.
  - Pod annotation `linkerd.io/inject: "disabled"` so service mesh sidecars do not keep completed Job pods alive.
  - `restartPolicy: "Never"`.
  - Ephemeral `emptyDir: {}` volume mounted at `/work`.
  - Optional secret mount (`WORKER_ENV_SECRET`, e.g., `infra-guy-env`) mounted read-only at `/run/secrets/infra-guy` and injected via `envFrom`.

### 3. Helm Chart Updates (`homelab-k8s/helm/infra-guy`)
- Add RBAC `Role` and `RoleBinding` granting the `infra-guy-sa` ServiceAccount permissions on `batch/v1` `jobs` (`create`, `get`, `list`, `watch`, `delete`) in namespace `infra-guy`.
- Configure `deployment.yaml` with:
  - `WORKER_MODE=k8s`
  - `WORKER_IMAGE="{{ .Values.image.repository }}:{{ .Values.image.tag }}"`
  - `WORKER_IMAGE_PULL_POLICY="{{ .Values.image.pullPolicy }}"`
  - `WORKER_SERVICE_ACCOUNT="{{ include "infra-guy.serviceAccountName" . }}"`
  - `WORKER_ENV_SECRET="{{ .Values.envSecret }}"`
  - `INTERNAL_BASE_URL="http://{{ include "infra-guy.fullname" . }}:{{ .Values.service.port }}"`
