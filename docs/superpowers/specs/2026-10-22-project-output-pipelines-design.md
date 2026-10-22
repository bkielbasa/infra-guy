# Project Output Pipelines (Cross-Project Dependencies) Design

**Date:** 2026-10-22
**Status:** Approved

## 1. Goal

Allow a Terraform project's outputs to be mapped as inputs (`TF_VAR_*`) to downstream projects within the same tenant, and automatically trigger downstream runs when an upstream project's tracked apply run succeeds (either when mapped outputs change or on every succeeded apply).

## 2. Domain & State Output Extraction

### 2.1 State Output Extraction (`internal/domain/state/state.go`)
- Add `OutputValue` and `ExtractOutputs(data []byte) (map[string]OutputValue, error)` to parse the top-level `"outputs"` object from a Terraform `.tfstate` JSON document:
  ```go
  type OutputValue struct {
      Raw       json.RawMessage
      EnvString string
      Sensitive bool
  }
  ```
  - If the JSON `"value"` is a string, `EnvString` is the unquoted string.
  - If the JSON `"value"` is a number, boolean, array, or object, `EnvString` is its compact JSON representation (`string(rawValue)`).
  - If `"value"` is `null`, the output is omitted from env injection.
- Add `Snapshot.Outputs() map[string]OutputValue` and `Snapshot.ChangedOutputsSincePrevious() map[string]bool`:
  - Compares `ExtractOutputs(s.Data)` against the previous state history entry (`s.History[len(s.History)-2].Data`).
  - If `len(s.History) <= 1`, all current outputs in `s.Data` are marked as changed (`true`).
  - Otherwise, an output `k` is marked changed if it was added, removed, or its compact `Raw` JSON bytes differ from the previous entry.

### 2.2 Dependency Domain (`internal/domain/dependency/dependency.go`)
- Entities & Value Objects:
  - `ID string`, `MappingID string`
  - `Mapping`:
    - `ID MappingID`
    - `OutputName string` (trimmed, non-empty identifier)
    - `InputName string` (trimmed; if empty when creating, defaults to `OutputName`)
    - `EnvKey() string`: returns `InputName` if it starts with `TF_VAR_`, otherwise `"TF_VAR_" + InputName`.
  - `Snapshot` / `Dependency`:
    - `ID ID`
    - `TenantID string`
    - `ProjectID project.ID` (downstream project)
    - `DependsOnProjectID project.ID` (upstream project)
    - `TriggerAlways bool` (`false` = trigger downstream only when at least one mapped output changed; `true` = trigger on every succeeded tracked apply)
    - `Mappings []Mapping`
    - `CreatedAt time.Time`
- Invariants:
  - `ProjectID != DependsOnProjectID` (`ErrSelfDependency`).
  - Cycle detection: `WouldCreateCycle(edges []Edge, downstream, upstream project.ID) bool` checks if there is already a directed path from `upstream` to `downstream` (where an edge `X -> Y` means `X` depends on `Y`); if so, returns `ErrCycleDetected`.
  - Duplicate `InputName` within the same dependency updates or replaces the existing mapping for that input variable.

### 2.3 Run Trigger (`internal/domain/run/run.go`)
- Add `TriggerPipeline Trigger = "pipeline"`.
- Treat `TriggerPipeline` as a tracked run alongside `TriggerPush` and `TriggerManual`.

## 3. Database Schema (`internal/infrastructure/postgres/migrations/007_project_dependencies.sql`)

```sql
CREATE TABLE IF NOT EXISTS project_dependencies (
    id text PRIMARY KEY,
    tenant_id text NOT NULL,
    project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    depends_on_project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    trigger_always boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL,
    UNIQUE (project_id, depends_on_project_id)
);

CREATE INDEX IF NOT EXISTS project_dependencies_project_idx ON project_dependencies (project_id);
CREATE INDEX IF NOT EXISTS project_dependencies_upstream_idx ON project_dependencies (depends_on_project_id);

CREATE TABLE IF NOT EXISTS project_dependency_mappings (
    id text PRIMARY KEY,
    dependency_id text NOT NULL REFERENCES project_dependencies(id) ON DELETE CASCADE,
    output_name text NOT NULL,
    input_name text NOT NULL,
    created_at timestamptz NOT NULL,
    UNIQUE (dependency_id, input_name)
);
```

## 4. Application Service & Worker Integration

### 4.1 Dependency Application Service (`internal/application/dependency/service.go`)
- Methods:
  - `Create(ctx, tenantID, projectID, dependsOnProjectID, triggerAlways)` — verifies both projects belong to `tenantID`, checks for cycles across existing tenant dependencies, and inserts the dependency.
  - `SetTriggerAlways(ctx, tenantID, depID, triggerAlways)`
  - `Delete(ctx, tenantID, depID)`
  - `AddMapping(ctx, tenantID, depID, outputName, inputName)` — defaults `inputName` to `outputName` if blank.
  - `RemoveMapping(ctx, tenantID, depID, mappingID)`
  - `ListByProject(ctx, projectID)` — lists upstream dependencies for `projectID`.
  - `ListDownstream(ctx, upstreamProjectID)` — lists dependencies where `DependsOnProjectID == upstreamProjectID`.
  - `ResolveForProject(ctx, projectID) (env map[string]string, messages []string, err error)`:
    - Loads upstream dependencies for `projectID`.
    - For each dependency, loads upstream project metadata and current state (`States.Get`).
    - Extracts outputs and resolves each mapping into `env[m.EnvKey()] = out.EnvString`, recording human-readable log messages (including warnings when a mapped output is not found in upstream state).
  - `ShouldTriggerDownstream(dep dependency.Snapshot, changedOutputs map[string]bool) bool`:
    - Returns `true` if `dep.TriggerAlways` is `true`.
    - Otherwise returns `true` if any mapping `m` in `dep.Mappings` has `changedOutputs[m.OutputName] == true`.

### 4.2 Worker Integration (`internal/application/worker/worker.go`)
- Add `DependencyResolver` interface to `Worker`:
  ```go
  type DependencyResolver interface {
      ResolveForProject(ctx context.Context, projectID project.ID) (map[string]string, []string, error)
  }
  ```
- In `Worker.runJob`, after resolving `Contexts`:
  - If `w.Dependencies != nil`, call `w.Dependencies.ResolveForProject(ctx, ps.ID)`.
  - Log any returned messages via `logger.LogBeforef`.
  - Merge the returned `depEnv` into `ctxEnv`.

### 4.3 Pipeline Auto-Triggering (`cmd/server/main.go`)
- Wire `runsSvc.OnTerminal`:
  - Invoked when a run reaches a terminal status.
  - Only proceeds when `!rs.PlanOnly` and `rs.Status == run.StatusSucceeded`.
  - Loads upstream state via `statesRepo.Get(ctx, string(ps.ID))` and computes `changed := st.Snapshot().ChangedOutputsSincePrevious()`.
  - Calls `depSvc.ListDownstream(ctx, ps.ID)`.
  - For each downstream dependency where `depSvc.ShouldTriggerDownstream(dep.Snapshot(), changed)` is `true`:
    - Loads downstream project `dp`; skips if `dp == nil` or `dp.Snapshot().Locked`.
    - Obtains installation token for `dp.Snapshot().TenantID`, resolves `BranchTip` and `Lookup` for `dp.Snapshot().DefaultBranch`, and calls `runsSvc.Open` with `Trigger: run.TriggerPipeline`, `TriggeredBy: "pipeline:" + ps.Name`.

## 5. HTTP & UI (`internal/interfaces/http`)

- Add `Dependencies *dependencyapp.Service` to `ServerConfig`.
- Add tab link `Dependencies` (`/projects/{id}/tabs/dependencies`) to `internal/interfaces/http/templates/project.html`.
- Routes:
  - `GET /projects/{id}/tabs/dependencies` — renders `tab_dependencies` fragment (or full project page on direct navigation).
  - `POST /projects/{id}/dependencies` — adds an upstream dependency (`depends_on_project_id`, `trigger_always`).
  - `POST /projects/{id}/dependencies/{depID}/trigger-mode` — updates `trigger_always`.
  - `POST /projects/{id}/dependencies/{depID}/delete` — removes an upstream dependency.
  - `POST /projects/{id}/dependencies/{depID}/mappings` — adds an output-to-input mapping (`output_name`, `input_name`).
  - `POST /projects/{id}/dependencies/{depID}/mappings/{mapID}/delete` — deletes a mapping.
- Update `isTracked(s run.Snapshot)` in `internal/interfaces/http/ui.go` to include `s.Trigger == run.TriggerPipeline`.
