# Phase 1: Terraform Version Selection (#1) & Contexts (#4) — Design Spec

**Date:** 2026-10-21
**Issues Covered in Phase 1:**
- Issue #1: `feat(terraform): add support for selecting versions`
- Issue #4: `feat(context): Add contexts`

---

## 0. Multi-Phase Roadmap Across All 5 Open Issues

During design clarification, all 5 open GitHub issues were clarified and decomposed into 3 sequential phases:

1. **Phase 1 (This Spec): Terraform Versions (#1) + Contexts (#4)**
   - Per-project Terraform version selection (dropdown from HashiCorp Releases API with fallback list; on-demand cached binary installer).
   - Org-scoped Contexts containing `plain_env`, `secret_env`, and `secret_file` (relative path in `$HOME`), encrypted at rest in Postgres (AES-256-GCM), write-only in UI/API, attached many-to-many (ordered) to projects, and materialized into an isolated per-step `$HOME` (`<runWorkDir>/.home`).
2. **Phase 2: Isolated Kubernetes Job Worker (#5)**
   - One-shot K8s `Job` per execution step (`init`, `plan`, `apply`), communicating with the server via a run-scoped bearer token and storing `tfplan` in Postgres so no shared `ReadWriteOnce` PVC is needed across pods (while retaining `WORKER_MODE=local` fallback for local development).
3. **Phase 3: OPA Approval Policies (#2) & Policy Editor/Tester (#3)**
   - Capture `terraform show -json tfplan` (`plan_json`) during the `plan` step.
   - Org-wide OPA/Rego approval policies (`allow` boolean + optional `deny` messages) evaluated against `{"plan": ..., "current_user": ..., "project": ...}` when confirming a run in `needs_approval` (`Approval`) state (one policy per project).
   - Built-in online Rego editor & tester in the Policy Create/Edit UI, plus a "Copy Policy Input JSON" helper on Run pages.

---

## 1. Goals & Global Constraints (Phase 1)

- **Default Terraform Version:** `1.9.8` (matching the binary pre-baked in `Dockerfile`).
- **Strict Semver Validation:** Project `TerraformVersion` must match `^\d+\.\d+\.\d+$` (e.g., `1.9.8`, `1.10.5`). Empty input defaults to `1.9.8`.
- **Context Variable Precedence:** A project can attach zero or more org-scoped contexts in a deterministic order (`position ASC`). Later contexts override earlier contexts if they define the same environment variable key or `$HOME` relative file path.
- **Secret Security:**
  - `secret_env` and `secret_file` values are encrypted at rest in Postgres using AES-256-GCM (`internal/infrastructure/crypto`).
  - Secret values are write-only in the UI: never rendered back into HTML responses or forms after creation.
  - Neither context env vars nor secret files may override security-critical environment variables (`PATH`, `HOME`, `USER`, `TF_IN_AUTOMATION`, `TF_INPUT`, or any key in `terraform.blockedEnv`).
  - `secret_file` keys must be clean relative paths within `$HOME` (leading `~/` stripped, `/` prefix rejected, `..` path segments rejected) and written with `0600` permissions inside an isolated per-step directory `<runWorkDir>/.home` (`0700`).

---

## 2. Database Schema (`004_tf_versions_and_contexts.sql`)

```sql
ALTER TABLE projects
    ADD COLUMN terraform_version text NOT NULL DEFAULT '1.9.8';

CREATE TABLE contexts (
    id         text PRIMARY KEY,
    tenant_id  text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name       text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (tenant_id, name)
);

CREATE INDEX contexts_tenant_idx ON contexts (tenant_id);

CREATE TABLE context_entries (
    id               text PRIMARY KEY,
    context_id       text NOT NULL REFERENCES contexts(id) ON DELETE CASCADE,
    kind             text NOT NULL CHECK (kind IN ('plain_env', 'secret_env', 'secret_file')),
    key              text NOT NULL,
    value_text       text NOT NULL DEFAULT '',
    value_ciphertext bytea,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    UNIQUE (context_id, kind, key)
);

CREATE INDEX context_entries_context_idx ON context_entries (context_id);

CREATE TABLE project_contexts (
    project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    context_id text NOT NULL REFERENCES contexts(id) ON DELETE CASCADE,
    position   integer NOT NULL,
    PRIMARY KEY (project_id, context_id)
);

CREATE INDEX project_contexts_project_idx ON project_contexts (project_id, position);
```

---

## 3. Domain Model

### 3.1 `internal/domain/project`
- Add `TerraformVersion string` to `project.Snapshot`.
- `project.DefaultTerraformVersion = "1.9.8"`.
- `project.New(...)` initializes `TerraformVersion: DefaultTerraformVersion`.
- `(*Project).SetTerraformVersion(v string) error`:
  - Trims whitespace; if empty, sets `DefaultTerraformVersion`.
  - Validates against `^\d+\.\d+\.\d+$` (`ErrInvalidTerraformVersion` on mismatch).
- Extend `project.Repository` (or `tfcontext.Repository`) to manage project-context attachments. Keeping attachment methods on `tfcontext.Repository` (`SetProjectContexts`, `ListProjectContexts`) keeps `project.Repository` focused on the `projects` table while `project_contexts` joins directly with `contexts`.

### 3.2 `internal/domain/tfcontext`
- `ID string`
- `EntryKind string` with constants:
  - `KindPlainEnv EntryKind = "plain_env"`
  - `KindSecretEnv EntryKind = "secret_env"`
  - `KindSecretFile EntryKind = "secret_file"`
- `Entry`:
  - `ID string`
  - `Kind EntryKind`
  - `Key string` (Env var name for `plain_env`/`secret_env`; normalized relative `$HOME` path for `secret_file`)
  - `Value string` (Plaintext value; populated for `plain_env` always, and for `secret_env`/`secret_file` only when decrypted for worker execution or on initial creation before persistence)
  - `Secret bool` (`true` for `secret_env` and `secret_file`)
  - `CreatedAt, UpdatedAt time.Time`
- `ValidateEnvKey(raw string) (string, error)`:
  - Trims space, checks `^[A-Za-z_][A-Za-z0-9_]*$`, and rejects reserved names (`PATH`, `HOME`, `USER`, `TF_IN_AUTOMATION`, `TF_INPUT`, `DATABASE_URL`, `POSTGRES_PASSWORD`, `BASIC_AUTH_USER`, `BASIC_AUTH_PASSWORD`, `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_APP_PRIVATE_KEY_FILE`, `GITHUB_APP_WEBHOOK_SECRET`, `GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET`, `CONTEXT_ENCRYPTION_KEY`, `TF_LOG`, `GIT_TRACE`).
- `ValidateFilePath(raw string) (string, error)`:
  - Trims space, strips optional leading `~/`, rejects empty string, rejects paths starting with `/` or `\`, rejects paths containing `\x00` or `..` segments, normalizes with `path.Clean`, and ensures the result is not `.` or `..` and does not start with `../`.
- `Resolve(contexts []*Context) (env map[string]string, files map[string][]byte)`:
  - Iterates `contexts` in attachment order (`0..N-1`) and their entries, applying later values over earlier ones into `env` (for `plain_env` and `secret_env`) and `files` (for `secret_file`).

---

## 4. Infrastructure Components

### 4.1 Encryption at Rest (`internal/infrastructure/crypto`)
- `Sealer` struct holding a 32-byte AES-256 key.
- `NewSealer(secretMaterial []byte) (*Sealer, error)`: derives a 32-byte key via `sha256.Sum256(secretMaterial)` (requires non-empty `secretMaterial`).
- `Seal(plaintext []byte) ([]byte, error)`: generates a 12-byte random nonce via `crypto/rand`, encrypts with AES-256-GCM, and returns `nonce || ciphertext`.
- `Open(sealed []byte) ([]byte, error)`: splits 12-byte nonce and decrypts with AES-256-GCM.
- In `cmd/server/main.go`: uses `os.Getenv("CONTEXT_ENCRYPTION_KEY")` if non-empty; otherwise falls back to the PEM bytes of `GITHUB_APP_PRIVATE_KEY_FILE` (with domain separation prefix `"infra-guy-context-v1:"`). Also adds `"CONTEXT_ENCRYPTION_KEY": true` to `terraform.blockedEnv`.

### 4.2 Terraform Version Catalog & Binary Installer (`internal/infrastructure/terraform`)
- **Version Catalog (`versions.go`):**
  - `VersionCatalog` fetches `https://releases.hashicorp.com/terraform/index.json` (with 5s timeout and 1h TTL cache), parses version keys matching `^\d+\.\d+\.\d+$` with major `>= 1`, sorts them descending by semver, and returns up to the latest 30 stable versions (always ensuring `1.9.8` is included).
  - If the HTTP call fails, returns the fallback list: `[]string{"1.11.4", "1.10.5", "1.9.8", "1.8.5", "1.7.5", "1.6.6", "1.5.7"}`.
- **Binary Installer (`installer.go`):**
  - `BinaryLocator` interface: `Ensure(ctx context.Context, version string) (string, error)` returning the executable path.
  - `Installer`:
    - Validates `version` with `project.ValidateTerraformVersion`.
    - Checks `<CacheDir>/terraform-<version>`: if an executable file exists, returns its path.
    - If `DefaultBin` (e.g. `"terraform"` in `PATH`) reports `version` via `terraform version -json`, caches or returns that path without network download.
    - Otherwise downloads `<BaseURL>/terraform/<version>/terraform_<version>_<GOOS>_<GOARCH>.zip` (default `BaseURL = "https://releases.hashicorp.com"`), extracts the `terraform` entry from the zip archive (bounded to 250 MB to prevent zip bombs), writes to `<CacheDir>/.tmp-terraform-<version>-<rand>` with `0755`, and atomically renames to `<CacheDir>/terraform-<version>`.

### 4.3 Terraform Executor (`internal/infrastructure/terraform/terraform.go`)
- Extend `terraform.Request` and `worker.Request` with:
  - `TerraformVersion string`
  - `Env map[string]string`
  - `Files map[string][]byte`
- `Executor` holds `Binary BinaryLocator` (if `nil`, defaults to `"terraform"` on `PATH` for backward compatibility in existing unit tests).
- During `Executor.Run(ctx, req)`:
  1. Resolves binary path via `Binary.Ensure(ctx, req.TerraformVersion)` when `Binary != nil` and `req.TerraformVersion != ""`.
  2. Creates `<req.WorkDir>/.home` with mode `0700`.
  3. Writes each `relPath, content` in `req.Files` to `filepath.Join(req.WorkDir, ".home", filepath.FromSlash(relPath))` (creating parent dirs with `0700` and file with `0600`), verifying that the destination path stays within `<req.WorkDir>/.home`.
  4. Builds process environment via `commandEnv(req.GitHubToken, homeDir, req.Env)`:
     - Filters `os.Environ()` through `blockedEnv` (also stripping existing `HOME=`).
     - Appends user `req.Env` entries (sorted by key for determinism, re-checking `!blockedEnv[k]`).
     - Appends `HOME=<homeDir>`, `TF_IN_AUTOMATION=1`, `TF_INPUT=0` last so they cannot be overridden.

---

## 5. Application & HTTP UI Layer

### 5.1 `internal/application/tfcontext`
- `Service`:
  - `List(ctx, tenantID) ([]*tfcontext.Context, error)` (metadata + entry keys/kinds; secret values masked/empty).
  - `Get(ctx, tenantID, id) (*tfcontext.Context, error)` (verifies tenant ownership).
  - `Create(ctx, tenantID, name) (*tfcontext.Context, error)`
  - `Delete(ctx, tenantID, id) error`
  - `UpsertEntry(ctx, tenantID, contextID, kind, key, value) error`
  - `DeleteEntry(ctx, tenantID, contextID, entryID) error`
  - `SetProjectContexts(ctx, tenantID, projectID, contextIDs []tfcontext.ID) error` (verifies all `contextIDs` belong to `tenantID`).
  - `ListProjectContexts(ctx, projectID) ([]*tfcontext.Context, error)`
  - `ResolveForProject(ctx, projectID) (env map[string]string, files map[string][]byte, err error)` (decrypts secret entries and merges in attachment order for the worker).

### 5.2 UI Routes & Templates (`internal/interfaces/http`)
- **Topbar (`layout.html`):** Add navigation links for `Projects` (`/`) and `Contexts` (`/contexts`).
- **Context Pages:**
  - `GET /contexts` -> lists tenant contexts (`contexts.html`) + inline form to create a new context (`POST /contexts`).
  - `GET /contexts/{id}` -> shows context details (`context.html`), table of entries (`Kind`, `Key / Path in $HOME`, `Value` showing plaintext for `plain_env` and `••••••••` for `secret_env`/`secret_file`, and a `Delete` button per entry), form to add/replace an entry (`POST /contexts/{id}/entries`), and button to delete the context (`POST /contexts/{id}/delete`).
- **Project Pages (`new_project.html` & `details.html`):**
  - **Terraform Version `<select name="terraform_version">`**: populated from `VersionCatalog.List(ctx)`.
  - **Attached Contexts (`<input type="checkbox" name="context_ids" value="{{.ID}}">`)**: lists the tenant's contexts so the user can attach/detach contexts when creating or editing a project.

---

## 6. Testing Strategy

1. **Unit Tests:**
   - `internal/domain/project`: `SetTerraformVersion` semver validation and default fallback.
   - `internal/domain/tfcontext`: env key validation, reserved key rejection, `$HOME` file path normalization & traversal rejection (`../`, `/etc/passwd`, `~/.kube/config`), and multi-context override resolution.
   - `internal/infrastructure/crypto`: AES-256-GCM seal/open round-trip, random nonce uniqueness, tamper detection.
   - `internal/infrastructure/terraform`:
     - `VersionCatalog` parsing of HashiCorp JSON index + offline fallback.
     - `Installer` downloading & extracting a zip archive from an `httptest.Server` + caching behavior.
     - `Executor` injecting `Env`, creating isolated `HOME=<workDir>/.home`, materializing `0600` secret files, and invoking the selected Terraform binary.
2. **Postgres Integration Tests (`internal/infrastructure/postgres`):**
   - `ProjectRepo` persisting and loading `terraform_version`.
   - `ContextRepo` CRUD, encrypted entry storage (`value_text` empty and `value_ciphertext` non-empty for secrets), ordered `project_contexts` attachment, and decryption via `ResolveForProject`.
3. **HTTP UI Tests (`internal/interfaces/http`):**
   - Creating and updating a project with `terraform_version` and `context_ids`.
   - Creating a context, adding `plain_env`, `secret_env`, and `secret_file` entries, verifying secret values are never rendered in `GET /contexts/{id}`, and deleting entries/contexts.
