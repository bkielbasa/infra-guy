package projectapp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"infra-guy/internal/domain/folder"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/state"
)

type memProjects struct {
	items map[project.ID]*project.Project
}

func (m *memProjects) Save(_ context.Context, p *project.Project) error {
	m.items[p.Snapshot().ID] = project.Restore(p.Snapshot())
	return nil
}
func (m *memProjects) Get(_ context.Context, id project.ID) (*project.Project, error) {
	return m.items[id], nil
}
func (m *memProjects) List(context.Context, string) ([]*project.Project, error) { return nil, nil }
func (m *memProjects) FindByRepo(_ context.Context, tenantID string, repoID int64) ([]*project.Project, error) {
	var out []*project.Project
	for _, p := range m.items {
		s := p.Snapshot()
		if s.TenantID == tenantID && s.GitHubRepoID == repoID {
			out = append(out, project.Restore(s))
		}
	}
	return out, nil
}
func (m *memProjects) SetLocked(_ context.Context, id project.ID, locked bool) error {
	if p := m.items[id]; p != nil {
		s := p.Snapshot()
		s.Locked = locked
		m.items[id] = project.Restore(s)
	}
	return nil
}
func (m *memProjects) ReleaseActiveRun(_ context.Context, id project.ID, runID string) error {
	if p := m.items[id]; p != nil {
		_ = p.ReleaseActiveRun(runID)
	}
	return nil
}
func (m *memProjects) Delete(_ context.Context, id project.ID) error {
	delete(m.items, id)
	return nil
}

type memStates struct{ items map[string]*state.State }

func (m *memStates) Load(_ context.Context, projectID string) (*state.State, error) {
	if st, ok := m.items[projectID]; ok {
		return state.Restore(st.Snapshot()), nil
	}
	return nil, nil
}

func (m *memStates) Save(_ context.Context, s *state.State) error {
	m.items[s.Snapshot().ProjectID] = state.Restore(s.Snapshot())
	return nil
}

func (m *memStates) ForceUnlock(ctx context.Context, projectID string) error {
	st, err := m.Load(ctx, projectID)
	if err != nil || st == nil {
		return err
	}
	st.ForceUnlock()
	return m.Save(ctx, st)
}

func TestCreateUpdateAndForceUnlock(t *testing.T) {
	ctx := context.Background()
	projects := &memProjects{items: map[project.ID]*project.Project{}}
	states := &memStates{items: map[string]*state.State{}}
	svc := &Service{Projects: projects, States: states, Now: func() time.Time { return time.Unix(1, 0).UTC() }, NewID: func() string { return "p1" }}
	repo, err := project.ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, "", "infra", repo, 1, "", "terraform", false); err == nil {
		t.Fatal("create without a tenant must fail")
	}
	if len(projects.items) != 0 {
		t.Fatal("rejected create was saved")
	}
	created, err := svc.Create(ctx, "ten", "infra", repo, 1, "", "terraform", false)
	if err != nil {
		t.Fatal(err)
	}
	snap := created.Snapshot()
	if snap.TenantID != "ten" || snap.GitHubRepoID != 1 {
		t.Fatalf("placement not stored: %#v", snap)
	}
	if snap.StateToken == "" || snap.DefaultBranch != "main" || snap.WorkingDirectory != "terraform" {
		t.Fatalf("%#v", snap)
	}
	if snap.TerraformVersion != project.DefaultTerraformVersion {
		t.Fatalf("default terraform version = %q", snap.TerraformVersion)
	}
	updated, err := svc.Update(ctx, UpdateInput{ID: "p1", TerraformVersion: "1.10.5"})
	if err != nil || updated.Snapshot().StateToken != snap.StateToken {
		t.Fatal(err)
	}
	if updated.Snapshot().WorkingDirectory != "" {
		t.Fatalf("empty workdir must mean repository root, got %q", updated.Snapshot().WorkingDirectory)
	}
	if updated.Snapshot().TerraformVersion != "1.10.5" {
		t.Fatalf("updated terraform version = %q, want 1.10.5", updated.Snapshot().TerraformVersion)
	}
	if _, err := svc.Update(ctx, UpdateInput{ID: "p1", TerraformVersion: "bad-ver"}); err == nil {
		t.Fatal("expected invalid terraform version error")
	}
	if err := svc.SetTerraformVersion(ctx, "p1", "1.9.8"); err != nil {
		t.Fatal(err)
	}
	if projects.items["p1"].Snapshot().TerraformVersion != "1.9.8" {
		t.Fatalf("SetTerraformVersion = %q, want 1.9.8", projects.items["p1"].Snapshot().TerraformVersion)
	}
	if err := svc.UpdateApprovalPolicy(ctx, "p1", "  pol-99  "); err != nil {
		t.Fatal(err)
	}
	if projects.items["p1"].Snapshot().ApprovalPolicyID != "pol-99" {
		t.Fatalf("UpdateApprovalPolicy = %q, want pol-99", projects.items["p1"].Snapshot().ApprovalPolicyID)
	}
	if err := svc.UpdateApprovalPolicy(ctx, "p1", ""); err != nil {
		t.Fatal(err)
	}
	if projects.items["p1"].Snapshot().ApprovalPolicyID != "" {
		t.Fatalf("UpdateApprovalPolicy clear = %q, want empty", projects.items["p1"].Snapshot().ApprovalPolicyID)
	}
	if err := svc.SetTags(ctx, "p1", map[string]string{"env": "prod", "team": "core"}); err != nil {
		t.Fatal(err)
	}
	if got := project.FormatTags(projects.items["p1"].Snapshot().Tags); got != "env:prod, team:core" {
		t.Fatalf("SetTags = %q, want env:prod, team:core", got)
	}
	if err := created.ClaimActiveRun("run-1"); err != nil {
		t.Fatal(err)
	}
	_ = projects.Save(ctx, created)
	if err := svc.SetLocked(ctx, "p1", true); err != nil || !projects.items["p1"].Snapshot().Locked {
		t.Fatalf("lock: %v", err)
	}
	st := state.New("p1")
	_, _ = st.Lock([]byte(`{"ID":"L1"}`))
	_ = states.Save(ctx, st)
	if err := svc.ForceUnlock(ctx, "p1"); err != ErrActiveRun {
		t.Fatalf("force unlock: %v", err)
	}
	if states.items["p1"].Snapshot().LockID != "L1" {
		t.Fatal("lock cleared while a run was active")
	}
	_ = created.ReleaseActiveRun("run-1")
	_ = projects.Save(ctx, created)
	if err := svc.ForceUnlock(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if states.items["p1"].Snapshot().LockID != "" {
		t.Fatal("lock remains")
	}
}

func TestSyncAndFindByRepoUpdatesRenamedRepo(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(200, 0).UTC()
	projects := &memProjects{items: map[project.ID]*project.Project{}}
	svc := &Service{
		Projects: projects,
		Now:      func() time.Time { return now },
		NewID:    func() string { return "p1" },
	}
	repo, err := project.ParseSourceRepo("old-owner/old-name")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, "t1", "infra", repo, 42, "main", ".", false); err != nil {
		t.Fatal(err)
	}

	matched, err := svc.SyncAndFindByRepo(ctx, "t1", 42, "new-owner", "new-name")
	if err != nil {
		t.Fatalf("SyncAndFindByRepo: %v", err)
	}
	if len(matched) != 1 {
		t.Fatalf("got %d matched projects, want 1", len(matched))
	}
	if matched[0].Snapshot().Repo.Owner != "new-owner" || matched[0].Snapshot().Repo.Name != "new-name" {
		t.Fatalf("returned project not renamed: %+v", matched[0].Snapshot())
	}
	saved, _ := projects.Get(ctx, "p1")
	if saved.Snapshot().Repo.Owner != "new-owner" || saved.Snapshot().Repo.Name != "new-name" {
		t.Fatalf("persisted project not renamed: %+v", saved.Snapshot())
	}
}

func TestCreateAssignsRootFolder(t *testing.T) {
	ctx := context.Background()
	projects := &memProjects{items: map[project.ID]*project.Project{}}
	dirs := &memFolderDir{}
	svc := &Service{
		Projects: projects,
		Folders:  dirs,
		Now:      func() time.Time { return time.Unix(1, 0).UTC() },
		NewID:    func() string { return "p1" },
	}
	repo, err := project.ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, "", "infra", repo, 1, "", "terraform", false); err == nil {
		t.Fatal("create without a tenant must fail")
	}
	if dirs.roots != 0 {
		t.Fatal("empty tenant called EnsureRoot")
	}
	created, err := svc.Create(ctx, "ten", "infra", repo, 1, "", "terraform", false)
	if err != nil {
		t.Fatal(err)
	}
	if created.Snapshot().FolderID != "root-ten" || dirs.roots != 1 {
		t.Fatalf("folder id = %q roots = %d", created.Snapshot().FolderID, dirs.roots)
	}
}

func TestMoveToFolderClearsOverrideAndKeepsDrift(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(50, 0).UTC()
	projects := &memProjects{items: map[project.ID]*project.Project{}}
	dirs := &memFolderDir{items: map[string]folder.Snapshot{
		"dest":  {ID: "dest", TenantID: "ten", ParentID: "root", Name: "Dest"},
		"other": {ID: "other", TenantID: "other", ParentID: "root", Name: "Other"},
	}}
	svc := &Service{Projects: projects, Folders: dirs, Now: func() time.Time { return now }}
	p := mustFolderProject(t, "p1", "ten", "old")
	snap := p.Snapshot()
	snap.PolicyIDs = []string{"pol-b", "pol-a"}
	p = project.Restore(snap)
	if err := p.SetTagOverride(map[string]string{"env": "prod"}); err != nil {
		t.Fatal(err)
	}
	p.SetDriftConfig(true, 15, now)
	p.RecordDriftStatus(project.DriftStatusDrifted, now)
	projects.items["p1"] = p

	if err := svc.MoveToFolder(ctx, "p1", "dest", []string{"other-pol"}, false); err != nil {
		t.Fatal(err)
	}
	got := projects.items["p1"].Snapshot()
	if got.FolderID != "dest" || got.TagsOverridden || len(got.Tags) != 0 {
		t.Fatalf("moved = %#v", got)
	}
	if len(got.PolicyIDs) != 2 || got.PolicyIDs[0] != "pol-b" || got.PolicyIDs[1] != "pol-a" {
		t.Fatalf("policies = %#v", got.PolicyIDs)
	}
	if !got.DriftEnabled || got.DriftIntervalMinutes != 15 || got.DriftStatus != project.DriftStatusDrifted {
		t.Fatalf("drift = enabled %v interval %d status %q", got.DriftEnabled, got.DriftIntervalMinutes, got.DriftStatus)
	}

	if err := svc.MoveToFolder(ctx, "p1", "other", []string{"z", "a", "a"}, true); err == nil || err.Error() != "folder other not found" {
		t.Fatalf("other tenant: %v", err)
	}
	if projects.items["p1"].Snapshot().FolderID != "dest" {
		t.Fatal("rejected move changed the folder")
	}

	if err := svc.MoveToFolder(ctx, "p1", "dest", []string{" z ", "a", "a"}, true); err != nil {
		t.Fatal(err)
	}
	got = projects.items["p1"].Snapshot()
	if len(got.PolicyIDs) != 2 || got.PolicyIDs[0] != "a" || got.PolicyIDs[1] != "z" {
		t.Fatalf("replaced policies = %#v", got.PolicyIDs)
	}
	if !got.DriftEnabled || got.DriftIntervalMinutes != 15 || got.DriftStatus != project.DriftStatusDrifted {
		t.Fatalf("drift changed after policy replace: %#v", got)
	}
}

func TestDriftOnlySaveKeepsPoliciesAndTagOverride(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(60, 0).UTC()
	projects := &memProjects{items: map[project.ID]*project.Project{}}
	svc := &Service{Projects: projects, Now: func() time.Time { return now }}
	p := mustFolderProject(t, "p1", "ten", "root")
	p.SetPolicyIDs([]string{"pol-b", "pol-a"})
	if err := p.SetTagOverride(map[string]string{"env": "prod"}); err != nil {
		t.Fatal(err)
	}
	p.SetDriftConfig(false, 15, now)
	p.RecordDriftStatus(project.DriftStatusDrifted, now)
	projects.items["p1"] = p

	if err := svc.SetDriftConfig(ctx, "p1", true, 30); err != nil {
		t.Fatal(err)
	}
	got := projects.items["p1"].Snapshot()
	if !got.TagsOverridden || got.Tags["env"] != "prod" {
		t.Fatalf("tag override = %#v overridden=%v", got.Tags, got.TagsOverridden)
	}
	if len(got.PolicyIDs) != 2 || got.PolicyIDs[0] != "pol-a" || got.PolicyIDs[1] != "pol-b" {
		t.Fatalf("policies = %#v", got.PolicyIDs)
	}
	if !got.DriftEnabled || got.DriftIntervalMinutes != 30 || got.DriftStatus != project.DriftStatusDrifted {
		t.Fatalf("drift = enabled %v interval %d status %q", got.DriftEnabled, got.DriftIntervalMinutes, got.DriftStatus)
	}
}

type memFolderDir struct {
	items map[string]folder.Snapshot
	roots int
}

func (m *memFolderDir) EnsureRoot(_ context.Context, tenantID string) (folder.Snapshot, error) {
	m.roots++
	if m.items == nil {
		m.items = map[string]folder.Snapshot{}
	}
	for _, f := range m.items {
		if f.TenantID == tenantID && f.ParentID == "" {
			return f, nil
		}
	}
	f := folder.Snapshot{ID: "root-" + tenantID, TenantID: tenantID, Name: "Default", TagsSet: true, Tags: map[string]string{}}
	m.items[f.ID] = f
	return f, nil
}

func (m *memFolderDir) Get(_ context.Context, tenantID, id string) (folder.Snapshot, error) {
	f, ok := m.items[id]
	if !ok {
		return folder.Snapshot{}, fmt.Errorf("folder %s not found", id)
	}
	if f.TenantID != tenantID {
		return f, nil
	}
	return f, nil
}

func mustFolderProject(t *testing.T, id, tenantID, folderID string) *project.Project {
	t.Helper()
	repo, err := project.ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	p, err := project.New(project.ID(id), id, repo, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetPlacement(tenantID, 1); err != nil {
		t.Fatal(err)
	}
	p.SetFolderID(folderID)
	return p
}
