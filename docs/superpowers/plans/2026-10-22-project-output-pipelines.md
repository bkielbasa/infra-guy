# Project Output Pipelines Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enable Terraform projects in `infra-guy` to map outputs from upstream projects into `TF_VAR_*` environment variables and automatically trigger downstream runs when an upstream tracked apply run succeeds.

**Architecture:**
1. Parse top-level `"outputs"` from stored `.tfstate` JSON in `internal/domain/state` and compute changed outputs between consecutive state history versions.
2. Introduce a `dependency` domain (`internal/domain/dependency`), Postgres schema (`007_project_dependencies.sql`), repository (`internal/infrastructure/postgres/dependency_repo.go`), and application service (`internal/application/dependency/service.go`) with cycle detection, output-to-`TF_VAR_` resolution, and downstream trigger orchestration.
3. Integrate `DependencyResolver` into `worker.Worker` so mapped upstream outputs are injected into the Terraform execution environment (`TF_VAR_*`), wire `runSvc.OnTerminal` in `cmd/server/main.go` (before `jobID > 0` so K8s worker pods also trigger downstream runs), and expose a **Dependencies** tab on the project UI page.

**Tech Stack:** Go 1.24, PostgreSQL (`pgx/v5`), HTML templates + HTMX (`internal/interfaces/http`).

---

## File Structure

- **Modify:** `internal/domain/state/state.go` — Add `OutputValue`, `ExtractOutputs`, `Snapshot.Outputs()`, and `Snapshot.ChangedOutputsSincePrevious()`.
- **Modify:** `internal/domain/state/state_test.go` — Unit tests for `.tfstate` output extraction and diffing.
- **Modify:** `internal/domain/run/run.go` — Add `TriggerPipeline Trigger = "pipeline"`.
- **Create:** `internal/domain/dependency/dependency.go` — Domain model (`Dependency`, `Snapshot`, `Mapping`, `Edge`, `Repository`), `EnvKey()`, self-dependency check, and `WouldCreateCycle`.
- **Create:** `internal/domain/dependency/dependency_test.go` — Unit tests for mapping validation, `EnvKey()`, self-dependency rejection, and cycle detection.
- **Create:** `internal/infrastructure/postgres/migrations/007_project_dependencies.sql` — Tables `project_dependencies` and `project_dependency_mappings`.
- **Create:** `internal/infrastructure/postgres/dependency_repo.go` — `DependencyRepo` implementing `dependency.Repository`.
- **Create:** `internal/application/dependency/service.go` — Application service (`Create`, `SetTriggerAlways`, `Delete`, `AddMapping`, `RemoveMapping`, `ListByProject`, `ListDownstream`, `ResolveForProject`, `ShouldTriggerDownstream`, `TriggerDownstreamRuns`).
- **Create:** `internal/application/dependency/service_test.go` — Unit tests for CRUD, tenant isolation, cycle prevention, output resolution, and downstream run triggering.
- **Modify:** `internal/application/worker/worker.go` — Add `DependencyResolver` interface and `Dependencies` field on `Worker`, merging resolved `TF_VAR_*` env vars in `runJob`.
- **Modify:** `internal/application/worker/worker_test.go` — Test that worker resolves upstream dependency env vars and passes them to `Executor`.
- **Modify:** `cmd/server/main.go` — Instantiate `dependencyRepo` and `dependencySvc`, pass `Dependencies: dependencySvc` to `worker.Worker` and `httpapi.ServerConfig`, and invoke `dependencySvc.TriggerDownstreamRuns` inside `runSvc.OnTerminal`.
- **Modify:** `internal/interfaces/http/server.go` — Add `Dependencies *dependencyapp.Service` to `ServerConfig`.
- **Modify:** `internal/interfaces/http/ui.go` — Update `isTracked` to include `run.TriggerPipeline`; add routes and handlers for the project Dependencies tab (`tabDependencies`, `createDependency`, `updateDependencyTriggerMode`, `deleteDependency`, `addDependencyMapping`, `deleteDependencyMapping`).
- **Modify:** `internal/interfaces/http/templates/project.html` — Add `Dependencies` tab link.
- **Create:** `internal/interfaces/http/templates/tab_dependencies.html` — Template for managing upstream dependencies, output mappings, and viewing downstream dependents.
- **Modify:** `internal/interfaces/http/ui_test.go` — HTTP tests for the Dependencies tab, adding/removing dependencies and mappings, and `TriggerPipeline` in tracked runs.

---

### Task 1: State Output Extraction & `TriggerPipeline`

**Files:**
- Modify: `internal/domain/state/state.go`
- Modify: `internal/domain/state/state_test.go`
- Modify: `internal/domain/run/run.go`

- [ ] **Step 1: Write failing tests in `internal/domain/state/state_test.go`**

Append the following tests to `internal/domain/state/state_test.go`:

```go
func TestExtractOutputsAndChangedOutputs(t *testing.T) {
	tfstateV1 := []byte(`{
		"version": 4,
		"serial": 1,
		"outputs": {
			"vpc_id": {
				"value": "vpc-12345",
				"type": "string",
				"sensitive": false
			},
			"subnet_ids": {
				"value": ["sub-1", "sub-2"],
				"type": ["tuple", ["string", "string"]],
				"sensitive": false
			},
			"db_password": {
				"value": "s3cr3t",
				"type": "string",
				"sensitive": true
			},
			"empty_val": {
				"value": null,
				"type": "string"
			}
		}
	}`)

	outs, err := ExtractOutputs(tfstateV1)
	if err != nil {
		t.Fatalf("ExtractOutputs failed: %v", err)
	}
	if len(outs) != 3 {
		t.Fatalf("expected 3 non-null outputs, got %d: %+v", len(outs), outs)
	}
	if outs["vpc_id"].EnvString != "vpc-12345" || outs["vpc_id"].Sensitive {
		t.Fatalf("unexpected vpc_id output: %+v", outs["vpc_id"])
	}
	if outs["subnet_ids"].EnvString != `["sub-1","sub-2"]` {
		t.Fatalf("unexpected subnet_ids EnvString: %q", outs["subnet_ids"].EnvString)
	}
	if outs["db_password"].EnvString != "s3cr3t" || !outs["db_password"].Sensitive {
		t.Fatalf("unexpected db_password output: %+v", outs["db_password"])
	}

	st := New("proj-1")
	now := time.Unix(100, 0).UTC()
	if err := st.Put(tfstateV1, "", "run-1", now); err != nil {
		t.Fatal(err)
	}
	changed1 := st.Snapshot().ChangedOutputsSincePrevious()
	if !changed1["vpc_id"] || !changed1["subnet_ids"] || !changed1["db_password"] {
		t.Fatalf("expected all outputs marked changed on first state, got %+v", changed1)
	}

	tfstateV2 := []byte(`{
		"version": 4,
		"serial": 2,
		"outputs": {
			"vpc_id": {
				"value": "vpc-12345",
				"type": "string"
			},
			"subnet_ids": {
				"value": ["sub-1", "sub-2", "sub-3"],
				"type": ["tuple", ["string", "string", "string"]]
			}
		}
	}`)
	if err := st.Put(tfstateV2, "", "run-2", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	changed2 := st.Snapshot().ChangedOutputsSincePrevious()
	if changed2["vpc_id"] {
		t.Fatalf("expected vpc_id unchanged, got %+v", changed2)
	}
	if !changed2["subnet_ids"] {
		t.Fatalf("expected subnet_ids changed, got %+v", changed2)
	}
	if !changed2["db_password"] {
		t.Fatalf("expected removed output db_password marked changed, got %+v", changed2)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/state/...`
Expected: FAIL (`undefined: ExtractOutputs`).

- [ ] **Step 3: Implement `ExtractOutputs`, `Snapshot.Outputs()`, `Snapshot.ChangedOutputsSincePrevious()`, and `run.TriggerPipeline`**

In `internal/domain/state/state.go`, add `"bytes"` to imports and append:

```go
type OutputValue struct {
	Raw       json.RawMessage
	EnvString string
	Sensitive bool
}

func ExtractOutputs(data []byte) (map[string]OutputValue, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]OutputValue{}, nil
	}
	var payload struct {
		Outputs map[string]struct {
			Value     json.RawMessage `json:"value"`
			Sensitive bool            `json:"sensitive"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	out := make(map[string]OutputValue, len(payload.Outputs))
	for k, v := range payload.Outputs {
		raw := bytes.TrimSpace(v.Value)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			continue
		}
		var compacted bytes.Buffer
		if err := json.Compact(&compacted, raw); err != nil {
			compacted.Reset()
			compacted.Write(raw)
		}
		compactBytes := compacted.Bytes()
		var strVal string
		if err := json.Unmarshal(compactBytes, &strVal); err != nil {
			strVal = string(compactBytes)
		}
		out[k] = OutputValue{
			Raw:       append(json.RawMessage(nil), compactBytes...),
			EnvString: strVal,
			Sensitive: v.Sensitive,
		}
	}
	return out, nil
}

func (s Snapshot) Outputs() map[string]OutputValue {
	outs, err := ExtractOutputs(s.Data)
	if err != nil {
		return map[string]OutputValue{}
	}
	return outs
}

func (s Snapshot) ChangedOutputsSincePrevious() map[string]bool {
	curr := s.Outputs()
	changed := make(map[string]bool, len(curr))
	if len(s.History) <= 1 {
		for k := range curr {
			changed[k] = true
		}
		return changed
	}
	prev, err := ExtractOutputs(s.History[len(s.History)-2].Data)
	if err != nil {
		for k := range curr {
			changed[k] = true
		}
		return changed
	}
	for k, cv := range curr {
		pv, ok := prev[k]
		if !ok || !bytes.Equal(cv.Raw, pv.Raw) {
			changed[k] = true
		}
	}
	for k := range prev {
		if _, ok := curr[k]; !ok {
			changed[k] = true
		}
	}
	return changed
}
```

In `internal/domain/run/run.go`, add `TriggerPipeline`:

```go
const (
	TriggerPullRequest Trigger = "pull_request"
	TriggerPush        Trigger = "push"
	TriggerManual      Trigger = "manual"
	TriggerPipeline    Trigger = "pipeline"
)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/domain/state/... ./internal/domain/run/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/state/state.go internal/domain/state/state_test.go internal/domain/run/run.go
git commit -m "feat(state): extract tfstate outputs and add pipeline run trigger"
```

---

### Task 2: Dependency Domain Model & Cycle Detection

**Files:**
- Create: `internal/domain/dependency/dependency.go`
- Create: `internal/domain/dependency/dependency_test.go`

- [ ] **Step 1: Write failing tests in `internal/domain/dependency/dependency_test.go`**

```go
package dependency

import (
	"errors"
	"testing"
	"time"

	"infra-guy/internal/domain/project"
)

func TestNewDependencyValidation(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	if _, err := New("dep-1", "ten-1", "proj-a", "proj-a", false, now); !errors.Is(err, ErrSelfDependency) {
		t.Fatalf("expected ErrSelfDependency, got %v", err)
	}
	dep, err := New("dep-1", "ten-1", "proj-b", "proj-a", true, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dep.Snapshot().ProjectID != "proj-b" || dep.Snapshot().DependsOnProjectID != "proj-a" || !dep.Snapshot().TriggerAlways {
		t.Fatalf("unexpected snapshot: %+v", dep.Snapshot())
	}
}

func TestNewMappingAndEnvKey(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	m1, err := NewMapping("m-1", "vpc_id", "", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m1.InputName != "vpc_id" || m1.EnvKey() != "TF_VAR_vpc_id" {
		t.Fatalf("expected default input_name=vpc_id and TF_VAR_vpc_id, got %+v (%s)", m1, m1.EnvKey())
	}

	m2, err := NewMapping("m-2", "vpc_id", "TF_VAR_network_vpc_id", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m2.InputName != "network_vpc_id" || m2.EnvKey() != "TF_VAR_network_vpc_id" {
		t.Fatalf("expected normalized input_name=network_vpc_id and TF_VAR_network_vpc_id, got %+v (%s)", m2, m2.EnvKey())
	}

	if _, err := NewMapping("m-3", "bad name!", "vpc_id", now); err == nil {
		t.Fatal("expected error for invalid output_name")
	}
}

func TestWouldCreateCycle(t *testing.T) {
	edges := []Edge{
		{ProjectID: "proj-b", DependsOnProjectID: "proj-a"},
		{ProjectID: "proj-c", DependsOnProjectID: "proj-b"},
	}
	if WouldCreateCycle(edges, "proj-d", "proj-c") {
		t.Fatal("proj-d -> proj-c should not create a cycle")
	}
	if !WouldCreateCycle(edges, "proj-a", "proj-c") {
		t.Fatal("proj-a -> proj-c should create a cycle (c -> b -> a -> c)")
	}
	if !WouldCreateCycle(edges, "proj-a", "proj-a") {
		t.Fatal("self edge should be detected as cycle")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/dependency/...`
Expected: FAIL.

- [ ] **Step 3: Implement `internal/domain/dependency/dependency.go`**

```go
package dependency

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"infra-guy/internal/domain/project"
)

var (
	ErrSelfDependency = errors.New("project cannot depend on itself")
	ErrCycleDetected  = errors.New("dependency would create a cycle")
	identPattern      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type ID string

type Mapping struct {
	ID         string
	OutputName string
	InputName  string
	CreatedAt  time.Time
}

func (m Mapping) EnvKey() string {
	if strings.HasPrefix(m.InputName, "TF_VAR_") {
		return m.InputName
	}
	return "TF_VAR_" + m.InputName
}

func NewMapping(id, outputName, inputName string, now time.Time) (Mapping, error) {
	if strings.TrimSpace(id) == "" {
		return Mapping{}, errors.New("mapping id is required")
	}
	outName := strings.TrimSpace(outputName)
	if !identPattern.MatchString(outName) {
		return Mapping{}, fmt.Errorf("invalid output name %q", outName)
	}
	inName := strings.TrimSpace(inputName)
	inName = strings.TrimPrefix(inName, "TF_VAR_")
	if inName == "" {
		inName = outName
	}
	if !identPattern.MatchString(inName) {
		return Mapping{}, fmt.Errorf("invalid input variable name %q", inName)
	}
	return Mapping{
		ID:         id,
		OutputName: outName,
		InputName:  inName,
		CreatedAt:  now.UTC(),
	}, nil
}

type Edge struct {
	ProjectID          project.ID
	DependsOnProjectID project.ID
}

type Snapshot struct {
	ID                 ID
	TenantID           string
	ProjectID          project.ID
	DependsOnProjectID project.ID
	TriggerAlways      bool
	Mappings           []Mapping
	CreatedAt          time.Time
}

type Dependency struct {
	s Snapshot
}

func New(id ID, tenantID string, projectID, dependsOnProjectID project.ID, triggerAlways bool, now time.Time) (*Dependency, error) {
	if strings.TrimSpace(string(id)) == "" {
		return nil, errors.New("dependency id is required")
	}
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("tenant id is required")
	}
	if strings.TrimSpace(string(projectID)) == "" || strings.TrimSpace(string(dependsOnProjectID)) == "" {
		return nil, errors.New("project and upstream project are required")
	}
	if projectID == dependsOnProjectID {
		return nil, ErrSelfDependency
	}
	return &Dependency{
		s: Snapshot{
			ID:                 id,
			TenantID:           tenantID,
			ProjectID:          projectID,
			DependsOnProjectID: dependsOnProjectID,
			TriggerAlways:      triggerAlways,
			CreatedAt:          now.UTC(),
		},
	}, nil
}

func Restore(s Snapshot) *Dependency {
	cp := s
	cp.Mappings = append([]Mapping(nil), s.Mappings...)
	return &Dependency{s: cp}
}

func (d *Dependency) Snapshot() Snapshot {
	cp := d.s
	cp.Mappings = append([]Mapping(nil), d.s.Mappings...)
	return cp
}

func (d *Dependency) SetMappings(mappings []Mapping) {
	d.s.Mappings = append([]Mapping(nil), mappings...)
}

func (d *Dependency) SetTriggerAlways(v bool) {
	d.s.TriggerAlways = v
}

// WouldCreateCycle checks whether adding an edge (downstream depends on upstream)
// would introduce a cycle in the directed dependency graph.
func WouldCreateCycle(edges []Edge, downstream, upstream project.ID) bool {
	if downstream == upstream {
		return true
	}
	adj := make(map[project.ID][]project.ID)
	for _, e := range edges {
		adj[e.ProjectID] = append(adj[e.ProjectID], e.DependsOnProjectID)
	}
	visited := make(map[project.ID]bool)
	queue := []project.ID{upstream}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		if curr == downstream {
			return true
		}
		if visited[curr] {
			continue
		}
		visited[curr] = true
		for _, next := range adj[curr] {
			if !visited[next] {
				queue = append(queue, next)
			}
		}
	}
	return false
}

type Repository interface {
	Create(ctx context.Context, d *Dependency) error
	Get(ctx context.Context, id ID) (*Dependency, error)
	SetTriggerAlways(ctx context.Context, id ID, triggerAlways bool) error
	Delete(ctx context.Context, id ID) error
	UpsertMapping(ctx context.Context, depID ID, m Mapping) error
	DeleteMapping(ctx context.Context, depID ID, mappingID string) error
	ListByProject(ctx context.Context, projectID project.ID) ([]*Dependency, error)
	ListDownstream(ctx context.Context, upstreamProjectID project.ID) ([]*Dependency, error)
	ListByTenant(ctx context.Context, tenantID string) ([]*Dependency, error)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/domain/dependency/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/dependency/dependency.go internal/domain/dependency/dependency_test.go
git commit -m "feat(dependency): add dependency domain model and cycle detection"
```

---

### Task 3: Postgres Migration & Dependency Repository

**Files:**
- Create: `internal/infrastructure/postgres/migrations/007_project_dependencies.sql`
- Create: `internal/infrastructure/postgres/dependency_repo.go`

- [ ] **Step 1: Create migration `internal/infrastructure/postgres/migrations/007_project_dependencies.sql`**

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

- [ ] **Step 2: Create `internal/infrastructure/postgres/dependency_repo.go`**

```go
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/dependency"
	"infra-guy/internal/domain/project"
)

var _ dependency.Repository = (*DependencyRepo)(nil)

type DependencyRepo struct {
	pool *pgxpool.Pool
}

func NewDependencyRepo(pool *pgxpool.Pool) *DependencyRepo {
	return &DependencyRepo{pool: pool}
}

func (r *DependencyRepo) Create(ctx context.Context, d *dependency.Dependency) error {
	s := d.Snapshot()
	_, err := r.pool.Exec(ctx, `
		INSERT INTO project_dependencies (id, tenant_id, project_id, depends_on_project_id, trigger_always, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		s.ID, s.TenantID, s.ProjectID, s.DependsOnProjectID, s.TriggerAlways, s.CreatedAt,
	)
	return err
}

func (r *DependencyRepo) Get(ctx context.Context, id dependency.ID) (*dependency.Dependency, error) {
	var s dependency.Snapshot
	var created time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, project_id, depends_on_project_id, trigger_always, created_at
		FROM project_dependencies WHERE id = $1`, id,
	).Scan(&s.ID, &s.TenantID, &s.ProjectID, &s.DependsOnProjectID, &s.TriggerAlways, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt = utcTime(created)
	mappings, err := r.loadMappings(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	s.Mappings = mappings
	return dependency.Restore(s), nil
}

func (r *DependencyRepo) SetTriggerAlways(ctx context.Context, id dependency.ID, triggerAlways bool) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE project_dependencies SET trigger_always = $2 WHERE id = $1`,
		id, triggerAlways,
	)
	return err
}

func (r *DependencyRepo) Delete(ctx context.Context, id dependency.ID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM project_dependencies WHERE id = $1`, id)
	return err
}

func (r *DependencyRepo) UpsertMapping(ctx context.Context, depID dependency.ID, m dependency.Mapping) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO project_dependency_mappings (id, dependency_id, output_name, input_name, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (dependency_id, input_name) DO UPDATE SET
			output_name = EXCLUDED.output_name`,
		m.ID, depID, m.OutputName, m.InputName, m.CreatedAt,
	)
	return err
}

func (r *DependencyRepo) DeleteMapping(ctx context.Context, depID dependency.ID, mappingID string) error {
	_, err := r.pool.Exec(ctx, `
		DELETE FROM project_dependency_mappings WHERE dependency_id = $1 AND id = $2`,
		depID, mappingID,
	)
	return err
}

func (r *DependencyRepo) ListByProject(ctx context.Context, projectID project.ID) ([]*dependency.Dependency, error) {
	return r.listWhere(ctx, `project_id = $1`, projectID)
}

func (r *DependencyRepo) ListDownstream(ctx context.Context, upstreamProjectID project.ID) ([]*dependency.Dependency, error) {
	return r.listWhere(ctx, `depends_on_project_id = $1`, upstreamProjectID)
}

func (r *DependencyRepo) ListByTenant(ctx context.Context, tenantID string) ([]*dependency.Dependency, error) {
	return r.listWhere(ctx, `tenant_id = $1`, tenantID)
}

func (r *DependencyRepo) listWhere(ctx context.Context, predicate string, arg any) ([]*dependency.Dependency, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, project_id, depends_on_project_id, trigger_always, created_at
		FROM project_dependencies WHERE `+predicate+` ORDER BY created_at, id`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dependency.Dependency
	for rows.Next() {
		var s dependency.Snapshot
		var created time.Time
		if err := rows.Scan(&s.ID, &s.TenantID, &s.ProjectID, &s.DependsOnProjectID, &s.TriggerAlways, &created); err != nil {
			return nil, err
		}
		s.CreatedAt = utcTime(created)
		out = append(out, dependency.Restore(s))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, d := range out {
		mappings, err := r.loadMappings(ctx, d.Snapshot().ID)
		if err != nil {
			return nil, err
		}
		d.SetMappings(mappings)
	}
	return out, nil
}

func (r *DependencyRepo) loadMappings(ctx context.Context, depID dependency.ID) ([]dependency.Mapping, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, output_name, input_name, created_at
		FROM project_dependency_mappings WHERE dependency_id = $1 ORDER BY created_at, id`, depID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dependency.Mapping
	for rows.Next() {
		var m dependency.Mapping
		var created time.Time
		if err := rows.Scan(&m.ID, &m.OutputName, &m.InputName, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = utcTime(created)
		out = append(out, m)
	}
	return out, rows.Err()
}
```

- [ ] **Step 3: Verify build and tests**

Run: `go test ./internal/infrastructure/postgres/...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/infrastructure/postgres/migrations/007_project_dependencies.sql internal/infrastructure/postgres/dependency_repo.go
git commit -m "feat(postgres): add project_dependencies schema and repository"
```

---

### Task 4: Dependency Application Service

**Files:**
- Create: `internal/application/dependency/service.go`
- Create: `internal/application/dependency/service_test.go`

- [ ] **Step 1: Write failing tests in `internal/application/dependency/service_test.go`**

```go
package dependencyapp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	runapp "infra-guy/internal/application/run"
	"infra-guy/internal/domain/dependency"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/domain/state"
)

type memDepRepo struct {
	items map[dependency.ID]*dependency.Dependency
}

func newMemDepRepo() *memDepRepo {
	return &memDepRepo{items: map[dependency.ID]*dependency.Dependency{}}
}

func (m *memDepRepo) Create(_ context.Context, d *dependency.Dependency) error {
	for _, existing := range m.items {
		es := existing.Snapshot()
		if es.ProjectID == d.Snapshot().ProjectID && es.DependsOnProjectID == d.Snapshot().DependsOnProjectID {
			return errors.New("duplicate dependency")
		}
	}
	m.items[d.Snapshot().ID] = dependency.Restore(d.Snapshot())
	return nil
}

func (m *memDepRepo) Get(_ context.Context, id dependency.ID) (*dependency.Dependency, error) {
	d, ok := m.items[id]
	if !ok {
		return nil, nil
	}
	return dependency.Restore(d.Snapshot()), nil
}

func (m *memDepRepo) SetTriggerAlways(_ context.Context, id dependency.ID, triggerAlways bool) error {
	d, ok := m.items[id]
	if !ok {
		return nil
	}
	d.SetTriggerAlways(triggerAlways)
	return nil
}

func (m *memDepRepo) Delete(_ context.Context, id dependency.ID) error {
	delete(m.items, id)
	return nil
}

func (m *memDepRepo) UpsertMapping(_ context.Context, depID dependency.ID, mp dependency.Mapping) error {
	d := m.items[depID]
	s := d.Snapshot()
	replaced := false
	for i := range s.Mappings {
		if s.Mappings[i].InputName == mp.InputName {
			s.Mappings[i].OutputName = mp.OutputName
			replaced = true
			break
		}
	}
	if !replaced {
		s.Mappings = append(s.Mappings, mp)
	}
	m.items[depID] = dependency.Restore(s)
	return nil
}

func (m *memDepRepo) DeleteMapping(_ context.Context, depID dependency.ID, mappingID string) error {
	d := m.items[depID]
	s := d.Snapshot()
	var kept []dependency.Mapping
	for _, mp := range s.Mappings {
		if mp.ID != mappingID {
			kept = append(kept, mp)
		}
	}
	s.Mappings = kept
	m.items[depID] = dependency.Restore(s)
	return nil
}

func (m *memDepRepo) ListByProject(_ context.Context, projectID project.ID) ([]*dependency.Dependency, error) {
	var out []*dependency.Dependency
	for _, d := range m.items {
		if d.Snapshot().ProjectID == projectID {
			out = append(out, dependency.Restore(d.Snapshot()))
		}
	}
	return out, nil
}

func (m *memDepRepo) ListDownstream(_ context.Context, upstreamProjectID project.ID) ([]*dependency.Dependency, error) {
	var out []*dependency.Dependency
	for _, d := range m.items {
		if d.Snapshot().DependsOnProjectID == upstreamProjectID {
			out = append(out, dependency.Restore(d.Snapshot()))
		}
	}
	return out, nil
}

func (m *memDepRepo) ListByTenant(_ context.Context, tenantID string) ([]*dependency.Dependency, error) {
	var out []*dependency.Dependency
	for _, d := range m.items {
		if d.Snapshot().TenantID == tenantID {
			out = append(out, dependency.Restore(d.Snapshot()))
		}
	}
	return out, nil
}

type memProjRepo struct {
	items map[project.ID]*project.Project
}

func (m *memProjRepo) Create(_ context.Context, p *project.Project) error {
	m.items[p.Snapshot().ID] = project.Restore(p.Snapshot())
	return nil
}
func (m *memProjRepo) Get(_ context.Context, id project.ID) (*project.Project, error) {
	p, ok := m.items[id]
	if !ok {
		return nil, nil
	}
	return project.Restore(p.Snapshot()), nil
}
func (m *memProjRepo) GetByRepoAndDir(_ context.Context, _ int64, _ string) (*project.Project, error) {
	return nil, nil
}
func (m *memProjRepo) ListByRepo(_ context.Context, _ int64) ([]*project.Project, error) {
	return nil, nil
}
func (m *memProjRepo) ListByTenant(_ context.Context, tenantID string) ([]*project.Project, error) {
	var out []*project.Project
	for _, p := range m.items {
		if p.Snapshot().TenantID == tenantID {
			out = append(out, project.Restore(p.Snapshot()))
		}
	}
	return out, nil
}
func (m *memProjRepo) Save(_ context.Context, p *project.Project) error {
	m.items[p.Snapshot().ID] = project.Restore(p.Snapshot())
	return nil
}
func (m *memProjRepo) Delete(_ context.Context, id project.ID) error {
	delete(m.items, id)
	return nil
}

type memStateRepo struct {
	items map[string]*state.State
}

func (m *memStateRepo) Get(_ context.Context, projectID string) (*state.State, error) {
	if st, ok := m.items[projectID]; ok {
		return state.Restore(st.Snapshot()), nil
	}
	return state.New(projectID), nil
}
func (m *memStateRepo) Save(_ context.Context, st *state.State) error {
	m.items[st.Snapshot().ProjectID] = state.Restore(st.Snapshot())
	return nil
}

type fakeRunOpener struct {
	opened []runapp.OpenRequest
}

func (f *fakeRunOpener) Open(_ context.Context, req runapp.OpenRequest) (*run.Run, error) {
	f.opened = append(f.opened, req)
	return run.Open(run.OpenInput{
		ID:        run.ID(fmt.Sprintf("run-%d", len(f.opened))),
		ProjectID: run.ProjectID(req.ProjectID),
		SHA:       req.SHA,
		Trigger:   req.Trigger,
		Now:       time.Unix(100, 0).UTC(),
	})
}

type fakeTokenSource struct{}

func (fakeTokenSource) InstallationToken(_ context.Context, _ string) (string, error) {
	return "ghs_test", nil
}

type fakeBrancher struct{}

func (fakeBrancher) BranchTip(_ context.Context, _, _, _ string) (string, error) {
	return "deadbeef1234567", nil
}

type fakeCommitLookup struct{}

func (fakeCommitLookup) Lookup(_ context.Context, _ project.SourceRepo, _, _ string) (string, string, error) {
	return "feat: downstream commit", "alice", nil
}

func TestServiceCycleDetectionAndResolution(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(100, 0).UTC()
	pNet, _ := project.New("p-net", "ten-1", "network", project.SourceRepo{Owner: "o", Name: "r"}, 1, "main", "net", true, "t1", now)
	pDb, _ := project.New("p-db", "ten-1", "database", project.SourceRepo{Owner: "o", Name: "r"}, 1, "main", "db", true, "t2", now)
	pApp, _ := project.New("p-app", "ten-1", "app", project.SourceRepo{Owner: "o", Name: "r"}, 1, "main", "app", false, "t3", now)
	pOther, _ := project.New("p-other", "ten-2", "other", project.SourceRepo{Owner: "o", Name: "r"}, 2, "main", "other", false, "t4", now)

	projs := &memProjRepo{items: map[project.ID]*project.Project{
		"p-net": pNet, "p-db": pDb, "p-app": pApp, "p-other": pOther,
	}}
	stNet := state.New("p-net")
	_ = stNet.Put([]byte(`{"version":4,"serial":1,"outputs":{"vpc_id":{"value":"vpc-999","type":"string"},"subnets":{"value":["s1","s2"]}}}`), "", "r1", now)
	states := &memStateRepo{items: map[string]*state.State{"p-net": stNet}}

	seq := 0
	svc := &Service{
		Dependencies: newMemDepRepo(),
		Projects:     projs,
		States:       states,
		Now:          func() time.Time { return now },
		NewID: func() string {
			seq++
			return fmt.Sprintf("id-%d", seq)
		},
	}

	// Cross-tenant dependency is rejected
	if _, err := svc.Create(ctx, "ten-1", "p-db", "p-other", false); err == nil {
		t.Fatal("expected cross-tenant dependency creation to fail")
	}

	// Valid chain: p-app -> p-db -> p-net
	depDbNet, err := svc.Create(ctx, "ten-1", "p-db", "p-net", false)
	if err != nil {
		t.Fatalf("Create p-db -> p-net failed: %v", err)
	}
	if _, err := svc.Create(ctx, "ten-1", "p-app", "p-db", false); err != nil {
		t.Fatalf("Create p-app -> p-db failed: %v", err)
	}

	// Cycle: p-net -> p-app must fail with ErrCycleDetected
	if _, err := svc.Create(ctx, "ten-1", "p-net", "p-app", false); !errors.Is(err, dependency.ErrCycleDetected) {
		t.Fatalf("expected ErrCycleDetected, got %v", err)
	}

	// Add mappings on p-db -> p-net
	if err := svc.AddMapping(ctx, "ten-1", depDbNet.Snapshot().ID, "vpc_id", ""); err != nil {
		t.Fatalf("AddMapping vpc_id failed: %v", err)
	}
	if err := svc.AddMapping(ctx, "ten-1", depDbNet.Snapshot().ID, "subnets", "network_subnets"); err != nil {
		t.Fatalf("AddMapping subnets failed: %v", err)
	}
	if err := svc.AddMapping(ctx, "ten-1", depDbNet.Snapshot().ID, "missing_out", "missing_in"); err != nil {
		t.Fatalf("AddMapping missing_out failed: %v", err)
	}

	env, msgs, err := svc.ResolveForProject(ctx, "p-db")
	if err != nil {
		t.Fatalf("ResolveForProject failed: %v", err)
	}
	if env["TF_VAR_vpc_id"] != "vpc-999" {
		t.Fatalf("expected TF_VAR_vpc_id=vpc-999, got %q", env["TF_VAR_vpc_id"])
	}
	if env["TF_VAR_network_subnets"] != `["s1","s2"]` {
		t.Fatalf("expected TF_VAR_network_subnets=[\"s1\",\"s2\"], got %q", env["TF_VAR_network_subnets"])
	}
	if _, ok := env["TF_VAR_missing_in"]; ok {
		t.Fatalf("expected missing output not to be injected into env")
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 log messages, got %d: %v", len(msgs), msgs)
	}

	// TriggerDownstreamRuns when vpc_id changed
	opener := &fakeRunOpener{}
	svc.Runs = opener
	svc.Tokens = fakeTokenSource{}
	svc.Checkout = fakeBrancher{}
	svc.Lookup = fakeCommitLookup{}

	runNet, _ := run.Open(run.OpenInput{
		ID: "run-net-1", ProjectID: "p-net", Trigger: run.TriggerPush, SHA: "abc1234", PlanOnly: false, Now: now,
	})
	_ = runNet.BeginStep(run.StepInit, now)
	_, _ = runNet.CompleteStep(run.StepInit, run.StepResult{ExitCode: 0}, now)
	_ = runNet.BeginStep(run.StepPlan, now)
	_, _ = runNet.CompleteStep(run.StepPlan, run.StepResult{ExitCode: 0, PlanText: "Plan: 1 to add, 0 to change, 0 to destroy."}, now)
	_, _ = runNet.Confirm("alice", now)
	_ = runNet.BeginStep(run.StepApply, now)
	_, _ = runNet.CompleteStep(run.StepApply, run.StepResult{ExitCode: 0}, now)

	svc.TriggerDownstreamRuns(ctx, pNet, runNet)
	if len(opener.opened) != 1 {
		t.Fatalf("expected 1 downstream run triggered, got %d", len(opener.opened))
	}
	if opener.opened[0].ProjectID != "p-db" || opener.opened[0].Trigger != run.TriggerPipeline || opener.opened[0].TriggeredBy != "pipeline:network" {
		t.Fatalf("unexpected downstream OpenRequest: %+v", opener.opened[0])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/application/dependency/...`
Expected: FAIL.

- [ ] **Step 3: Implement `internal/application/dependency/service.go`**

```go
package dependencyapp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	runapp "infra-guy/internal/application/run"
	"infra-guy/internal/domain/dependency"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/domain/state"
)

var ErrNotFound = errors.New("dependency not found")

type RunOpener interface {
	Open(ctx context.Context, req runapp.OpenRequest) (*run.Run, error)
}

type TokenSource interface {
	InstallationToken(ctx context.Context, tenantID string) (string, error)
}

type Brancher interface {
	BranchTip(ctx context.Context, cloneURL, token, branch string) (string, error)
}

type CommitLookup interface {
	Lookup(ctx context.Context, repo project.SourceRepo, token, sha string) (message, author string, err error)
}

type Service struct {
	Dependencies dependency.Repository
	Projects     project.Repository
	States       state.Repository
	Runs         RunOpener
	Tokens       TokenSource
	Checkout     Brancher
	Lookup       CommitLookup
	Now          func() time.Time
	NewID        func() string
}

func (s *Service) Get(ctx context.Context, tenantID string, id dependency.ID) (*dependency.Dependency, error) {
	d, err := s.Dependencies.Get(ctx, id)
	if err != nil || d == nil {
		return nil, err
	}
	if d.Snapshot().TenantID != tenantID {
		return nil, nil
	}
	return d, nil
}

func (s *Service) Create(ctx context.Context, tenantID string, projectID, dependsOnProjectID project.ID, triggerAlways bool) (*dependency.Dependency, error) {
	p, err := s.Projects.Get(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if p == nil || p.Snapshot().TenantID != tenantID {
		return nil, ErrNotFound
	}
	up, err := s.Projects.Get(ctx, dependsOnProjectID)
	if err != nil {
		return nil, err
	}
	if up == nil || up.Snapshot().TenantID != tenantID {
		return nil, fmt.Errorf("upstream project not found")
	}

	existing, err := s.Dependencies.ListByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	edges := make([]dependency.Edge, 0, len(existing))
	for _, item := range existing {
		is := item.Snapshot()
		if is.ProjectID == projectID && is.DependsOnProjectID == dependsOnProjectID {
			return nil, fmt.Errorf("dependency on %s already exists", up.Snapshot().Name)
		}
		edges = append(edges, dependency.Edge{
			ProjectID:          is.ProjectID,
			DependsOnProjectID: is.DependsOnProjectID,
		})
	}
	if dependency.WouldCreateCycle(edges, projectID, dependsOnProjectID) {
		return nil, dependency.ErrCycleDetected
	}

	d, err := dependency.New(dependency.ID(s.NewID()), tenantID, projectID, dependsOnProjectID, triggerAlways, s.Now())
	if err != nil {
		return nil, err
	}
	if err := s.Dependencies.Create(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Service) SetTriggerAlways(ctx context.Context, tenantID string, depID dependency.ID, triggerAlways bool) error {
	d, err := s.Get(ctx, tenantID, depID)
	if err != nil {
		return err
	}
	if d == nil {
		return ErrNotFound
	}
	return s.Dependencies.SetTriggerAlways(ctx, depID, triggerAlways)
}

func (s *Service) Delete(ctx context.Context, tenantID string, depID dependency.ID) error {
	d, err := s.Get(ctx, tenantID, depID)
	if err != nil {
		return err
	}
	if d == nil {
		return ErrNotFound
	}
	return s.Dependencies.Delete(ctx, depID)
}

func (s *Service) AddMapping(ctx context.Context, tenantID string, depID dependency.ID, outputName, inputName string) error {
	d, err := s.Get(ctx, tenantID, depID)
	if err != nil {
		return err
	}
	if d == nil {
		return ErrNotFound
	}
	m, err := dependency.NewMapping(s.NewID(), outputName, inputName, s.Now())
	if err != nil {
		return err
	}
	return s.Dependencies.UpsertMapping(ctx, depID, m)
}

func (s *Service) RemoveMapping(ctx context.Context, tenantID string, depID dependency.ID, mappingID string) error {
	d, err := s.Get(ctx, tenantID, depID)
	if err != nil {
		return err
	}
	if d == nil {
		return ErrNotFound
	}
	return s.Dependencies.DeleteMapping(ctx, depID, mappingID)
}

func (s *Service) ListByProject(ctx context.Context, projectID project.ID) ([]*dependency.Dependency, error) {
	return s.Dependencies.ListByProject(ctx, projectID)
}

func (s *Service) ListDownstream(ctx context.Context, upstreamProjectID project.ID) ([]*dependency.Dependency, error) {
	return s.Dependencies.ListDownstream(ctx, upstreamProjectID)
}

func (s *Service) ResolveForProject(ctx context.Context, projectID project.ID) (map[string]string, []string, error) {
	deps, err := s.Dependencies.ListByProject(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	env := make(map[string]string)
	var messages []string
	for _, dep := range deps {
		ds := dep.Snapshot()
		if len(ds.Mappings) == 0 {
			continue
		}
		upProj, err := s.Projects.Get(ctx, ds.DependsOnProjectID)
		if err != nil {
			return nil, nil, err
		}
		upName := string(ds.DependsOnProjectID)
		if upProj != nil {
			upName = upProj.Snapshot().Name
		}
		var outputs map[string]state.OutputValue
		if s.States != nil {
			st, err := s.States.Get(ctx, string(ds.DependsOnProjectID))
			if err != nil {
				return nil, nil, err
			}
			if st != nil {
				outputs = st.Snapshot().Outputs()
			}
		}
		for _, m := range ds.Mappings {
			outVal, ok := outputs[m.OutputName]
			if !ok {
				messages = append(messages, fmt.Sprintf("Warning: upstream project %q has no output %q (mapped to %s)", upName, m.OutputName, m.EnvKey()))
				continue
			}
			env[m.EnvKey()] = outVal.EnvString
			messages = append(messages, fmt.Sprintf("Injected %s from upstream project %q output %q", m.EnvKey(), upName, m.OutputName))
		}
	}
	return env, messages, nil
}

func (s *Service) ShouldTriggerDownstream(dep dependency.Snapshot, changedOutputs map[string]bool) bool {
	if dep.TriggerAlways {
		return true
	}
	for _, m := range dep.Mappings {
		if changedOutputs[m.OutputName] {
			return true
		}
	}
	return false
}

func (s *Service) TriggerDownstreamRuns(ctx context.Context, upstreamProj *project.Project, finishedRun *run.Run) {
	if s == nil || upstreamProj == nil || finishedRun == nil || s.Runs == nil || s.Tokens == nil || s.Checkout == nil {
		return
	}
	rs := finishedRun.Snapshot()
	if rs.PlanOnly || rs.Status != run.StatusSucceeded {
		return
	}
	ps := upstreamProj.Snapshot()

	var changed map[string]bool
	if s.States != nil {
		if st, err := s.States.Get(ctx, string(ps.ID)); err == nil && st != nil {
			changed = st.Snapshot().ChangedOutputsSincePrevious()
		}
	}

	deps, err := s.Dependencies.ListDownstream(ctx, ps.ID)
	if err != nil {
		log.Printf("pipeline list downstream for %s: %v", ps.ID, err)
		return
	}
	for _, dep := range deps {
		ds := dep.Snapshot()
		if !s.ShouldTriggerDownstream(ds, changed) {
			continue
		}
		dp, err := s.Projects.Get(ctx, ds.ProjectID)
		if err != nil || dp == nil {
			continue
		}
		dps := dp.Snapshot()
		if dps.Locked {
			continue
		}
		token, err := s.Tokens.InstallationToken(ctx, dps.TenantID)
		if err != nil {
			log.Printf("pipeline token for downstream %s: %v", dps.ID, err)
			continue
		}
		sha, err := s.Checkout.BranchTip(ctx, dps.Repo.CloneURL(), token, dps.DefaultBranch)
		if err != nil {
			log.Printf("pipeline branch tip for downstream %s: %v", dps.ID, err)
			continue
		}
		var msg, author string
		if s.Lookup != nil {
			msg, author, _ = s.Lookup.Lookup(ctx, dps.Repo, token, sha)
		}
		if _, err := s.Runs.Open(ctx, runapp.OpenRequest{
			ProjectID:   dps.ID,
			Trigger:     run.TriggerPipeline,
			SHA:         sha,
			Branch:      dps.DefaultBranch,
			Message:     msg,
			Author:      author,
			TriggeredBy: "pipeline:" + ps.Name,
			PlanOnly:    false,
		}); err != nil && !errors.Is(err, runapp.ErrDuplicate) {
			log.Printf("pipeline open run for downstream %s: %v", dps.ID, err)
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/application/dependency/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/application/dependency/service.go internal/application/dependency/service_test.go
git commit -m "feat(dependency): add dependency application service and pipeline trigger"
```

---

### Task 5: Worker Integration & Server Wiring

**Files:**
- Modify: `internal/application/worker/worker.go`
- Modify: `internal/application/worker/worker_test.go`
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Write failing test in `internal/application/worker/worker_test.go`**

Add a test in `internal/application/worker/worker_test.go` verifying that `Worker.Dependencies` resolves `TF_VAR_*` env vars and merges them with `Worker.Contexts`:

```go
type fakeDependencyResolver struct {
	env      map[string]string
	messages []string
	err      error
}

func (f fakeDependencyResolver) ResolveForProject(_ context.Context, _ project.ID) (map[string]string, []string, error) {
	return f.env, f.messages, f.err
}

func TestWorkerResolvesUpstreamDependenciesIntoEnv(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(100, 0).UTC()
	p, _ := project.New("p1", "ten-1", "app", project.SourceRepo{Owner: "o", Name: "r"}, 1, "main", "", true, "st", now)
	projs := &fakeProjects{p: p}
	runs := newFakeRuns()
	q := &fakeQueue{}
	svc := &runapp.Service{Projects: projs, Runs: runs, Schedule: q, Now: func() time.Time { return now }, NewID: func() string { return "r1" }}
	r, err := svc.Open(ctx, runapp.OpenRequest{ProjectID: "p1", Trigger: run.TriggerPipeline, SHA: "abcdef1234567", Branch: "main", TriggeredBy: "pipeline:net"})
	if err != nil {
		t.Fatal(err)
	}
	var capturedEnv map[string]string
	exec := &capturingExecutor{onRun: func(req Request) {
		capturedEnv = req.Env
	}}
	w := &Worker{
		WorkerID: "w1", Queue: q, Projects: projs, Runs: runs, Service: svc,
		Checkout: fakeCheckout{}, Executor: exec, Tokens: fakeTokens{},
		Contexts: fakeContexts{env: map[string]string{"AWS_REGION": "eu-central-1", "TF_VAR_vpc_id": "from-ctx"}},
		Dependencies: fakeDependencyResolver{
			env:      map[string]string{"TF_VAR_vpc_id": "vpc-from-upstream"},
			messages: []string{`Injected TF_VAR_vpc_id from upstream project "network" output "vpc_id"`},
		},
		WorkDir: t.TempDir(), Now: func() time.Time { return now },
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if capturedEnv["AWS_REGION"] != "eu-central-1" || capturedEnv["TF_VAR_vpc_id"] != "vpc-from-upstream" {
		t.Fatalf("expected merged env with upstream override, got %+v", capturedEnv)
	}
	updated, _ := runs.Get(ctx, r.Snapshot().ID)
	if !strings.Contains(updated.Snapshot().Steps[0].Logs, "Injected TF_VAR_vpc_id") {
		t.Fatalf("expected dependency injection log in step logs, got %q", updated.Snapshot().Steps[0].Logs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/application/worker/...`
Expected: FAIL (`unknown field Dependencies in struct literal of type Worker`).

- [ ] **Step 3: Update `internal/application/worker/worker.go` and `cmd/server/main.go`**

In `internal/application/worker/worker.go`:
1. Add `DependencyResolver`:
```go
type DependencyResolver interface {
	ResolveForProject(ctx context.Context, projectID project.ID) (map[string]string, []string, error)
}
```
2. Add `Dependencies DependencyResolver` to `type Worker struct`.
3. In `runJob`, right after `if w.Contexts != nil { ... }`:
```go
	if w.Dependencies != nil {
		depEnv, depMsgs, derr := w.Dependencies.ResolveForProject(ctx, ps.ID)
		if derr != nil {
			if ferr := w.Service.Fail(ctx, job.RunID, job.Step, derr.Error()); ferr != nil {
				return ferr
			}
			return w.completeAndCleanup(ctx, job)
		}
		for _, msg := range depMsgs {
			logger.LogBeforef("%s", msg)
		}
		if len(depEnv) > 0 {
			if ctxEnv == nil {
				ctxEnv = make(map[string]string, len(depEnv))
			}
			for k, v := range depEnv {
				ctxEnv[k] = v
			}
		}
	}
```

In `cmd/server/main.go`:
1. Import `dependencyapp "infra-guy/internal/application/dependency"`.
2. Instantiate `dependencyRepo := postgres.NewDependencyRepo(pool)` and `dependencySvc`:
```go
	dependencyRepo := postgres.NewDependencyRepo(pool)
	dependencySvc := &dependencyapp.Service{
		Dependencies: dependencyRepo,
		Projects:     projects,
		States:       states,
		Tokens:       tokenSource,
		Checkout:     git.Checkout{},
		Lookup:       git.CommitLookup{},
		Now:          now,
		NewID:        newID,
	}
```
3. Set `dependencySvc.Runs = runSvc` after `runSvc` is created, and inside `runSvc.OnTerminal` call `dependencySvc.TriggerDownstreamRuns(ctx, p, r)`:
```go
		OnTerminal: func(ctx context.Context, p *project.Project, r *run.Run) {
			ps := p.Snapshot()
			rs := r.Snapshot()
			if wsCache != nil {
				_ = wsCache.Delete(ctx, worker.WorkspaceCacheKey(ps.TenantID, string(ps.ID), string(rs.ID)))
			}
			_ = os.RemoveAll(filepath.Join(workDir, string(rs.ID)))
			dependencySvc.TriggerDownstreamRuns(ctx, p, r)
		},
```
4. Pass `Dependencies: dependencySvc` to both `w := &worker.Worker{...}` and `httpapi.NewServer(httpapi.ServerConfig{...})`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/application/worker/... ./cmd/server/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/application/worker/worker.go internal/application/worker/worker_test.go cmd/server/main.go
git commit -m "feat(worker): inject upstream dependency outputs and trigger downstream runs"
```

---

### Task 6: HTTP Routes, Dependencies Tab UI & Tests

**Files:**
- Modify: `internal/interfaces/http/server.go`
- Modify: `internal/interfaces/http/ui.go`
- Modify: `internal/interfaces/http/templates/project.html`
- Create: `internal/interfaces/http/templates/tab_dependencies.html`
- Modify: `internal/interfaces/http/ui_test.go`

- [ ] **Step 1: Write failing HTTP tests in `internal/interfaces/http/ui_test.go`**

Add tests in `internal/interfaces/http/ui_test.go` that:
1. Create two projects (`network` and `app`) in the same tenant and store state on `network` with output `"vpc_id": "vpc-777"`.
2. `POST /projects/{appID}/dependencies` with `depends_on_project_id={netID}` and verify redirect to `/projects/{appID}/tabs/dependencies`.
3. `POST /projects/{appID}/dependencies/{depID}/mappings` with `output_name=vpc_id` and `input_name=network_vpc_id`.
4. `GET /projects/{appID}/tabs/dependencies` (both HTMX and full-page) and verify `"network"`, `"vpc_id"`, and `"TF_VAR_network_vpc_id"` appear in the response, and `GET /projects/{netID}/tabs/dependencies` shows `"app"` under Downstream Dependents.
5. Verify a run with `Trigger: run.TriggerPipeline` appears under Tracked runs (`GET /projects/{appID}/tabs/tracked`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/interfaces/http/...`
Expected: FAIL.

- [ ] **Step 3: Implement `ServerConfig.Dependencies`, UI routes/handlers, and templates**

1. In `internal/interfaces/http/server.go`:
   - Import `dependencyapp "infra-guy/internal/application/dependency"`.
   - Add `Dependencies *dependencyapp.Service` to `ServerConfig`.

2. In `internal/interfaces/http/templates/project.html`:
   - Add the Dependencies tab link inside `<nav class="tabs">`:
   ```html
   <nav class="tabs">
     <a href="/projects/{{.ID}}" hx-get="/projects/{{.ID}}/tabs/tracked" hx-target="#tab-panel" hx-swap="outerHTML">Tracked runs</a>
     <a href="/projects/{{.ID}}/tabs/pulls" hx-get="/projects/{{.ID}}/tabs/pulls" hx-target="#tab-panel" hx-swap="outerHTML">Pull requests</a>
     <a href="/projects/{{.ID}}/tabs/state" hx-get="/projects/{{.ID}}/tabs/state" hx-target="#tab-panel" hx-swap="outerHTML">State history</a>
     <a href="/projects/{{.ID}}/tabs/dependencies" hx-get="/projects/{{.ID}}/tabs/dependencies" hx-target="#tab-panel" hx-swap="outerHTML">Dependencies</a>
   </nav>
   ```

3. Create `internal/interfaces/http/templates/tab_dependencies.html`:
   ```html
   {{define "tab_dependencies"}}
   <div id="tab-panel">
     <section class="group">
       <h3>Upstream Dependencies</h3>
       <p class="sub">Outputs from upstream projects are injected as <code>TF_VAR_*</code> environment variables during runs and can automatically trigger this project when upstream applies succeed.</p>
       {{if .CandidateProjects}}
       <form method="post" action="/projects/{{.ProjectID}}/dependencies" class="actions" style="margin: 12px 0;">
         <select name="depends_on_project_id" required>
           <option value="">Select upstream project…</option>
           {{range .CandidateProjects}}
           <option value="{{.ID}}">{{.Name}}</option>
           {{end}}
         </select>
         <label class="inline"><input type="checkbox" name="trigger_always"> Trigger on every apply</label>
         <button type="submit">Add dependency</button>
       </form>
       {{end}}

       {{if .Upstream}}
         {{range .Upstream}}
         <div class="card" style="margin-top: 12px; padding: 12px; border: 1px solid var(--border, #ddd); border-radius: 6px;">
           <div class="actions" style="justify-content: space-between;">
             <div>
               <strong><a href="/projects/{{.UpstreamProjectID}}">{{.UpstreamProjectName}}</a></strong>
               {{if .AvailableOutputs}}
               <span class="sub"> · Available outputs: {{range $i, $o := .AvailableOutputs}}{{if $i}}, {{end}}<code>{{$o}}</code>{{end}}</span>
               {{else}}
               <span class="sub"> · No outputs in current state</span>
               {{end}}
             </div>
             <div class="actions">
               <form method="post" action="/projects/{{$.ProjectID}}/dependencies/{{.ID}}/trigger-mode" class="inline">
                 <input type="hidden" name="trigger_always" value="{{if .TriggerAlways}}off{{else}}on{{end}}">
                 <button type="submit">{{if .TriggerAlways}}Trigger: Every apply{{else}}Trigger: When mapped outputs change{{end}}</button>
               </form>
               <form method="post" action="/projects/{{$.ProjectID}}/dependencies/{{.ID}}/delete" class="inline">
                 <button type="submit" class="btn-danger">Remove</button>
               </form>
             </div>
           </div>

           {{if .Mappings}}
           <table style="margin-top: 8px;">
             <thead>
               <tr><th>Upstream Output</th><th>Injected Variable</th><th>Current Value</th><th></th></tr>
             </thead>
             <tbody>
               {{range .Mappings}}
               <tr>
                 <td><code>{{.OutputName}}</code></td>
                 <td><code>{{.EnvKey}}</code></td>
                 <td><code>{{.PreviewValue}}</code></td>
                 <td>
                   <form method="post" action="/projects/{{$.ProjectID}}/dependencies/{{.DepID}}/mappings/{{.ID}}/delete" class="inline">
                     <button type="submit" class="btn-danger">Delete</button>
                   </form>
                 </td>
               </tr>
               {{end}}
             </tbody>
           </table>
           {{else}}
           <p class="sub" style="margin-top: 8px;">No output mappings configured yet.</p>
           {{end}}

           <form method="post" action="/projects/{{$.ProjectID}}/dependencies/{{.ID}}/mappings" class="actions" style="margin-top: 8px;">
             <input type="text" name="output_name" placeholder="Upstream output (e.g. vpc_id)" list="outputs-{{.ID}}" required>
             {{if .AvailableOutputs}}
             <datalist id="outputs-{{.ID}}">
               {{range .AvailableOutputs}}<option value="{{.}}">{{end}}
             </datalist>
             {{end}}
             <input type="text" name="input_name" placeholder="Input variable name (optional, defaults to output)">
             <button type="submit">Map output</button>
           </form>
         </div>
         {{end}}
       {{else}}
         <p class="empty">No upstream dependencies.</p>
       {{end}}
     </section>

     <section class="group" style="margin-top: 24px;">
       <h3>Downstream Dependents</h3>
       {{if .Downstream}}
       <table>
         <thead>
           <tr><th>Project</th><th>Trigger Mode</th><th>Mapped Outputs</th></tr>
         </thead>
         <tbody>
           {{range .Downstream}}
           <tr>
             <td><a href="/projects/{{.ProjectID}}">{{.ProjectName}}</a></td>
             <td>{{if .TriggerAlways}}Every succeeded apply{{else}}When mapped outputs change{{end}}</td>
             <td>{{if .MappedOutputs}}{{range $i, $m := .MappedOutputs}}{{if $i}}, {{end}}<code>{{$m}}</code>{{end}}{{else}}<em>none</em>{{end}}</td>
           </tr>
           {{end}}
         </tbody>
       </table>
       {{else}}
       <p class="empty">No downstream projects depend on this project.</p>
       {{end}}
     </section>
   </div>
   {{end}}
   ```

4. In `internal/interfaces/http/ui.go`:
   - Update `isTracked`:
     ```go
     func isTracked(s run.Snapshot) bool {
     	return s.Trigger == run.TriggerPush || s.Trigger == run.TriggerManual || s.Trigger == run.TriggerPipeline
     }
     ```
   - Register routes in `u.routes(mux)`:
     ```go
     mux.HandleFunc("GET /projects/{id}/tabs/dependencies", u.tabDependencies)
     mux.HandleFunc("POST /projects/{id}/dependencies", u.createDependency)
     mux.HandleFunc("POST /projects/{id}/dependencies/{depID}/trigger-mode", u.updateDependencyTriggerMode)
     mux.HandleFunc("POST /projects/{id}/dependencies/{depID}/delete", u.deleteDependency)
     mux.HandleFunc("POST /projects/{id}/dependencies/{depID}/mappings", u.addDependencyMapping)
     mux.HandleFunc("POST /projects/{id}/dependencies/{depID}/mappings/{mapID}/delete", u.deleteDependencyMapping)
     ```
   - Implement `tabDependencies` (returning fragment when `HX-Request == "true"`, or full `"project"` page otherwise) and the 5 POST handlers redirecting back to `/projects/{id}/tabs/dependencies`.

- [ ] **Step 4: Run full test suite**

Run: `go test ./...`
Expected: PASS across all packages.

- [ ] **Step 5: Commit**

```bash
git add internal/interfaces/http/server.go internal/interfaces/http/ui.go internal/interfaces/http/templates/project.html internal/interfaces/http/templates/tab_dependencies.html internal/interfaces/http/ui_test.go
git commit -m "feat(ui): add project dependencies tab and output mapping controls"
```
