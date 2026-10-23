# DB & Business Logic Layer Separation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extract all business rules, cryptography, expiration checks, deduplication, and retry/reap policies out of `internal/infrastructure/postgres` into `internal/domain/*` and `internal/application/*`, enforce a strict `HTTP / Worker → Application Services → Repositories / DB` boundary, and remove redundant adapter types in `cmd/server/main.go`.

**Architecture:**
1. **Pure Persistence (`internal/infrastructure/postgres`):** Repositories only translate between domain types/snapshots and SQL rows (`pgx.ErrNoRows` → `nil, nil`, atomic SQL transactions, row scanning).
2. **Domain Invariants & Types (`internal/domain/*`):** Domain entities enforce business invariants (`Project.ClaimActiveRun` rejects locked projects with `ErrLocked`; `Project.RenameRepo` updates repo owner/name) and define shared domain value objects (`user.Session`, `run.Job`, `run.ReapDecision`, `tfcontext.Entry.Ciphertext`).
3. **Application Orchestration (`internal/application/*`):** Services own secret encryption/decryption (`tfcontext.Service`), context attachment deduplication (`tfcontext.Service`), OAuth state expiry & GitHub identity construction (`auth.Service`), stale job reap policy & claim backoff (`worker.Worker`), and state/project/run operations (`state.Service`, `project.Service`, `run.Service`).
4. **Strict Interface Boundary (`internal/interfaces/http`, `internal/application/worker`, `cmd/server/main.go`):** HTTP handlers, `worker.Worker`, and `dependency.Service` depend on application services rather than raw domain repositories; `postgres.WorkerQueue` and `identityAdapter` are deleted.

**Tech Stack:** Go 1.23+, PostgreSQL (`jackc/pgx/v5`), standard `testing` + `testcontainers-go` integration suite.

---

## File Structure & Responsibilities

| File | Action | Responsibility Change |
|---|---|---|
| `internal/domain/tfcontext/context.go` | Modify | Add `Ciphertext []byte` to `Entry`; remove `decryptSecrets bool` from `Repository.Get` and `Repository.ListByProject`. |
| `internal/application/tfcontext/service.go` | Modify | Add `SecretSealer` interface and `Sealer` field; seal secrets in `UpsertEntry`, open secrets in `ResolveForProject`, deduplicate/filter `contextIDs` in `SetProjectContexts`. |
| `internal/application/tfcontext/service_test.go` | Modify | Test secret sealing/opening and context attachment deduplication in `tfcontext.Service`. |
| `internal/infrastructure/postgres/context_repo.go` | Modify | Remove `crypto.Sealer`; persist/load `Value` and `Ciphertext` verbatim; insert pre-deduplicated `contextIDs`. |
| `internal/domain/user/user.go` | Modify | Add `Session` struct (`ID, UserID, TenantID, Role, Login, AccessToken, ExpiresAt`). |
| `internal/application/auth/service.go` | Modify | Alias `Session = user.Session`; update `Store` interface (`UpsertUser` takes `user.Identity`, `ConsumeOAuthState` returns `expiresAt`); validate OAuth expiry and construct `user.GitHubIdentity` in `Complete`. |
| `internal/application/auth/service_test.go` | Modify | Update mock store and add unit test for expired OAuth state rejection in `auth.Service`. |
| `internal/infrastructure/postgres/identity_repo.go` | Modify | Alias `Session = user.Session`; `UpsertUser` persists caller-supplied `user.Identity`; `ConsumeOAuthState` atomically deletes and returns `expires_at`. |
| `internal/domain/project/project.go` | Modify | Add `ErrLocked`; enforce `!p.s.Locked` in `ClaimActiveRun`; add `RenameRepo(owner, name string, now time.Time) error`. |
| `internal/domain/project/project_test.go` | Modify | Test `ClaimActiveRun` on locked project and `RenameRepo`. |
| `internal/domain/run/run.go` | Modify | Add `Job` and `ReapDecision` value types. |
| `internal/infrastructure/postgres/worker_queue.go` | Delete | Remove adapter made redundant by `run.Job`. |
| `internal/infrastructure/postgres/queue.go` | Modify | Alias `Job = run.Job`; delegate lock check to `p.ClaimActiveRun` and accept `retryDelay time.Duration` in `Claim`; accept `decide func(run.Job) run.ReapDecision` in `Reap`. |
| `internal/infrastructure/postgres/queue_test.go` | Modify | Update `Claim` and `Reap` calls to pass `retryDelay` and `worker.ReapPolicy`. |
| `internal/application/worker/worker.go` | Modify | Alias `Job = run.Job`; add `ClaimRetryDelay`, `MaxStepAttempts`, `ReapPolicy`; replace `project.Repository` with `ProjectGetter` and remove `run.Repository` in favor of `run.Service`. |
| `internal/application/worker/worker_test.go` | Modify | Test `ReapPolicy` and update fake queue/service wiring. |
| `internal/infrastructure/postgres/project_repo.go` | Modify | Remove `firstNonEmptyVersion` fallback from `Save`. |
| `internal/infrastructure/postgres/state_repo.go` | Modify | Return `nil, nil` on `pgx.ErrNoRows` in `Load`. |
| `internal/infrastructure/postgres/repos_test.go` | Modify | Update repo integration tests for `ContextRepo`, `IdentityStore`, `ProjectRepo`, and `StateRepo`. |
| `internal/application/state/service.go` | Modify | Add `Outputs`, `ChangedOutputsSincePrevious`, and `ForceUnlock` methods. |
| `internal/application/state/service_test.go` | Modify | Add unit tests for `Outputs`, `ChangedOutputsSincePrevious`, and `ForceUnlock`. |
| `internal/application/project/service.go` | Modify | Add `StateForceUnlocker` interface (`States` field) for `ForceUnlock`; add `SyncAndFindByRepo`. |
| `internal/application/project/service_test.go` | Modify | Test `SyncAndFindByRepo` and `ForceUnlock` delegating to `StateForceUnlocker`. |
| `internal/application/run/service.go` | Modify | Add `BeginStep(ctx, id, step)` method. |
| `internal/application/run/service_test.go` | Modify | Add unit test for `BeginStep`. |
| `internal/application/dependency/service.go` | Modify | Replace `project.Repository` and `state.Repository` with `ProjectGetter` and `StateReader` interfaces satisfied by `project.Service` and `state.Service`. |
| `internal/application/dependency/service_test.go` | Modify | Update unit tests to use `ProjectGetter` and `StateReader`. |
| `internal/interfaces/http/server.go` | Modify | Remove `ProjectRepo` and `StateRepo` from `ServerConfig`; wire `cfg.Projects` into `httpapi.New`. |
| `internal/interfaces/http/state.go` | Modify | Replace `projects project.Repository` with `projects ProjectMatcher` (`SyncAndFindByRepo`). |
| `internal/interfaces/http/webhook.go` | Modify | Replace `findMatchingProjects` with `a.projects.SyncAndFindByRepo`. |
| `internal/interfaces/http/ui.go` | Modify | Use `u.cfg.Projects.ForceUnlock(ctx, id)` and `u.cfg.States.Outputs(ctx, projectID)`. |
| `internal/interfaces/http/state_test.go` | Modify | Update test helper to pass `ProjectMatcher`. |
| `internal/interfaces/http/webhook_test.go` | Modify | Update webhook tests to pass `*projectapp.Service` to `httpapi.New`. |
| `internal/interfaces/http/ui_test.go` | Modify | Update `ServerConfig` wiring and mock stores in UI tests. |
| `cmd/server/main.go` | Modify | Remove `identityAdapter` and `postgres.NewWorkerQueue`; construct `stateSvc` and `projectSvc` once and wire services cleanly. |

---

### Task 1: Context Encryption & Deduplication Separation

**Files:**
- Modify: `internal/domain/tfcontext/context.go`
- Modify: `internal/application/tfcontext/service.go`
- Modify: `internal/application/tfcontext/service_test.go`
- Modify: `internal/infrastructure/postgres/context_repo.go`
- Modify: `internal/infrastructure/postgres/repos_test.go`
- Modify: `internal/interfaces/http/ui_test.go`
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Update `Entry` and `Repository` in `internal/domain/tfcontext/context.go`**

In `internal/domain/tfcontext/context.go`, add `Ciphertext []byte` to `Entry` and remove `decryptSecrets bool` from `Repository.Get` and `Repository.ListByProject`:

```go
type Entry struct {
	Key        string
	Value      string
	Ciphertext []byte
	Kind       EntryKind
	Secret     bool
	UpdatedAt  time.Time
}
```

```go
type Repository interface {
	Save(ctx context.Context, c *Context) error
	Get(ctx context.Context, id ID) (*Context, error)
	ListByTenant(ctx context.Context, tenantID string) ([]*Context, error)
	Delete(ctx context.Context, id ID) error

	UpsertEntry(ctx context.Context, contextID ID, e Entry) error
	DeleteEntry(ctx context.Context, contextID ID, key string) error

	SetProjectContexts(ctx context.Context, projectID project.ID, contextIDs []ID) error
	ListByProject(ctx context.Context, projectID project.ID) ([]*Context, error)
}
```

- [ ] **Step 2: Write failing tests in `internal/application/tfcontext/service_test.go` for secret sealing/opening and project context deduplication**

Update `internal/application/tfcontext/service_test.go` so `memRepo` matches the new `Repository` signature, add a fake `SecretSealer`, and add tests verifying `UpsertEntry` seals secrets, `ResolveForProject` decrypts them without mutating stored ciphertext, and `SetProjectContexts` deduplicates and filters empty IDs:

```go
package tfcontext_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	tfcontextapp "github.com/klimczak/infra-guy/internal/application/tfcontext"
	"github.com/klimczak/infra-guy/internal/domain/project"
	"github.com/klimczak/infra-guy/internal/domain/tfcontext"
)

type fakeSealer struct{}

func (fakeSealer) Seal(plaintext []byte) ([]byte, error) {
	return append([]byte("enc:"), plaintext...), nil
}

func (fakeSealer) Open(ciphertext []byte) ([]byte, error) {
	if !bytes.HasPrefix(ciphertext, []byte("enc:")) {
		return nil, errors.New("invalid ciphertext")
	}
	return bytes.TrimPrefix(ciphertext, []byte("enc:")), nil
}

type memRepo struct {
	byID      map[tfcontext.ID]*tfcontext.Context
	byProject map[project.ID][]tfcontext.ID
}

func newMemRepo() *memRepo {
	return &memRepo{
		byID:      map[tfcontext.ID]*tfcontext.Context{},
		byProject: map[project.ID][]tfcontext.ID{},
	}
}

func (m *memRepo) Save(_ context.Context, c *tfcontext.Context) error {
	cp := *c
	m.byID[c.ID()] = &cp
	return nil
}

func (m *memRepo) Get(_ context.Context, id tfcontext.ID) (*tfcontext.Context, error) {
	c, ok := m.byID[id]
	if !ok {
		return nil, nil
	}
	cp := *c
	cp.SetEntries(append([]tfcontext.Entry(nil), c.Entries()...))
	return &cp, nil
}

func (m *memRepo) ListByTenant(_ context.Context, tenantID string) ([]*tfcontext.Context, error) {
	var out []*tfcontext.Context
	for _, c := range m.byID {
		if c.TenantID() == tenantID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memRepo) Delete(_ context.Context, id tfcontext.ID) error {
	delete(m.byID, id)
	return nil
}

func (m *memRepo) UpsertEntry(_ context.Context, id tfcontext.ID, e tfcontext.Entry) error {
	c, ok := m.byID[id]
	if !ok {
		return tfcontextapp.ErrNotFound
	}
	entries := c.Entries()
	replaced := false
	for i := range entries {
		if entries[i].Key == e.Key {
			entries[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, e)
	}
	c.SetEntries(entries)
	return nil
}

func (m *memRepo) DeleteEntry(_ context.Context, id tfcontext.ID, key string) error {
	c, ok := m.byID[id]
	if !ok {
		return nil
	}
	var kept []tfcontext.Entry
	for _, e := range c.Entries() {
		if e.Key != key {
			kept = append(kept, e)
		}
	}
	c.SetEntries(kept)
	return nil
}

func (m *memRepo) SetProjectContexts(_ context.Context, pid project.ID, ids []tfcontext.ID) error {
	m.byProject[pid] = append([]tfcontext.ID(nil), ids...)
	return nil
}

func (m *memRepo) ListByProject(_ context.Context, pid project.ID) ([]*tfcontext.Context, error) {
	var out []*tfcontext.Context
	for _, id := range m.byProject[pid] {
		if c, ok := m.byID[id]; ok {
			cp := *c
			cp.SetEntries(append([]tfcontext.Entry(nil), c.Entries()...))
			out = append(out, &cp)
		}
	}
	return out, nil
}
```

And add the new test case at the bottom of `internal/application/tfcontext/service_test.go`:

```go
func TestServiceSealsSecretsAndDeduplicatesProjectContexts(t *testing.T) {
	svc, repo := newSvc()
	svc.Sealer = fakeSealer{}
	ctx := context.Background()

	c1, err := svc.Create(ctx, "t-1", "aws", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpsertEntry(ctx, "t-1", c1.ID(), "AWS_SECRET", "super-secret", tfcontext.KindSecretEnv); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpsertEntry(ctx, "t-1", c1.ID(), "AWS_REGION", "eu-west-1", tfcontext.KindEnv); err != nil {
		t.Fatal(err)
	}

	stored, err := svc.Get(ctx, "t-1", c1.ID())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range stored.Entries() {
		if e.Key == "AWS_SECRET" {
			if e.Value != "" {
				t.Fatalf("expected empty plaintext Value on stored secret, got %q", e.Value)
			}
			if string(e.Ciphertext) != "enc:super-secret" {
				t.Fatalf("expected sealed Ciphertext, got %q", string(e.Ciphertext))
			}
		}
	}

	if err := svc.SetProjectContexts(ctx, "t-1", "p-1", []tfcontext.ID{"", c1.ID(), c1.ID(), ""}); err != nil {
		t.Fatal(err)
	}
	if got := repo.byProject["p-1"]; len(got) != 1 || got[0] != c1.ID() {
		t.Fatalf("expected deduplicated [c1], got %v", got)
	}

	rc, err := svc.ResolveForProject(ctx, "p-1")
	if err != nil {
		t.Fatal(err)
	}
	if rc.Env["AWS_SECRET"] != "super-secret" || rc.Env["AWS_REGION"] != "eu-west-1" {
		t.Fatalf("unexpected resolved env: %+v", rc.Env)
	}
}
```

- [ ] **Step 3: Update `internal/application/tfcontext/service.go` to seal/open secrets and deduplicate `contextIDs`**

Replace `internal/application/tfcontext/service.go` with:

```go
package tfcontext

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/klimczak/infra-guy/internal/domain/project"
	"github.com/klimczak/infra-guy/internal/domain/tfcontext"
)

var (
	ErrNotFound  = errors.New("tfcontext: not found")
	ErrForbidden = errors.New("tfcontext: forbidden")
)

type SecretSealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(ciphertext []byte) ([]byte, error)
}

type Service struct {
	Contexts tfcontext.Repository
	Sealer   SecretSealer
	Now      func() time.Time
	NewID    func() tfcontext.ID
}

func (s *Service) Create(ctx context.Context, tenantID, name, description string) (*tfcontext.Context, error) {
	now := s.Now()
	c, err := tfcontext.New(s.NewID(), tenantID, name, description, now)
	if err != nil {
		return nil, err
	}
	if err := s.Contexts.Save(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Service) Get(ctx context.Context, tenantID string, id tfcontext.ID) (*tfcontext.Context, error) {
	c, err := s.Contexts.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound
	}
	if c.TenantID() != tenantID {
		return nil, ErrForbidden
	}
	return c, nil
}

func (s *Service) ListByTenant(ctx context.Context, tenantID string) ([]*tfcontext.Context, error) {
	return s.Contexts.ListByTenant(ctx, tenantID)
}

func (s *Service) Delete(ctx context.Context, tenantID string, id tfcontext.ID) error {
	if _, err := s.Get(ctx, tenantID, id); err != nil {
		return err
	}
	return s.Contexts.Delete(ctx, id)
}

func (s *Service) UpsertEntry(ctx context.Context, tenantID string, id tfcontext.ID, key, value string, kind tfcontext.EntryKind) error {
	if _, err := s.Get(ctx, tenantID, id); err != nil {
		return err
	}
	entry, err := tfcontext.NewEntry(key, value, kind, s.Now())
	if err != nil {
		return err
	}
	if entry.Secret && s.Sealer != nil {
		ct, err := s.Sealer.Seal([]byte(entry.Value))
		if err != nil {
			return fmt.Errorf("seal context entry %q: %w", entry.Key, err)
		}
		entry.Ciphertext = ct
		entry.Value = ""
	}
	return s.Contexts.UpsertEntry(ctx, id, entry)
}

func (s *Service) DeleteEntry(ctx context.Context, tenantID string, id tfcontext.ID, key string) error {
	if _, err := s.Get(ctx, tenantID, id); err != nil {
		return err
	}
	return s.Contexts.DeleteEntry(ctx, id, key)
}

func (s *Service) SetProjectContexts(ctx context.Context, tenantID string, projectID project.ID, contextIDs []tfcontext.ID) error {
	seen := make(map[tfcontext.ID]bool, len(contextIDs))
	deduped := make([]tfcontext.ID, 0, len(contextIDs))
	for _, cid := range contextIDs {
		if cid == "" || seen[cid] {
			continue
		}
		seen[cid] = true
		if _, err := s.Get(ctx, tenantID, cid); err != nil {
			return err
		}
		deduped = append(deduped, cid)
	}
	return s.Contexts.SetProjectContexts(ctx, projectID, deduped)
}

func (s *Service) ListByProject(ctx context.Context, projectID project.ID) ([]*tfcontext.Context, error) {
	return s.Contexts.ListByProject(ctx, projectID)
}

func (s *Service) ResolveForProject(ctx context.Context, projectID project.ID) (tfcontext.ResolvedContext, error) {
	contexts, err := s.Contexts.ListByProject(ctx, projectID)
	if err != nil {
		return tfcontext.ResolvedContext{}, err
	}
	for _, c := range contexts {
		entries := append([]tfcontext.Entry(nil), c.Entries()...)
		for i := range entries {
			if entries[i].Secret && len(entries[i].Ciphertext) > 0 && s.Sealer != nil {
				pt, err := s.Sealer.Open(entries[i].Ciphertext)
				if err != nil {
					return tfcontext.ResolvedContext{}, fmt.Errorf("decrypt context entry %q: %w", entries[i].Key, err)
				}
				entries[i].Value = string(pt)
			}
		}
		c.SetEntries(entries)
	}
	return tfcontext.Resolve(contexts), nil
}
```

- [ ] **Step 4: Strip `crypto.Sealer` and deduplication from `internal/infrastructure/postgres/context_repo.go`**

Replace `internal/infrastructure/postgres/context_repo.go` with:

```go
package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klimczak/infra-guy/internal/domain/project"
	"github.com/klimczak/infra-guy/internal/domain/tfcontext"
)

type ContextRepo struct {
	pool *pgxpool.Pool
}

func NewContextRepo(pool *pgxpool.Pool) *ContextRepo {
	return &ContextRepo{pool: pool}
}

func (r *ContextRepo) Save(ctx context.Context, c *tfcontext.Context) error {
	s := c.Snapshot()
	_, err := r.pool.Exec(ctx, `
		INSERT INTO contexts (id, tenant_id, name, description, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			description = EXCLUDED.description,
			updated_at = EXCLUDED.updated_at
	`, string(s.ID), s.TenantID, s.Name, s.Description, utcTime(s.CreatedAt), utcTime(s.UpdatedAt))
	return err
}

func (r *ContextRepo) Get(ctx context.Context, id tfcontext.ID) (*tfcontext.Context, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, description, created_at, updated_at
		FROM contexts WHERE id = $1
	`, string(id))
	c, err := scanContext(row)
	if err != nil || c == nil {
		return c, err
	}
	entries, err := r.loadEntries(ctx, id)
	if err != nil {
		return nil, err
	}
	c.SetEntries(entries)
	return c, nil
}

func (r *ContextRepo) ListByTenant(ctx context.Context, tenantID string) ([]*tfcontext.Context, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, name, description, created_at, updated_at
		FROM contexts WHERE tenant_id = $1 ORDER BY name ASC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tfcontext.Context
	for rows.Next() {
		c, err := scanContext(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range out {
		entries, err := r.loadEntries(ctx, c.ID())
		if err != nil {
			return nil, err
		}
		c.SetEntries(entries)
	}
	return out, nil
}

func (r *ContextRepo) Delete(ctx context.Context, id tfcontext.ID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM contexts WHERE id = $1`, string(id))
	return err
}

func (r *ContextRepo) UpsertEntry(ctx context.Context, contextID tfcontext.ID, e tfcontext.Entry) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO context_entries (context_id, key, kind, secret, value_text, value_ciphertext, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (context_id, key) DO UPDATE SET
			kind = EXCLUDED.kind,
			secret = EXCLUDED.secret,
			value_text = EXCLUDED.value_text,
			value_ciphertext = EXCLUDED.value_ciphertext,
			updated_at = EXCLUDED.updated_at
	`, string(contextID), e.Key, string(e.Kind), e.Secret, e.Value, e.Ciphertext, utcTime(e.UpdatedAt))
	return err
}

func (r *ContextRepo) DeleteEntry(ctx context.Context, contextID tfcontext.ID, key string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM context_entries WHERE context_id = $1 AND key = $2`, string(contextID), key)
	return err
}

func (r *ContextRepo) SetProjectContexts(ctx context.Context, projectID project.ID, contextIDs []tfcontext.ID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM project_contexts WHERE project_id = $1`, string(projectID)); err != nil {
		return err
	}
	for i, cid := range contextIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO project_contexts (project_id, context_id, position)
			VALUES ($1, $2, $3)
		`, string(projectID), string(cid), i); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *ContextRepo) ListByProject(ctx context.Context, projectID project.ID) ([]*tfcontext.Context, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT c.id, c.tenant_id, c.name, c.description, c.created_at, c.updated_at
		FROM project_contexts pc
		JOIN contexts c ON c.id = pc.context_id
		WHERE pc.project_id = $1
		ORDER BY pc.position ASC
	`, string(projectID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tfcontext.Context
	for rows.Next() {
		c, err := scanContext(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range out {
		entries, err := r.loadEntries(ctx, c.ID())
		if err != nil {
			return nil, err
		}
		c.SetEntries(entries)
	}
	return out, nil
}

func (r *ContextRepo) loadEntries(ctx context.Context, contextID tfcontext.ID) ([]tfcontext.Entry, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT key, kind, secret, value_text, value_ciphertext, updated_at
		FROM context_entries
		WHERE context_id = $1
		ORDER BY key ASC
	`, string(contextID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tfcontext.Entry
	for rows.Next() {
		var (
			e    tfcontext.Entry
			kind string
		)
		if err := rows.Scan(&e.Key, &kind, &e.Secret, &e.Value, &e.Ciphertext, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.Kind = tfcontext.EntryKind(kind)
		e.UpdatedAt = utcTime(e.UpdatedAt)
		e.Secret = e.Kind == tfcontext.KindSecretEnv || e.Kind == tfcontext.KindSecretFile
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanContext(row pgx.Row) (*tfcontext.Context, error) {
	var (
		s  tfcontext.Snapshot
		id string
	)
	err := row.Scan(&id, &s.TenantID, &s.Name, &s.Description, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.ID = tfcontext.ID(id)
	s.CreatedAt = utcTime(s.CreatedAt)
	s.UpdatedAt = utcTime(s.UpdatedAt)
	return tfcontext.Rehydrate(s), nil
}
```

- [ ] **Step 5: Update `cmd/server/main.go`, `internal/infrastructure/postgres/repos_test.go`, and `internal/interfaces/http/ui_test.go`**

In `cmd/server/main.go`, update `contextRepo` and `contextSvc`:

```go
	contextRepo := postgres.NewContextRepo(pool)
	contextSvc := &tfcontextapp.Service{
		Contexts: contextRepo,
		Sealer:   sealer,
		Now:      now,
		NewID:    func() tfcontext.ID { return tfcontext.ID(newID()) },
	}
```

In `internal/infrastructure/postgres/repos_test.go`, update `TestContextRepoRoundTripAndEncryption` (around lines 576–687) to test `NewContextRepo(pool)` persisting `Value` and `Ciphertext` directly:

```go
func TestContextRepoRoundTripAndEncryption(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	tenantID := seedTenant(t, pool)
	projRepo := postgres.NewProjectRepo(pool)
	p := newSampleProject(t, tenantID)
	if err := projRepo.Save(ctx, p); err != nil {
		t.Fatalf("save project: %v", err)
	}

	sealer, err := crypto.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	repo := postgres.NewContextRepo(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	c1, err := tfcontext.New("ctx-1", tenantID, "aws-prod", "prod creds", now)
	if err != nil {
		t.Fatalf("new context: %v", err)
	}
	c2, err := tfcontext.New("ctx-2", tenantID, "override", "override region", now)
	if err != nil {
		t.Fatalf("new context 2: %v", err)
	}
	if err := repo.Save(ctx, c1); err != nil {
		t.Fatalf("save c1: %v", err)
	}
	if err := repo.Save(ctx, c2); err != nil {
		t.Fatalf("save c2: %v", err)
	}

	e1, _ := tfcontext.NewEntry("AWS_REGION", "eu-central-1", tfcontext.KindEnv, now)
	e2, _ := tfcontext.NewEntry("AWS_SECRET_ACCESS_KEY", "", tfcontext.KindSecretEnv, now)
	e2.Ciphertext, err = sealer.Seal([]byte("super-secret"))
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	e3, _ := tfcontext.NewEntry(".credentials.json", "", tfcontext.KindSecretFile, now)
	e3.Ciphertext, err = sealer.Seal([]byte(`{"token":"xyz"}`))
	if err != nil {
		t.Fatalf("seal secret file: %v", err)
	}
	e4, _ := tfcontext.NewEntry("AWS_REGION", "us-east-1", tfcontext.KindEnv, now)

	for _, op := range []struct {
		cid tfcontext.ID
		e   tfcontext.Entry
	}{
		{c1.ID(), e1},
		{c1.ID(), e2},
		{c1.ID(), e3},
		{c2.ID(), e4},
	} {
		if err := repo.UpsertEntry(ctx, op.cid, op.e); err != nil {
			t.Fatalf("upsert %s: %v", op.e.Key, err)
		}
	}

	gotMeta, err := repo.Get(ctx, c1.ID())
	if err != nil || gotMeta == nil {
		t.Fatalf("get meta: %v %v", gotMeta, err)
	}
	for _, entry := range gotMeta.Entries() {
		if entry.Secret && entry.Value != "" {
			t.Fatalf("secret %s leaked plaintext: %q", entry.Key, entry.Value)
		}
		if entry.Secret && len(entry.Ciphertext) == 0 {
			t.Fatalf("secret %s missing ciphertext", entry.Key)
		}
		if !entry.Secret && entry.Value != "eu-central-1" {
			t.Fatalf("non-secret value mismatch: %q", entry.Value)
		}
	}

	if err := repo.SetProjectContexts(ctx, p.ID(), []tfcontext.ID{c1.ID(), c2.ID()}); err != nil {
		t.Fatalf("set project contexts: %v", err)
	}
	attached, err := repo.ListByProject(ctx, p.ID())
	if err != nil {
		t.Fatalf("list by project: %v", err)
	}
	if len(attached) != 2 || attached[0].ID() != c1.ID() || attached[1].ID() != c2.ID() {
		t.Fatalf("unexpected attached order: %+v", attached)
	}

	for _, c := range attached {
		entries := c.Entries()
		for i := range entries {
			if entries[i].Secret && len(entries[i].Ciphertext) > 0 {
				pt, err := sealer.Open(entries[i].Ciphertext)
				if err != nil {
					t.Fatalf("open %s: %v", entries[i].Key, err)
				}
				entries[i].Value = string(pt)
			}
		}
		c.SetEntries(entries)
	}
	rc := tfcontext.Resolve(attached)
	if rc.Env["AWS_REGION"] != "us-east-1" {
		t.Fatalf("expected override AWS_REGION=us-east-1, got %q", rc.Env["AWS_REGION"])
	}
	if rc.Env["AWS_SECRET_ACCESS_KEY"] != "super-secret" {
		t.Fatalf("expected decrypted secret, got %q", rc.Env["AWS_SECRET_ACCESS_KEY"])
	}
	if string(rc.Files[".credentials.json"]) != `{"token":"xyz"}` {
		t.Fatalf("expected decrypted secret file, got %q", string(rc.Files[".credentials.json"]))
	}

	if err := repo.DeleteEntry(ctx, c1.ID(), "AWS_REGION"); err != nil {
		t.Fatalf("delete entry: %v", err)
	}
	afterDel, _ := repo.Get(ctx, c1.ID())
	if len(afterDel.Entries()) != 2 {
		t.Fatalf("expected 2 entries after delete, got %d", len(afterDel.Entries()))
	}

	list, err := repo.ListByTenant(ctx, tenantID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list by tenant: %v len=%d", err, len(list))
	}

	if err := repo.Delete(ctx, c2.ID()); err != nil {
		t.Fatalf("delete c2: %v", err)
	}
	attachedAfter, _ := repo.ListByProject(ctx, p.ID())
	if len(attachedAfter) != 1 || attachedAfter[0].ID() != c1.ID() {
		t.Fatalf("expected only c1 after deleting c2, got %+v", attachedAfter)
	}
}
```

In `internal/interfaces/http/ui_test.go`, update `memContextRepo.Get` and `memContextRepo.ListByProject` (around lines 956 and 1018) to drop `_ bool`:

```go
func (m *memContextRepo) Get(_ context.Context, id tfcontext.ID) (*tfcontext.Context, error) {
	c, ok := m.byID[id]
	if !ok {
		return nil, nil
	}
	return c, nil
}
```

```go
func (m *memContextRepo) ListByProject(_ context.Context, pid project.ID) ([]*tfcontext.Context, error) {
	var out []*tfcontext.Context
	for _, id := range m.byProject[pid] {
		if c, ok := m.byID[id]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}
```

- [ ] **Step 6: Run tests and commit Task 1**

Run:
```bash
go test ./internal/domain/tfcontext/... ./internal/application/tfcontext/... ./internal/interfaces/http/... ./cmd/...
```
Expected: PASS.

Commit:
```bash
git add internal/domain/tfcontext/context.go internal/application/tfcontext/service.go internal/application/tfcontext/service_test.go internal/infrastructure/postgres/context_repo.go internal/infrastructure/postgres/repos_test.go internal/interfaces/http/ui_test.go cmd/server/main.go
git commit -m "refactor(tfcontext): move secret sealing and context deduplication to application service"
```

---

### Task 2: Identity Store & Sessions Separation

**Files:**
- Modify: `internal/domain/user/user.go`
- Modify: `internal/application/auth/service.go`
- Modify: `internal/application/auth/service_test.go`
- Modify: `internal/infrastructure/postgres/identity_repo.go`
- Modify: `internal/infrastructure/postgres/repos_test.go`
- Modify: `internal/interfaces/http/ui_test.go`
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Add `user.Session` to `internal/domain/user/user.go`**

Update `internal/domain/user/user.go` to add `time` import and `Session` struct:

```go
package user

import (
	"strconv"
	"time"
)

type User struct {
	ID           string
	GitHubUserID int64
	Login        string
	DisplayName  string
}

type Identity struct {
	ID       string
	UserID   string
	Provider string
	Issuer   string
	Subject  string
}

func GitHubIdentity(userID string, ghID int64) Identity {
	return Identity{
		UserID:   userID,
		Provider: "github",
		Issuer:   "https://github.com",
		Subject:  strconv.FormatInt(ghID, 10),
	}
}

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

- [ ] **Step 2: Write failing tests in `internal/application/auth/service_test.go` for OAuth state expiry and `user.Identity` creation**

Update `internal/application/auth/service_test.go` so `memStore` implements the updated `Store` interface (`UpsertUser` accepting `user.Identity` and `ConsumeOAuthState` returning `(time.Time, bool, error)`) and add a test for expired OAuth states:

```go
package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/klimczak/infra-guy/internal/application/auth"
	"github.com/klimczak/infra-guy/internal/domain/tenant"
	"github.com/klimczak/infra-guy/internal/domain/user"
	"github.com/klimczak/infra-guy/internal/infrastructure/github"
)

type memStore struct {
	mu      sync.Mutex
	users   map[int64]user.User
	idents  map[string]user.Identity
	tenants map[string]tenant.Tenant
	members map[string][]tenant.Membership
	sess    map[string]auth.Session
	states  map[string]time.Time
}

func newMemStore() *memStore {
	return &memStore{
		users:   map[int64]user.User{},
		idents:  map[string]user.Identity{},
		tenants: map[string]tenant.Tenant{},
		members: map[string][]tenant.Membership{},
		sess:    map[string]auth.Session{},
		states:  map[string]time.Time{},
	}
}

func (m *memStore) UpsertUser(_ context.Context, u user.User, ident user.Identity, _ time.Time) (user.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.users[u.GitHubUserID]; ok {
		existing.Login = u.Login
		existing.DisplayName = u.DisplayName
		m.users[u.GitHubUserID] = existing
		ident.UserID = existing.ID
		m.idents[ident.Provider+":"+ident.Subject] = ident
		return existing, nil
	}
	m.users[u.GitHubUserID] = u
	ident.UserID = u.ID
	m.idents[ident.Provider+":"+ident.Subject] = ident
	return u, nil
}

func (m *memStore) EnsureDefaultTenantForUser(_ context.Context, u user.User, slug, name string, _ time.Time) (tenant.Tenant, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mem := range m.members[u.ID] {
		if t, ok := m.tenants[mem.TenantID]; ok {
			return t, mem.Role, nil
		}
	}
	t := tenant.Tenant{ID: "t-" + u.ID, Slug: slug, Name: name}
	m.tenants[t.ID] = t
	m.members[u.ID] = append(m.members[u.ID], tenant.Membership{
		TenantID: t.ID, UserID: u.ID, Role: tenant.RoleOwner,
	})
	return t, tenant.RoleOwner, nil
}

func (m *memStore) SaveOAuthState(_ context.Context, state string, exp time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[state] = exp
	return nil
}

func (m *memStore) ConsumeOAuthState(_ context.Context, state string) (time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.states[state]
	if !ok {
		return time.Time{}, false, nil
	}
	delete(m.states, state)
	return exp, true, nil
}

func (m *memStore) CreateSession(_ context.Context, s auth.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sess[s.ID] = s
	return nil
}

func (m *memStore) LoadSession(_ context.Context, id string, now time.Time) (*auth.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sess[id]
	if !ok || !s.ExpiresAt.After(now) {
		return nil, nil
	}
	cp := s
	return &cp, nil
}

func (m *memStore) DeleteSession(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sess, id)
	return nil
}
```

And add a test at the bottom of `internal/application/auth/service_test.go` verifying expired OAuth state rejection and `user.Identity` construction:

```go
func TestOAuthFlowRejectsExpiredState(t *testing.T) {
	ghSrv := httptest.NewServer(http.NotFoundHandler())
	defer ghSrv.Close()

	store := newMemStore()
	now := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
	svc := &auth.Service{
		OAuth: &github.OAuth{
			ClientID:     "cid",
			ClientSecret: "csec",
			AuthorizeURL: ghSrv.URL + "/authorize",
			TokenURL:     ghSrv.URL + "/token",
			APIBaseURL:   ghSrv.URL,
			HTTP:         ghSrv.Client(),
		},
		Store: store,
		Now:   func() time.Time { return now },
		NewID: func() string { return "state-exp" },
	}

	_, state, err := svc.Begin(context.Background(), "http://localhost/auth/callback")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	now = now.Add(15 * time.Minute)
	if _, _, err := svc.Complete(context.Background(), "code-abc", state, "http://localhost/auth/callback"); err == nil {
		t.Fatalf("expected error for expired OAuth state")
	}
}
```

- [ ] **Step 3: Update `internal/application/auth/service.go`**

Replace `internal/application/auth/service.go` with:

```go
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/klimczak/infra-guy/internal/domain/tenant"
	"github.com/klimczak/infra-guy/internal/domain/user"
	"github.com/klimczak/infra-guy/internal/infrastructure/github"
)

type Session = user.Session

type Store interface {
	UpsertUser(ctx context.Context, u user.User, ident user.Identity, now time.Time) (user.User, error)
	EnsureDefaultTenantForUser(ctx context.Context, u user.User, slug, name string, now time.Time) (tenant.Tenant, string, error)
	SaveOAuthState(ctx context.Context, state string, expiresAt time.Time) error
	ConsumeOAuthState(ctx context.Context, state string) (expiresAt time.Time, ok bool, err error)
	CreateSession(ctx context.Context, s Session) error
	LoadSession(ctx context.Context, id string, now time.Time) (*Session, error)
	DeleteSession(ctx context.Context, id string) error
}

type Service struct {
	OAuth      *github.OAuth
	Store      Store
	Now        func() time.Time
	NewID      func() string
	SessionTTL time.Duration
	StateTTL   time.Duration
}

var errOAuthState = errors.New("invalid or expired oauth state")

func (s *Service) Begin(ctx context.Context, redirectURI string) (authorizeURL, state string, err error) {
	if s.OAuth == nil || s.OAuth.ClientID == "" {
		return "", "", errors.New("github oauth not configured")
	}
	state = s.NewID()
	ttl := s.StateTTL
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	if err := s.Store.SaveOAuthState(ctx, state, s.Now().Add(ttl)); err != nil {
		return "", "", fmt.Errorf("save oauth state: %w", err)
	}
	return s.OAuth.AuthorizeRedirectURL(state, redirectURI), state, nil
}

func (s *Service) Complete(ctx context.Context, code, state, redirectURI string) (sessionID string, ghUserID int64, err error) {
	if code == "" || state == "" {
		return "", 0, errOAuthState
	}
	now := s.Now()
	expiresAt, ok, err := s.Store.ConsumeOAuthState(ctx, state)
	if err != nil {
		return "", 0, fmt.Errorf("consume oauth state: %w", err)
	}
	if !ok || expiresAt.Before(now) {
		return "", 0, errOAuthState
	}
	accessToken, err := s.OAuth.Exchange(ctx, code, redirectURI)
	if err != nil {
		return "", 0, fmt.Errorf("exchange code: %w", err)
	}
	ghID, login, name, err := s.OAuth.CurrentUser(ctx, accessToken)
	if err != nil {
		return "", 0, fmt.Errorf("fetch github user: %w", err)
	}
	candidateID := s.NewID()
	ident := user.GitHubIdentity(candidateID, ghID)
	if ident.Provider != "github" || ident.Subject == "" {
		return "", 0, errors.New("invalid github identity")
	}
	stored, err := s.Store.UpsertUser(ctx, user.User{
		ID:           candidateID,
		GitHubUserID: ghID,
		Login:        login,
		DisplayName:  name,
	}, ident, now)
	if err != nil {
		return "", 0, fmt.Errorf("upsert user: %w", err)
	}
	slug := sanitizeSlug(login)
	display := name
	if display == "" {
		display = login
	}
	ten, role, err := s.Store.EnsureDefaultTenantForUser(ctx, stored, slug, display, now)
	if err != nil {
		return "", 0, fmt.Errorf("ensure tenant: %w", err)
	}
	ttl := s.SessionTTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	sid := s.NewID()
	if err := s.Store.CreateSession(ctx, Session{
		ID:          sid,
		UserID:      stored.ID,
		TenantID:    ten.ID,
		Role:        role,
		Login:       stored.Login,
		AccessToken: accessToken,
		ExpiresAt:   now.Add(ttl),
	}); err != nil {
		return "", 0, fmt.Errorf("create session: %w", err)
	}
	return sid, ghID, nil
}

func (s *Service) Authenticate(ctx context.Context, sessionID string) (*Session, error) {
	if sessionID == "" {
		return nil, nil
	}
	return s.Store.LoadSession(ctx, sessionID, s.Now())
}

func (s *Service) Logout(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	return s.Store.DeleteSession(ctx, sessionID)
}

func sanitizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "default"
	}
	return out
}
```

- [ ] **Step 4: Update `internal/infrastructure/postgres/identity_repo.go` and `cmd/server/main.go`**

In `internal/infrastructure/postgres/identity_repo.go`:
1. Alias `type Session = user.Session`.
2. Update `UpsertUser` to accept `ident user.Identity` and persist `ident.Provider`, `ident.Issuer`, `ident.Subject` for `out.ID` without calling `user.GitHubIdentity`.
3. Update `ConsumeOAuthState` to return `(time.Time, bool, error)` without checking `now`.

```go
func (s *IdentityStore) UpsertUser(ctx context.Context, u user.User, ident user.Identity, now time.Time) (user.User, error) {
	var out user.User
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (id, github_user_id, login, display_name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)
		ON CONFLICT (github_user_id) DO UPDATE SET
			login = EXCLUDED.login,
			display_name = EXCLUDED.display_name,
			updated_at = EXCLUDED.updated_at
		RETURNING id, github_user_id, login, display_name
	`, u.ID, u.GitHubUserID, u.Login, u.DisplayName, utcTime(now)).Scan(
		&out.ID, &out.GitHubUserID, &out.Login, &out.DisplayName,
	)
	if err != nil {
		return user.User{}, err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO identities (id, user_id, provider, issuer, subject, created_at, updated_at)
		VALUES ('ident-' || $1, $1, $2, $3, $4, $5, $5)
		ON CONFLICT (provider, subject) DO UPDATE SET
			user_id = EXCLUDED.user_id,
			issuer = EXCLUDED.issuer,
			updated_at = EXCLUDED.updated_at
	`, out.ID, ident.Provider, ident.Issuer, ident.Subject, utcTime(now))
	if err != nil {
		return user.User{}, err
	}
	return out, nil
}
```

```go
func (s *IdentityStore) ConsumeOAuthState(ctx context.Context, state string) (time.Time, bool, error) {
	var expires time.Time
	err := s.pool.QueryRow(ctx, `
		DELETE FROM oauth_states WHERE state = $1 RETURNING expires_at
	`, state).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return utcTime(expires), true, nil
}

type Session = user.Session
```

In `cmd/server/main.go`, delete `type identityAdapter struct` and all its methods (lines 176–215), and pass `identStore` directly to `tenantTokenSource`, `authapp.Service`, and `installation.Service`:

```go
	identStore := postgres.NewIdentityStore(pool)
	ghTokens := &tenantTokenSource{
		inner:   baseTokenSource,
		install: identStore,
	}
```

```go
	authSvc := &authapp.Service{
		OAuth: ghOAuth,
		Store: identStore,
		Now:   now,
		NewID: newID,
	}
	installSvc := &installation.Service{
		Store: identStore,
		App:   ghAppClient,
		Now:   now,
	}
```

- [ ] **Step 5: Update `internal/interfaces/http/ui_test.go` and `internal/infrastructure/postgres/repos_test.go`**

In `internal/interfaces/http/ui_test.go`, update `memAuthStore.UpsertUser` and `memAuthStore.ConsumeOAuthState` (around lines 798 and 833):

```go
func (m *memAuthStore) UpsertUser(_ context.Context, u user.User, _ user.Identity, _ time.Time) (user.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.users[u.GitHubUserID]; ok {
		existing.Login = u.Login
		existing.DisplayName = u.DisplayName
		m.users[u.GitHubUserID] = existing
		return existing, nil
	}
	m.users[u.GitHubUserID] = u
	return u, nil
}
```

```go
func (m *memAuthStore) ConsumeOAuthState(_ context.Context, s string) (time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.states[s]
	delete(m.states, s)
	if !ok {
		return time.Time{}, false, nil
	}
	return exp, true, nil
}
```

In `internal/infrastructure/postgres/repos_test.go`, update `TestIdentityStoreLifecycle` (around lines 473–548):

```go
	u, err := store.UpsertUser(ctx, user.User{
		ID:           "user-1",
		GitHubUserID: 42,
		Login:        "octocat",
		DisplayName:  "The Octocat",
	}, user.GitHubIdentity("user-1", 42), now)
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	u2, err := store.UpsertUser(ctx, user.User{
		ID:           "user-different-id",
		GitHubUserID: 42,
		Login:        "octocat-renamed",
		DisplayName:  "Octo",
	}, user.GitHubIdentity("user-different-id", 42), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("re-upsert user: %v", err)
	}
```

And for `ConsumeOAuthState`:

```go
	if err := store.SaveOAuthState(ctx, "st-1", now.Add(5*time.Minute)); err != nil {
		t.Fatalf("save state: %v", err)
	}
	if exp, ok, err := store.ConsumeOAuthState(ctx, "st-1"); err != nil || !ok || !exp.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("consume state: exp=%v ok=%v err=%v", exp, ok, err)
	}
	if _, ok, err := store.ConsumeOAuthState(ctx, "st-1"); err != nil || ok {
		t.Fatalf("re-consume should fail: ok=%v err=%v", ok, err)
	}
```

- [ ] **Step 6: Run tests and commit Task 2**

Run:
```bash
go test ./internal/domain/user/... ./internal/application/auth/... ./internal/interfaces/http/... ./cmd/...
```
Expected: PASS.

Commit:
```bash
git add internal/domain/user/user.go internal/application/auth/service.go internal/application/auth/service_test.go internal/infrastructure/postgres/identity_repo.go internal/infrastructure/postgres/repos_test.go internal/interfaces/http/ui_test.go cmd/server/main.go
git commit -m "refactor(auth): move oauth expiry and identity creation to auth service and unify user.Session"
```

---

### Task 3: Domain Lock Invariant, Job Queue, Claim & Reap Policy Separation

**Files:**
- Modify: `internal/domain/project/project.go`
- Modify: `internal/domain/project/project_test.go`
- Modify: `internal/domain/run/run.go`
- Delete: `internal/infrastructure/postgres/worker_queue.go`
- Modify: `internal/infrastructure/postgres/queue.go`
- Modify: `internal/infrastructure/postgres/queue_test.go`
- Modify: `internal/infrastructure/postgres/repos_test.go`
- Modify: `internal/application/project/service_test.go`
- Modify: `internal/application/worker/worker.go`
- Modify: `internal/application/worker/worker_test.go`
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Add `ErrLocked` to `Project.ClaimActiveRun` and test in `internal/domain/project/project_test.go`**

In `internal/domain/project/project.go`, add `ErrLocked`:

```go
var (
	ErrBusy              = errors.New("project already has an active run")
	ErrLocked            = errors.New("project is locked")
	ErrNotTrackedBranch  = errors.New("ref is not the project's tracked branch")
	ErrAlreadyLocked     = errors.New("project is already locked")
	ErrNotLocked         = errors.New("project is not locked")
	ErrInvalidConfig     = errors.New("invalid project configuration")
	ErrDriftIntervalLow  = errors.New("drift check interval must be at least 5 minutes")
	ErrScheduledQueueMin = errors.New("scheduled queue max runs must be >= 1")
)
```

And update `ClaimActiveRun`:

```go
func (p *Project) ClaimActiveRun(runID string) error {
	if runID == "" {
		return errors.New("runID required")
	}
	if p.s.Locked {
		return ErrLocked
	}
	if p.s.ActiveRunID != "" && p.s.ActiveRunID != runID {
		return ErrBusy
	}
	p.s.ActiveRunID = runID
	return nil
}
```

In `internal/domain/project/project_test.go`, add a test verifying `ClaimActiveRun` returns `ErrLocked` when the project is locked:

```go
func TestClaimActiveRunRejectsLockedProject(t *testing.T) {
	p := mustNew(t)
	now := time.Date(2026, 4, 18, 13, 0, 0, 0, time.UTC)
	if err := p.Lock("user-1", "maintenance", now); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := p.ClaimActiveRun("run-1"); !errors.Is(err, project.ErrLocked) {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
}
```

In `internal/infrastructure/postgres/repos_test.go` (lines 57–60) and `internal/application/project/service_test.go` (lines 120–126), call `ClaimActiveRun` before `Lock` where tests construct a project that already had an active run before being locked:

In `internal/infrastructure/postgres/repos_test.go`:
```go
	if err := loaded.ClaimActiveRun("run-123"); err != nil {
		t.Fatalf("claim active: %v", err)
	}
	if err := loaded.Lock("alice", "maintenance", time.Now().UTC()); err != nil {
		t.Fatalf("lock: %v", err)
	}
```

In `internal/application/project/service_test.go`:
```go
	if err := p.ClaimActiveRun("run-stuck"); err != nil {
		t.Fatalf("claim active: %v", err)
	}
	if err := repo.Save(ctx, p); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := svc.Lock(ctx, p.ID(), "alice", "maint"); err != nil {
		t.Fatalf("lock: %v", err)
	}
```

- [ ] **Step 2: Add `Job` and `ReapDecision` to `internal/domain/run/run.go`**

In `internal/domain/run/run.go`, append `Job` and `ReapDecision`:

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

- [ ] **Step 3: Delete `internal/infrastructure/postgres/worker_queue.go` and update `internal/infrastructure/postgres/queue.go`**

Delete `internal/infrastructure/postgres/worker_queue.go` (using `git rm internal/infrastructure/postgres/worker_queue.go`).

Replace `internal/infrastructure/postgres/queue.go` with:

```go
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klimczak/infra-guy/internal/domain/project"
	"github.com/klimczak/infra-guy/internal/domain/run"
)

type Job = run.Job

type Queue struct {
	pool *pgxpool.Pool
}

func NewQueue(pool *pgxpool.Pool) *Queue { return &Queue{pool: pool} }

func (q *Queue) Enqueue(ctx context.Context, runID run.ID, step run.StepName) error {
	_, err := q.pool.Exec(ctx, `
		INSERT INTO run_jobs (run_id, step, status, available_at)
		VALUES ($1, $2, 'pending', now())
	`, string(runID), string(step))
	return err
}

func (q *Queue) Claim(ctx context.Context, workerID string, retryDelay time.Duration) (*run.Job, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var (
		job       run.Job
		runID     string
		step      string
		projectID string
	)
	err = tx.QueryRow(ctx, `
		SELECT j.id, j.run_id, j.step, j.attempts, r.project_id
		FROM run_jobs j
		JOIN runs r ON r.id = j.run_id
		WHERE j.status = 'pending' AND j.available_at <= now()
		ORDER BY j.id
		FOR UPDATE OF j SKIP LOCKED
		LIMIT 1
	`).Scan(&job.ID, &runID, &step, &job.Attempts, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	job.RunID = run.ID(runID)
	job.Step = run.StepName(step)

	pRow := tx.QueryRow(ctx,
		`SELECT `+projectSelectCols+` FROM projects WHERE id = $1 FOR UPDATE`, projectID)
	p, err := scanProject(pRow)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("project %s missing for run %s", projectID, runID)
	}

	claimErr := p.ClaimActiveRun(string(job.RunID))
	if errors.Is(claimErr, project.ErrLocked) || errors.Is(claimErr, project.ErrBusy) {
		_, err = tx.Exec(ctx, `
			UPDATE run_jobs
			SET available_at = now() + make_interval(secs => $2)
			WHERE id = $1
		`, job.ID, retryDelay.Seconds())
		if err != nil {
			return nil, err
		}
		return nil, tx.Commit(ctx)
	}
	if claimErr != nil {
		return nil, claimErr
	}
	if _, err := tx.Exec(ctx, `
		UPDATE projects SET active_run_id = $2, updated_at = now() WHERE id = $1
	`, projectID, string(job.RunID)); err != nil {
		return nil, err
	}

	_, err = tx.Exec(ctx, `
		UPDATE run_jobs
		SET status = 'running', locked_by = $2, locked_at = now(), attempts = attempts + 1
		WHERE id = $1
	`, job.ID, workerID)
	if err != nil {
		return nil, err
	}
	job.Attempts++

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &job, nil
}

func (q *Queue) Complete(ctx context.Context, jobID int64) error {
	_, err := q.pool.Exec(ctx, `UPDATE run_jobs SET status = 'done' WHERE id = $1`, jobID)
	return err
}

func (q *Queue) Fail(ctx context.Context, jobID int64, reason string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE run_jobs SET status = 'failed', last_error = $2 WHERE id = $1
	`, jobID, reason)
	return err
}

func (q *Queue) Heartbeat(ctx context.Context, jobID int64) error {
	_, err := q.pool.Exec(ctx, `UPDATE run_jobs SET locked_at = now() WHERE id = $1`, jobID)
	return err
}

func (q *Queue) Reap(ctx context.Context, staleAfter time.Duration, decide func(run.Job) run.ReapDecision) (requeue []run.Job, failed []run.Job, err error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id, run_id, step, attempts
		FROM run_jobs
		WHERE status = 'running' AND locked_at < now() - ($1::bigint * interval '1 millisecond')
		FOR UPDATE SKIP LOCKED
	`, staleAfter.Milliseconds())
	if err != nil {
		return nil, nil, err
	}
	var stale []run.Job
	for rows.Next() {
		var j run.Job
		var runID, step string
		if err := rows.Scan(&j.ID, &runID, &step, &j.Attempts); err != nil {
			rows.Close()
			return nil, nil, err
		}
		j.RunID = run.ID(runID)
		j.Step = run.StepName(step)
		stale = append(stale, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	for _, j := range stale {
		d := decide(j)
		if d.Fail {
			if _, err := tx.Exec(ctx, `
				UPDATE run_jobs SET status = 'failed', last_error = $2 WHERE id = $1
			`, j.ID, d.Reason); err != nil {
				return nil, nil, err
			}
			failed = append(failed, j)
		} else {
			if _, err := tx.Exec(ctx, `
				UPDATE run_jobs SET status = 'pending', locked_by = NULL, locked_at = NULL, available_at = now()
				WHERE id = $1
			`, j.ID); err != nil {
				return nil, nil, err
			}
			requeue = append(requeue, j)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return requeue, failed, nil
}
```

- [ ] **Step 4: Add `ReapPolicy` and `ClaimRetryDelay` to `internal/application/worker/worker.go` and update `worker_test.go`, `queue_test.go`, and `cmd/server/main.go`**

In `internal/application/worker/worker.go`, update `Job`, `Queue`, `ClaimRetryDelay`, `MaxStepAttempts`, `ReapPolicy`, and `Tick`:

```go
const (
	StaleAfter        = 5 * time.Minute
	HeartbeatInterval = 30 * time.Second
	PollInterval      = 1 * time.Second
	ClaimRetryDelay   = 5 * time.Second
	MaxStepAttempts   = 2
)

type Job = run.Job

func ReapPolicy(job run.Job) run.ReapDecision {
	if job.Step == run.StepApply || job.Attempts >= MaxStepAttempts {
		return run.ReapDecision{
			Fail:   true,
			Reason: "worker lease expired during step",
		}
	}
	return run.ReapDecision{Fail: false}
}

type Queue interface {
	Claim(ctx context.Context, workerID string, retryDelay time.Duration) (*run.Job, error)
	Complete(ctx context.Context, jobID int64) error
	Fail(ctx context.Context, jobID int64, reason string) error
	Heartbeat(ctx context.Context, jobID int64) error
	Reap(ctx context.Context, staleAfter time.Duration, decide func(run.Job) run.ReapDecision) (requeue, failed []run.Job, err error)
}
```

And in `Worker.Tick`:

```go
func (w *Worker) Tick(ctx context.Context) (bool, error) {
	if _, failed, err := w.Queue.Reap(ctx, StaleAfter, ReapPolicy); err != nil {
		slog.Warn("reap failed", "err", err)
	} else {
		for _, job := range failed {
			_ = w.Service.Fail(ctx, job.RunID, job.Step, ReapPolicy(job).Reason)
		}
	}

	job, err := w.Queue.Claim(ctx, w.WorkerID, ClaimRetryDelay)
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, nil
	}
	w.runJob(ctx, *job)
	return true, nil
}
```

In `cmd/server/main.go`, pass `Queue: queue` directly to `worker.Worker` (replacing `postgres.NewWorkerQueue(queue)`).

In `internal/application/worker/worker_test.go`, update `fakeQueue.Claim` and `fakeQueue.Reap` signatures to match `Queue`, and add a unit test for `ReapPolicy`:

```go
func (q *fakeQueue) Claim(_ context.Context, _ string, _ time.Duration) (*worker.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.jobs) == 0 {
		return nil, nil
	}
	j := q.jobs[0]
	q.jobs = q.jobs[1:]
	j.Attempts++
	return &j, nil
}
```

```go
func (q *fakeQueue) Reap(_ context.Context, _ time.Duration, _ func(worker.Job) run.ReapDecision) ([]worker.Job, []worker.Job, error) {
	return nil, nil, nil
}
```

```go
func TestReapPolicy(t *testing.T) {
	if d := worker.ReapPolicy(run.Job{Step: run.StepPlan, Attempts: 1}); d.Fail {
		t.Fatalf("expected plan attempt 1 to requeue, got %+v", d)
	}
	if d := worker.ReapPolicy(run.Job{Step: run.StepPlan, Attempts: 2}); !d.Fail || d.Reason == "" {
		t.Fatalf("expected plan attempt 2 to fail with reason, got %+v", d)
	}
	if d := worker.ReapPolicy(run.Job{Step: run.StepApply, Attempts: 1}); !d.Fail || d.Reason == "" {
		t.Fatalf("expected apply attempt 1 to fail immediately, got %+v", d)
	}
}
```

In `internal/infrastructure/postgres/queue_test.go`, update all `q.Claim(ctx, ...)` calls to pass `5*time.Second` and `q.Reap(ctx, ...)` calls to pass `worker.ReapPolicy` (importing `github.com/klimczak/infra-guy/internal/application/worker`).

- [ ] **Step 5: Run tests and commit Task 3**

Run:
```bash
go test ./internal/domain/project/... ./internal/domain/run/... ./internal/application/project/... ./internal/application/worker/... ./cmd/...
```
Expected: PASS.

Commit:
```bash
git rm internal/infrastructure/postgres/worker_queue.go
git add internal/domain/project/project.go internal/domain/project/project_test.go internal/domain/run/run.go internal/infrastructure/postgres/queue.go internal/infrastructure/postgres/queue_test.go internal/infrastructure/postgres/repos_test.go internal/application/project/service_test.go internal/application/worker/worker.go internal/application/worker/worker_test.go cmd/server/main.go
git commit -m "refactor(worker,queue): move lock check to domain, reap policy to worker, and remove WorkerQueue adapter"
```

---

### Task 4: Project & State Repo Cleanup and Service Boundary Enforcement

**Files:**
- Modify: `internal/domain/project/project.go`
- Modify: `internal/domain/project/project_test.go`
- Modify: `internal/infrastructure/postgres/project_repo.go`
- Modify: `internal/infrastructure/postgres/state_repo.go`
- Modify: `internal/infrastructure/postgres/repos_test.go`
- Modify: `internal/application/state/service.go`
- Modify: `internal/application/state/service_test.go`
- Modify: `internal/application/project/service.go`
- Modify: `internal/application/project/service_test.go`
- Modify: `internal/application/run/service.go`
- Modify: `internal/application/run/service_test.go`
- Modify: `internal/application/worker/worker.go`
- Modify: `internal/application/worker/worker_test.go`
- Modify: `internal/application/dependency/service.go`
- Modify: `internal/application/dependency/service_test.go`
- Modify: `internal/interfaces/http/server.go`
- Modify: `internal/interfaces/http/state.go`
- Modify: `internal/interfaces/http/webhook.go`
- Modify: `internal/interfaces/http/ui.go`
- Modify: `internal/interfaces/http/state_test.go`
- Modify: `internal/interfaces/http/webhook_test.go`
- Modify: `internal/interfaces/http/ui_test.go`
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Add `Project.RenameRepo` to `internal/domain/project/project.go` and test in `internal/domain/project/project_test.go`**

In `internal/domain/project/project.go`, add `RenameRepo`:

```go
func (p *Project) RenameRepo(owner, name string, now time.Time) error {
	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	if owner == "" || name == "" {
		return errors.New("repo owner and name required")
	}
	p.s.RepoOwner = owner
	p.s.RepoName = name
	p.s.UpdatedAt = now
	return nil
}
```

In `internal/domain/project/project_test.go`, add a unit test for `RenameRepo`:

```go
func TestRenameRepo(t *testing.T) {
	p := mustNew(t)
	now := time.Date(2026, 4, 18, 14, 0, 0, 0, time.UTC)
	if err := p.RenameRepo("new-org", "new-repo", now); err != nil {
		t.Fatalf("rename repo: %v", err)
	}
	if p.RepoOwner() != "new-org" || p.RepoName() != "new-repo" {
		t.Fatalf("unexpected repo: %s/%s", p.RepoOwner(), p.RepoName())
	}
	if err := p.RenameRepo("", "new-repo", now); err == nil {
		t.Fatalf("expected error on empty owner")
	}
}
```

- [ ] **Step 2: Clean up `internal/infrastructure/postgres/project_repo.go`, `state_repo.go`, and `repos_test.go`**

In `internal/infrastructure/postgres/project_repo.go`, remove `firstNonEmptyVersion` and pass `s.TerraformVersion` directly in `Save`:

```go
	_, err := r.pool.Exec(ctx, `
		INSERT INTO projects (`+projectSelectCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
		ON CONFLICT (id) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id,
			name = EXCLUDED.name,
			repo_owner = EXCLUDED.repo_owner,
			repo_name = EXCLUDED.repo_name,
			github_repo_id = EXCLUDED.github_repo_id,
			working_directory = EXCLUDED.working_directory,
			tracked_branch = EXCLUDED.tracked_branch,
			terraform_version = EXCLUDED.terraform_version,
			engine = EXCLUDED.engine,
			auto_apply = EXCLUDED.auto_apply,
			pr_runs_enabled = EXCLUDED.pr_runs_enabled,
			locked = EXCLUDED.locked,
			locked_by = EXCLUDED.locked_by,
			locked_at = EXCLUDED.locked_at,
			lock_reason = EXCLUDED.lock_reason,
			active_run_id = EXCLUDED.active_run_id,
			drift_enabled = EXCLUDED.drift_enabled,
			drift_check_interval = EXCLUDED.drift_check_interval,
			drift_reconcile = EXCLUDED.drift_reconcile,
			scheduled_queue_max_runs = EXCLUDED.scheduled_queue_max_runs,
			updated_at = EXCLUDED.updated_at
	`,
		string(s.ID), s.TenantID, s.Name, s.RepoOwner, s.RepoName, s.GitHubRepoID, s.WorkingDirectory, s.TrackedBranch,
		s.TerraformVersion, string(s.Engine), s.AutoApply, s.PRRunsEnabled,
		s.Locked, s.LockedBy, nullableTime(s.LockedAt), s.LockReason, nullIfEmpty(s.ActiveRunID),
		s.DriftEnabled, int64(s.DriftCheckInterval), s.DriftReconcile, s.ScheduledQueueMaxRuns,
		utcTime(s.UpdatedAt), utcTime(s.CreatedAt),
	)
	return err
```

In `internal/infrastructure/postgres/state_repo.go`, return `nil, nil` on `pgx.ErrNoRows` in `Load`:

```go
func (r *StateRepo) Load(ctx context.Context, projectID string) (*state.RemoteState, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT project_id, serial, lineage, body, updated_at,
		       lock_id, lock_operation, lock_who, lock_version, lock_created, lock_path, lock_info
		FROM remote_states WHERE project_id = $1
	`, projectID)

	var (
		s                                                 state.Snapshot
		lockID, lockOp, lockWho, lockVer, lockPath, lockI *string
		lockCreated                                       *time.Time
	)
	err := row.Scan(
		&s.ProjectID, &s.Serial, &s.Lineage, &s.Body, &s.UpdatedAt,
		&lockID, &lockOp, &lockWho, &lockVer, &lockCreated, &lockPath, &lockI,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.UpdatedAt = utcTime(s.UpdatedAt)
	if lockID != nil && *lockID != "" {
		created := time.Time{}
		if lockCreated != nil {
			created = utcTime(*lockCreated)
		}
		s.Lock = &state.LockInfo{
			ID:        deref(lockID),
			Operation: deref(lockOp),
			Who:       deref(lockWho),
			Version:   deref(lockVer),
			Created:   created,
			Path:      deref(lockPath),
			Info:      deref(lockI),
		}
	}
	return state.Rehydrate(s), nil
}
```

In `internal/infrastructure/postgres/repos_test.go`, update `TestStateRepoLockPutUnlock` (around lines 163–168) to expect `st == nil` on initial load and initialize `st = state.New(string(p.ID()))`:

```go
	st, err := states.Load(ctx, string(p.ID()))
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if st != nil {
		t.Fatalf("expected nil state before first save, got %+v", st)
	}
	st = state.New(string(p.ID()))
```

- [ ] **Step 3: Expand `state.Service`, `project.Service`, and `run.Service` and update their tests**

In `internal/application/state/service.go`, add `Outputs`, `ChangedOutputsSincePrevious`, and `ForceUnlock`:

```go
func (s *Service) Outputs(ctx context.Context, projectID string) (map[string]state.OutputValue, error) {
	st, err := s.States.Load(ctx, projectID)
	if err != nil || st == nil {
		return nil, err
	}
	return st.Outputs()
}

func (s *Service) ChangedOutputsSincePrevious(ctx context.Context, projectID string) (map[string]bool, error) {
	versions, err := s.States.ListVersions(ctx, projectID, 2)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, nil
	}
	latest, err := state.ParseOutputs(versions[0].Body)
	if err != nil {
		return nil, err
	}
	var prev map[string]state.OutputValue
	if len(versions) > 1 {
		prev, err = state.ParseOutputs(versions[1].Body)
		if err != nil {
			return nil, err
		}
	}
	return state.DiffOutputs(prev, latest), nil
}

func (s *Service) ForceUnlock(ctx context.Context, projectID string) error {
	st, err := s.States.Load(ctx, projectID)
	if err != nil || st == nil {
		return err
	}
	st.ForceUnlock()
	return s.States.Save(ctx, st)
}
```

In `internal/application/state/service_test.go`, add tests for `Outputs`, `ChangedOutputsSincePrevious`, and `ForceUnlock`:

```go
func TestOutputsChangedAndForceUnlock(t *testing.T) {
	svc := &appstate.Service{
		States: newMemRepo(),
		Now:    func() time.Time { return time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC) },
	}
	ctx := context.Background()

	info := state.LockInfo{ID: "lock-1", Operation: "OperationTypeApply", Who: "me"}
	if _, err := svc.Lock(ctx, "proj-1", info); err != nil {
		t.Fatalf("lock: %v", err)
	}
	body1 := []byte(`{"version":4,"serial":1,"lineage":"lin-1","outputs":{"vpc_id":{"value":"vpc-1","type":"string","sensitive":false}}}`)
	if err := svc.Put(ctx, "proj-1", "lock-1", body1); err != nil {
		t.Fatalf("put 1: %v", err)
	}
	body2 := []byte(`{"version":4,"serial":2,"lineage":"lin-1","outputs":{"vpc_id":{"value":"vpc-2","type":"string","sensitive":false}}}`)
	if err := svc.Put(ctx, "proj-1", "lock-1", body2); err != nil {
		t.Fatalf("put 2: %v", err)
	}

	outs, err := svc.Outputs(ctx, "proj-1")
	if err != nil || string(outs["vpc_id"].Value) != `"vpc-2"` {
		t.Fatalf("outputs: %+v err=%v", outs, err)
	}
	changed, err := svc.ChangedOutputsSincePrevious(ctx, "proj-1")
	if err != nil || !changed["vpc_id"] {
		t.Fatalf("changed outputs: %+v err=%v", changed, err)
	}

	if err := svc.ForceUnlock(ctx, "proj-1"); err != nil {
		t.Fatalf("force unlock: %v", err)
	}
	if _, err := svc.Lock(ctx, "proj-1", state.LockInfo{ID: "lock-2"}); err != nil {
		t.Fatalf("lock after force unlock should succeed: %v", err)
	}
}
```

In `internal/application/project/service.go`, replace `state` import with `StateForceUnlocker` and add `SyncAndFindByRepo`:

```go
type StateForceUnlocker interface {
	ForceUnlock(ctx context.Context, projectID string) error
}

type Service struct {
	Projects project.Repository
	States   StateForceUnlocker
	Now      func() time.Time
	NewID    func() project.ID
}
```

Update `ForceUnlock` in `internal/application/project/service.go`:

```go
func (s *Service) ForceUnlock(ctx context.Context, id project.ID) error {
	p, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	p.ForceReleaseActiveRun(s.Now())
	if err := s.Projects.Save(ctx, p); err != nil {
		return err
	}
	if s.States == nil {
		return nil
	}
	return s.States.ForceUnlock(ctx, string(id))
}
```

And add `SyncAndFindByRepo` to `internal/application/project/service.go`:

```go
func (s *Service) SyncAndFindByRepo(ctx context.Context, tenantID string, repoID int64, ownerLogin, repoName string) ([]*project.Project, error) {
	if repoID > 0 {
		var (
			matched []*project.Project
			err     error
		)
		if tenantID != "" {
			matched, err = s.Projects.FindByTenantAndGitHubRepoID(ctx, tenantID, repoID)
		} else {
			matched, err = s.Projects.FindByGitHubRepoID(ctx, repoID)
		}
		if err != nil {
			return nil, err
		}
		if len(matched) > 0 {
			for _, p := range matched {
				if ownerLogin != "" && repoName != "" && (p.RepoOwner() != ownerLogin || p.RepoName() != repoName) {
					if err := p.RenameRepo(ownerLogin, repoName, s.Now()); err == nil {
						_ = s.Projects.Save(ctx, p)
					}
				}
			}
			return matched, nil
		}
	}
	if tenantID != "" {
		return s.Projects.FindByTenantAndRepo(ctx, tenantID, ownerLogin, repoName)
	}
	return s.Projects.FindByRepo(ctx, ownerLogin, repoName)
}
```

In `internal/application/project/service_test.go`, update `TestForceUnlockClearsActiveRunAndStateLock` to use `stateapp.Service` (or a `StateForceUnlocker` double) on `svc.States`, and add a test for `SyncAndFindByRepo`:

```go
type fakeStateForceUnlocker struct {
	unlocked []string
}

func (f *fakeStateForceUnlocker) ForceUnlock(_ context.Context, projectID string) error {
	f.unlocked = append(f.unlocked, projectID)
	return nil
}
```

And in `TestForceUnlockClearsActiveRunAndStateLock`:

```go
	states := &fakeStateForceUnlocker{}
	svc.States = states
	if err := svc.ForceUnlock(ctx, p.ID()); err != nil {
		t.Fatalf("force unlock: %v", err)
	}
	if len(states.unlocked) != 1 || states.unlocked[0] != string(p.ID()) {
		t.Fatalf("expected ForceUnlock on state for %s, got %v", p.ID(), states.unlocked)
	}
```

And add `TestSyncAndFindByRepoUpdatesRenamedRepo` in `internal/application/project/service_test.go`:

```go
func TestSyncAndFindByRepoUpdatesRenamedRepo(t *testing.T) {
	svc, repo := newSvc()
	ctx := context.Background()

	p, err := svc.Create(ctx, projectapp.CreateInput{
		TenantID:     "t-1",
		Name:         "net",
		RepoOwner:    "old-org",
		RepoName:     "old-repo",
		GitHubRepoID: 999,
	})
	if err != nil {
		t.Fatal(err)
	}

	matched, err := svc.SyncAndFindByRepo(ctx, "t-1", 999, "new-org", "new-repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 1 || matched[0].ID() != p.ID() {
		t.Fatalf("expected 1 matched project, got %+v", matched)
	}
	reloaded, _ := repo.Get(ctx, p.ID())
	if reloaded.RepoOwner() != "new-org" || reloaded.RepoName() != "new-repo" {
		t.Fatalf("expected renamed repo new-org/new-repo, got %s/%s", reloaded.RepoOwner(), reloaded.RepoName())
	}
}
```

In `internal/application/run/service.go`, add `BeginStep`:

```go
func (s *Service) BeginStep(ctx context.Context, id run.ID, step run.StepName) (*run.Run, error) {
	r, err := s.Runs.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, ErrRunNotFound
	}
	if err := r.StartStep(step, s.Now()); err != nil {
		return nil, err
	}
	if err := s.Runs.Save(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}
```

In `internal/application/run/service_test.go`, add a unit test for `BeginStep`:

```go
func TestBeginStepStartsAndSavesRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	runs, err := f.svc.TriggerPush(ctx, f.proj, "refs/heads/main", "1111111111111111111111111111111111111111", "alice", nil)
	if err != nil || len(runs) != 1 {
		t.Fatalf("trigger push: %v", err)
	}
	started, err := f.svc.BeginStep(ctx, runs[0].ID(), run.StepPlan)
	if err != nil {
		t.Fatalf("begin step: %v", err)
	}
	if started.Status() != run.StatusRunning {
		t.Fatalf("expected status running, got %s", started.Status())
	}
}
```

- [ ] **Step 4: Update `worker.Worker` and `dependency.Service` to depend on service interfaces instead of repositories**

In `internal/application/worker/worker.go`:
1. Define `ProjectGetter`:
```go
type ProjectGetter interface {
	Get(ctx context.Context, id project.ID) (*project.Project, error)
}
```
2. Update `Worker` struct to remove `Runs run.Repository` and use `Projects ProjectGetter`:
```go
type Worker struct {
	WorkerID     string
	Queue        Queue
	Projects     ProjectGetter
	Service      *apprun.Service
	Contexts     ContextResolver
	Dependencies DependencyEvaluator
	Source       SourceFetcher
	Workspaces   WorkspaceStore
	Runner       TerraformRunner
	Backend      BackendConfig
	StateSigner  *auth.Signer
	Now          func() time.Time
}
```
3. In `runJob`, call `r, err := w.Service.BeginStep(ctx, job.RunID, job.Step)` instead of loading/mutating/saving `w.Runs`:
```go
func (w *Worker) runJob(ctx context.Context, job Job) {
	log := slog.With("worker", w.WorkerID, "job", job.ID, "run", string(job.RunID), "step", string(job.Step))

	r, err := w.Service.BeginStep(ctx, job.RunID, job.Step)
	if err != nil {
		if errors.Is(err, apprun.ErrRunNotFound) {
			log.Error("run missing for job", "err", err)
			_ = w.Queue.Fail(ctx, job.ID, "run not found")
			return
		}
		log.Warn("begin step rejected", "err", err)
		_ = w.Queue.Fail(ctx, job.ID, err.Error())
		return
	}

	proj, err := w.Projects.Get(ctx, r.ProjectID())
	if err != nil || proj == nil {
		log.Error("project missing for run", "project", string(r.ProjectID()), "err", err)
		_ = w.Service.Fail(ctx, r.ID(), job.Step, "project not found")
		_ = w.Queue.Fail(ctx, job.ID, "project not found")
		return
	}
```
4. In `removeIfTerminal`, call `w.Service.Get(ctx, id)` instead of `w.Runs.Get(ctx, id)`:
```go
func (w *Worker) removeIfTerminal(ctx context.Context, id run.ID) {
	if w.Workspaces == nil {
		return
	}
	r, err := w.Service.Get(ctx, id)
	if err != nil || r == nil {
		return
	}
	if r.Status().IsTerminal() {
		_ = w.Workspaces.Remove(string(id))
	}
}
```

In `internal/application/worker/worker_test.go`, remove `Runs: runs` from `&worker.Worker{...}`.

In `internal/application/dependency/service.go`, replace `Projects project.Repository` and `States state.Repository` with `ProjectGetter` and `StateReader`:

```go
type ProjectGetter interface {
	Get(ctx context.Context, id project.ID) (*project.Project, error)
}

type StateReader interface {
	Outputs(ctx context.Context, projectID string) (map[string]state.OutputValue, error)
	ChangedOutputsSincePrevious(ctx context.Context, projectID string) (map[string]bool, error)
}

type Service struct {
	Deps     dependency.Repository
	Projects ProjectGetter
	States   StateReader
	Runs     RunTrigger
	Now      func() time.Time
	NewID    func() dependency.ID
}
```

Update `ResolveInputs` and `TriggerDownstream` in `internal/application/dependency/service.go` to call `s.States.Outputs(ctx, string(d.DependsOnProjectID))` and `s.States.ChangedOutputsSincePrevious(ctx, string(completedProjectID))`, and delete the private `changedOutputs` helper in `internal/application/dependency/service.go`:

```go
func (s *Service) ResolveInputs(ctx context.Context, projectID project.ID) (ResolvedInputs, error) {
	deps, err := s.Deps.ListByProject(ctx, projectID)
	if err != nil {
		return ResolvedInputs{}, err
	}
	if len(deps) == 0 {
		return ResolvedInputs{}, nil
	}
	env := make(map[string]string)
	for _, d := range deps {
		if len(d.OutputMappings) == 0 {
			continue
		}
		outputs, err := s.States.Outputs(ctx, string(d.DependsOnProjectID))
		if err != nil {
			return ResolvedInputs{}, fmt.Errorf("parse upstream state outputs for project %s: %w", d.DependsOnProjectID, err)
		}
		for _, m := range d.OutputMappings {
			ov, ok := outputs[m.OutputName]
			if !ok {
				if d.WaitForOutputs {
					return ResolvedInputs{
						Blocked:       true,
						BlockedReason: fmt.Sprintf("waiting for upstream project %s output %q", d.DependsOnProjectID, m.OutputName),
					}, nil
				}
				continue
			}
			env["TF_VAR_"+m.InputName] = state.FormatTFVar(ov)
		}
	}
	return ResolvedInputs{Env: env}, nil
}

func (s *Service) TriggerDownstream(ctx context.Context, completedProjectID project.ID, sha string) error {
	if s.Runs == nil {
		return nil
	}
	dependents, err := s.Deps.ListDependents(ctx, completedProjectID)
	if err != nil {
		return err
	}
	if len(dependents) == 0 {
		return nil
	}

	changed, err := s.States.ChangedOutputsSincePrevious(ctx, string(completedProjectID))
	if err != nil {
		return err
	}

	for _, dep := range dependents {
		if !dependency.ShouldTrigger(dep, changed) {
			continue
		}
		downstream, err := s.Projects.Get(ctx, dep.ProjectID)
		if err != nil || downstream == nil {
			continue
		}
		ref := "refs/heads/" + downstream.TrackedBranch()
		_, _ = s.Runs.TriggerScheduled(ctx, downstream, ref, sha, "dependency:"+string(completedProjectID))
	}
	return nil
}
```

In `internal/application/dependency/service_test.go`, wrap the in-memory state repo in `&stateapp.Service{States: stateRepo, Now: ...}` (or implement `Outputs` and `ChangedOutputsSincePrevious` on `memStateRepo`) so `svc.States` satisfies `StateReader`.

- [ ] **Step 5: Enforce the HTTP boundary in `internal/interfaces/http/*` and `cmd/server/main.go`**

In `internal/interfaces/http/state.go`, replace `projects project.Repository` with `ProjectMatcher`:

```go
type ProjectMatcher interface {
	SyncAndFindByRepo(ctx context.Context, tenantID string, repoID int64, ownerLogin, repoName string) ([]*project.Project, error)
}

type API struct {
	states        *stateapp.Service
	runs          *runapp.Service
	projects      ProjectMatcher
	installations InstallationLookup
	verifier      *github.WebhookVerifier
	signer        *auth.Signer
	webhookDedup  WebhookDedup
}

func New(
	states *stateapp.Service,
	runs *runapp.Service,
	projects ProjectMatcher,
	installations InstallationLookup,
	verifier *github.WebhookVerifier,
	signer *auth.Signer,
	dedup WebhookDedup,
) *API {
	return &API{
		states:        states,
		runs:          runs,
		projects:      projects,
		installations: installations,
		verifier:      verifier,
		signer:        signer,
		webhookDedup:  dedup,
	}
}
```

In `internal/interfaces/http/webhook.go`, replace `a.findMatchingProjects(...)` with `a.projects.SyncAndFindByRepo(...)` and delete `findMatchingProjects` and `renameProjectRepo`:

```go
	projects, err := a.projects.SyncAndFindByRepo(r.Context(), inst.TenantID, env.Repository.ID, syncOwner, syncName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
```

In `internal/interfaces/http/server.go`, remove `ProjectRepo` and `StateRepo` from `ServerConfig` and pass `cfg.Projects` to `New`:

```go
type ServerConfig struct {
	States        *stateapp.Service
	Runs          *runapp.Service
	Projects      *projectapp.Service
	Contexts      *tfcontextapp.Service
	Dependencies  *dependencyapp.Service
	Auth          *authapp.Service
	Installations *installation.Service
	GitHubApp     *github.AppClient
	Verifier      *github.WebhookVerifier
	Signer        *auth.Signer
	WebhookDedup  WebhookDedup
	SecureCookies bool
}

func NewServer(cfg ServerConfig) http.Handler {
	api := New(cfg.States, cfg.Runs, cfg.Projects, cfg.Installations, cfg.Verifier, cfg.Signer, cfg.WebhookDedup)
	ui := newUI(cfg)
```

In `internal/interfaces/http/ui.go`:
1. In `handleProjectForceUnlock`, call `u.cfg.Projects.ForceUnlock(r.Context(), id)`:
```go
func (u *uiHandler) handleProjectForceUnlock(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.requireRole(w, r, tenant.RoleAdmin); !ok {
		return
	}
	id := project.ID(r.PathValue("id"))
	if _, ok := u.loadTenantProject(w, r, id); !ok {
		return
	}
	if err := u.cfg.Projects.ForceUnlock(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	redirectBack(w, r, "/projects/"+string(id))
}
```
2. In `buildProjectDependenciesView`, use `u.cfg.States.Outputs` instead of `u.cfg.StateRepo`:
```go
		var availableOutputs []string
		if u.cfg.States != nil {
			if outs, err := u.cfg.States.Outputs(ctx, string(ds.DependsOnProjectID)); err == nil {
				for k := range outs {
					availableOutputs = append(availableOutputs, k)
				}
				sort.Strings(availableOutputs)
			}
		}
```

In `internal/interfaces/http/state_test.go`, `internal/interfaces/http/webhook_test.go`, and `internal/interfaces/http/ui_test.go`:
- In `state_test.go`: pass `nil` for `ProjectMatcher` in `httpapi.New(...)`.
- In `webhook_test.go`: wrap `projRepo` in `&projectapp.Service{Projects: projRepo, Now: time.Now}` when calling `httpapi.New(...)`.
- In `ui_test.go`: set `States: stateSvc` on `projectSvc` (`projectSvc := &projectapp.Service{Projects: projRepo, States: stateSvc, Now: now, NewID: ...}`) and remove `ProjectRepo` and `StateRepo` from `httpapi.ServerConfig`.

In `cmd/server/main.go`, construct `stateSvc` and `projectSvc` once and wire them cleanly:

```go
	stateSvc := &stateapp.Service{States: stateRepo, Now: now}
	projectSvc := &projectapp.Service{
		Projects: projectRepo,
		States:   stateSvc,
		Now:      now,
		NewID:    func() project.ID { return project.ID(newID()) },
	}
	dependencySvc := &dependencyapp.Service{
		Deps:     dependencyRepo,
		Projects: projectSvc,
		States:   stateSvc,
		Runs:     runSvc,
		Now:      now,
		NewID:    func() dependency.ID { return dependency.ID(newID()) },
	}
```

And in `worker.Worker` and `httpapi.NewServer` in `cmd/server/main.go`:

```go
	w := &worker.Worker{
		WorkerID:     workerID,
		Queue:        queue,
		Projects:     projectSvc,
		Service:      runSvc,
		Contexts:     contextSvc,
		Dependencies: dependencySvc,
		Source:       sourceFetcher,
		Workspaces:   workspaces,
		Runner:       runner,
		Backend:      worker.BackendConfig{BaseURL: cfg.PublicBaseURL},
		StateSigner:  signer,
		Now:          now,
	}
```

```go
	handler := httpapi.NewServer(httpapi.ServerConfig{
		States:        stateSvc,
		Runs:          runSvc,
		Projects:      projectSvc,
		Contexts:      contextSvc,
		Dependencies:  dependencySvc,
		Auth:          authSvc,
		Installations: installSvc,
		GitHubApp:     ghAppClient,
		Verifier:      verifier,
		Signer:        signer,
		WebhookDedup:  dedupRepo,
		SecureCookies: strings.HasPrefix(strings.ToLower(cfg.PublicBaseURL), "https://"),
	})
```

- [ ] **Step 6: Run the full test suite and commit Task 4**

Run:
```bash
go test ./...
```
Expected: PASS across all packages.

Commit:
```bash
git add internal/domain/project/project.go internal/domain/project/project_test.go internal/infrastructure/postgres/project_repo.go internal/infrastructure/postgres/state_repo.go internal/infrastructure/postgres/repos_test.go internal/application/state/service.go internal/application/state/service_test.go internal/application/project/service.go internal/application/project/service_test.go internal/application/run/service.go internal/application/run/service_test.go internal/application/worker/worker.go internal/application/worker/worker_test.go internal/application/dependency/service.go internal/application/dependency/service_test.go internal/interfaces/http/server.go internal/interfaces/http/state.go internal/interfaces/http/webhook.go internal/interfaces/http/ui.go internal/interfaces/http/state_test.go internal/interfaces/http/webhook_test.go internal/interfaces/http/ui_test.go cmd/server/main.go
git commit -m "refactor(architecture): enforce strict service boundary and clean up project and state repos"
```
