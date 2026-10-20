package runapp

import (
	"context"
	"testing"
	"time"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
)

type memProjects struct{ items map[project.ID]*project.Project }

func (m *memProjects) Save(_ context.Context, p *project.Project) error {
	m.items[p.Snapshot().ID] = project.Restore(p.Snapshot())
	return nil
}
func (m *memProjects) Get(_ context.Context, id project.ID) (*project.Project, error) {
	return m.items[id], nil
}
func (m *memProjects) List(context.Context) ([]*project.Project, error) { return nil, nil }
func (m *memProjects) Delete(_ context.Context, id project.ID) error {
	delete(m.items, id)
	return nil
}

type memRuns struct{ items map[run.ID]*run.Run }

func (m *memRuns) InsertIfAbsent(_ context.Context, r *run.Run) (bool, error) {
	snap := r.Snapshot()
	for _, existing := range m.items {
		es := existing.Snapshot()
		if es.ProjectID == snap.ProjectID && es.SHA == snap.SHA && es.Trigger == snap.Trigger && samePR(es.PRNumber, snap.PRNumber) && (es.Status == run.StatusQueued || es.Status == run.StatusRunning) {
			return false, nil
		}
	}
	m.items[snap.ID] = run.Restore(snap)
	return true, nil
}
func samePR(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
func (m *memRuns) Save(_ context.Context, r *run.Run) error {
	m.items[r.Snapshot().ID] = run.Restore(r.Snapshot())
	return nil
}
func (m *memRuns) Get(_ context.Context, id run.ID) (*run.Run, error) { return m.items[id], nil }
func (m *memRuns) ListByProject(context.Context, run.ProjectID) ([]*run.Run, error) {
	return nil, nil
}

type memSched struct{ steps []run.StepName }

func (m *memSched) Enqueue(_ context.Context, _ run.ID, step run.StepName) error {
	m.steps = append(m.steps, step)
	return nil
}

func TestOpenEnqueuesOnlyInit(t *testing.T) {
	ctx := context.Background()
	repo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New("p1", "infra", repo, time.Unix(0, 0).UTC())
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	runs := &memRuns{items: map[run.ID]*run.Run{}}
	sched := &memSched{}
	svc := &Service{Projects: projects, Runs: runs, Schedule: sched, Now: func() time.Time { return time.Unix(1, 0).UTC() }, NewID: func() string { return "run-1" }}
	opened, err := svc.Open(ctx, OpenRequest{ProjectID: "p1", Trigger: run.TriggerPullRequest, SHA: "abc", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if opened.Snapshot().PlanOnly != true || len(sched.steps) != 1 || sched.steps[0] != run.StepInit {
		t.Fatalf("steps %#v planOnly %v", sched.steps, opened.Snapshot().PlanOnly)
	}
	if _, err := svc.Open(ctx, OpenRequest{ProjectID: "p1", Trigger: run.TriggerPullRequest, SHA: "abc", PlanOnly: true}); err != ErrDuplicate {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestPlanOnlyAdvanceDoesNotEnqueueApply(t *testing.T) {
	ctx := context.Background()
	repo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New("p1", "infra", repo, time.Unix(0, 0).UTC())
	_ = p.ClaimActiveRun("run-1")
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	r, _ := run.Open(run.OpenInput{ID: "run-1", ProjectID: "p1", PlanOnly: true, Trigger: run.TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC()})
	_ = r.BeginStep(run.StepPlan, time.Unix(2, 0).UTC())
	runs := &memRuns{items: map[run.ID]*run.Run{"run-1": r}}
	sched := &memSched{}
	svc := &Service{Projects: projects, Runs: runs, Schedule: sched, Now: func() time.Time { return time.Unix(3, 0).UTC() }}
	err := svc.Advance(ctx, "run-1", run.StepPlan, run.StepResult{PlanText: "Plan: 1 to add, 0 to change, 0 to destroy.\n"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sched.steps) != 0 {
		t.Fatalf("enqueued %#v", sched.steps)
	}
	if projects.items["p1"].Snapshot().ActiveRunID != "" {
		t.Fatal("active run not released")
	}
	if runs.items["run-1"].Snapshot().Status != run.StatusPlanned {
		t.Fatal(runs.items["run-1"].Snapshot().Status)
	}
}

func beginStep(t *testing.T, r *run.Run, name run.StepName, now time.Time) {
	t.Helper()
	if err := r.BeginStep(name, now); err != nil {
		t.Fatal(err)
	}
}

func completeStep(t *testing.T, r *run.Run, name run.StepName, result run.StepResult, now time.Time) {
	t.Helper()
	if _, err := r.CompleteStep(name, result, now); err != nil {
		t.Fatal(err)
	}
}

func needsApprovalRun(t *testing.T) (*run.Run, time.Time) {
	t.Helper()
	now := time.Unix(10, 0).UTC()
	r, err := run.Open(run.OpenInput{
		ID: "run-1", ProjectID: "p1", AutoApprove: false, PlanOnly: false,
		Trigger: run.TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	beginStep(t, r, run.StepInit, now)
	completeStep(t, r, run.StepInit, run.StepResult{}, now)
	beginStep(t, r, run.StepPlan, now)
	completeStep(t, r, run.StepPlan, run.StepResult{PlanText: "Plan: 1 to add, 0 to change, 0 to destroy.\n"}, now)
	if r.Snapshot().Status != run.StatusNeedsApproval {
		t.Fatalf("status %s", r.Snapshot().Status)
	}
	return r, now
}

func testService(t *testing.T, p *project.Project, r *run.Run) (*Service, *memSched, *memProjects) {
	t.Helper()
	if err := p.ClaimActiveRun(string(r.Snapshot().ID)); err != nil {
		t.Fatal(err)
	}
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	runs := &memRuns{items: map[run.ID]*run.Run{r.Snapshot().ID: r}}
	sched := &memSched{}
	svc := &Service{
		Projects: projects, Runs: runs, Schedule: sched,
		Now: func() time.Time { return time.Unix(20, 0).UTC() },
	}
	return svc, sched, projects
}

func TestConfirmNeedsApprovalEnqueuesApplyKeepsActiveRun(t *testing.T) {
	ctx := context.Background()
	repo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New("p1", "infra", repo, time.Unix(0, 0).UTC())
	r, _ := needsApprovalRun(t)
	svc, sched, projects := testService(t, p, r)
	if err := svc.Confirm(ctx, r.Snapshot().ID, "operator"); err != nil {
		t.Fatal(err)
	}
	snap := runsFromService(t, svc, r.Snapshot().ID).Snapshot()
	if snap.ApprovedBy != "operator" || snap.Status != run.StatusRunning {
		t.Fatalf("snap %#v", snap)
	}
	if len(sched.steps) != 1 || sched.steps[0] != run.StepApply {
		t.Fatalf("steps %#v", sched.steps)
	}
	if projects.items["p1"].Snapshot().ActiveRunID != "run-1" {
		t.Fatalf("active run %q", projects.items["p1"].Snapshot().ActiveRunID)
	}
}

func runsFromService(t *testing.T, svc *Service, id run.ID) *run.Run {
	t.Helper()
	r, err := svc.Runs.Get(context.Background(), id)
	if err != nil || r == nil {
		t.Fatal(err)
	}
	return r
}

func TestCancelNeedsApprovalReleasesActiveRun(t *testing.T) {
	ctx := context.Background()
	repo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New("p1", "infra", repo, time.Unix(0, 0).UTC())
	r, _ := needsApprovalRun(t)
	svc, sched, projects := testService(t, p, r)
	if err := svc.Cancel(ctx, r.Snapshot().ID); err != nil {
		t.Fatal(err)
	}
	snap := runsFromService(t, svc, r.Snapshot().ID).Snapshot()
	if snap.Status != run.StatusCancelled {
		t.Fatalf("status %s", snap.Status)
	}
	if len(sched.steps) != 0 {
		t.Fatalf("enqueued %#v", sched.steps)
	}
	if projects.items["p1"].Snapshot().ActiveRunID != "" {
		t.Fatalf("active run %q", projects.items["p1"].Snapshot().ActiveRunID)
	}
}

func TestFailReleasesActiveRun(t *testing.T) {
	ctx := context.Background()
	repo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New("p1", "infra", repo, time.Unix(0, 0).UTC())
	r, err := run.Open(run.OpenInput{
		ID: "run-1", ProjectID: "p1", Trigger: run.TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	beginStep(t, r, run.StepInit, time.Unix(1, 0).UTC())
	svc, sched, projects := testService(t, p, r)
	if err := svc.Fail(ctx, r.Snapshot().ID, run.StepInit, "boom"); err != nil {
		t.Fatal(err)
	}
	snap := runsFromService(t, svc, r.Snapshot().ID).Snapshot()
	if snap.Status != run.StatusFailed {
		t.Fatalf("status %s", snap.Status)
	}
	if len(sched.steps) != 0 {
		t.Fatalf("enqueued %#v", sched.steps)
	}
	if projects.items["p1"].Snapshot().ActiveRunID != "" {
		t.Fatalf("active run %q", projects.items["p1"].Snapshot().ActiveRunID)
	}
}

func TestAdvanceInitSuccessEnqueuesPlanKeepsActiveRun(t *testing.T) {
	ctx := context.Background()
	repo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New("p1", "infra", repo, time.Unix(0, 0).UTC())
	r, err := run.Open(run.OpenInput{
		ID: "run-1", ProjectID: "p1", Trigger: run.TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	beginStep(t, r, run.StepInit, time.Unix(1, 0).UTC())
	svc, sched, projects := testService(t, p, r)
	if err := svc.Advance(ctx, r.Snapshot().ID, run.StepInit, run.StepResult{}); err != nil {
		t.Fatal(err)
	}
	if len(sched.steps) != 1 || sched.steps[0] != run.StepPlan {
		t.Fatalf("steps %#v", sched.steps)
	}
	if projects.items["p1"].Snapshot().ActiveRunID != "run-1" {
		t.Fatalf("active run %q", projects.items["p1"].Snapshot().ActiveRunID)
	}
}
