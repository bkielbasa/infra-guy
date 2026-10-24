package installation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"infra-guy/internal/domain/tenant"
)

type memIdentity struct {
	mu            sync.Mutex
	tenants       map[string]tenant.Tenant
	installations map[string]tenant.Installation
}

func newMemIdentity() *memIdentity {
	return &memIdentity{
		tenants:       make(map[string]tenant.Tenant),
		installations: make(map[string]tenant.Installation),
	}
}

func (m *memIdentity) UpsertTenant(ctx context.Context, t tenant.Tenant, now time.Time) (tenant.Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, ex := range m.tenants {
		if ex.GitHubAccountID == t.GitHubAccountID {
			ex.Name = t.Name
			ex.AccountType = t.AccountType
			m.tenants[id] = ex
			return ex, nil
		}
	}
	m.tenants[t.ID] = t
	return t, nil
}

func (m *memIdentity) UpsertInstallation(ctx context.Context, in tenant.Installation, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, ex := range m.installations {
		if ex.TenantID == in.TenantID && ex.GitHubInstallationID != in.GitHubInstallationID && ex.Active {
			ex.Active = false
			m.installations[id] = ex
		}
	}
	for id, ex := range m.installations {
		if ex.GitHubInstallationID == in.GitHubInstallationID {
			in.ID = ex.ID
			in.Active = true
			m.installations[id] = in
			return nil
		}
	}
	in.Active = true
	m.installations[in.ID] = in
	return nil
}

func (m *memIdentity) ActiveInstallation(ctx context.Context, tenantID string) (*tenant.Installation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, in := range m.installations {
		if in.TenantID == tenantID && in.Active {
			copy := in
			return &copy, nil
		}
	}
	return nil, nil
}

func (m *memIdentity) InstallationByGitHubID(ctx context.Context, githubInstallationID int64) (*tenant.Installation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, in := range m.installations {
		if in.GitHubInstallationID == githubInstallationID {
			copy := in
			return &copy, nil
		}
	}
	return nil, nil
}

func (m *memIdentity) DeactivateInstallation(ctx context.Context, githubInstallationID int64, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, in := range m.installations {
		if in.GitHubInstallationID == githubInstallationID {
			in.Active = false
			m.installations[id] = in
		}
	}
	return nil
}

func (m *memIdentity) onlyTenant() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.tenants {
		return id
	}
	return ""
}

type fakeGitHub struct {
	repos map[int64][]Repo
}

func (f *fakeGitHub) ListRepos(ctx context.Context, installationID int64) ([]Repo, error) {
	if f.repos == nil {
		return nil, nil
	}
	return f.repos[installationID], nil
}

func seqID(prefix string) func() string {
	var count int
	return func() string {
		count++
		return prefix + "-" + string(rune('0'+count))
	}
}

func TestApplyReinstallKeepsTenant(t *testing.T) {
	store := newMemIdentity()
	svc := &Service{Store: store, NewID: seqID("id")}
	ev := InstallationEvent{Action: "created", InstallationID: 1, AccountID: 50, AccountLogin: "acme", AccountType: "Organization"}
	if err := svc.Apply(context.Background(), "installation", ev); err != nil {
		t.Fatal(err)
	}
	ev.InstallationID = 2
	if err := svc.Apply(context.Background(), "installation", ev); err != nil {
		t.Fatal(err)
	}
	active, err := store.ActiveInstallation(context.Background(), store.onlyTenant())
	if err != nil || active.GitHubInstallationID != 2 || !active.Active {
		t.Fatalf("%+v %v", active, err)
	}
}

func TestApplySuspendDeactivates(t *testing.T) {
	store := newMemIdentity()
	svc := &Service{Store: store, NewID: seqID("id")}
	ev := InstallationEvent{Action: "created", InstallationID: 1, AccountID: 50, AccountLogin: "acme", AccountType: "Organization"}
	if err := svc.Apply(context.Background(), "installation", ev); err != nil {
		t.Fatal(err)
	}
	suspendEv := InstallationEvent{Action: "suspend", InstallationID: 1, AccountID: 50, AccountLogin: "acme-renamed", AccountType: "Organization"}
	if err := svc.Apply(context.Background(), "installation", suspendEv); err != nil {
		t.Fatal(err)
	}
	active, err := store.ActiveInstallation(context.Background(), store.onlyTenant())
	if err != nil {
		t.Fatal(err)
	}
	if active != nil {
		t.Fatalf("expected nil active installation, got %+v", active)
	}
	tenantID := store.onlyTenant()
	if store.tenants[tenantID].Name != "acme-renamed" {
		t.Fatalf("expected tenant name acme-renamed, got %q", store.tenants[tenantID].Name)
	}
}

func TestApplySuspendWithoutAccountUpdatesDeactivates(t *testing.T) {
	store := newMemIdentity()
	svc := &Service{Store: store, NewID: seqID("id")}
	ev := InstallationEvent{Action: "created", InstallationID: 1, AccountID: 50, AccountLogin: "acme", AccountType: "Organization"}
	if err := svc.Apply(context.Background(), "installation", ev); err != nil {
		t.Fatal(err)
	}
	suspendEv := InstallationEvent{Action: "suspend", InstallationID: 1}
	if err := svc.Apply(context.Background(), "installation", suspendEv); err != nil {
		t.Fatal(err)
	}
	active, err := store.ActiveInstallation(context.Background(), store.onlyTenant())
	if err != nil {
		t.Fatal(err)
	}
	if active != nil {
		t.Fatalf("expected nil active installation, got %+v", active)
	}
}

func TestReposSuccess(t *testing.T) {
	store := newMemIdentity()
	gh := &fakeGitHub{
		repos: map[int64][]Repo{
			1: {
				{ID: 10, Owner: "acme", Name: "infra", DefaultBranch: "master"},
				{ID: 20, Owner: "acme", Name: "apps", DefaultBranch: "main"},
			},
		},
	}
	svc := &Service{Store: store, GitHub: gh, NewID: seqID("id")}
	ev := InstallationEvent{Action: "created", InstallationID: 1, AccountID: 50, AccountLogin: "acme", AccountType: "Organization"}
	if err := svc.Apply(context.Background(), "installation", ev); err != nil {
		t.Fatal(err)
	}
	tenantID := store.onlyTenant()
	repos, err := svc.Repos(context.Background(), tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0].Name != "infra" || repos[0].DefaultBranch != "master" || repos[1].Name != "apps" || repos[1].DefaultBranch != "main" {
		t.Fatalf("unexpected repos: %+v", repos)
	}
}

func TestReposNotInstalled(t *testing.T) {
	store := newMemIdentity()
	svc := &Service{Store: store, NewID: seqID("id")}
	_, err := svc.Repos(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, errNotInstalled) && !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("expected ErrNotInstalled, got %v", err)
	}
}
