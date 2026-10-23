# Database & Business Logic Layer Separation — Design Spec

**Date:** 2026-10-23
**Status:** Draft for Review

## 1. Goal & Motivation

Separate database/persistence logic (`internal/infrastructure/postgres`) from business, cryptographic, and orchestration logic (`internal/domain/*`, `internal/application/*`), and enforce a strict three-tier boundary so transport and worker packages (`internal/interfaces/http`, `internal/application/worker`) only interact with Application Services — never raw repositories.

### Current Boundary Violations Identified

1. **`internal/infrastructure/postgres/context_repo.go`**
   - Owns `*crypto.Sealer`, seals secrets on `UpsertEntry`, conditionally decrypts secrets in `Get`/`ListByProject(ctx, id, decryptSecrets bool)`, infers `Secret` from `EntryKind`, and deduplicates `contextIDs` in `SetProjectContexts`.
2. **`internal/infrastructure/postgres/queue.go` & `worker_queue.go`**
   - `Queue.Claim` checks `p.Snapshot().Locked` outside domain `p.ClaimActiveRun`, hardcodes `5 seconds` retry delay, and accepts an unused `projects project.Repository` parameter.
   - `Queue.Reap` hardcodes retry rules (`job.Step == run.StepApply || job.Attempts >= 2`) and user-facing failure messages (`"worker lost the step"`, `"worker lost the apply; check state before starting another run"`), which are also duplicated in `internal/application/worker/worker.go`.
   - `postgres.WorkerQueue` exists only to translate `postgres.Job` to `worker.Job`.
3. **`internal/infrastructure/postgres/identity_repo.go`**
   - `UpsertUser` calls domain constructor `user.GitHubIdentity(out.ID, out.GitHubUserID)`.
   - `ConsumeOAuthState` evaluates expiration (`!expires.Before(now)`).
   - `postgres.Session` duplicates `authapp.Session`, requiring a 55-line `identityAdapter` in `cmd/server/main.go`.
4. **`internal/infrastructure/postgres/project_repo.go` & `state_repo.go`**
   - `ProjectRepo.Save` applies `firstNonEmptyVersion` default logic in SQL persistence.
   - `StateRepo.Load` constructs a new domain `state.New(projectID)` on `pgx.ErrNoRows` instead of returning `(nil, nil)` like all other repositories.
5. **Direct Repository Access from HTTP, Worker & Cross-Service Callers**
   - `internal/interfaces/http/server.go` passes `cfg.Projects.Projects` (`project.Repository`) to `httpapi.New`.
   - `internal/interfaces/http/webhook.go` calls `a.projects.FindByRepo`, mutates `Snapshot.Repo`, calls `project.Restore`, and saves via `a.projects.Save`.
   - `internal/interfaces/http/ui.go` reaches into `u.cfg.States.States` (`state.Repository`) for `ForceUnlock` and `tabDependencies`.
   - `internal/application/worker/worker.go` holds raw `Projects project.Repository` and `Runs run.Repository` alongside `Service *runapp.Service`, mutating `r.BeginStep` and calling `w.Runs.Save` directly.

---

## 2. Target Architecture

```
┌──────────────────────────────────────────────────────────┐
│  Transport / Entrypoints                                 │
│  cmd/server, internal/interfaces/http, application/worker│
└────────────────────────────┬─────────────────────────────┘
                             │ calls only Application Services
                             ▼
┌──────────────────────────────────────────────────────────┐
│  Application Services (internal/application/*)           │
│  Orchestration, crypto sealing, OAuth expiry, retry/reap │
│  policies, repo sync, cross-entity coordination          │
└─────────────┬──────────────────────────────┬─────────────┘
              │ mutates/validates            │ calls Repository interfaces
              ▼                              ▼
┌────────────────────────────┐ ┌───────────────────────────┐
│  Domain (internal/domain/*)│ │  Persistence              │
│  Pure entities, value      │ │  (infrastructure/postgres)│
│  objects, invariants       │ │  Pure SQL & transactions  │
└────────────────────────────┘ └───────────────────────────┘
```

---

## 3. Component Design

### 3.1 Context Encryption & Deduplication (`domain/tfcontext`, `application/tfcontext`, `postgres/context_repo.go`)

- **Domain (`internal/domain/tfcontext/context.go`)**:
  - Add `Ciphertext []byte` to `tfcontext.Entry`.
  - Update `tfcontext.Repository` interface:
    - `Get(ctx context.Context, id ID) (*Context, error)` (drop `decryptSecrets bool`).
    - `ListByProject(ctx context.Context, projectID project.ID) ([]*Context, error)` (drop `decryptSecrets bool`).
- **Application (`internal/application/tfcontext/service.go`)**:
  - Add `Sealer SecretSealer` interface (`Seal(plaintext []byte) ([]byte, error)` and `Open(ciphertext []byte) ([]byte, error)`) to `tfcontextapp.Service`.
  - In `UpsertEntry`: construct entry via `tfcontext.NewEntry`; if `entry.Secret`, seal `[]byte(value)` into `entry.Ciphertext` and clear `entry.Value` before calling `s.Contexts.UpsertEntry`.
  - In `SetProjectContexts`: filter empty IDs, deduplicate `contextIDs` preserving order, verify each belongs to `tenantID`, then call `s.Contexts.SetProjectContexts(ctx, projectID, deduped)`.
  - In `ResolveForProject`: call `s.Contexts.ListByProject(ctx, projectID)`, decrypt any secret entry's `Ciphertext` using `s.Sealer.Open`, populate `Value`, and pass to `tfcontext.Resolve(contexts)`.
- **Postgres (`internal/infrastructure/postgres/context_repo.go`)**:
  - `NewContextRepo(pool *pgxpool.Pool) *ContextRepo` — remove `*crypto.Sealer` dependency.
  - `UpsertEntry` persists `e.Value` (`value_text`) and `e.Ciphertext` (`value_ciphertext`) as given.
  - `loadEntries` scans `value_text` and `value_ciphertext`, sets `e.Secret = e.Kind == tfcontext.KindSecretEnv || e.Kind == tfcontext.KindSecretFile`, and returns entries without decrypting.
  - `SetProjectContexts` deletes existing mappings and inserts the provided `contextIDs` slice at index `0..n-1`.

### 3.2 Job Queue, Claim & Reap (`domain/project`, `domain/run`, `application/worker`, `postgres/queue.go`)

- **Domain (`internal/domain/project/project.go` & `internal/domain/run/run.go`)**:
  - Add `ErrLocked = errors.New("project is locked")` to `internal/domain/project`.
  - Update `Project.ClaimActiveRun(runID string) error` to return `ErrLocked` first if `p.locked` is true.
  - Add `Job` and `ReapDecision` to `internal/domain/run`:
    ```go
    type Job struct {
        ID       int64
        RunID    ID
        Step     StepName
        Attempts int
    }

    type ReapDecision struct {
        Fail   bool
        Reason string
    }
    ```
- **Postgres (`internal/infrastructure/postgres/queue.go`)**:
  - Delete `internal/infrastructure/postgres/worker_queue.go`.
  - `Queue.Claim(ctx context.Context, workerID string, retryDelay time.Duration) (*run.Job, error)`:
    - Removes unused `projects project.Repository` parameter.
    - Calls `p.ClaimActiveRun(string(job.RunID))`.
    - If `errors.Is(claimErr, project.ErrLocked) || errors.Is(claimErr, project.ErrBusy)`, updates `available_at = now() + make_interval(secs => $2)` using `retryDelay.Seconds()`, commits, and returns `(nil, nil)`.
  - `Queue.Reap(ctx context.Context, staleAfter time.Duration, decide func(run.Job) run.ReapDecision) (requeue []run.Job, failed []run.Job, err error)`:
    - Queries stale running jobs with `FOR UPDATE SKIP LOCKED`.
    - For each stale job, invokes `d := decide(job)`.
    - If `d.Fail`, updates `status = 'failed', last_error = $2` with `d.Reason` and appends to `failed`.
    - Otherwise updates `status = 'pending', locked_at = NULL` and appends to `requeue`.
- **Worker (`internal/application/worker/worker.go`)**:
  - Use `run.Job` (aliased as `worker.Job = run.Job` so external callers/tests remain ergonomic).
  - Define `const ClaimRetryDelay = 5 * time.Second` and `const MaxStepAttempts = 2`.
  - Define `ReapPolicy(job run.Job) run.ReapDecision`:
    - If `job.Step == run.StepApply`: returns `ReapDecision{Fail: true, Reason: "worker lost the apply; check state before starting another run"}`.
    - If `job.Attempts >= MaxStepAttempts`: returns `ReapDecision{Fail: true, Reason: "worker lost the step"}`.
    - Otherwise: returns `ReapDecision{Fail: false}`.
  - In `Worker.Tick`: call `w.Queue.Reap(ctx, StaleAfter, ReapPolicy)` and use `ReapPolicy(job).Reason` when calling `w.Service.Fail`.

### 3.3 Identity Store & Sessions (`domain/user`, `application/auth`, `postgres/identity_repo.go`)

- **Domain (`internal/domain/user/user.go`)**:
  - Add `Session` struct to `internal/domain/user`:
    ```go
    type Session struct {
        ID          string
        UserID      string
        TenantID    string
        Role        string
        Login       string
        AccessToken string
        ExpiresAt   time.Time
    }
    ```
  - Alias `type Session = user.Session` in `internal/application/auth`.
- **Application (`internal/application/auth/service.go`)**:
  - Update `Store` interface:
    - `UpsertUser(ctx context.Context, u user.User, ident user.Identity, now time.Time) (user.User, error)`
    - `ConsumeOAuthState(ctx context.Context, state string) (expiresAt time.Time, ok bool, err error)`
  - In `Complete`:
    - Call `expiresAt, ok, err := s.Store.ConsumeOAuthState(ctx, state)`. Return `errOAuthState` if `!ok || expiresAt.Before(now)`.
    - Build `ident := user.GitHubIdentity(candidateID, ghID)` and pass to `s.Store.UpsertUser(ctx, u, ident, now)` (the repo binds `ident.UserID = stored.ID` if an existing user row's ID was preserved on conflict).
- **Postgres (`internal/infrastructure/postgres/identity_repo.go`)**:
  - `UpsertUser(ctx context.Context, u user.User, ident user.Identity, now time.Time) (user.User, error)` persists `u` and `ident` (with `ident.UserID = out.ID`) without calling `user.GitHubIdentity`.
  - `ConsumeOAuthState(ctx context.Context, state string) (time.Time, bool, error)` executes `DELETE FROM oauth_states WHERE state = $1 RETURNING expires_at` and returns `( utcTime(expires), true, nil )` or `( time.Time{}, false, nil )` on `pgx.ErrNoRows`.
  - `SaveSession` and `GetSession` accept/return `user.Session`.
- **Entrypoint (`cmd/server/main.go`)**:
  - Delete `identityAdapter` and pass `postgres.NewIdentityStore(pool)` directly to `authapp.Service`, `installation.Service`, and `tenantTokenSource`.

### 3.4 Project & State Repositories and Service Methods (`domain/project`, `application/project`, `application/state`, `postgres/project_repo.go`, `postgres/state_repo.go`)

- **Postgres (`internal/infrastructure/postgres/project_repo.go` & `state_repo.go`)**:
  - Remove `firstNonEmptyVersion` from `ProjectRepo.Save` (domain `project.New` and `project.Restore` already set `DefaultTerraformVersion`).
  - Change `StateRepo.Load(ctx, projectID)` to return `(nil, nil)` when `errors.Is(err, pgx.ErrNoRows)`.
- **Domain (`internal/domain/project/project.go`)**:
  - Add `RenameRepo(owner, name string, now time.Time) error` to `project.Project` so repository renames are validated and applied through the domain aggregate rather than mutating a `Snapshot` struct in `webhook.go`.
- **State Application Service (`internal/application/state/service.go`)**:
  - `loadState` already handles `st == nil` by returning `state.New(projectID)`.
  - Add service methods so external packages never touch `state.Repository` directly:
    - `Outputs(ctx context.Context, projectID string) (map[string]state.OutputValue, error)`
    - `ChangedOutputsSincePrevious(ctx context.Context, projectID string) (map[string]bool, error)`
    - `ForceUnlock(ctx context.Context, projectID string) error`
- **Project Application Service (`internal/application/project/service.go`)**:
  - Add `States StateForceUnlocker` (`ForceUnlock(ctx context.Context, projectID string) error`) field on `projectapp.Service`.
  - Change `ForceUnlock(ctx context.Context, id project.ID) error` to use `s.States.ForceUnlock(ctx, string(id))` instead of taking a `state.Repository` argument from HTTP.
  - Add `SyncAndFindByRepo(ctx context.Context, tenantID string, repoID int64, ownerLogin, repoName string) ([]*project.Project, error)` to `projectapp.Service`:
    - Calls `s.Projects.FindByRepo(ctx, tenantID, repoID)`.
    - If `ownerLogin != ""` and `repoName != ""`, updates any project whose `Repo.Owner != ownerLogin || Repo.Name != repoName` via `p.RenameRepo(ownerLogin, repoName, s.now())` and `s.Projects.Save(ctx, p)`.

### 3.5 Run Service, Dependency Service, Worker & HTTP Boundaries

- **Run Application Service (`internal/application/run/service.go`)**:
  - Add `BeginStep(ctx context.Context, id run.ID, step run.StepName) (*run.Run, error)`:
    - Loads `r` via `s.Runs.Get(ctx, id)`.
    - If the target step is not already `StepRunning`, calls `r.BeginStep(step, s.now())` and saves via `s.Runs.Save(ctx, r)`.
    - Returns `r`.
- **Worker (`internal/application/worker/worker.go`)**:
  - Replace `Projects project.Repository` with `Projects ProjectGetter` (`Get(ctx context.Context, id project.ID) (*project.Project, error)` — satisfied by `*projectapp.Service`).
  - Remove `Runs run.Repository` from `Worker`; use `w.Service.BeginStep(ctx, job.RunID, job.Step)` and `w.Service.Get(ctx, job.RunID)`.
- **Dependency Application Service (`internal/application/dependency/service.go`)**:
  - Replace `Projects project.Repository` with `Projects ProjectGetter` (satisfied by `*projectapp.Service`).
  - Replace `States state.Repository` with `States StateReader` (`Outputs(ctx, projectID string) (map[string]state.OutputValue, error)` and `ChangedOutputsSincePrevious(ctx, projectID string) (map[string]bool, error)` — satisfied by `*stateapp.Service`).
- **HTTP Layer (`internal/interfaces/http/server.go`, `state.go`, `webhook.go`, `ui.go`)**:
  - `httpapi.New` accepts `projects ProjectMatcher` (`SyncAndFindByRepo(ctx context.Context, tenantID string, repoID int64, ownerLogin, repoName string) ([]*project.Project, error)`) instead of `project.Repository`.
  - `NewServer` passes `cfg.Projects` (`*projectapp.Service`) to `httpapi.New`.
  - `webhook.go` calls `a.projects.SyncAndFindByRepo(...)` instead of calling `FindByRepo` / `Restore` / `Save` on `project.Repository`.
  - `ui.go`:
    - `forceUnlock` calls `u.cfg.Projects.ForceUnlock(r.Context(), id)`.
    - `tabDependencies` calls `u.cfg.States.Outputs(r.Context(), string(ds.DependsOnProjectID))` instead of `u.cfg.States.States.Load(...)`.

---

## 4. Error Handling & Invariants

- All existing domain errors (`project.ErrBusy`, `project.ErrLocked`, `run.ErrActiveConflict`, `state.ErrLocked`, `state.ErrLockMismatch`, `policy.ErrDuplicateName`, `dependency.ErrCycleDetected`) and HTTP status codes remain unchanged.
- Database transactions in `postgres` (`Queue.Claim`, `Queue.Reap`, `RunRepo.InsertIfAbsent`, `RunRepo.Save`, `StateRepo.Save`, `ContextRepo.SetProjectContexts`, `IdentityStore.UpsertUser`, `IdentityStore.UpsertInstallation`) remain atomic in `internal/infrastructure/postgres`.

---

## 5. Testing Strategy

1. **Domain Unit Tests (`internal/domain/...`)**:
   - Test `Project.ClaimActiveRun` returning `ErrLocked` when locked, and `Project.RenameRepo`.
2. **Postgres Integration Tests (`internal/infrastructure/postgres/...`)**:
   - Update `context_repo_test.go` to test raw `Value`/`Ciphertext` persistence without `crypto.Sealer`.
   - Update `queue_test.go` to pass `retryDelay` to `Claim` and `worker.ReapPolicy` to `Reap`.
   - Update `identity_repo_test.go` to pass `user.Identity` to `UpsertUser` and test `ConsumeOAuthState` returning `(expiresAt, ok, err)`.
   - Update `state_repo_test.go` to verify `Load` returns `(nil, nil)` when no state row exists.
3. **Application & HTTP Tests (`internal/application/...`, `internal/interfaces/http/...`)**:
   - Test secret encryption/decryption and deduplication in `internal/application/tfcontext/service_test.go`.
   - Test OAuth state expiry and `GitHubIdentity` creation in `internal/application/auth/service_test.go`.
   - Test `SyncAndFindByRepo` and `ForceUnlock` in `internal/application/project/service_test.go`.
   - Test `BeginStep` in `internal/application/run/service_test.go`.
   - Verify full test suite (`go test ./...`) passes with zero regressions.
