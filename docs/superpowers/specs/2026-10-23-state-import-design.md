# Import State & Remove State History Design

## Overview

Allow users to upload/import an existing `.tfstate` JSON file into a project from the project's **State** tab in the UI, and remove state history storage and display across both the UI and Postgres database.

## Goals

1. **Remove State History (Database & UI):**
   - Drop the `state_history` table in Postgres via migration `009_drop_state_history.sql` and stop persisting historical state snapshots.
   - Remove `HistoryEntry`, `Snapshot.History`, and `State.NewHistory()` from `internal/domain/state` and `StateService.History` from `internal/application/state`.
   - Preserve output-change tracking (`ChangedOutputsSincePrevious`) for downstream dependency pipelines by computing `ChangedOutputs map[string]bool` in `State.Put` / `State.Import` (comparing previous `s.Data` outputs with new `data` outputs before overwriting `s.Data`) and storing `changed_outputs jsonb` on `states`.
   - Rename the project tab from **State history** to **State** and stop rendering historical rows or raw state JSON in the UI.
2. **Import State in the UI:**
   - Display current state metadata on the **State** tab (`Serial`, `Size`, `Last updated`, `Lock status`, or `"No stored state."` when empty).
   - Provide an **Import state** file upload form (`POST /projects/{id}/state/import`, `enctype="multipart/form-data"`) on the **State** tab.
   - Validate that the uploaded file is a valid JSON object, the project belongs to the active tenant, no run is currently active on the project, and the state is not locked (`ErrLocked`).

## Architecture & Changes

### 1. Database Migration (`internal/infrastructure/postgres/migrations/009_drop_state_history.sql`)

```sql
DROP TABLE IF EXISTS state_history;

ALTER TABLE states
    ADD COLUMN IF NOT EXISTS changed_outputs jsonb NOT NULL DEFAULT '{}'::jsonb;
```

### 2. Domain Layer (`internal/domain/state/state.go`)

- Remove `HistoryEntry`, `Snapshot.History`, `State.restored`, and `State.NewHistory()`.
- Add `ErrInvalidState = errors.New("invalid terraform state JSON")`.
- Add `ChangedOutputs map[string]bool` to `Snapshot` (cloned in `cloneSnapshot`).
- Add `Metadata` struct:
  ```go
  type Metadata struct {
      Exists    bool
      Serial    *int64
      Size      int
      Locked    bool
      UpdatedAt time.Time
  }
  ```
  and `Snapshot.Metadata() Metadata`.
- In `State.Put(data []byte, lockID, runID string, now time.Time) error`:
  - Compute `s.s.ChangedOutputs = diffOutputs(s.s.Data, data)` before updating `s.s.Data`.
  - Update `s.s.Data`, `s.s.Serial`, and `s.s.UpdatedAt`.
- Add `State.Import(data []byte, now time.Time) error`:
  - Return `ErrLocked` if `s.s.LockID != ""`.
  - Validate `data` is a non-empty JSON object (`map[string]json.RawMessage`), returning `ErrInvalidState` otherwise.
  - Compute `s.s.ChangedOutputs = diffOutputs(s.s.Data, data)`.
  - Update `s.s.Data`, `s.s.Serial = readSerial(data)`, and `s.s.UpdatedAt = now`.
- Update `Snapshot.ChangedOutputsSincePrevious() map[string]bool` to return a clone of `s.ChangedOutputs` (or if `s.ChangedOutputs == nil` on a legacy row with non-empty `s.Data`, mark current outputs as `true`).

### 3. Persistence Layer (`internal/infrastructure/postgres/state_repo.go`)

- `StateRepo.Load(ctx, projectID)`:
  - Select `data, lock_id, lock_info, serial, updated_at, changed_outputs` from `states`.
  - Unmarshal `changed_outputs` into `s.ChangedOutputs`.
  - Remove `loadHistory`.
- `StateRepo.Save(ctx, st)`:
  - Marshal `s.ChangedOutputs` to JSON (`{}` when empty) and update `states` (`data, lock_id, lock_info, serial, updated_at, changed_outputs`).
  - Remove `INSERT INTO state_history`.

### 4. Application Layer (`internal/application/state/service.go`)

- Replace `History(ctx, projectID)` with `Metadata(ctx, projectID string) (state.Metadata, error)`:
  - Loads state via `s.States.Load(ctx, projectID)`; if `nil` or `st.Empty()`, returns `state.Metadata{Locked: st != nil && st.Snapshot().LockID != ""}`.
  - Otherwise returns `st.Snapshot().Metadata()`.
- Add `Import(ctx context.Context, projectID string, data []byte) error`:
  - Loads state via `s.loadState(ctx, projectID)`.
  - Calls `st.Import(data, s.Now())`.
  - Saves via `s.States.Save(ctx, st)`.

### 5. HTTP & UI Layer (`internal/interfaces/http/ui.go`, `templates/project.html`, `templates/state_history.html`)

- Rename tab label in `internal/interfaces/http/templates/project.html` from `State history` to `State`.
- Update `u.tabState` (`GET /projects/{id}/tabs/state`) to support both HTMX fragment requests (`HX-Request: true`) and full-page loads (matching `u.tabDependencies`).
- Add route `POST /projects/{id}/state/import` (`u.importState`):
  - Verifies project & tenant ownership via `u.loadProject(w, r)`.
  - Rejects with `409 Conflict` if `p.Snapshot().Locked` or `anyNonTerminal(runs)`.
  - Parses multipart form (`r.ParseMultipartForm(32 << 20)`) and reads `state_file` (or `state_json` fallback).
  - Calls `u.cfg.States.Import(r.Context(), string(ps.ID), data)`:
    - Maps `state.ErrLocked` to `409 Conflict`.
    - Maps `state.ErrInvalidState` to `400 Bad Request`.
  - Redirects (`303 See Other`) to `/projects/{id}/tabs/state`.
- Update `internal/interfaces/http/templates/state_history.html` (`{{define "state_history"}}`):
  - Show current state metadata summary (`Serial`, `Size`, `Last updated`, `Status: Locked / Unlocked`) when state exists, or `No stored state.` when empty.
  - Render the **Import state** form (`POST /projects/{{.ProjectID}}/state/import`, `enctype="multipart/form-data"`) with `<input type="file" name="state_file" accept=".tfstate,.json,application/json" required>` and `<button type="submit">Import state</button>`.
- Update `projectMeta` and `runMeta` in `internal/interfaces/http/ui.go` to take `state.Metadata` instead of `[]state.HistoryEntry`.
