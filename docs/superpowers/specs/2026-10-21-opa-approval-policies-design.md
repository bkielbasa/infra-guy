# Phase 3 Design: OPA Approval Policies & Online Editor/Tester (Issues #2 & #3)

## Goal
1. Allow organizations to define reusable OPA/Rego approval policies (`policies` table) and attach an optional approval policy (`approval_policy_id`) to any project (Issue #2).
2. Capture the structured JSON representation of the Terraform plan (`terraform show -json tfplan` -> `runs.plan_json`) during `StepPlan`.
3. When a run reaches `needs_approval` (`RunStatusNeedsApproval`), evaluate the project's attached approval policy against:
   - `input.plan` (parsed `terraform show -json tfplan` object)
   - `input.current_user` (`id`, `login`, `role`)
   - `input.project` (`id`, `name`, `repo_owner`, `repo_name`, `working_directory`, `terraform_version`, `auto_approve`)
   - (Also populate `input.input.plan` as an alias for `input.plan` so policies written using either `input.plan` or the nested `input.input.plan` snippet from Issue #2 work identically.)
4. Enforce the policy both in the Run Details UI (showing Allowed/Denied status and any `deny` messages, and disabling the Approve button when denied) and server-side on `POST /projects/{id}/runs/{runID}/confirm`.
5. Provide an online Rego Policy Editor & Tester (`/policies`, `/policies/{id}`, `POST /policies/test`) and a **"Copy Policy Input JSON"** drawer/button on Run Details pages so users can write, paste input JSON, and interactively test policies in the browser (Issue #3).

## Architecture

### 1. Domain & Database (`internal/domain/policy`, `005_opa_approval_policies.sql`)
- `policy.Policy` aggregate:
  - `ID`, `TenantID`, `Name`, `Description`, `Rego`, `CreatedAt`, `UpdatedAt`.
  - Validates non-empty `TenantID`, `Name`, and valid Rego syntax via `opa.ValidateRego(rego)`.
- Database migration `005_opa_approval_policies.sql`:
  - Creates `policies` table (`id TEXT PRIMARY KEY`, `tenant_id TEXT NOT NULL`, `name TEXT NOT NULL`, `description TEXT NOT NULL DEFAULT ''`, `rego TEXT NOT NULL`, `created_at TIMESTAMPTZ NOT NULL`, `updated_at TIMESTAMPTZ NOT NULL`, `UNIQUE (tenant_id, name)`).
  - Alters `projects` table: `ADD COLUMN IF NOT EXISTS approval_policy_id TEXT REFERENCES policies(id) ON DELETE SET NULL`.
  - Alters `runs` table: `ADD COLUMN IF NOT EXISTS plan_json BYTEA`.

### 2. OPA Rego Evaluator (`internal/infrastructure/opa`)
- Uses `github.com/open-policy-agent/opa/v1/rego` and `github.com/open-policy-agent/opa/v1/ast`.
- Parses the Rego module with `ast.ParseModule("policy.rego", regoSource)` to validate syntax and automatically discover the declared `package` path (e.g., `package infraguy.approval`, `package policy`, `package main`).
- Evaluates `<pkg>.allow` and `<pkg>.deny` against the input document:
  - `allow`: boolean (`true` if truthy; defaults to `false` if undefined, unless the policy only defines `deny` rules without an `allow` rule, in which case `allow` is `len(deny) == 0`).
  - `deny`: optional set or array of strings (`deny contains msg if { ... }` or `deny[msg] { ... }`). If `len(deny) > 0`, `Allowed` is always `false`.
- Supports both Rego v1 syntax and Rego v0 compatibility fallback (`ast.RegoV1` with fallback to `ast.RegoV0`) so policies written with or without `if` / `contains` keywords work seamlessly.

### 3. Terraform Plan JSON Capture (`internal/infrastructure/terraform`, `internal/domain/run`, `internal/application/worker`)
- During `StepPlan`, after `terraform plan -input=false -no-color -out=tfplan` and `terraform show -no-color tfplan`, `Executor.Run` also runs `terraform show -json tfplan` and returns its output in `Result.PlanJSON []byte`.
- `run.StepResult` and `run.Snapshot` carry `PlanJSON []byte`, persisted in `runs.plan_json` by `postgres.RunRepo`.

### 4. UI & HTTP Endpoints (`internal/interfaces/http`)
- **Top-level navigation:** Add `Policies` link (`/policies`) next to `Projects` and `Contexts`.
- **Policies List & Create (`/policies`):**
  - Lists all policies for the tenant with links to edit/view.
  - Includes a side-by-side **New Policy + Live Policy Tester** form (with a default starter Rego policy and sample Input JSON, plus a "Test Policy" button powered by `POST /policies/test`).
- **Policy Detail / Edit (`/policies/{id}`):**
  - Edit policy `Name`, `Description`, and `Rego` source, or delete the policy.
  - Integrated **Live Policy Tester** right on the page so changes can be tested against custom/copied Input JSON before or after saving.
- **Project Create & Project Details (`/projects`, `/projects/{id}`):**
  - Dropdown to select an `ApprovalPolicyID` (`None (anyone with project access can approve)` or any org policy).
- **Run Details (`/projects/{id}/runs/{runID}`):**
  - When `PlanJSON` is present (or on any run), provides a collapsible **"Policy Input JSON"** section with a **"Copy Policy Input JSON"** button containing the exact JSON payload (`plan`, `current_user`, `project`) for this run and current user.
  - When the run is in `needs_approval` and the project has an `ApprovalPolicyID`:
    - Evaluates the policy for the current user and displays the policy name, Allowed/Denied badge, and any `deny` reasons.
    - Disables the Approve button if denied, and enforces the same check in `handleConfirmRun` (`403 Forbidden` if denied).
