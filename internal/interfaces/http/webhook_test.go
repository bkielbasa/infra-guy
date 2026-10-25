package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"infra-guy/internal/application/installation"
	projectapp "infra-guy/internal/application/project"
	runapp "infra-guy/internal/application/run"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/domain/tenant"
)

type fakeOpen struct {
	mu        sync.Mutex
	reqs      []runapp.OpenRequest
	runsByPrj map[project.ID][]*run.Run
	approvals map[run.ID][]run.Approval
	confirmed map[run.ID]string
	err       error
}

func (f *fakeOpen) Open(_ context.Context, in runapp.OpenRequest) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, in)
	return nil, f.err
}

func (f *fakeOpen) ListByProject(_ context.Context, projectID project.ID) ([]*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runsByPrj == nil {
		return nil, nil
	}
	return f.runsByPrj[projectID], nil
}

func (f *fakeOpen) Approve(_ context.Context, id run.ID, a run.Approval) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.approvals == nil {
		f.approvals = make(map[run.ID][]run.Approval)
	}
	f.approvals[id] = append(f.approvals[id], a)
	return nil
}

func (f *fakeOpen) Confirm(_ context.Context, id run.ID, actor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.confirmed == nil {
		f.confirmed = make(map[run.ID]string)
	}
	f.confirmed[id] = actor
	return nil
}

func (f *fakeOpen) calls() []runapp.OpenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]runapp.OpenRequest, len(f.reqs))
	copy(cp, f.reqs)
	return cp
}

func (f *fakeOpen) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = nil
	f.runsByPrj = nil
	f.approvals = nil
	f.confirmed = nil
	f.err = nil
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type webhookProjects struct {
	mu    sync.Mutex
	items map[project.ID]*project.Project
	err   error
}

func newWebhookProjects() *webhookProjects {
	return &webhookProjects{items: make(map[project.ID]*project.Project)}
}

func (m *webhookProjects) Save(_ context.Context, p *project.Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.items[p.Snapshot().ID] = project.Restore(p.Snapshot())
	return nil
}

func (m *webhookProjects) Get(_ context.Context, id project.ID) (*project.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	return m.items[id], nil
}

func (m *webhookProjects) List(_ context.Context, tenantID string) ([]*project.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*project.Project
	for _, p := range m.items {
		if p.Snapshot().TenantID == tenantID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (m *webhookProjects) FindByRepo(_ context.Context, tenantID string, repoID int64) ([]*project.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	var out []*project.Project
	for _, p := range m.items {
		s := p.Snapshot()
		if s.TenantID == tenantID && s.GitHubRepoID == repoID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Snapshot().ID < out[j].Snapshot().ID
	})
	return out, nil
}

func (m *webhookProjects) SetLocked(_ context.Context, id project.ID, locked bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.items[id]; p != nil {
		s := p.Snapshot()
		s.Locked = locked
		m.items[id] = project.Restore(s)
	}
	return nil
}

func (m *webhookProjects) ReleaseActiveRun(_ context.Context, id project.ID, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.items[id]; p != nil {
		_ = p.ReleaseActiveRun(runID)
	}
	return nil
}

func (m *webhookProjects) Delete(_ context.Context, id project.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, id)
	return nil
}

type memInstallationStore struct {
	mu            sync.Mutex
	tenants       map[string]tenant.Tenant
	installations map[int64]tenant.Installation
}

func newMemInstallationStore() *memInstallationStore {
	return &memInstallationStore{
		tenants:       make(map[string]tenant.Tenant),
		installations: make(map[int64]tenant.Installation),
	}
}

func (m *memInstallationStore) UpsertTenant(_ context.Context, t tenant.Tenant, _ time.Time) (tenant.Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.tenants {
		if ex.GitHubAccountID == t.GitHubAccountID {
			ex.Name = t.Name
			ex.AccountType = t.AccountType
			m.tenants[ex.ID] = ex
			return ex, nil
		}
	}
	m.tenants[t.ID] = t
	return t, nil
}

func (m *memInstallationStore) UpsertInstallation(_ context.Context, in tenant.Installation, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, ex := range m.installations {
		if ex.TenantID == in.TenantID && ex.GitHubInstallationID != in.GitHubInstallationID && ex.Active {
			ex.Active = false
			m.installations[id] = ex
		}
	}
	if ex, ok := m.installations[in.GitHubInstallationID]; ok {
		in.ID = ex.ID
	}
	m.installations[in.GitHubInstallationID] = in
	return nil
}

func (m *memInstallationStore) ActiveInstallation(_ context.Context, tenantID string) (*tenant.Installation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, in := range m.installations {
		if in.TenantID == tenantID && in.Active {
			cp := in
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *memInstallationStore) InstallationByGitHubID(_ context.Context, githubInstallationID int64) (*tenant.Installation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if in, ok := m.installations[githubInstallationID]; ok {
		cp := in
		return &cp, nil
	}
	return nil, nil
}

func (m *memInstallationStore) DeactivateInstallation(_ context.Context, githubInstallationID int64, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if in, ok := m.installations[githubInstallationID]; ok {
		in.Active = false
		m.installations[githubInstallationID] = in
	}
	return nil
}

func makeProject(t *testing.T, id, name, tenantID string, repoID int64, defaultBranch string) *project.Project {
	t.Helper()
	repo, err := project.ParseSourceRepo("acme/infra")
	if err != nil {
		t.Fatal(err)
	}
	p, err := project.New(project.ID(id), name, repo, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetDefaultBranch(defaultBranch); err != nil {
		t.Fatal(err)
	}
	if err := p.SetPlacement(tenantID, repoID); err != nil {
		t.Fatal(err)
	}
	return p
}

func postWebhook(t *testing.T, h http.Handler, event, secret string, body []byte, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	} else if secret != "" {
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestGitHubWebhook(t *testing.T) {
	const secret = "app-secret"

	instStore := newMemInstallationStore()
	var idCount int
	newID := func() string {
		idCount++
		return fmt.Sprintf("id-%d", idCount)
	}
	instSvc := &installation.Service{
		Store: instStore,
		NewID: newID,
		Now:   time.Now,
	}

	// Seed tenant and installation 9
	ctx := context.Background()
	ten, err := instStore.UpsertTenant(ctx, tenant.Tenant{
		ID:              "ten-1",
		Name:            "acme",
		AccountType:     "organization",
		GitHubAccountID: 50,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := instStore.UpsertInstallation(ctx, tenant.Installation{
		ID:                   "inst-9",
		TenantID:             ten.ID,
		AccountLogin:         "acme",
		AccountType:          "organization",
		GitHubInstallationID: 9,
		Active:               true,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Projects:
	// - p1: tenant ten-1, repo 5, default branch main
	// - p2: tenant ten-1, repo 5, default branch main
	// - p3: tenant ten-1, repo 5, default branch dev
	// - p4: tenant ten-2, repo 5, default branch main
	projects := newWebhookProjects()
	p1 := makeProject(t, "p1", "infra-prod", "ten-1", 5, "main")
	p2 := makeProject(t, "p2", "infra-stage", "ten-1", 5, "main")
	p3 := makeProject(t, "p3", "infra-dev", "ten-1", 5, "dev")
	p4 := makeProject(t, "p4", "infra-other", "ten-2", 5, "main")
	_ = projects.Save(ctx, p1)
	_ = projects.Save(ctx, p2)
	_ = projects.Save(ctx, p3)
	_ = projects.Save(ctx, p4)

	open := &fakeOpen{}
	projSvc := &projectapp.Service{Projects: projects, Now: time.Now}
	h := New(nil, open, projSvc, WithWebhook(instSvc, secret))

	pushPayload := []byte(`{
		"ref":"refs/heads/main",
		"after":"ffffffffffffffffffffffffffffffffffffffff",
		"head_commit":{"message":"ship it","author":{"name":"Cara"}},
		"sender":{"login":"cara"},
		"installation":{"id":9,"account":{"id":50,"login":"acme","type":"Organization"}},
		"repository":{"id":5,"full_name":"acme/infra","name":"infra","owner":{"login":"acme"}}
	}`)

	t.Run("bad signature returns 401 and calls no opener", func(t *testing.T) {
		open.reset()
		// Missing signature
		rr := postWebhook(t, h, "push", "", pushPayload, "")
		if rr.Code != http.StatusUnauthorized || len(open.calls()) != 0 {
			t.Fatalf("missing sig code=%d calls=%d", rr.Code, len(open.calls()))
		}
		// Wrong signature
		rr = postWebhook(t, h, "push", "", pushPayload, sign("wrong-secret", pushPayload))
		if rr.Code != http.StatusUnauthorized || len(open.calls()) != 0 {
			t.Fatalf("wrong sig code=%d calls=%d", rr.Code, len(open.calls()))
		}
	})

	t.Run("ping returns 200 and calls no opener", func(t *testing.T) {
		open.reset()
		body := []byte(`{"zen":"Keep it logically awesome."}`)
		rr := postWebhook(t, h, "ping", secret, body, "")
		if rr.Code != http.StatusOK || len(open.calls()) != 0 {
			t.Fatalf("ping code=%d calls=%d", rr.Code, len(open.calls()))
		}
	})

	t.Run("malformed json returns 200 and calls no opener", func(t *testing.T) {
		open.reset()
		rr := postWebhook(t, h, "push", secret, []byte(`{not valid json`), "")
		if rr.Code != http.StatusOK || len(open.calls()) != 0 {
			t.Fatalf("malformed code=%d calls=%d", rr.Code, len(open.calls()))
		}
	})

	t.Run("push opens one run per matching project in tenant", func(t *testing.T) {
		open.reset()
		rr := postWebhook(t, h, "push", secret, pushPayload, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("push code=%d", rr.Code)
		}
		calls := open.calls()
		if len(calls) != 2 {
			t.Fatalf("expected 2 calls (p1, p2), got %d: %#v", len(calls), calls)
		}
		// Deterministic order: p1 then p2
		if calls[0].ProjectID != "p1" || calls[1].ProjectID != "p2" {
			t.Fatalf("unexpected call project IDs: %s, %s", calls[0].ProjectID, calls[1].ProjectID)
		}
		for _, c := range calls {
			if c.Trigger != run.TriggerPush || c.PlanOnly || c.SHA != "ffffffffffffffffffffffffffffffffffffffff" || c.Branch != "main" || c.Message != "ship it" || c.Author != "Cara" || c.TriggeredBy != "cara" {
				t.Fatalf("unexpected call payload: %#v", c)
			}
		}
	})

	t.Run("push zeros after-SHA opens nothing", func(t *testing.T) {
		open.reset()
		zerosPayload := []byte(`{
			"ref":"refs/heads/main",
			"after":"0000000000000000000000000000000000000000",
			"head_commit":{"message":"deleted","author":{"name":"Cara"}},
			"sender":{"login":"cara"},
			"installation":{"id":9,"account":{"id":50,"login":"acme","type":"Organization"}},
			"repository":{"id":5,"full_name":"acme/infra","name":"infra","owner":{"login":"acme"}}
		}`)
		rr := postWebhook(t, h, "push", secret, zerosPayload, "")
		if rr.Code != http.StatusOK || len(open.calls()) != 0 {
			t.Fatalf("zeros code=%d calls=%d", rr.Code, len(open.calls()))
		}
	})

	t.Run("push other branch opens nothing", func(t *testing.T) {
		open.reset()
		otherBranchPayload := []byte(`{
			"ref":"refs/heads/feature-branch",
			"after":"ffffffffffffffffffffffffffffffffffffffff",
			"head_commit":{"message":"other","author":{"name":"Cara"}},
			"sender":{"login":"cara"},
			"installation":{"id":9,"account":{"id":50,"login":"acme","type":"Organization"}},
			"repository":{"id":5,"full_name":"acme/infra","name":"infra","owner":{"login":"acme"}}
		}`)
		rr := postWebhook(t, h, "push", secret, otherBranchPayload, "")
		if rr.Code != http.StatusOK || len(open.calls()) != 0 {
			t.Fatalf("other branch code=%d calls=%d", rr.Code, len(open.calls()))
		}
	})

	t.Run("pull request opened opens all matching projects plan-only", func(t *testing.T) {
		open.reset()
		prOpened := []byte(`{
			"action":"opened",
			"pull_request":{
				"number":12,
				"title":"add lock",
				"draft":false,
				"head":{"sha":"abc123","ref":"feat","repo":{"full_name":"acme/infra"}}
			},
			"repository":{"id":5,"full_name":"acme/infra","name":"infra","owner":{"login":"acme"}},
			"installation":{"id":9,"account":{"id":50,"login":"acme","type":"Organization"}},
			"sender":{"login":"alice"}
		}`)
		rr := postWebhook(t, h, "pull_request", secret, prOpened, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("pr opened code=%d", rr.Code)
		}
		calls := open.calls()
		// Should open for p1, p2, p3 in tenant ten-1
		if len(calls) != 3 {
			t.Fatalf("expected 3 calls (p1, p2, p3), got %d: %#v", len(calls), calls)
		}
		for _, c := range calls {
			if c.Trigger != run.TriggerPullRequest || !c.PlanOnly || c.SHA != "abc123" || c.Branch != "feat" || c.PRTitle != "add lock" || c.TriggeredBy != "alice" {
				t.Fatalf("unexpected pr call: %#v", c)
			}
			if c.PRNumber == nil || *c.PRNumber != 12 {
				t.Fatalf("unexpected pr number: %#v", c.PRNumber)
			}
		}
	})

	t.Run("pull request actions synchronize reopened ready_for_review open runs", func(t *testing.T) {
		for _, action := range []string{"synchronize", "reopened", "ready_for_review"} {
			open.reset()
			payload := []byte(fmt.Sprintf(`{
				"action":"%s",
				"pull_request":{
					"number":15,
					"title":"update",
					"draft":false,
					"head":{"sha":"def456","ref":"feat-2","repo":{"full_name":"acme/infra"}}
				},
				"repository":{"id":5,"full_name":"acme/infra","name":"infra","owner":{"login":"acme"}},
				"installation":{"id":9,"account":{"id":50,"login":"acme","type":"Organization"}},
				"sender":{"login":"bob"}
			}`, action))
			rr := postWebhook(t, h, "pull_request", secret, payload, "")
			if rr.Code != http.StatusOK || len(open.calls()) != 3 {
				t.Fatalf("action %s code=%d calls=%d", action, rr.Code, len(open.calls()))
			}
		}
	})

	t.Run("draft and fork pull request open nothing", func(t *testing.T) {
		// Draft
		open.reset()
		draftPayload := []byte(`{
			"action":"opened",
			"pull_request":{
				"number":14,
				"title":"draft pr",
				"draft":true,
				"head":{"sha":"abc123","ref":"draft","repo":{"full_name":"acme/infra"}}
			},
			"repository":{"id":5,"full_name":"acme/infra","name":"infra","owner":{"login":"acme"}},
			"installation":{"id":9,"account":{"id":50,"login":"acme","type":"Organization"}},
			"sender":{"login":"alice"}
		}`)
		rr := postWebhook(t, h, "pull_request", secret, draftPayload, "")
		if rr.Code != http.StatusOK || len(open.calls()) != 0 {
			t.Fatalf("draft code=%d calls=%d", rr.Code, len(open.calls()))
		}

		// Fork
		open.reset()
		forkPayload := []byte(`{
			"action":"opened",
			"pull_request":{
				"number":14,
				"title":"fork pr",
				"draft":false,
				"head":{"sha":"abc123","ref":"fork-branch","repo":{"full_name":"mallory/infra"}}
			},
			"repository":{"id":5,"full_name":"acme/infra","name":"infra","owner":{"login":"acme"}},
			"installation":{"id":9,"account":{"id":50,"login":"acme","type":"Organization"}},
			"sender":{"login":"mallory"}
		}`)
		rr = postWebhook(t, h, "pull_request", secret, forkPayload, "")
		if rr.Code != http.StatusOK || len(open.calls()) != 0 {
			t.Fatalf("fork code=%d calls=%d", rr.Code, len(open.calls()))
		}

		// Closed
		open.reset()
		closedPayload := []byte(`{
			"action":"closed",
			"pull_request":{
				"number":14,
				"title":"closed pr",
				"draft":false,
				"head":{"sha":"abc123","ref":"feat","repo":{"full_name":"acme/infra"}}
			},
			"repository":{"id":5,"full_name":"acme/infra","name":"infra","owner":{"login":"acme"}},
			"installation":{"id":9,"account":{"id":50,"login":"acme","type":"Organization"}},
			"sender":{"login":"alice"}
		}`)
		rr = postWebhook(t, h, "pull_request", secret, closedPayload, "")
		if rr.Code != http.StatusOK || len(open.calls()) != 0 {
			t.Fatalf("closed code=%d calls=%d", rr.Code, len(open.calls()))
		}
	})

	t.Run("duplicate run returns 200", func(t *testing.T) {
		open.reset()
		open.err = runapp.ErrDuplicate
		rr := postWebhook(t, h, "push", secret, pushPayload, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("duplicate code=%d", rr.Code)
		}
	})

	t.Run("opener error returns 500", func(t *testing.T) {
		open.reset()
		open.err = errors.New("boom")
		rr := postWebhook(t, h, "push", secret, pushPayload, "")
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("error code=%d", rr.Code)
		}
	})

	t.Run("repo rename updates project repo", func(t *testing.T) {
		open.reset()
		renamedPush := []byte(`{
			"ref":"refs/heads/main",
			"after":"ffffffffffffffffffffffffffffffffffffffff",
			"head_commit":{"message":"rename ship","author":{"name":"Cara"}},
			"sender":{"login":"cara"},
			"installation":{"id":9,"account":{"id":50,"login":"acme-corp","type":"Organization"}},
			"repository":{"id":5,"full_name":"acme-corp/infra-new","name":"infra-new","owner":{"login":"acme-corp"}}
		}`)
		rr := postWebhook(t, h, "push", secret, renamedPush, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("rename code=%d", rr.Code)
		}
		p, err := projects.Get(ctx, "p1")
		if err != nil || p == nil {
			t.Fatalf("get p1 err=%v", err)
		}
		if p.Snapshot().Repo.Owner != "acme-corp" || p.Snapshot().Repo.Name != "infra-new" {
			t.Fatalf("expected renamed repo acme-corp/infra-new, got %#v", p.Snapshot().Repo)
		}
	})

	t.Run("unknown installation upserts installation and returns 200 without opening", func(t *testing.T) {
		open.reset()
		unknownPush := []byte(`{
			"ref":"refs/heads/main",
			"after":"ffffffffffffffffffffffffffffffffffffffff",
			"head_commit":{"message":"init","author":{"name":"Dave"}},
			"sender":{"login":"dave"},
			"installation":{"id":99,"account":{"id":990,"login":"newcorp","type":"Organization"}},
			"repository":{"id":999,"full_name":"newcorp/newrepo","name":"newrepo","owner":{"login":"newcorp"}}
		}`)
		rr := postWebhook(t, h, "push", secret, unknownPush, "")
		if rr.Code != http.StatusOK || len(open.calls()) != 0 {
			t.Fatalf("unknown inst code=%d calls=%d", rr.Code, len(open.calls()))
		}
		inst, err := instStore.InstallationByGitHubID(ctx, 99)
		if err != nil || inst == nil {
			t.Fatalf("installation 99 not upserted: %v, %#v", err, inst)
		}
		if !inst.Active || inst.AccountLogin != "newcorp" {
			t.Fatalf("installation 99 properties mismatch: %#v", inst)
		}
	})

	t.Run("suspend marks installation inactive and subsequent push returns 200 without opening", func(t *testing.T) {
		open.reset()
		suspendPayload := []byte(`{
			"action":"suspend",
			"installation":{"id":9,"account":{"id":50,"login":"acme","type":"Organization"}}
		}`)
		rr := postWebhook(t, h, "installation", secret, suspendPayload, "")
		if rr.Code != http.StatusOK || len(open.calls()) != 0 {
			t.Fatalf("suspend code=%d calls=%d", rr.Code, len(open.calls()))
		}
		inst, err := instStore.InstallationByGitHubID(ctx, 9)
		if err != nil || inst == nil || inst.Active {
			t.Fatalf("expected inactive installation 9, got err=%v inst=%#v", err, inst)
		}

		// Subsequent push returns 200 and opens nothing
		open.reset()
		rr = postWebhook(t, h, "push", secret, pushPayload, "")
		if rr.Code != http.StatusOK || len(open.calls()) != 0 {
			t.Fatalf("post-suspend push code=%d calls=%d", rr.Code, len(open.calls()))
		}
	})
}

func TestWebhookMonorepoPathFilteringAndChatOps(t *testing.T) {
	const secret = "app-secret"
	ctx := context.Background()

	instStore := newMemInstallationStore()
	instSvc := &installation.Service{
		Store: instStore,
		NewID: func() string { return "id-1" },
		Now:   time.Now,
	}
	ten, err := instStore.UpsertTenant(ctx, tenant.Tenant{
		ID: "ten-1", Name: "acme", AccountType: "organization", GitHubAccountID: 50,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := instStore.UpsertInstallation(ctx, tenant.Installation{
		ID: "inst-9", TenantID: ten.ID, AccountLogin: "acme", AccountType: "organization",
		GitHubInstallationID: 9, Active: true,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	projects := newWebhookProjects()
	pProd := makeProject(t, "p-prod", "infra-prod", "ten-1", 5, "main")
	if err := pProd.SetWorkingDirectory("envs/prod"); err != nil {
		t.Fatal(err)
	}
	pStage := makeProject(t, "p-stage", "infra-stage", "ten-1", 5, "main")
	if err := pStage.SetWorkingDirectory("envs/stage"); err != nil {
		t.Fatal(err)
	}
	_ = projects.Save(ctx, pProd)
	_ = projects.Save(ctx, pStage)

	open := &fakeOpen{}
	projSvc := &projectapp.Service{Projects: projects, Now: time.Now}
	h := New(nil, open, projSvc, WithWebhook(instSvc, secret))

	t.Run("monorepo push only opens workspace matching modified path and parses committed_at", func(t *testing.T) {
		open.reset()
		payload := []byte(`{
			"ref":"refs/heads/main",
			"after":"1111111111111111111111111111111111111111",
			"commits":[{"added":[],"modified":["envs/prod/main.tf"],"removed":[]}],
			"head_commit":{
				"message":"update prod vpc",
				"timestamp":"2026-10-24T12:30:00Z",
				"author":{"name":"Alice"}
			},
			"sender":{"login":"alice"},
			"installation":{"id":9,"account":{"id":50,"login":"acme","type":"Organization"}},
			"repository":{"id":5,"full_name":"acme/infra","name":"infra","owner":{"login":"acme"}}
		}`)
		rr := postWebhook(t, h, "push", secret, payload, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("push code=%d", rr.Code)
		}
		calls := open.calls()
		if len(calls) != 1 || calls[0].ProjectID != "p-prod" {
			t.Fatalf("expected only p-prod to open, got %#v", calls)
		}
		wantCommitted := time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC)
		if calls[0].CommittedAt == nil || !calls[0].CommittedAt.Equal(wantCommitted) {
			t.Fatalf("expected CommittedAt=%v, got %#v", wantCommitted, calls[0].CommittedAt)
		}
	})

	t.Run("ChatOps /infra-guy plan, approve, and apply on pull_request issue_comment", func(t *testing.T) {
		open.reset()
		prNum := 77
		committed := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
		rProdWait := run.Restore(run.Snapshot{
			ID: "r-prod-wait", ProjectID: "p-prod", Trigger: run.TriggerPullRequest,
			Status: run.StatusNeedsApproval, SHA: "abcdef1234567890", Branch: "feat/vpc",
			PRNumber: &prNum, PRTitle: "VPC peer", CommittedAt: &committed,
		})
		rStagePlanned := run.Restore(run.Snapshot{
			ID: "r-stage-plan", ProjectID: "p-stage", Trigger: run.TriggerPullRequest,
			PlanOnly: true, Status: run.StatusPlanned, SHA: "abcdef1234567890", Branch: "feat/vpc",
			PRNumber: &prNum, PRTitle: "VPC peer", CommittedAt: &committed,
		})
		open.runsByPrj = map[project.ID][]*run.Run{
			"p-prod":  {rProdWait},
			"p-stage": {rStagePlanned},
		}

		// 1. /infra-guy approve infra-prod
		approvePayload := []byte(`{
			"action":"created",
			"issue":{"number":77,"title":"VPC peer","pull_request":{"url":"https://api.github.com/repos/acme/infra/pulls/77"}},
			"comment":{"body":"/infra-guy approve infra-prod"},
			"sender":{"id":42,"login":"reviewer"},
			"installation":{"id":9,"account":{"id":50,"login":"acme","type":"Organization"}},
			"repository":{"id":5,"full_name":"acme/infra","name":"infra","owner":{"login":"acme"}}
		}`)
		rr := postWebhook(t, h, "issue_comment", secret, approvePayload, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("approve comment code=%d", rr.Code)
		}
		if len(open.approvals["r-prod-wait"]) != 1 || open.approvals["r-prod-wait"][0].Login != "reviewer" {
			t.Fatalf("expected approval on r-prod-wait by reviewer, got %#v", open.approvals)
		}
		if len(open.approvals["r-stage-plan"]) != 0 {
			t.Fatalf("did not expect approval on p-stage when targeting infra-prod")
		}

		// 2. /infra-guy apply (confirms r-prod-wait and opens non-plan-only run on p-stage)
		applyPayload := []byte(`{
			"action":"created",
			"issue":{"number":77,"title":"VPC peer","pull_request":{"url":"https://api.github.com/repos/acme/infra/pulls/77"}},
			"comment":{"body":"/infra-guy apply"},
			"sender":{"id":42,"login":"reviewer"},
			"installation":{"id":9,"account":{"id":50,"login":"acme","type":"Organization"}},
			"repository":{"id":5,"full_name":"acme/infra","name":"infra","owner":{"login":"acme"}}
		}`)
		rr = postWebhook(t, h, "issue_comment", secret, applyPayload, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("apply comment code=%d", rr.Code)
		}
		if open.confirmed["r-prod-wait"] != "reviewer" {
			t.Fatalf("expected r-prod-wait confirmed by reviewer, got %#v", open.confirmed)
		}
		calls := open.calls()
		if len(calls) != 1 || calls[0].ProjectID != "p-stage" || calls[0].PlanOnly || calls[0].SHA != "abcdef1234567890" {
			t.Fatalf("expected non-plan-only run opened for p-stage, got %#v", calls)
		}

		// 3. /infra-guy plan infra-stage
		open.reqs = nil
		planPayload := []byte(`{
			"action":"created",
			"issue":{"number":77,"title":"VPC peer","pull_request":{"url":"https://api.github.com/repos/acme/infra/pulls/77"}},
			"comment":{"body":"/infra-guy plan infra-stage"},
			"sender":{"id":42,"login":"reviewer"},
			"installation":{"id":9,"account":{"id":50,"login":"acme","type":"Organization"}},
			"repository":{"id":5,"full_name":"acme/infra","name":"infra","owner":{"login":"acme"}}
		}`)
		rr = postWebhook(t, h, "issue_comment", secret, planPayload, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("plan comment code=%d", rr.Code)
		}
		calls = open.calls()
		if len(calls) != 1 || calls[0].ProjectID != "p-stage" || !calls[0].PlanOnly {
			t.Fatalf("expected plan-only run opened for p-stage, got %#v", calls)
		}
	})
}
