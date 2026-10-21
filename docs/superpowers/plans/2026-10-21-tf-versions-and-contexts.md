# Phase 1: Terraform Versions & Contexts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add per-project Terraform version selection (with HashiCorp Releases catalog and on-demand cached binary installer) and org-scoped Contexts (supporting plain-text env vars, encrypted secret env vars, and encrypted `$HOME` secret files injected into an isolated per-step `$HOME`).

**Architecture:** We extend the `project` domain with `TerraformVersion` (defaulting to `1.9.8`) and introduce a new `tfcontext` domain + application service backed by AES-256-GCM encryption (`internal/infrastructure/crypto`) and Postgres tables (`contexts`, `context_entries`, `project_contexts`). The Terraform infrastructure package gains a `VersionCatalog` and on-demand `Installer`, and `terraform.Executor` materializes attached context files into `<workDir>/.home` (`HOME=<workDir>/.home`, `0600`) and injects resolved context env vars into every Terraform invocation.

**Tech Stack:** Go 1.25, PostgreSQL (`pgx/v5`), standard library `crypto/aes` + `crypto/cipher` (AES-256-GCM), `archive/zip`, `html/template`, HTMX.

**Spec:** `docs/superpowers/specs/2026-10-21-tf-versions-and-contexts-design.md`

## Global Constraints

- Default Terraform version is `"1.9.8"`.
- Terraform versions must match strict semver `^\d+\.\d+\.\d+$` (`project.ValidateTerraformVersion`).
- Context entry kinds are `"plain_env"`, `"secret_env"`, and `"secret_file"`.
- Reserved environment variable keys (`PATH`, `HOME`, `USER`, `TF_IN_AUTOMATION`, `TF_INPUT`, `DATABASE_URL`, `POSTGRES_PASSWORD`, `BASIC_AUTH_USER`, `BASIC_AUTH_PASSWORD`, `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_APP_PRIVATE_KEY_FILE`, `GITHUB_APP_WEBHOOK_SECRET`, `GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET`, `CONTEXT_ENCRYPTION_KEY`, `TF_LOG`, `GIT_TRACE`) must be rejected by `tfcontext.ValidateEnvKey` and filtered by `terraform.blockedEnv`.
- `secret_file` keys must be clean relative paths within `$HOME` (leading `~/` stripped, `/` prefix rejected, `..` traversal rejected) and written with mode `0600` inside `<workDir>/.home` (`0700`).
- Secret values (`secret_env` and `secret_file`) are encrypted at rest with AES-256-GCM and never rendered back in the UI after creation.

---

### Task 1: Project `TerraformVersion` Domain & Application Support

**Files:**
- Modify: `internal/domain/project/project.go`
- Modify: `internal/domain/project/project_test.go`
- Modify: `internal/application/project/service.go`
- Modify: `internal/application/project/service_test.go`

**Interfaces:**
- Produces:
  - `project.DefaultTerraformVersion = "1.9.8"`
  - `project.ErrInvalidTerraformVersion`
  - `project.ValidateTerraformVersion(raw string) (string, error)`
  - `(*project.Project).SetTerraformVersion(raw string) error`
  - `project.Snapshot.TerraformVersion string`
  - `projectapp.UpdateInput.TerraformVersion string`
  - `(*projectapp.Service).SetTerraformVersion(ctx context.Context, id project.ID, version string) error`

- [ ] **Step 1: Write the failing domain and application tests**

Add `TestTerraformVersion` to `internal/domain/project/project_test.go`:

```go
func TestTerraformVersion(t *testing.T) {
	p := sample(t)
	if p.Snapshot().TerraformVersion != DefaultTerraformVersion {
		t.Fatalf("default version = %q, want %q", p.Snapshot().TerraformVersion, DefaultTerraformVersion)
	}
	if err := p.SetTerraformVersion("1.10.5"); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot().TerraformVersion != "1.10.5" {
		t.Fatalf("version = %q, want 1.10.5", p.Snapshot().TerraformVersion)
	}
	if err := p.SetTerraformVersion(""); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot().TerraformVersion != DefaultTerraformVersion {
		t.Fatalf("empty reset = %q, want %q", p.Snapshot().TerraformVersion, DefaultTerraformVersion)
	}
	for _, bad := range []string{"latest", "1.9", "v1.9.8", "../1.9.8", "1.9.8-beta1", "1.9.8;id"} {
		if err := p.SetTerraformVersion(bad); err == nil {
			t.Fatalf("accepted invalid version %q", bad)
		}
	}
}
```

Add version assertions to `TestCreateUpdateAndForceUnlock` in `internal/application/project/service_test.go`:

```go
	if snap.TerraformVersion != project.DefaultTerraformVersion {
		t.Fatalf("default terraform version = %q", snap.TerraformVersion)
	}
	updated, err := svc.Update(ctx, UpdateInput{ID: "p1", TerraformVersion: "1.10.5"})
	if err != nil || updated.Snapshot().StateToken != snap.StateToken {
		t.Fatal(err)
	}
	if updated.Snapshot().TerraformVersion != "1.10.5" {
		t.Fatalf("updated terraform version = %q, want 1.10.5", updated.Snapshot().TerraformVersion)
	}
	if _, err := svc.Update(ctx, UpdateInput{ID: "p1", TerraformVersion: "bad-ver"}); err == nil {
		t.Fatal("expected invalid terraform version error")
	}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/domain/project/... ./internal/application/project/...`
Expected: FAIL with `DefaultTerraformVersion` and `SetTerraformVersion` undefined.

- [ ] **Step 3: Implement `TerraformVersion` in domain and application**

Update `internal/domain/project/project.go`:

```go
const DefaultTerraformVersion = "1.9.8"

var (
	ErrBusy                     = errors.New("another run is active")
	ErrActiveRunMismatch        = errors.New("active run does not match")
	ErrInvalidTerraformVersion  = errors.New("invalid terraform version")
)
```

Add `TerraformVersion string` to `Snapshot` in `internal/domain/project/project.go`, initialize `TerraformVersion: DefaultTerraformVersion` inside `New`, and add:

```go
func ValidateTerraformVersion(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return DefaultTerraformVersion, nil
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return "", ErrInvalidTerraformVersion
	}
	for _, part := range parts {
		if part == "" {
			return "", ErrInvalidTerraformVersion
		}
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return "", ErrInvalidTerraformVersion
			}
		}
	}
	return v, nil
}

func (p *Project) SetTerraformVersion(raw string) error {
	v, err := ValidateTerraformVersion(raw)
	if err != nil {
		return err
	}
	p.s.TerraformVersion = v
	return nil
}
```

Update `internal/application/project/service.go`:
- Add `TerraformVersion string` to `UpdateInput`.
- In `Update`, if `in.TerraformVersion != ""` call `if err := p.SetTerraformVersion(in.TerraformVersion); err != nil { return nil, err }`.
- Add `SetTerraformVersion(ctx context.Context, id project.ID, version string) error` on `*Service` so callers can set a non-default version after `Create` or independently:

```go
func (s *Service) SetTerraformVersion(ctx context.Context, id project.ID, version string) error {
	p, err := s.Projects.Get(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("project %s not found", id)
	}
	if err := p.SetTerraformVersion(version); err != nil {
		return err
	}
	return s.Projects.Save(ctx, p)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/domain/project/... ./internal/application/project/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/domain/project/project.go internal/domain/project/project_test.go internal/application/project/service.go internal/application/project/service_test.go
git commit -m "Add TerraformVersion to project domain and service."
```

---

### Task 2: Context Domain (`internal/domain/tfcontext`) & AES-256-GCM Sealer (`internal/infrastructure/crypto`)

**Files:**
- Create: `internal/domain/tfcontext/context.go`
- Create: `internal/domain/tfcontext/context_test.go`
- Create: `internal/infrastructure/crypto/seal.go`
- Create: `internal/infrastructure/crypto/seal_test.go`

**Interfaces:**
- Produces:
  - `tfcontext.ID`, `tfcontext.EntryKind` (`KindPlainEnv`, `KindSecretEnv`, `KindSecretFile`)
  - `tfcontext.Entry`, `tfcontext.Snapshot`, `tfcontext.Context`, `tfcontext.Repository`
  - `tfcontext.New(id ID, tenantID, name string, now time.Time) (*Context, error)`
  - `tfcontext.Restore(s Snapshot) *Context`
  - `tfcontext.ValidateEnvKey(raw string) (string, error)`
  - `tfcontext.ValidateFilePath(raw string) (string, error)`
  - `tfcontext.NewEntry(id string, kind EntryKind, key, value string, now time.Time) (Entry, error)`
  - `tfcontext.Resolve(contexts []*Context) (env map[string]string, files map[string][]byte)`
  - `crypto.Sealer`, `crypto.NewSealer(secret []byte) (*Sealer, error)`
  - `(*crypto.Sealer).Seal(plaintext []byte) ([]byte, error)`
  - `(*crypto.Sealer).Open(sealed []byte) ([]byte, error)`

- [ ] **Step 1: Write the failing unit tests for `tfcontext` and `crypto`**

Create `internal/domain/tfcontext/context_test.go`:

```go
package tfcontext

import (
	"testing"
	"time"
)

func TestValidateEnvKey(t *testing.T) {
	key, err := ValidateEnvKey("  TF_VAR_region ")
	if err != nil || key != "TF_VAR_region" {
		t.Fatalf("got %q, err %v", key, err)
	}
	for _, bad := range []string{
		"", "123_VAR", "FOO-BAR", "FOO=BAR",
		"PATH", "HOME", "USER", "TF_IN_AUTOMATION", "TF_INPUT",
		"DATABASE_URL", "POSTGRES_PASSWORD", "GITHUB_APP_PRIVATE_KEY",
		"CONTEXT_ENCRYPTION_KEY", "TF_LOG", "GIT_TRACE",
	} {
		if _, err := ValidateEnvKey(bad); err == nil {
			t.Fatalf("accepted invalid or reserved env key %q", bad)
		}
	}
}

func TestValidateFilePath(t *testing.T) {
	got, err := ValidateFilePath("~/.kube/config")
	if err != nil || got != ".kube/config" {
		t.Fatalf("got %q, err %v", got, err)
	}
	got2, err := ValidateFilePath("  .aws/credentials ")
	if err != nil || got2 != ".aws/credentials" {
		t.Fatalf("got %q, err %v", got2, err)
	}
	for _, bad := range []string{
		"", "/", "/etc/passwd", "../secret", ".kube/../../etc/passwd",
		"~", "~/", ".", "..", "foo\\bar", "a\x00b",
	} {
		if _, err := ValidateFilePath(bad); err == nil {
			t.Fatalf("accepted unsafe file path %q", bad)
		}
	}
}

func TestResolvePrecedence(t *testing.T) {
	now := time.Unix(10, 0).UTC()
	c1, err := New("c1", "ten-1", "base", now)
	if err != nil {
		t.Fatal(err)
	}
	e1, _ := NewEntry("e1", KindPlainEnv, "TF_VAR_env", "dev", now)
	e2, _ := NewEntry("e2", KindSecretEnv, "AWS_SECRET_ACCESS_KEY", "old-secret", now)
	e3, _ := NewEntry("e3", KindSecretFile, "~/.kube/config", "cluster-a", now)
	c1.SetEntries([]Entry{e1, e2, e3})

	c2, err := New("c2", "ten-1", "prod-override", now)
	if err != nil {
		t.Fatal(err)
	}
	e4, _ := NewEntry("e4", KindPlainEnv, "TF_VAR_env", "prod", now)
	e5, _ := NewEntry("e5", KindSecretFile, ".kube/config", "cluster-b", now)
	c2.SetEntries([]Entry{e4, e5})

	env, files := Resolve([]*Context{c1, c2})
	if env["TF_VAR_env"] != "prod" {
		t.Fatalf("TF_VAR_env = %q, want prod", env["TF_VAR_env"])
	}
	if env["AWS_SECRET_ACCESS_KEY"] != "old-secret" {
		t.Fatalf("AWS_SECRET_ACCESS_KEY = %q, want old-secret", env["AWS_SECRET_ACCESS_KEY"])
	}
	if string(files[".kube/config"]) != "cluster-b" {
		t.Fatalf(".kube/config = %q, want cluster-b", string(files[".kube/config"]))
	}
}
```

Create `internal/infrastructure/crypto/seal_test.go`:

```go
package crypto

import (
	"bytes"
	"testing"
)

func TestSealerRoundTripAndTamper(t *testing.T) {
	if _, err := NewSealer(nil); err == nil {
		t.Fatal("expected error for empty secret key material")
	}
	s, err := NewSealer([]byte("test-key-material"))
	if err != nil {
		t.Fatal(err)
	}
	ct1, err := s.Seal([]byte("super-secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	ct2, err := s.Seal([]byte("super-secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ct1, ct2) {
		t.Fatal("identical plaintexts produced identical ciphertexts (nonce reuse)")
	}
	pt, err := s.Open(ct1)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != "super-secret-value" {
		t.Fatalf("opened %q, want super-secret-value", string(pt))
	}
	ct1[len(ct1)-1] ^= 0xff
	if _, err := s.Open(ct1); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/domain/tfcontext/... ./internal/infrastructure/crypto/...`
Expected: FAIL because `context.go` and `seal.go` do not exist yet.

- [ ] **Step 3: Implement `internal/domain/tfcontext/context.go` and `internal/infrastructure/crypto/seal.go`**

Create `internal/domain/tfcontext/context.go`:

```go
package tfcontext

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"infra-guy/internal/domain/project"
)

type ID string

type EntryKind string

const (
	KindPlainEnv   EntryKind = "plain_env"
	KindSecretEnv  EntryKind = "secret_env"
	KindSecretFile EntryKind = "secret_file"
)

var (
	ErrInvalidEnvKey   = errors.New("invalid or reserved environment variable name")
	ErrInvalidFilePath = errors.New("invalid relative file path in $HOME")
	ErrInvalidKind     = errors.New("invalid context entry kind")
)

var reservedEnvKeys = map[string]bool{
	"PATH":                        true,
	"HOME":                        true,
	"USER":                        true,
	"TF_IN_AUTOMATION":            true,
	"TF_INPUT":                    true,
	"DATABASE_URL":                true,
	"POSTGRES_PASSWORD":           true,
	"BASIC_AUTH_USER":             true,
	"BASIC_AUTH_PASSWORD":         true,
	"GITHUB_APP_ID":               true,
	"GITHUB_APP_PRIVATE_KEY":      true,
	"GITHUB_APP_PRIVATE_KEY_FILE": true,
	"GITHUB_APP_WEBHOOK_SECRET":   true,
	"GITHUB_APP_CLIENT_ID":        true,
	"GITHUB_APP_CLIENT_SECRET":    true,
	"CONTEXT_ENCRYPTION_KEY":      true,
	"TF_LOG":                      true,
	"GIT_TRACE":                   true,
}

type Entry struct {
	ID        string
	Kind      EntryKind
	Key       string
	Value     string
	Secret    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Snapshot struct {
	ID        ID
	TenantID  string
	Name      string
	Entries   []Entry
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Context struct{ s Snapshot }

func New(id ID, tenantID, name string, now time.Time) (*Context, error) {
	name = strings.TrimSpace(name)
	if id == "" || strings.TrimSpace(tenantID) == "" || name == "" {
		return nil, fmt.Errorf("context requires id, tenant_id, and name")
	}
	return &Context{s: Snapshot{
		ID: id, TenantID: tenantID, Name: name, CreatedAt: now, UpdatedAt: now,
	}}, nil
}

func Restore(s Snapshot) *Context {
	cp := s
	cp.Entries = append([]Entry(nil), s.Entries...)
	return &Context{s: cp}
}

func (c *Context) Snapshot() Snapshot {
	cp := c.s
	cp.Entries = append([]Entry(nil), c.s.Entries...)
	return cp
}

func (c *Context) SetEntries(entries []Entry) {
	c.s.Entries = append([]Entry(nil), entries...)
}

func ValidateEnvKey(raw string) (string, error) {
	k := strings.TrimSpace(raw)
	if k == "" || reservedEnvKeys[k] {
		return "", ErrInvalidEnvKey
	}
	for i := 0; i < len(k); i++ {
		ch := k[i]
		isAlpha := (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || ch == '_'
		isDigit := ch >= '0' && ch <= '9'
		if i == 0 && !isAlpha {
			return "", ErrInvalidEnvKey
		}
		if !isAlpha && !isDigit {
			return "", ErrInvalidEnvKey
		}
	}
	return k, nil
}

func ValidateFilePath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	p = strings.TrimPrefix(p, "~/")
	if p == "" || p == "~" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return "", ErrInvalidFilePath
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", ErrInvalidFilePath
		}
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", ErrInvalidFilePath
	}
	return clean, nil
}

func NewEntry(id string, kind EntryKind, key, value string, now time.Time) (Entry, error) {
	if id == "" {
		return Entry{}, fmt.Errorf("entry id is required")
	}
	var normKey string
	var err error
	var secret bool
	switch kind {
	case KindPlainEnv:
		normKey, err = ValidateEnvKey(key)
	case KindSecretEnv:
		normKey, err = ValidateEnvKey(key)
		secret = true
	case KindSecretFile:
		normKey, err = ValidateFilePath(key)
		secret = true
	default:
		return Entry{}, ErrInvalidKind
	}
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		ID: id, Kind: kind, Key: normKey, Value: value, Secret: secret,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

func Resolve(contexts []*Context) (map[string]string, map[string][]byte) {
	env := make(map[string]string)
	files := make(map[string][]byte)
	for _, c := range contexts {
		if c == nil {
			continue
		}
		for _, e := range c.s.Entries {
			switch e.Kind {
			case KindPlainEnv, KindSecretEnv:
				env[e.Key] = e.Value
			case KindSecretFile:
				files[e.Key] = []byte(e.Value)
			}
		}
	}
	return env, files
}

type Repository interface {
	Create(ctx context.Context, c *Context) error
	Get(ctx context.Context, id ID, decryptSecrets bool) (*Context, error)
	ListByTenant(ctx context.Context, tenantID string) ([]*Context, error)
	Delete(ctx context.Context, id ID) error
	UpsertEntry(ctx context.Context, contextID ID, entry Entry) error
	DeleteEntry(ctx context.Context, contextID ID, entryID string) error
	SetProjectContexts(ctx context.Context, projectID project.ID, contextIDs []ID) error
	ListByProject(ctx context.Context, projectID project.ID, decryptSecrets bool) ([]*Context, error)
}
```

Create `internal/infrastructure/crypto/seal.go`:

```go
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
)

type Sealer struct {
	key [32]byte
}

func NewSealer(secretMaterial []byte) (*Sealer, error) {
	if len(secretMaterial) == 0 {
		return nil, errors.New("crypto: secret material cannot be empty")
	}
	return &Sealer{key: sha256.Sum256(secretMaterial)}, nil
}

func (s *Sealer) Seal(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func (s *Sealer) Open(sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("crypto: ciphertext too short")
	}
	return gcm.Open(nil, sealed[:ns], sealed[ns:], nil)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/domain/tfcontext/... ./internal/infrastructure/crypto/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/domain/tfcontext/ internal/infrastructure/crypto/
git commit -m "Add tfcontext domain model and AES-256-GCM sealer."
```

---

### Task 3: Postgres Migration (`004_tf_versions_and_contexts.sql`), `ProjectRepo`, and `ContextRepo`

**Files:**
- Create: `internal/infrastructure/postgres/migrations/004_tf_versions_and_contexts.sql`
- Modify: `internal/infrastructure/postgres/project_repo.go`
- Create: `internal/infrastructure/postgres/context_repo.go`
- Modify: `internal/infrastructure/postgres/repos_test.go`

**Interfaces:**
- Consumes: `project.Snapshot.TerraformVersion`, `tfcontext.*`, `*crypto.Sealer`
- Produces:
  - `postgres.ContextRepo` implementing `tfcontext.Repository`
  - `postgres.NewContextRepo(pool *pgxpool.Pool, sealer *crypto.Sealer) *ContextRepo`
  - `postgres.ProjectRepo` persisting `terraform_version`

- [ ] **Step 1: Write failing Postgres integration tests**

In `internal/infrastructure/postgres/repos_test.go`:
1. Add imports `"infra-guy/internal/domain/tfcontext"` and `"infra-guy/internal/infrastructure/crypto"`.
2. In `TestProjectRunAndStateRoundTrip`, set `if err := p.SetTerraformVersion("1.10.5"); err != nil { t.Fatal(err) }` before `projects.Save(ctx, p)` and verify `loadedProj.Snapshot().TerraformVersion == "1.10.5"`.
3. Add `TestContextRepoCRUDAndEncryption`:

```go
func TestContextRepoCRUDAndEncryption(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	sealer, err := crypto.NewSealer([]byte("test-encryption-key"))
	if err != nil {
		t.Fatal(err)
	}
	repo := NewContextRepo(pool, sealer)
	projects := NewProjectRepo(pool)

	now := time.Unix(1_700_000_000, 0).UTC()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantID := "ten-ctx-" + suffix
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, github_account_id, account_type, created_at, updated_at)
		VALUES ($1, 'acme', $2, 'organization', now(), now())`, tenantID, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}

	c1, err := tfcontext.New(tfcontext.ID("ctx-1-"+suffix), tenantID, "base", now)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := tfcontext.New(tfcontext.ID("ctx-2-"+suffix), tenantID, "override", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, c1); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, c2); err != nil {
		t.Fatal(err)
	}

	ePlain, _ := tfcontext.NewEntry("e1-"+suffix, tfcontext.KindPlainEnv, "TF_VAR_env", "staging", now)
	eSec, _ := tfcontext.NewEntry("e2-"+suffix, tfcontext.KindSecretEnv, "AWS_SECRET_ACCESS_KEY", "s3cr3t", now)
	eFile, _ := tfcontext.NewEntry("e3-"+suffix, tfcontext.KindSecretFile, "~/.kube/config", "kube-yaml-1", now)
	for _, e := range []tfcontext.Entry{ePlain, eSec, eFile} {
		if err := repo.UpsertEntry(ctx, c1.Snapshot().ID, e); err != nil {
			t.Fatal(err)
		}
	}
	eOverride, _ := tfcontext.NewEntry("e4-"+suffix, tfcontext.KindPlainEnv, "TF_VAR_env", "production", now)
	if err := repo.UpsertEntry(ctx, c2.Snapshot().ID, eOverride); err != nil {
		t.Fatal(err)
	}

	// Verify secret is not stored in value_text in raw SQL
	var rawText string
	var rawCipher []byte
	if err := pool.QueryRow(ctx, `SELECT value_text, value_ciphertext FROM context_entries WHERE id = $1`, eSec.ID).Scan(&rawText, &rawCipher); err != nil {
		t.Fatal(err)
	}
	if rawText != "" || len(rawCipher) == 0 {
		t.Fatalf("secret stored in plaintext or missing ciphertext: text=%q cipherLen=%d", rawText, len(rawCipher))
	}

	// Get with decryptSecrets=false masks secret values
	masked, err := repo.Get(ctx, c1.Snapshot().ID, false)
	if err != nil || masked == nil {
		t.Fatalf("get masked: %v", err)
	}
	for _, e := range masked.Snapshot().Entries {
		if e.Secret && e.Value != "" {
			t.Fatalf("expected masked secret to have empty value, got %q", e.Value)
		}
		if !e.Secret && e.Value != "staging" {
			t.Fatalf("plain value = %q, want staging", e.Value)
		}
	}

	// Attach contexts in order [c1, c2] to a project and resolve decrypted
	srcRepo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New(project.ID("proj-ctx-"+suffix), "infra", srcRepo, now)
	_ = p.SetPlacement(tenantID, 100)
	p.SetStateToken("tok")
	if err := projects.Save(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetProjectContexts(ctx, p.Snapshot().ID, []tfcontext.ID{c1.Snapshot().ID, c2.Snapshot().ID}); err != nil {
		t.Fatal(err)
	}
	attached, err := repo.ListByProject(ctx, p.Snapshot().ID, true)
	if err != nil || len(attached) != 2 {
		t.Fatalf("ListByProject: %v len=%d", err, len(attached))
	}
	env, files := tfcontext.Resolve(attached)
	if env["TF_VAR_env"] != "production" || env["AWS_SECRET_ACCESS_KEY"] != "s3cr3t" || string(files[".kube/config"]) != "kube-yaml-1" {
		t.Fatalf("resolved env=%v files=%v", env, files)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/infrastructure/postgres/...`
Expected: FAIL with `NewContextRepo` undefined.

- [ ] **Step 3: Create migration `004_tf_versions_and_contexts.sql`, update `ProjectRepo`, and create `ContextRepo`**

Create `internal/infrastructure/postgres/migrations/004_tf_versions_and_contexts.sql`:

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

Update `internal/infrastructure/postgres/project_repo.go` to include `terraform_version`:
- Add `terraform_version` to `projectColumns`:
  ```go
  const projectColumns = `id, tenant_id, github_repo_id, name, repo_owner, repo_name, default_branch, working_directory,
  	terraform_version, auto_approve, locked, state_token, active_run_id, created_at, updated_at`
  ```
- In `Save`, pass `firstNonEmptyVersion(s.TerraformVersion)` (`VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)` and `terraform_version = EXCLUDED.terraform_version`).
- In `scanProject`, scan `&s.TerraformVersion` after `&s.WorkingDirectory`.

Create `internal/infrastructure/postgres/context_repo.go`:

```go
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/tfcontext"
	"infra-guy/internal/infrastructure/crypto"
)

var _ tfcontext.Repository = (*ContextRepo)(nil)

type ContextRepo struct {
	pool   *pgxpool.Pool
	sealer *crypto.Sealer
}

func NewContextRepo(pool *pgxpool.Pool, sealer *crypto.Sealer) *ContextRepo {
	return &ContextRepo{pool: pool, sealer: sealer}
}

func (r *ContextRepo) Create(ctx context.Context, c *tfcontext.Context) error {
	s := c.Snapshot()
	_, err := r.pool.Exec(ctx, `
		INSERT INTO contexts (id, tenant_id, name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)`,
		s.ID, s.TenantID, s.Name, s.CreatedAt, s.UpdatedAt,
	)
	return err
}

func (r *ContextRepo) Get(ctx context.Context, id tfcontext.ID, decryptSecrets bool) (*tfcontext.Context, error) {
	var s tfcontext.Snapshot
	var created, updated time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, created_at, updated_at
		FROM contexts WHERE id = $1`, id,
	).Scan(&s.ID, &s.TenantID, &s.Name, &created, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt = utcTime(created)
	s.UpdatedAt = utcTime(updated)
	entries, err := r.loadEntries(ctx, s.ID, decryptSecrets)
	if err != nil {
		return nil, err
	}
	s.Entries = entries
	return tfcontext.Restore(s), nil
}

func (r *ContextRepo) ListByTenant(ctx context.Context, tenantID string) ([]*tfcontext.Context, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, name, created_at, updated_at
		FROM contexts WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tfcontext.Context
	for rows.Next() {
		var s tfcontext.Snapshot
		var created, updated time.Time
		if err := rows.Scan(&s.ID, &s.TenantID, &s.Name, &created, &updated); err != nil {
			return nil, err
		}
		s.CreatedAt = utcTime(created)
		s.UpdatedAt = utcTime(updated)
		out = append(out, tfcontext.Restore(s))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range out {
		entries, err := r.loadEntries(ctx, c.Snapshot().ID, false)
		if err != nil {
			return nil, err
		}
		c.SetEntries(entries)
	}
	return out, nil
}

func (r *ContextRepo) Delete(ctx context.Context, id tfcontext.ID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM contexts WHERE id = $1`, id)
	return err
}

func (r *ContextRepo) UpsertEntry(ctx context.Context, contextID tfcontext.ID, e tfcontext.Entry) error {
	var textVal string
	var cipherVal []byte
	if e.Secret {
		ct, err := r.sealer.Seal([]byte(e.Value))
		if err != nil {
			return err
		}
		cipherVal = ct
	} else {
		textVal = e.Value
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO context_entries (id, context_id, kind, key, value_text, value_ciphertext, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (context_id, kind, key) DO UPDATE SET
			value_text = EXCLUDED.value_text,
			value_ciphertext = EXCLUDED.value_ciphertext,
			updated_at = EXCLUDED.updated_at`,
		e.ID, contextID, e.Kind, e.Key, textVal, cipherVal, e.CreatedAt, e.UpdatedAt,
	)
	return err
}

func (r *ContextRepo) DeleteEntry(ctx context.Context, contextID tfcontext.ID, entryID string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM context_entries WHERE context_id = $1 AND id = $2`, contextID, entryID)
	return err
}

func (r *ContextRepo) SetProjectContexts(ctx context.Context, projectID project.ID, contextIDs []tfcontext.ID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM project_contexts WHERE project_id = $1`, projectID); err != nil {
		return err
	}
	seen := make(map[tfcontext.ID]bool, len(contextIDs))
	pos := 0
	for _, cid := range contextIDs {
		if cid == "" || seen[cid] {
			continue
		}
		seen[cid] = true
		if _, err := tx.Exec(ctx, `
			INSERT INTO project_contexts (project_id, context_id, position)
			VALUES ($1, $2, $3)`, projectID, cid, pos); err != nil {
			return err
		}
		pos++
	}
	return tx.Commit(ctx)
}

func (r *ContextRepo) ListByProject(ctx context.Context, projectID project.ID, decryptSecrets bool) ([]*tfcontext.Context, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT c.id, c.tenant_id, c.name, c.created_at, c.updated_at
		FROM project_contexts pc
		JOIN contexts c ON c.id = pc.context_id
		WHERE pc.project_id = $1
		ORDER BY pc.position ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tfcontext.Context
	for rows.Next() {
		var s tfcontext.Snapshot
		var created, updated time.Time
		if err := rows.Scan(&s.ID, &s.TenantID, &s.Name, &created, &updated); err != nil {
			return nil, err
		}
		s.CreatedAt = utcTime(created)
		s.UpdatedAt = utcTime(updated)
		out = append(out, tfcontext.Restore(s))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range out {
		entries, err := r.loadEntries(ctx, c.Snapshot().ID, decryptSecrets)
		if err != nil {
			return nil, err
		}
		c.SetEntries(entries)
	}
	return out, nil
}

func (r *ContextRepo) loadEntries(ctx context.Context, contextID tfcontext.ID, decryptSecrets bool) ([]tfcontext.Entry, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, kind, key, value_text, value_ciphertext, created_at, updated_at
		FROM context_entries WHERE context_id = $1 ORDER BY created_at, id`, contextID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tfcontext.Entry
	for rows.Next() {
		var e tfcontext.Entry
		var textVal string
		var cipherVal []byte
		var created, updated time.Time
		if err := rows.Scan(&e.ID, &e.Kind, &e.Key, &textVal, &cipherVal, &created, &updated); err != nil {
			return nil, err
		}
		e.CreatedAt = utcTime(created)
		e.UpdatedAt = utcTime(updated)
		e.Secret = e.Kind == tfcontext.KindSecretEnv || e.Kind == tfcontext.KindSecretFile
		if e.Secret {
			if decryptSecrets && len(cipherVal) > 0 {
				pt, err := r.sealer.Open(cipherVal)
				if err != nil {
					return nil, err
				}
				e.Value = string(pt)
			}
		} else {
			e.Value = textVal
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run Postgres tests to verify they pass**

Run: `go test ./internal/infrastructure/postgres/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/infrastructure/postgres/
git commit -m "Add migration 004, ProjectRepo terraform_version, and ContextRepo."
```

---

### Task 4: Context Application Service (`internal/application/tfcontext`)

**Files:**
- Create: `internal/application/tfcontext/service.go`
- Create: `internal/application/tfcontext/service_test.go`

**Interfaces:**
- Consumes: `tfcontext.Repository`, `project.ID`
- Produces:
  - `tfcontextapp.Service` (`Contexts tfcontext.Repository`, `Now func() time.Time`, `NewID func() string`)
  - `(*Service).List(ctx, tenantID) ([]*tfcontext.Context, error)`
  - `(*Service).Get(ctx, tenantID string, id tfcontext.ID) (*tfcontext.Context, error)`
  - `(*Service).Create(ctx, tenantID, name string) (*tfcontext.Context, error)`
  - `(*Service).Delete(ctx, tenantID string, id tfcontext.ID) error`
  - `(*Service).UpsertEntry(ctx, tenantID string, contextID tfcontext.ID, kind tfcontext.EntryKind, key, value string) error`
  - `(*Service).DeleteEntry(ctx, tenantID string, contextID tfcontext.ID, entryID string) error`
  - `(*Service).SetProjectContexts(ctx, tenantID string, projectID project.ID, contextIDs []tfcontext.ID) error`
  - `(*Service).ListByProject(ctx, projectID project.ID) ([]*tfcontext.Context, error)`
  - `(*Service).ResolveForProject(ctx, projectID project.ID) (map[string]string, map[string][]byte, error)`

- [ ] **Step 1: Write failing unit tests for `tfcontextapp.Service`**

Create `internal/application/tfcontext/service_test.go`:

```go
package tfcontextapp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/tfcontext"
)

type memRepo struct {
	items    map[tfcontext.ID]*tfcontext.Context
	attached map[project.ID][]tfcontext.ID
}

func newMemRepo() *memRepo {
	return &memRepo{
		items:    map[tfcontext.ID]*tfcontext.Context{},
		attached: map[project.ID][]tfcontext.ID{},
	}
}

func (m *memRepo) Create(_ context.Context, c *tfcontext.Context) error {
	m.items[c.Snapshot().ID] = tfcontext.Restore(c.Snapshot())
	return nil
}

func (m *memRepo) Get(_ context.Context, id tfcontext.ID, decryptSecrets bool) (*tfcontext.Context, error) {
	c, ok := m.items[id]
	if !ok {
		return nil, nil
	}
	s := c.Snapshot()
	if !decryptSecrets {
		for i := range s.Entries {
			if s.Entries[i].Secret {
				s.Entries[i].Value = ""
			}
		}
	}
	return tfcontext.Restore(s), nil
}

func (m *memRepo) ListByTenant(_ context.Context, tenantID string) ([]*tfcontext.Context, error) {
	var out []*tfcontext.Context
	for _, c := range m.items {
		if c.Snapshot().TenantID == tenantID {
			cp, _ := m.Get(context.Background(), c.Snapshot().ID, false)
			out = append(out, cp)
		}
	}
	return out, nil
}

func (m *memRepo) Delete(_ context.Context, id tfcontext.ID) error {
	delete(m.items, id)
	return nil
}

func (m *memRepo) UpsertEntry(_ context.Context, contextID tfcontext.ID, entry tfcontext.Entry) error {
	c := m.items[contextID]
	s := c.Snapshot()
	replaced := false
	for i := range s.Entries {
		if s.Entries[i].Kind == entry.Kind && s.Entries[i].Key == entry.Key {
			s.Entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		s.Entries = append(s.Entries, entry)
	}
	m.items[contextID] = tfcontext.Restore(s)
	return nil
}

func (m *memRepo) DeleteEntry(_ context.Context, contextID tfcontext.ID, entryID string) error {
	c := m.items[contextID]
	s := c.Snapshot()
	var kept []tfcontext.Entry
	for _, e := range s.Entries {
		if e.ID != entryID {
			kept = append(kept, e)
		}
	}
	s.Entries = kept
	m.items[contextID] = tfcontext.Restore(s)
	return nil
}

func (m *memRepo) SetProjectContexts(_ context.Context, projectID project.ID, contextIDs []tfcontext.ID) error {
	m.attached[projectID] = append([]tfcontext.ID(nil), contextIDs...)
	return nil
}

func (m *memRepo) ListByProject(_ context.Context, projectID project.ID, decryptSecrets bool) ([]*tfcontext.Context, error) {
	var out []*tfcontext.Context
	for _, id := range m.attached[projectID] {
		c, _ := m.Get(context.Background(), id, decryptSecrets)
		if c != nil {
			out = append(out, c)
		}
	}
	return out, nil
}

func TestServiceTenantIsolationAndResolution(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	seq := 0
	svc := &Service{
		Contexts: repo,
		Now:      func() time.Time { return time.Unix(100, 0).UTC() },
		NewID: func() string {
			seq++
			return fmt.Sprintf("id-%d", seq)
		},
	}

	c1, err := svc.Create(ctx, "ten-1", "aws-creds")
	if err != nil {
		t.Fatal(err)
	}
	// Other tenant cannot access or modify c1
	if got, err := svc.Get(ctx, "ten-2", c1.Snapshot().ID); err != nil || got != nil {
		t.Fatalf("cross-tenant Get returned %v, %v", got, err)
	}
	if err := svc.UpsertEntry(ctx, "ten-2", c1.Snapshot().ID, tfcontext.KindPlainEnv, "FOO", "bar"); err == nil {
		t.Fatal("cross-tenant UpsertEntry succeeded")
	}

	// Valid entries on ten-1
	if err := svc.UpsertEntry(ctx, "ten-1", c1.Snapshot().ID, tfcontext.KindSecretEnv, "AWS_SECRET_ACCESS_KEY", "sec-123"); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpsertEntry(ctx, "ten-1", c1.Snapshot().ID, tfcontext.KindSecretFile, "~/.kube/config", "k8s-cfg"); err != nil {
		t.Fatal(err)
	}

	// Attaching context from another tenant fails
	if err := svc.SetProjectContexts(ctx, "ten-2", "proj-1", []tfcontext.ID{c1.Snapshot().ID}); err == nil {
		t.Fatal("cross-tenant SetProjectContexts succeeded")
	}
	if err := svc.SetProjectContexts(ctx, "ten-1", "proj-1", []tfcontext.ID{c1.Snapshot().ID}); err != nil {
		t.Fatal(err)
	}

	env, files, err := svc.ResolveForProject(ctx, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if env["AWS_SECRET_ACCESS_KEY"] != "sec-123" || string(files[".kube/config"]) != "k8s-cfg" {
		t.Fatalf("unexpected resolved env=%v files=%v", env, files)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/application/tfcontext/...`
Expected: FAIL because `service.go` does not exist yet.

- [ ] **Step 3: Implement `internal/application/tfcontext/service.go`**

Create `internal/application/tfcontext/service.go`:

```go
package tfcontextapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/tfcontext"
)

var ErrNotFound = errors.New("context not found")

type Service struct {
	Contexts tfcontext.Repository
	Now      func() time.Time
	NewID    func() string
}

func (s *Service) List(ctx context.Context, tenantID string) ([]*tfcontext.Context, error) {
	return s.Contexts.ListByTenant(ctx, tenantID)
}

func (s *Service) Get(ctx context.Context, tenantID string, id tfcontext.ID) (*tfcontext.Context, error) {
	c, err := s.Contexts.Get(ctx, id, false)
	if err != nil || c == nil {
		return nil, err
	}
	if c.Snapshot().TenantID != tenantID {
		return nil, nil
	}
	return c, nil
}

func (s *Service) Create(ctx context.Context, tenantID, name string) (*tfcontext.Context, error) {
	c, err := tfcontext.New(tfcontext.ID(s.NewID()), tenantID, name, s.Now())
	if err != nil {
		return nil, err
	}
	if err := s.Contexts.Create(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Service) Delete(ctx context.Context, tenantID string, id tfcontext.ID) error {
	c, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrNotFound
	}
	return s.Contexts.Delete(ctx, id)
}

func (s *Service) UpsertEntry(ctx context.Context, tenantID string, contextID tfcontext.ID, kind tfcontext.EntryKind, key, value string) error {
	c, err := s.Get(ctx, tenantID, contextID)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrNotFound
	}
	entry, err := tfcontext.NewEntry(s.NewID(), kind, key, value, s.Now())
	if err != nil {
		return err
	}
	return s.Contexts.UpsertEntry(ctx, contextID, entry)
}

func (s *Service) DeleteEntry(ctx context.Context, tenantID string, contextID tfcontext.ID, entryID string) error {
	c, err := s.Get(ctx, tenantID, contextID)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrNotFound
	}
	return s.Contexts.DeleteEntry(ctx, contextID, entryID)
}

func (s *Service) SetProjectContexts(ctx context.Context, tenantID string, projectID project.ID, contextIDs []tfcontext.ID) error {
	for _, cid := range contextIDs {
		if cid == "" {
			continue
		}
		c, err := s.Get(ctx, tenantID, cid)
		if err != nil {
			return err
		}
		if c == nil {
			return fmt.Errorf("context %s not found", cid)
		}
	}
	return s.Contexts.SetProjectContexts(ctx, projectID, contextIDs)
}

func (s *Service) ListByProject(ctx context.Context, projectID project.ID) ([]*tfcontext.Context, error) {
	return s.Contexts.ListByProject(ctx, projectID, false)
}

func (s *Service) ResolveForProject(ctx context.Context, projectID project.ID) (map[string]string, map[string][]byte, error) {
	contexts, err := s.Contexts.ListByProject(ctx, projectID, true)
	if err != nil {
		return nil, nil, err
	}
	env, files := tfcontext.Resolve(contexts)
	return env, files, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/application/tfcontext/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/application/tfcontext/
git commit -m "Add tfcontext application service."
```

---

### Task 5: Terraform `VersionCatalog`, On-Demand `Installer`, and `Executor` (`$HOME` + Context Injection)

**Files:**
- Create: `internal/infrastructure/terraform/versions.go`
- Create: `internal/infrastructure/terraform/installer.go`
- Modify: `internal/infrastructure/terraform/terraform.go`
- Modify: `internal/infrastructure/terraform/worker_executor.go`
- Modify: `internal/infrastructure/terraform/terraform_test.go`
- Modify: `internal/application/worker/worker.go`

**Interfaces:**
- Consumes: `project.ValidateTerraformVersion`, `project.DefaultTerraformVersion`
- Produces:
  - `terraform.FallbackVersions`
  - `terraform.VersionCatalog` (`URL string`, `Client *http.Client`, `TTL time.Duration`) with `(*VersionCatalog).List(ctx context.Context) []string`
  - `terraform.BinaryLocator` interface (`Ensure(ctx context.Context, version string) (string, error)`)
  - `terraform.Installer` (`CacheDir, BaseURL string`, `Client *http.Client`) implementing `BinaryLocator`
  - `terraform.Request` & `worker.Request` extended with `TerraformVersion string`, `Env map[string]string`, `Files map[string][]byte`
  - `terraform.Executor` (`Binary BinaryLocator`)

- [ ] **Step 1: Write failing tests for `VersionCatalog`, `Installer`, and `Executor` context/version injection**

Append to `internal/infrastructure/terraform/terraform_test.go`:

```go
func TestVersionCatalogParsesAndFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"versions":{"0.15.0":{},"1.9.8":{},"1.10.5":{},"1.11.0-beta1":{},"1.8.5":{}}}`))
	}))
	t.Cleanup(srv.Close)

	cat := &VersionCatalog{URL: srv.URL, Client: srv.Client(), TTL: time.Hour}
	got := cat.List(context.Background())
	if len(got) != 3 || got[0] != "1.10.5" || got[1] != "1.9.8" || got[2] != "1.8.5" {
		t.Fatalf("catalog got %v", got)
	}

	offline := &VersionCatalog{URL: "http://127.0.0.1:1"}
	fb := offline.List(context.Background())
	if len(fb) == 0 || fb[0] != FallbackVersions[0] {
		t.Fatalf("fallback got %v", fb)
	}
}

func TestInstallerDownloadsAndCachesBinary(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		fw, err := zw.Create("terraform")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte("#!/bin/sh\necho '{\"terraform_version\":\"1.8.5\"}'\n"))
		_ = zw.Close()
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)

	inst := &Installer{CacheDir: t.TempDir(), BaseURL: srv.URL, Client: srv.Client()}
	bin1, err := inst.Ensure(context.Background(), "1.8.5")
	if err != nil {
		t.Fatal(err)
	}
	bin2, err := inst.Ensure(context.Background(), "1.8.5")
	if err != nil {
		t.Fatal(err)
	}
	if bin1 != bin2 || hits != 1 {
		t.Fatalf("expected cached binary with 1 hit, got hits=%d bin1=%q bin2=%q", hits, bin1, bin2)
	}
}

func TestExecutorMaterializesHomeFilesAndContextEnv(t *testing.T) {
	dir := t.TempDir()
	fakeBin := filepath.Join(t.TempDir(), "fake-terraform")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"version\" ]; then echo '{\"terraform_version\":\"1.10.5\"}'; exit 0; fi\n" +
		"echo \"ENV_FOO=$TF_VAR_foo\"\n" +
		"echo \"HOME_DIR=$HOME\"\n" +
		"cat \"$HOME/.kube/config\"\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	e := Executor{Binary: staticBinary(fakeBin)}
	res, err := e.Run(context.Background(), Request{
		WorkDir:          dir,
		InternalBaseURL:  "http://127.0.0.1:8080",
		ProjectID:        "p1",
		RunID:            "r1",
		StateToken:       "tok",
		Step:             run.StepInit,
		TerraformVersion: "1.10.5",
		Env:              map[string]string{"TF_VAR_foo": "bar-val", "DATABASE_URL": "evil"},
		Files:            map[string][]byte{".kube/config": []byte("kubeconfig-payload\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, res.Logs)
	}
	if !strings.Contains(res.Logs, "ENV_FOO=bar-val") || !strings.Contains(res.Logs, "kubeconfig-payload") {
		t.Fatalf("missing injected env or file in logs: %s", res.Logs)
	}
	if res.Version != "1.10.5" {
		t.Fatalf("version = %q, want 1.10.5", res.Version)
	}
	info, err := os.Stat(filepath.Join(dir, ".home", ".kube", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode = %v, want 0600", info.Mode().Perm())
	}
}
```

(Also add `"archive/zip"` and `"bytes"` to imports in `internal/infrastructure/terraform/terraform_test.go`, and add helper `type staticBinary string` implementing `Ensure(context.Context, string) (string, error)`.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/infrastructure/terraform/...`
Expected: FAIL with `VersionCatalog`, `Installer`, and `staticBinary` undefined.

- [ ] **Step 3: Implement `versions.go`, `installer.go`, and update `terraform.go`, `worker_executor.go`, and `worker.go`**

Create `internal/infrastructure/terraform/versions.go`:

```go
package terraform

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"infra-guy/internal/domain/project"
)

var FallbackVersions = []string{
	"1.11.4",
	"1.10.5",
	"1.9.8",
	"1.8.5",
	"1.7.5",
	"1.6.6",
	"1.5.7",
}

type VersionCatalog struct {
	URL    string
	Client *http.Client
	TTL    time.Duration

	mu        sync.Mutex
	cached    []string
	expiresAt time.Time
}

func (c *VersionCatalog) List(ctx context.Context) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if len(c.cached) > 0 && now.Before(c.expiresAt) {
		return append([]string(nil), c.cached...)
	}
	versions, err := c.fetch(ctx)
	if err != nil || len(versions) == 0 {
		return append([]string(nil), FallbackVersions...)
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	c.cached = versions
	c.expiresAt = now.Add(ttl)
	return append([]string(nil), c.cached...)
}

func (c *VersionCatalog) fetch(ctx context.Context) ([]string, error) {
	u := c.URL
	if u == "" {
		u = "https://releases.hashicorp.com/terraform/index.json"
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var payload struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var parsed [][3]int
	var rawList []string
	for v := range payload.Versions {
		norm, err := project.ValidateTerraformVersion(v)
		if err != nil {
			continue
		}
		ver := parseSemver(norm)
		if ver[0] < 1 {
			continue
		}
		seen[norm] = true
		parsed = append(parsed, ver)
		rawList = append(rawList, norm)
	}
	if len(rawList) == 0 {
		return nil, nil
	}
	if !seen[project.DefaultTerraformVersion] {
		rawList = append(rawList, project.DefaultTerraformVersion)
	}
	sort.Slice(rawList, func(i, j int) bool {
		a, b := parseSemver(rawList[i]), parseSemver(rawList[j])
		if a[0] != b[0] {
			return a[0] > b[0]
		}
		if a[1] != b[1] {
			return a[1] > b[1]
		}
		return a[2] > b[2]
	})
	if len(rawList) > 30 {
		rawList = rawList[:30]
		hasDefault := false
		for _, v := range rawList {
			if v == project.DefaultTerraformVersion {
				hasDefault = true
				break
			}
		}
		if !hasDefault {
			rawList[len(rawList)-1] = project.DefaultTerraformVersion
		}
	}
	return rawList, nil
}

func parseSemver(v string) [3]int {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}
	}
	a, _ := strconv.Atoi(parts[0])
	b, _ := strconv.Atoi(parts[1])
	c, _ := strconv.Atoi(parts[2])
	return [3]int{a, b, c}
}
```

Create `internal/infrastructure/terraform/installer.go`:

```go
package terraform

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"infra-guy/internal/domain/project"
)

const maxTerraformArchiveBytes = 250 << 20

type BinaryLocator interface {
	Ensure(ctx context.Context, version string) (string, error)
}

type Installer struct {
	CacheDir string
	BaseURL  string
	Client   *http.Client

	mu sync.Mutex
}

func (in *Installer) Ensure(ctx context.Context, version string) (string, error) {
	norm, err := project.ValidateTerraformVersion(version)
	if err != nil {
		return "", err
	}
	in.mu.Lock()
	defer in.mu.Unlock()

	if in.CacheDir != "" {
		cached := filepath.Join(in.CacheDir, "terraform-"+norm)
		if info, err := os.Stat(cached); err == nil && !info.IsDir() {
			return cached, nil
		}
	}

	if in.BaseURL == "" {
		if path, err := exec.LookPath("terraform"); err == nil {
			if got := binaryVersion(ctx, path); got == norm {
				return path, nil
			}
		}
	}

	if in.CacheDir == "" {
		return "", fmt.Errorf("terraform %s not found and cache dir is empty", norm)
	}
	if err := os.MkdirAll(in.CacheDir, 0o755); err != nil {
		return "", err
	}

	base := in.BaseURL
	if base == "" {
		base = "https://releases.hashicorp.com"
	}
	archiveURL := fmt.Sprintf("%s/terraform/%s/terraform_%s_%s_%s.zip", base, norm, norm, runtime.GOOS, runtime.GOARCH)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return "", err
	}
	client := in.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download terraform %s: HTTP %d", norm, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTerraformArchiveBytes))
	if err != nil {
		return "", err
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", err
	}
	var binEntry *zip.File
	for _, f := range zr.File {
		if filepath.Base(f.Name) == "terraform" {
			binEntry = f
			break
		}
	}
	if binEntry == nil {
		return "", fmt.Errorf("terraform binary missing in archive for %s", norm)
	}
	rc, err := binEntry.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	tmp, err := os.CreateTemp(in.CacheDir, ".tmp-terraform-"+norm+"-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := io.Copy(tmp, io.LimitReader(rc, maxTerraformArchiveBytes)); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	dest := filepath.Join(in.CacheDir, "terraform-"+norm)
	if err := os.Rename(tmpPath, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func binaryVersion(ctx context.Context, bin string) string {
	out, err := exec.CommandContext(ctx, bin, "version", "-json").CombinedOutput()
	if err != nil {
		return ""
	}
	return parseVersionOutput(out)
}
```

Update `internal/infrastructure/terraform/terraform.go`:
- Add `Binary BinaryLocator` to `Executor`.
- Add `TerraformVersion string`, `Env map[string]string`, `Files map[string][]byte` to `Request` (and to `worker.Request` in `internal/application/worker/worker.go`, forwarding them in `internal/infrastructure/terraform/worker_executor.go`).
- Add `"CONTEXT_ENCRYPTION_KEY": true` to `blockedEnv`.
- In `Executor.Run`:
  1. Resolve `bin := "terraform"`; if `e.Binary != nil && req.TerraformVersion != ""` call `bin, err = e.Binary.Ensure(ctx, req.TerraformVersion)`.
  2. Create `homeDir := filepath.Join(req.WorkDir, ".home")` with `os.MkdirAll(homeDir, 0o700)` and write each `relPath, content` in `req.Files` using `materializeFiles(homeDir, req.Files)` (with mode `0600` and path traversal check).
  3. Build `env := commandEnv(req.GitHubToken, homeDir, req.Env)` which strips blocked keys and `HOME=`, appends sorted `req.Env` keys (if `!blockedEnv[k]` and `k != "HOME"` and `k != "PATH"`), and appends `"HOME="+homeDir` (when `homeDir != ""`), `"TF_IN_AUTOMATION=1"`, `"TF_INPUT=0"`.
  4. Use `bin` in `commandBin(ctx, bin, dir, env, args...)` and `terraformVersionBin(ctx, bin, dir, env)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/infrastructure/terraform/... ./internal/application/worker/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/infrastructure/terraform/ internal/application/worker/worker.go
git commit -m "Add Terraform VersionCatalog, Installer, and Executor context/home isolation."
```

---

### Task 6: Worker Integration for Terraform Version & Project Contexts

**Files:**
- Modify: `internal/application/worker/worker.go`
- Modify: `internal/application/worker/worker_test.go`

**Interfaces:**
- Consumes: `project.Snapshot.TerraformVersion`, `worker.ContextResolver`
- Produces:
  - `worker.ContextResolver` interface: `ResolveForProject(ctx context.Context, projectID project.ID) (map[string]string, map[string][]byte, error)`
  - `Worker.Contexts ContextResolver`

- [ ] **Step 1: Write failing worker unit test**

Add `TestTickPassesTerraformVersionAndResolvedContexts` to `internal/application/worker/worker_test.go`:

```go
type fakeContextResolver struct {
	env   map[string]string
	files map[string][]byte
	err   error
}

func (f fakeContextResolver) ResolveForProject(context.Context, project.ID) (map[string]string, map[string][]byte, error) {
	return f.env, f.files, f.err
}

func TestTickPassesTerraformVersionAndResolvedContexts(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	if err := p.SetTerraformVersion("1.10.5"); err != nil {
		t.Fatal(err)
	}
	r, err := run.Open(run.OpenInput{
		ID: "run-ctx", ProjectID: "p1", PlanOnly: true, Trigger: run.TriggerManual, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{res: Result{ExitCode: 0, Version: "1.10.5"}}
	fx := newFixture(t, r, p, &fakeQueue{claim: &Job{ID: 9, RunID: "run-ctx", Step: run.StepInit}}, exec)
	fx.w.Contexts = fakeContextResolver{
		env:   map[string]string{"TF_VAR_env": "prod"},
		files: map[string][]byte{".kube/config": []byte("k8s-data")},
	}
	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if fx.exec.req.TerraformVersion != "1.10.5" {
		t.Fatalf("TerraformVersion = %q, want 1.10.5", fx.exec.req.TerraformVersion)
	}
	if fx.exec.req.Env["TF_VAR_env"] != "prod" || string(fx.exec.req.Files[".kube/config"]) != "k8s-data" {
		t.Fatalf("executor req env=%v files=%v", fx.exec.req.Env, fx.exec.req.Files)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/application/worker/...`
Expected: FAIL with `fx.w.Contexts` undefined.

- [ ] **Step 3: Update `Worker` in `internal/application/worker/worker.go`**

In `internal/application/worker/worker.go`:
1. Define:
   ```go
   type ContextResolver interface {
   	ResolveForProject(ctx context.Context, projectID project.ID) (map[string]string, map[string][]byte, error)
   }
   ```
2. Add `Contexts ContextResolver` to `Worker`.
3. In `runJob`, before invoking `w.Executor.Run`:
   ```go
   var ctxEnv map[string]string
   var ctxFiles map[string][]byte
   if w.Contexts != nil {
   	var cerr error
   	ctxEnv, ctxFiles, cerr = w.Contexts.ResolveForProject(ctx, ps.ID)
   	if cerr != nil {
   		if ferr := w.Service.Fail(ctx, job.RunID, job.Step, cerr.Error()); ferr != nil {
   			return ferr
   		}
   		return w.completeAndCleanup(ctx, job)
   	}
   }
   ```
4. Pass `TerraformVersion: ps.TerraformVersion, Env: ctxEnv, Files: ctxFiles` inside `Request{...}` to `w.Executor.Run`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/application/worker/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/application/worker/worker.go internal/application/worker/worker_test.go
git commit -m "Pass project TerraformVersion and resolved Contexts in Worker."
```

---

### Task 7: HTTP UI for Terraform Version Selection, Context Management, and Server Wiring

**Files:**
- Modify: `internal/interfaces/http/server.go`
- Modify: `internal/interfaces/http/ui.go`
- Modify: `internal/interfaces/http/templates/layout.html`
- Modify: `internal/interfaces/http/templates/new_project.html`
- Modify: `internal/interfaces/http/templates/details.html`
- Create: `internal/interfaces/http/templates/contexts.html`
- Create: `internal/interfaces/http/templates/context.html`
- Modify: `internal/interfaces/http/static/app.css`
- Modify: `internal/interfaces/http/ui_test.go`
- Modify: `cmd/server/main.go`

**Interfaces:**
- Consumes: `*tfcontextapp.Service`, `httpapi.VersionLister` (`List(ctx context.Context) []string`), `(*projectapp.Service).SetTerraformVersion`
- Produces:
  - UI routes:
    - `GET /contexts`
    - `POST /contexts`
    - `GET /contexts/{id}`
    - `POST /contexts/{id}/entries`
    - `POST /contexts/{id}/entries/{entryID}/delete`
    - `POST /contexts/{id}/delete`
  - Project create & update support for `terraform_version` and `context_ids`
  - Full wiring in `cmd/server/main.go`

- [ ] **Step 1: Write failing HTTP UI tests in `internal/interfaces/http/ui_test.go`**

Append `TestProjectTerraformVersionAndContextsUI` to `internal/interfaces/http/ui_test.go`:

```go
func TestProjectTerraformVersionAndContextsUI(t *testing.T) {
	p := listProject(t)
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	runs := &memRuns{items: map[run.ID]*run.Run{}}
	states := &memStates{items: map[string]*state.State{}}
	proj, runsSvc, st := uiServices(projects, runs, states)
	ctxRepo := newUIMemContexts()
	seq := 0
	ctxSvc := &tfcontextapp.Service{
		Contexts: ctxRepo,
		Now:      time.Now,
		NewID: func() string {
			seq++
			return fmt.Sprintf("ctx-%d", seq)
		},
	}
	store := newUIIdentity("operator", "ten")
	gh := &uiGitHub{
		slug: "infra-guy", userID: 4, login: "operator",
		membershipState: "active", membershipRole: "admin", membershipStatus: http.StatusOK,
		repos: map[int64][]installation.Repo{100: {{ID: 101, Owner: "operator", Name: "my-infra"}}},
	}
	h := NewServer(ServerConfig{
		Projects: proj, Runs: runsSvc, States: st, Contexts: ctxSvc,
		Versions:      stubVersions{list: []string{"1.10.5", "1.9.8"}},
		Checkout:      &stubTip{sha: "abc1234"},
		Lookup:        &stubLookup{},
		Tokens:        uiTokenSource{store: store},
		PublicBaseURL: "http://localhost:8080",
		Auth: &authapp.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "id-1" },
			PublicBaseURL: "http://localhost:8080", ClientID: "cid",
		},
		Installations: &installation.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "inst-1" },
		},
	})

	// 1. Create a context
	rr := postUI(t, h, "/contexts", strings.NewReader("name=prod-secrets"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create context %d: %s", rr.Code, rr.Body.String())
	}

	// 2. Add plain_env, secret_env, and secret_file entries
	rr = postUI(t, h, "/contexts/ctx-1/entries", strings.NewReader("kind=plain_env&key=TF_VAR_env&value=prod"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("add plain_env %d: %s", rr.Code, rr.Body.String())
	}
	rr = postUI(t, h, "/contexts/ctx-1/entries", strings.NewReader("kind=secret_env&key=AWS_SECRET_ACCESS_KEY&value=super-secret-value"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("add secret_env %d: %s", rr.Code, rr.Body.String())
	}
	rr = postUI(t, h, "/contexts/ctx-1/entries", strings.NewReader("kind=secret_file&key=.kube/config&value=kube-secret-content"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("add secret_file %d: %s", rr.Code, rr.Body.String())
	}

	// 3. GET /contexts/ctx-1 shows plain value, masks secrets
	rr = getUI(t, h, "/contexts/ctx-1")
	body := rr.Body.String()
	if rr.Code != http.StatusOK || !strings.Contains(body, "TF_VAR_env") || !strings.Contains(body, "prod") {
		t.Fatalf("context page missing plain entry: %s", body)
	}
	if strings.Contains(body, "super-secret-value") || strings.Contains(body, "kube-secret-content") {
		t.Fatalf("context page leaked secret plaintext: %s", body)
	}

	// 4. Create project with terraform_version=1.10.5 and context_ids=ctx-1
	rr = postUI(t, h, "/projects", strings.NewReader("name=test&repo_id=101&branch=main&workdir=infra&terraform_version=1.10.5&context_ids=ctx-1"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create project %d: %s", rr.Code, rr.Body.String())
	}
	created := projects.items["new-p"]
	if created.Snapshot().TerraformVersion != "1.10.5" {
		t.Fatalf("created version = %q, want 1.10.5", created.Snapshot().TerraformVersion)
	}
	attached, _ := ctxSvc.ListByProject(context.Background(), "new-p")
	if len(attached) != 1 || attached[0].Snapshot().ID != "ctx-1" {
		t.Fatalf("attached contexts = %#v", attached)
	}

	// 5. Update project details (terraform_version and context_ids)
	rr = postUI(t, h, "/projects/p1", strings.NewReader("branch=main&workdir=infra&terraform_version=1.10.5&context_ids=ctx-1"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("update project %d: %s", rr.Code, rr.Body.String())
	}
	if projects.items["p1"].Snapshot().TerraformVersion != "1.10.5" {
		t.Fatalf("updated p1 version = %q", projects.items["p1"].Snapshot().TerraformVersion)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/interfaces/http/...`
Expected: FAIL with `Contexts` and `Versions` undefined on `ServerConfig`.

- [ ] **Step 3: Implement UI routes, templates, CSS, and `cmd/server/main.go` wiring**

1. In `internal/interfaces/http/server.go`:
   - Add `VersionLister` interface:
     ```go
     type VersionLister interface {
     	List(ctx context.Context) []string
     }
     ```
   - Add `Contexts *tfcontextapp.Service` and `Versions VersionLister` to `ServerConfig`.
2. In `internal/interfaces/http/ui.go`:
   - Register routes on `mux`:
     ```go
     mux.HandleFunc("GET /contexts", u.contexts)
     mux.HandleFunc("POST /contexts", u.createContext)
     mux.HandleFunc("GET /contexts/{id}", u.contextDetails)
     mux.HandleFunc("POST /contexts/{id}/entries", u.upsertContextEntry)
     mux.HandleFunc("POST /contexts/{id}/entries/{entryID}/delete", u.deleteContextEntry)
     mux.HandleFunc("POST /contexts/{id}/delete", u.deleteContext)
     ```
   - Add helper `u.versions(ctx context.Context) []string` that returns `u.cfg.Versions.List(ctx)` when `u.cfg.Versions != nil`, else `terraform.FallbackVersions`.
   - In `newProject` and `details`, pass `Versions`, `DefaultVersion` / `TerraformVersion`, and `Contexts` (with `Attached: true/false` on `details`).
   - In `create`, validate/set `r.FormValue("terraform_version")` via `u.cfg.Projects.SetTerraformVersion(r.Context(), p.Snapshot().ID, r.FormValue("terraform_version"))` and if `u.cfg.Contexts != nil` call `u.cfg.Contexts.SetProjectContexts(r.Context(), tenantID, p.Snapshot().ID, parseContextIDs(r.Form["context_ids"]))`.
   - In `update`, pass `TerraformVersion: r.FormValue("terraform_version")` in `projectapp.UpdateInput` and update `SetProjectContexts` when `u.cfg.Contexts != nil`.
   - In `projectMeta`, fall back `m.TerraformVersion` to `ps.TerraformVersion` when `latest == nil` or `latest.TerraformVersion == ""`.
   - Implement `u.contexts`, `u.createContext`, `u.contextDetails`, `u.upsertContextEntry`, `u.deleteContextEntry`, and `u.deleteContext`.
3. Update `internal/interfaces/http/templates/layout.html`:
   - Add nav links in `.topbar`:
     ```html
     <header class="topbar">
       <a class="brand" href="/">infra-guy</a>
       <nav class="topnav">
         <a href="/">Projects</a>
         <a href="/contexts">Contexts</a>
       </nav>
     </header>
     ```
4. Update `internal/interfaces/http/templates/new_project.html` and `details.html`:
   - Add `<label>Terraform version</label><select name="terraform_version">...</select>`.
   - Add `<label>Contexts</label>` with checkbox list `<input type="checkbox" name="context_ids" value="{{.ID}}">`.
5. Create `internal/interfaces/http/templates/contexts.html` and `internal/interfaces/http/templates/context.html` for listing/creating contexts and managing `plain_env`, `secret_env`, and `secret_file` entries.
6. Update `internal/interfaces/http/static/app.css` to style `select`, `textarea`, and `.topnav`.
7. Update `cmd/server/main.go`:
   - Initialize `crypto.NewSealer(encryptionSecret)` using `CONTEXT_ENCRYPTION_KEY` if set, or `"infra-guy-context-v1:" + pemBytes` from `GITHUB_APP_PRIVATE_KEY_FILE`.
   - Initialize `contextRepo := postgres.NewContextRepo(pool, sealer)` and `contextSvc := &tfcontextapp.Service{Contexts: contextRepo, Now: now, NewID: newID}`.
   - Initialize `tfInstaller := &terraform.Installer{CacheDir: filepath.Join(workDir, "bin")}` and `tfCatalog := &terraform.VersionCatalog{}`.
   - Pass `Executor: terraform.WorkerExecutor{Executor: terraform.Executor{Binary: tfInstaller}}` and `Contexts: contextSvc` to `worker.Worker`.
   - Pass `Contexts: contextSvc` and `Versions: tfCatalog` to `httpapi.ServerConfig`.

- [ ] **Step 4: Run all tests to verify they pass**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/server/main.go internal/interfaces/http/
git commit -m "Add UI and server wiring for Terraform versions and Contexts."
```
