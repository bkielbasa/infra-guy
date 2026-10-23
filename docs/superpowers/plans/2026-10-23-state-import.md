# Import State & Remove State History Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add UI state import (`POST /projects/{id}/state/import`) on the project **State** tab and remove state history from both the UI and Postgres database while keeping output-change detection working.

**Architecture:**
1. Drop `state_history` via migration `009_drop_state_history.sql` and add `changed_outputs jsonb` to `states`.
2. Update `internal/domain/state` (`State.Put`, `State.Import`, `Snapshot.Metadata`, `Snapshot.ChangedOutputsSincePrevious`), `internal/infrastructure/postgres/state_repo.go`, and `internal/application/state` (`Metadata`, `Import`).
3. Update `internal/interfaces/http` (`ui.go`, `project.html`, `state_history.html`) to rename the tab to **State**, show state metadata (no raw state JSON), and handle `.tfstate` multipart file upload.

**Tech Stack:** Go 1.26, PostgreSQL (`pgx/v5`), `net/http`, `html/template`.

---

### Task 1: Domain, Migration, Repository, and State Application Service

**Files:**
- Create: `internal/infrastructure/postgres/migrations/009_drop_state_history.sql`
- Modify: `internal/domain/state/state.go`
- Modify: `internal/domain/state/state_test.go`
- Modify: `internal/infrastructure/postgres/state_repo.go`
- Modify: `internal/infrastructure/postgres/repos_test.go`
- Modify: `internal/application/state/service.go`
- Modify: `internal/application/state/service_test.go`

- [ ] **Step 1: Write failing unit tests in `internal/domain/state/state_test.go` and `internal/application/state/service_test.go`**
  - Update `TestPutWithoutLockAppendsHistory` and `TestSnapshotIsolatedFromCallerMutation` to remove `History` checks and test `Metadata()` and `ChangedOutputs` isolation.
  - Add `TestImportState` in `internal/domain/state/state_test.go` testing:
    - importing valid `.tfstate` JSON sets `Data`, `Serial`, `UpdatedAt`, `ChangedOutputs`, and `Metadata()`
    - importing invalid JSON or JSON array/primitive returns `ErrInvalidState`
    - importing while locked returns `ErrLocked`
  - Add `TestMetadataAndImport` in `internal/application/state/service_test.go`.

- [ ] **Step 2: Implement domain changes in `internal/domain/state/state.go`**
  - Remove `HistoryEntry`, `Snapshot.History`, `State.restored`, `State.NewHistory()`.
  - Add `ErrInvalidState`, `Metadata`, `Snapshot.ChangedOutputs`, `Snapshot.Metadata()`, `State.Import(data []byte, now time.Time) error`, and `diffOutputs(prevData, nextData []byte) map[string]bool`.

- [ ] **Step 3: Create migration `009_drop_state_history.sql` and update `internal/infrastructure/postgres/state_repo.go` & `repos_test.go`**
  - Add `009_drop_state_history.sql` dropping `state_history` and adding `changed_outputs jsonb NOT NULL DEFAULT '{}'::jsonb` to `states`.
  - Update `StateRepo.Load` and `StateRepo.Save` to read/write `changed_outputs` and remove `state_history` queries.
  - Update `internal/infrastructure/postgres/repos_test.go` (`TestReposAndQueue` and replace `TestStateHistoryOnlyGrows` with `TestStateSaveAndImportPreservesChangedOutputs`).

- [ ] **Step 4: Implement `Metadata` and `Import` in `internal/application/state/service.go`**
  - Replace `History` with `Metadata(ctx context.Context, projectID string) (state.Metadata, error)` and add `Import(ctx context.Context, projectID string, data []byte) error`.

- [ ] **Step 5: Run domain, application, and postgres tests**
  - Run: `go test ./internal/domain/state/... ./internal/application/state/... ./internal/application/dependency/... ./internal/infrastructure/postgres/...`

---

### Task 2: HTTP UI State Tab & State Import Handler

**Files:**
- Modify: `internal/interfaces/http/ui.go`
- Modify: `internal/interfaces/http/templates/project.html`
- Modify: `internal/interfaces/http/templates/state_history.html`
- Modify: `internal/interfaces/http/ui_test.go`

- [ ] **Step 1: Write failing HTTP tests in `internal/interfaces/http/ui_test.go`**
  - Test `GET /projects/p1/tabs/state` (both HTMX and full page) renders state metadata (`Serial`, `Size`, `Updated`) and the import form without exposing raw state JSON.
  - Test `POST /projects/p1/state/import` with multipart `.tfstate` upload imports the state and redirects to `/projects/p1/tabs/state`.
  - Test `POST /projects/p1/state/import` rejects invalid JSON (`400`), locked project/state (`409`), active run (`409`), and cross-tenant access (`404`).

- [ ] **Step 2: Update `internal/interfaces/http/ui.go` and templates**
  - Rename tab link in `internal/interfaces/http/templates/project.html` from `State history` to `State`.
  - Update `internal/interfaces/http/templates/state_history.html` to display state metadata and the multipart file upload form (`POST /projects/{{.ProjectID}}/state/import`).
  - Update `projectMeta`, `runMeta`, `stateTab`, `tabState`, and add `importState` in `internal/interfaces/http/ui.go`.

- [ ] **Step 3: Run full test suite**
  - Run: `go test ./...`
