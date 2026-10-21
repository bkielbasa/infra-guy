# Phase 3: OPA Approval Policies & Online Editor/Tester Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add org-scoped OPA/Rego approval policies (`policies`), attach an optional approval policy to projects (`approval_policy_id`), capture `terraform show -json tfplan` (`runs.plan_json`), enforce approval policies in the Run UI and on `POST /projects/{id}/runs/{runID}/confirm`, and provide an online Rego Policy Editor & Tester (`/policies`, `/policies/{id}`, `POST /policies/test`) plus a "Copy Policy Input JSON" helper on Run Details pages (Issues #2 & #3).

**Architecture:** `internal/infrastructure/opa` wraps the official Open Policy Agent library (`github.com/open-policy-agent/opa/v1/rego` and `ast`) to validate Rego modules, auto-detect their `package` path, and evaluate `allow` (bool) and `deny` (messages) against `{"plan": ..., "current_user": ..., "project": ..., "input": {"plan": ...}}`. `internal/domain/policy` and `internal/application/policy` manage policy lifecycle and evaluation.

**Tech Stack:** Go 1.27, `github.com/open-policy-agent/opa`, `pgx/v5`, `html/template`.

---

### Task 1: OPA Rego Evaluator (`internal/infrastructure/opa`), `policy` Domain, & `policy` Application Service

**Files:**
- Modify: `go.mod` / `go.sum`
- Create: `internal/infrastructure/opa/evaluator.go`
- Create: `internal/infrastructure/opa/evaluator_test.go`
- Create: `internal/domain/policy/policy.go`
- Create: `internal/domain/policy/policy_test.go`
- Create: `internal/application/policy/service.go`
- Create: `internal/application/policy/service_test.go`

- [ ] **Step 1: Implement `internal/infrastructure/opa` (`Evaluator`) and unit tests**
  - Add `github.com/open-policy-agent/opa` dependency.
  - Implement `ParseAndValidate(code string) (*ast.Module, ast.RegoVersion, error)` trying `ast.RegoV1` first and falling back to `ast.RegoV0` so both Rego v1 (`allow if { ... }`, `deny contains msg if { ... }`) and Rego v0 (`allow { ... }`, `deny[msg] { ... }`) syntax are supported.
  - Implement `Evaluator.Validate(code string) error` and `Evaluator.Evaluate(ctx context.Context, code string, rawInput map[string]any) (EvalResult, error)`:
    - `EvalResult{Allowed bool, Deny []string}`.
    - Ensure `rawInput` also populates `rawInput["input"] = map[string]any{"plan": rawInput["plan"]}` if `"input"` is not already set, so policies referencing either `input.plan` or `input.input.plan` (from Issue #2's example) work seamlessly.
    - Query the module's package `<pkg>` (`module.Package.Path.String()`, e.g., `data.infraguy` or `data.policy`):
      - Extract `deny` (slice or map/set of strings, sorted deterministically).
      - Extract `allow` (bool). If the module defines `deny` rules but no `allow` rule, default `allow` to `len(deny) == 0`; otherwise require `allow == true && len(deny) == 0`.
  - Test `Evaluator` in `evaluator_test.go` (valid/invalid Rego syntax, `input.plan` and `input.input.plan` access, `input.current_user.login`, `input.project.name`, `allow` rules, `deny` messages, Rego v1 and v0 syntax).

- [ ] **Step 2: Implement `internal/domain/policy` and `internal/application/policy`**
  - `policy.Policy` aggregate (`ID`, `TenantID`, `Name`, `Description`, `Rego`, `CreatedAt`, `UpdatedAt`), `New`, `Rehydrate`, `Update(name, description, rego, now)`, `Repository` interface (`Insert`, `Update`, `Delete`, `Get`, `ListByTenant`).
  - `policyapp.Service` (`Policies policy.Repository`, `Engine Engine`, `Now`, `NewID`):
    - `Create(ctx, CreateInput) (*policy.Policy, error)` (validates Rego via `Engine.Validate`).
    - `Update(ctx, UpdateInput) (*policy.Policy, error)` (validates TenantID and Rego).
    - `Delete(ctx, tenantID, id) error`.
    - `Get(ctx, tenantID, id) (*policy.Policy, error)`.
    - `ListByTenant(ctx, tenantID) ([]*policy.Policy, error)`.
    - `TestRego(ctx, regoCode string, rawJSON []byte) (EvalResult, error)`.
    - `BuildInput(planJSON []byte, user UserInfo, proj ProjectInfo) (map[string]any, []byte, error)`.
    - `EvaluateForRun(ctx, tenantID string, policyID policy.ID, planJSON []byte, user UserInfo, proj ProjectInfo) (*RunPolicyDecision, error)`.
  - Run `go test ./internal/infrastructure/opa/... ./internal/domain/policy/... ./internal/application/policy/...` and commit.

---

### Task 2: Project `ApprovalPolicyID`, Run `PlanJSON`, Terraform `show -json`, & Postgres Migration `005`

**Files:**
- Modify: `internal/domain/project/project.go`
- Modify: `internal/domain/project/project_test.go`
- Modify: `internal/application/project/service.go`
- Modify: `internal/application/project/service_test.go`
- Modify: `internal/domain/run/run.go`
- Modify: `internal/domain/run/run_test.go`
- Modify: `internal/infrastructure/terraform/terraform.go`
- Modify: `internal/infrastructure/terraform/worker_adapter.go`
- Modify: `internal/infrastructure/terraform/terraform_test.go`
- Modify: `internal/application/worker/worker.go`
- Modify: `internal/application/worker/worker_test.go`
- Create: `internal/infrastructure/postgres/migrations/005_opa_approval_policies.sql`
- Modify: `internal/infrastructure/postgres/project_repo.go`
- Modify: `internal/infrastructure/postgres/project_repo_test.go`
- Modify: `internal/infrastructure/postgres/run_repo.go`
- Modify: `internal/infrastructure/postgres/run_repo_test.go`
- Create: `internal/infrastructure/postgres/policy_repo.go`
- Create: `internal/infrastructure/postgres/policy_repo_test.go`

- [ ] **Step 1: Add `ApprovalPolicyID` to `project` domain & application service**
  - Add `ApprovalPolicyID string` to `project.NewInput`, `project.Snapshot`, `project.Project`, and `ChangeApprovalPolicy(policyID string)`.
  - Add `ApprovalPolicyID string` to `projectapp.CreateInput` and `UpdateApprovalPolicy(ctx context.Context, id project.ID, policyID string) error` to `projectapp.Service`.
- [ ] **Step 2: Add `PlanJSON []byte` to `run`, `terraform`, and `worker`**
  - Add `PlanJSON []byte` to `run.StepResult` and `run.Snapshot`. In `CompleteStep`, when `step == StepPlan` and `len(res.PlanJSON) > 0`, store `r.planJSON = append([]byte(nil), res.PlanJSON...)`.
  - In `internal/infrastructure/terraform/terraform.go`:
    - During `StepPlan`, run `{"show", "-json", "tfplan"}` after `{"show", "-no-color", "tfplan"}`.
    - Capture its stdout into `Result.PlanJSON []byte` (without appending the potentially huge raw JSON to the human-readable `Result.Logs`).
  - Forward `PlanJSON` through `terraform.WorkerExecutor`, `worker.Result`, and `Worker.runJob` -> `w.Service.Advance`.
- [ ] **Step 3: Add migration `005_opa_approval_policies.sql`, update `ProjectRepo` and `RunRepo`, and implement `PolicyRepo`**
  - Migration `005_opa_approval_policies.sql`:
    - `CREATE TABLE IF NOT EXISTS policies (...)`
    - `ALTER TABLE projects ADD COLUMN IF NOT EXISTS approval_policy_id TEXT REFERENCES policies(id) ON DELETE SET NULL;`
    - `ALTER TABLE runs ADD COLUMN IF NOT EXISTS plan_json BYTEA;`
  - Update `project_repo.go` (`projectColumns`, `scanProject`, `Insert`, `Update`) for `approval_policy_id` (using `NULLIF($..., '')` so empty string is stored as `NULL` and satisfies the foreign key constraint!).
  - Update `run_repo.go` (`runColumns`, `scanRun`, `InsertIfAbsent`, `Save`) for `plan_json`.
  - Implement `PolicyRepo` in `policy_repo.go` and tests in `policy_repo_test.go`.
  - Run `go test ./...` and commit.

---

### Task 3: Policies UI, Online Rego Editor/Tester, Project Policy Attachment, & Run Approval Enforcement

**Files:**
- Modify: `internal/interfaces/http/server.go`
- Modify: `internal/interfaces/http/views.go`
- Modify: `internal/interfaces/http/templates/layout.html`
- Create: `internal/interfaces/http/templates/policies.html`
- Create: `internal/interfaces/http/templates/policy.html`
- Modify: `internal/interfaces/http/templates/projects.html`
- Modify: `internal/interfaces/http/templates/project.html`
- Modify: `internal/interfaces/http/templates/run.html`
- Modify: `internal/interfaces/http/server_test.go`
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Add Policies routes, Online Editor/Tester, and Project/Run integration in `internal/interfaces/http`**
  - Add `Policies *policyapp.Service` to `ServerConfig`.
  - Add `Policies` link (`/policies`) in `layout.html`.
  - Routes:
    - `GET /policies`: list policies + New Policy form with integrated **Online Policy Editor & Tester** (pre-filled with a starter Rego policy and sample Terraform plan + user + project Input JSON).
    - `POST /policies`: create policy (admin).
    - `POST /policies/test`: evaluate `rego` against `input_json` and return JSON `{"allowed": bool, "deny": [...], "error": "..."}` for the in-page tester.
    - `GET /policies/{id}`: view/edit policy + integrated **Online Policy Editor & Tester**.
    - `POST /policies/{id}`: update policy (admin).
    - `POST /policies/{id}/delete`: delete policy (admin).
    - `POST /projects/{id}/policy`: update a project's `ApprovalPolicyID` (admin).
  - Project Create (`projects.html`) & Project Details (`project.html`):
    - Add Approval Policy dropdown (`None (anyone with project access can approve)` + tenant policies).
  - Run Details (`run.html`) & `handleConfirmRun`:
    - Add collapsible **"Policy Input JSON"** card/drawer with a **"Copy Policy Input JSON"** button containing the formatted JSON (`plan`, `current_user`, `project`) for the run.
    - When `r.Status == RunStatusNeedsApproval` and `p.ApprovalPolicyID != ""`, evaluate the policy for the current user:
      - Show the policy name, Allowed/Denied badge, and any `deny` reasons on `run.html`, disabling the Approve button when denied.
      - In `handleConfirmRun`, evaluate the policy before calling `s.cfg.Runs.Confirm`; if denied, return `http.StatusForbidden` with the deny message.
- [ ] **Step 2: Wire `PolicyRepo`, `opa.Evaluator`, and `policyapp.Service` in `cmd/server/main.go` and add HTTP tests**
  - Add tests in `internal/interfaces/http/server_test.go` covering policy CRUD, `POST /policies/test`, project approval policy attachment, Run Details policy input JSON & evaluation badge, and `POST /projects/{id}/runs/{runID}/confirm` enforcement (allowed vs 403 denied).
  - Run `go test ./...` and commit.
