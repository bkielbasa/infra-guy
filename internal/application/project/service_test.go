package projectapp

import (
	"context"
	"testing"
	"time"

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
func (m *memProjects) List(context.Context) ([]*project.Project, error) { return nil, nil }
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
	return state.New(projectID), nil
}

func (m *memStates) Save(_ context.Context, s *state.State) error {
	m.items[s.Snapshot().ProjectID] = state.Restore(s.Snapshot())
	return nil
}

func TestCreateUpdateAndForceUnlock(t *testing.T) {
	ctx := context.Background()
	projects := &memProjects{items: map[project.ID]*project.Project{}}
	states := &memStates{items: map[string]*state.State{}}
	svc := &Service{Projects: projects, Now: func() time.Time { return time.Unix(1, 0).UTC() }, NewID: func() string { return "p1" }}
	created, err := svc.Create(ctx, "infra", "bklebasa/infra", "", "terraform", false)
	if err != nil {
		t.Fatal(err)
	}
	snap := created.Snapshot()
	if snap.WebhookSecret == "" || snap.StateToken == "" || snap.DefaultBranch != "main" || snap.WorkingDirectory != "terraform" {
		t.Fatalf("%#v", snap)
	}
	updated, err := svc.Update(ctx, UpdateInput{ID: "p1", Token: ""})
	if err != nil || updated.Snapshot().StateToken != snap.StateToken {
		t.Fatal(err)
	}
	if updated.Snapshot().WorkingDirectory != "" {
		t.Fatalf("empty workdir must mean repository root, got %q", updated.Snapshot().WorkingDirectory)
	}
	if err := svc.SetLocked(ctx, "p1", true); err != nil || !projects.items["p1"].Snapshot().Locked {
		t.Fatalf("lock: %v", err)
	}
	if err := created.ClaimActiveRun("run-1"); err != nil {
		t.Fatal(err)
	}
	_ = projects.Save(ctx, created)
	st := state.New("p1")
	_, _ = st.Lock([]byte(`{"ID":"L1"}`))
	_ = states.Save(ctx, st)
	if err := svc.ForceUnlock(ctx, "p1", states); err != ErrActiveRun {
		t.Fatalf("force unlock: %v", err)
	}
	if states.items["p1"].Snapshot().LockID != "L1" {
		t.Fatal("lock cleared while a run was active")
	}
	_ = created.ReleaseActiveRun("run-1")
	_ = projects.Save(ctx, created)
	if err := svc.ForceUnlock(ctx, "p1", states); err != nil {
		t.Fatal(err)
	}
	if states.items["p1"].Snapshot().LockID != "" {
		t.Fatal("lock remains")
	}
}
