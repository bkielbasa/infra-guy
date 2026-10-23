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

func (m *memProjRepo) Save(_ context.Context, p *project.Project) error {
	m.items[p.Snapshot().ID] = project.Restore(p.Snapshot())
	return nil
}
func (m *memProjRepo) SetLocked(_ context.Context, id project.ID, locked bool) error {
	if p, ok := m.items[id]; ok {
		s := p.Snapshot()
		s.Locked = locked
		m.items[id] = project.Restore(s)
	}
	return nil
}
func (m *memProjRepo) ReleaseActiveRun(_ context.Context, id project.ID, runID string) error {
	if p, ok := m.items[id]; ok {
		_ = p.ReleaseActiveRun(runID)
	}
	return nil
}
func (m *memProjRepo) Get(_ context.Context, id project.ID) (*project.Project, error) {
	p, ok := m.items[id]
	if !ok {
		return nil, nil
	}
	return project.Restore(p.Snapshot()), nil
}
func (m *memProjRepo) List(_ context.Context, tenantID string) ([]*project.Project, error) {
	var out []*project.Project
	for _, p := range m.items {
		if p.Snapshot().TenantID == tenantID {
			out = append(out, project.Restore(p.Snapshot()))
		}
	}
	return out, nil
}
func (m *memProjRepo) FindByRepo(_ context.Context, _ string, _ int64) ([]*project.Project, error) {
	return nil, nil
}
func (m *memProjRepo) Delete(_ context.Context, id project.ID) error {
	delete(m.items, id)
	return nil
}

type memStateRepo struct {
	items map[string]*state.State
}

func (m *memStateRepo) Load(_ context.Context, projectID string) (*state.State, error) {
	if st, ok := m.items[projectID]; ok {
		return state.Restore(st.Snapshot()), nil
	}
	return nil, nil
}
func (m *memStateRepo) Save(_ context.Context, st *state.State) error {
	m.items[st.Snapshot().ProjectID] = state.Restore(st.Snapshot())
	return nil
}
func (m *memStateRepo) Outputs(ctx context.Context, projectID string) (map[string]state.OutputValue, error) {
	st, err := m.Load(ctx, projectID)
	if err != nil || st == nil {
		return nil, err
	}
	return st.Snapshot().Outputs(), nil
}
func (m *memStateRepo) ChangedOutputsSincePrevious(ctx context.Context, projectID string) (map[string]bool, error) {
	st, err := m.Load(ctx, projectID)
	if err != nil || st == nil {
		return nil, err
	}
	return st.Snapshot().ChangedOutputsSincePrevious(), nil
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

func newTestProject(id project.ID, tenantID, name string, repo project.SourceRepo, repoID int64, branch, workDir string, autoApprove bool, stateToken string, now time.Time) *project.Project {
	return project.Restore(project.Snapshot{
		ID:               id,
		TenantID:         tenantID,
		GitHubRepoID:     repoID,
		Name:             name,
		Repo:             repo,
		DefaultBranch:    branch,
		WorkingDirectory: workDir,
		TerraformVersion: project.DefaultTerraformVersion,
		AutoApprove:      autoApprove,
		StateToken:       stateToken,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
}

func TestServiceCycleDetectionAndResolution(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(100, 0).UTC()
	pNet := newTestProject("p-net", "ten-1", "network", project.SourceRepo{Owner: "o", Name: "r"}, 1, "main", "net", true, "t1", now)
	pDb := newTestProject("p-db", "ten-1", "database", project.SourceRepo{Owner: "o", Name: "r"}, 1, "main", "db", true, "t2", now)
	pApp := newTestProject("p-app", "ten-1", "app", project.SourceRepo{Owner: "o", Name: "r"}, 1, "main", "app", false, "t3", now)
	pOther := newTestProject("p-other", "ten-2", "other", project.SourceRepo{Owner: "o", Name: "r"}, 2, "main", "other", false, "t4", now)

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
