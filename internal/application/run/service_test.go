package runapp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
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
func (m *memProjects) FindByRepo(context.Context, string, int64) ([]*project.Project, error) {
	return nil, nil
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

type memSched struct {
	steps []run.StepName
	err   error
}

func (m *memSched) Enqueue(_ context.Context, _ run.ID, step run.StepName) error {
	if m.err != nil {
		return m.err
	}
	m.steps = append(m.steps, step)
	return nil
}

func TestOpenEnqueueFailureAllowsRetry(t *testing.T) {
	ctx := context.Background()
	repo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New("p1", "infra", repo, time.Unix(0, 0).UTC())
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	runs := &memRuns{items: map[run.ID]*run.Run{}}
	sched := &memSched{err: errors.New("queue down")}
	ids := []string{"run-1", "run-2"}
	svc := &Service{Projects: projects, Runs: runs, Schedule: sched, Now: func() time.Time { return time.Unix(1, 0).UTC() },
		NewID: func() string { id := ids[0]; ids = ids[1:]; return id }}
	in := OpenRequest{ProjectID: "p1", Trigger: run.TriggerPush, SHA: "abc"}
	if _, err := svc.Open(ctx, in); err == nil || !strings.Contains(err.Error(), "queue down") {
		t.Fatalf("open error %v", err)
	}
	if st := runs.items["run-1"].Snapshot().Status; st != run.StatusFailed {
		t.Fatalf("failed enqueue left run %s", st)
	}
	sched.err = nil
	retried, err := svc.Open(ctx, in)
	if err != nil || retried.Snapshot().ID != "run-2" || len(sched.steps) != 1 || sched.steps[0] != run.StepInit {
		t.Fatalf("retry %v %#v", err, sched.steps)
	}
}

func TestAdvanceEnqueueFailureFailsRunAndReleases(t *testing.T) {
	ctx := context.Background()
	repo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New("p1", "infra", repo, time.Unix(0, 0).UTC())
	r, err := run.Open(run.OpenInput{
		ID: "run-1", ProjectID: "p1", Trigger: run.TriggerPush, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	beginStep(t, r, run.StepInit, time.Unix(1, 0).UTC())
	svc, sched, projects := testService(t, p, r)
	sched.err = errors.New("queue down")
	if err := svc.Advance(ctx, "run-1", run.StepInit, run.StepResult{}); err == nil {
		t.Fatal("expected enqueue error")
	}
	snap := runsFromService(t, svc, "run-1").Snapshot()
	if snap.Status != run.StatusFailed || !strings.Contains(snap.Error, "queue down") {
		t.Fatalf("run %s %q", snap.Status, snap.Error)
	}
	if projects.items["p1"].Snapshot().ActiveRunID != "" {
		t.Fatalf("active run %q", projects.items["p1"].Snapshot().ActiveRunID)
	}
}

func TestConfirmEnqueueFailureFailsRun(t *testing.T) {
	ctx := context.Background()
	repo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New("p1", "infra", repo, time.Unix(0, 0).UTC())
	r, _ := needsApprovalRun(t)
	svc, sched, projects := testService(t, p, r)
	sched.err = errors.New("queue down")
	if err := svc.Confirm(ctx, "run-1", "operator"); err == nil {
		t.Fatal("expected enqueue error")
	}
	if st := runsFromService(t, svc, "run-1").Snapshot().Status; st != run.StatusFailed {
		t.Fatalf("status %s", st)
	}
	if projects.items["p1"].Snapshot().ActiveRunID != "" {
		t.Fatalf("active run %q", projects.items["p1"].Snapshot().ActiveRunID)
	}
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
	if err := svc.Cancel(ctx, r.Snapshot().ID, "ada"); err != nil {
		t.Fatal(err)
	}
	snap := runsFromService(t, svc, r.Snapshot().ID).Snapshot()
	if snap.Status != run.StatusCancelled || snap.ApprovedBy != "ada" || snap.ApprovedAt == nil || !snap.ApprovedAt.Equal(time.Unix(20, 0).UTC()) {
		t.Fatalf("%#v", snap)
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

type recordedStatus struct {
	sha    string
	report StatusReport
}

type fakeStatusReporter struct {
	calls []recordedStatus
}

func (f *fakeStatusReporter) ReportCommitStatus(_ context.Context, _ project.Snapshot, sha string, status StatusReport) error {
	f.calls = append(f.calls, recordedStatus{sha: sha, report: status})
	return nil
}

type fakeMergeability struct {
	fn func(p project.Snapshot, r run.Snapshot) bool
}

func (f fakeMergeability) IsMergeable(_ context.Context, p project.Snapshot, r run.Snapshot) bool {
	return f.fn(p, r)
}

func TestPRCommitStatusLifecycle(t *testing.T) {
	ctx := context.Background()
	repo, _ := project.ParseSourceRepo("bkielbasa/infra")
	p, _ := project.New("p1", "infra", repo, time.Unix(0, 0).UTC())
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	runs := &memRuns{items: map[run.ID]*run.Run{}}
	sched := &memSched{}
	reporter := &fakeStatusReporter{}
	svc := &Service{
		Projects:       projects,
		Runs:           runs,
		Schedule:       sched,
		StatusReporter: reporter,
		PublicBaseURL:  "https://infra.example.com",
		Mergeability: fakeMergeability{fn: func(_ project.Snapshot, r run.Snapshot) bool {
			return len(r.Approvals) >= 2
		}},
		Now:   func() time.Time { return time.Unix(1, 0).UTC() },
		NewID: func() string { return "run-1" },
	}

	// 1. Open PR run -> pending "In progress"
	opened, err := svc.Open(ctx, OpenRequest{
		ProjectID: "p1", Trigger: run.TriggerPullRequest, SHA: "sha123", PlanOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reporter.calls) != 1 {
		t.Fatalf("expected 1 status call after Open, got %d", len(reporter.calls))
	}
	if got := reporter.calls[0]; got.sha != "sha123" || got.report.State != "pending" || got.report.Description != "In progress" || got.report.Context != "infra-guy/infra" || got.report.TargetURL != "https://infra.example.com/runs/run-1" {
		t.Fatalf("after Open: %+v", got)
	}

	// 2. Advance init -> no redundant status call, still in progress
	r := runs.items[opened.Snapshot().ID]
	beginStep(t, r, run.StepInit, time.Unix(2, 0).UTC())
	_ = runs.Save(ctx, r)
	if err := svc.Advance(ctx, "run-1", run.StepInit, run.StepResult{}); err != nil {
		t.Fatal(err)
	}
	if len(reporter.calls) != 1 {
		t.Fatalf("expected no extra status call after StepInit, got %d", len(reporter.calls))
	}

	// 3. Advance plan with changes (3 add, 3 destroy) -> pending "[add: 3, delete: 3]"
	r = runs.items["run-1"]
	beginStep(t, r, run.StepPlan, time.Unix(3, 0).UTC())
	_ = runs.Save(ctx, r)
	if err := svc.Advance(ctx, "run-1", run.StepPlan, run.StepResult{
		PlanText: "Plan: 3 to add, 0 to change, 3 to destroy.\n",
	}); err != nil {
		t.Fatal(err)
	}
	if len(reporter.calls) != 2 {
		t.Fatalf("expected 2 status calls after StepPlan, got %d", len(reporter.calls))
	}
	if got := reporter.calls[1]; got.report.State != "pending" || got.report.Description != "[add: 3, delete: 3]" {
		t.Fatalf("after StepPlan: %+v", got)
	}

	// 4. First approval (policy requires 2) -> still pending "[add: 3, delete: 3]"
	if err := svc.Approve(ctx, "run-1", run.Approval{UserID: "u1", Login: "alice"}); err != nil {
		t.Fatal(err)
	}
	if len(reporter.calls) != 3 {
		t.Fatalf("expected 3 status calls after first Approve, got %d", len(reporter.calls))
	}
	if got := reporter.calls[2]; got.report.State != "pending" || got.report.Description != "[add: 3, delete: 3]" {
		t.Fatalf("after first Approve: %+v", got)
	}

	// 5. Second approval (policy now allows!) -> success "[add: 3, delete: 3]"
	if err := svc.Approve(ctx, "run-1", run.Approval{UserID: "u2", Login: "bob"}); err != nil {
		t.Fatal(err)
	}
	if len(reporter.calls) != 4 {
		t.Fatalf("expected 4 status calls after second Approve, got %d", len(reporter.calls))
	}
	if got := reporter.calls[3]; got.report.State != "success" || got.report.Description != "[add: 3, delete: 3]" {
		t.Fatalf("after second Approve: %+v", got)
	}

	// 6. Unapprove bob -> back to pending "[add: 3, delete: 3]"
	if err := svc.Unapprove(ctx, "run-1", "bob", "u2"); err != nil {
		t.Fatal(err)
	}
	if len(reporter.calls) != 5 {
		t.Fatalf("expected 5 status calls after Unapprove, got %d", len(reporter.calls))
	}
	if got := reporter.calls[4]; got.report.State != "pending" || got.report.Description != "[add: 3, delete: 3]" {
		t.Fatalf("after Unapprove: %+v", got)
	}

	// 7. Cancel/decline planned PR run -> failure "Cancelled"
	if err := svc.Cancel(ctx, "run-1", "alice"); err != nil {
		t.Fatal(err)
	}
	if len(reporter.calls) != 6 {
		t.Fatalf("expected 6 status calls after Cancel, got %d", len(reporter.calls))
	}
	if got := reporter.calls[5]; got.report.State != "failure" || got.report.Description != "Cancelled" {
		t.Fatalf("after Cancel: %+v", got)
	}
}

func TestPRCommitStatusEmptyPlanAndFail(t *testing.T) {
	ctx := context.Background()
	repo, _ := project.ParseSourceRepo("bkielbasa/infra")
	p, _ := project.New("p1", "infra", repo, time.Unix(0, 0).UTC())
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	runs := &memRuns{items: map[run.ID]*run.Run{}}
	sched := &memSched{}
	reporter := &fakeStatusReporter{}
	ids := []string{"run-empty", "run-fail"}
	svc := &Service{
		Projects:       projects,
		Runs:           runs,
		Schedule:       sched,
		StatusReporter: reporter,
		PublicBaseURL:  "https://infra.example.com",
		Now:            func() time.Time { return time.Unix(1, 0).UTC() },
		NewID:          func() string { id := ids[0]; ids = ids[1:]; return id },
	}

	// Empty plan -> immediately success "No changes"
	if _, err := svc.Open(ctx, OpenRequest{ProjectID: "p1", Trigger: run.TriggerPullRequest, SHA: "sha-empty", PlanOnly: true}); err != nil {
		t.Fatal(err)
	}
	r := runs.items["run-empty"]
	beginStep(t, r, run.StepPlan, time.Unix(2, 0).UTC())
	_ = runs.Save(ctx, r)
	if err := svc.Advance(ctx, "run-empty", run.StepPlan, run.StepResult{
		PlanText: "No changes. Your infrastructure matches the configuration.\n",
	}); err != nil {
		t.Fatal(err)
	}
	last := reporter.calls[len(reporter.calls)-1]
	if last.sha != "sha-empty" || last.report.State != "success" || last.report.Description != "No changes" {
		t.Fatalf("empty plan status: %+v", last)
	}

	// Failed plan -> failure "Plan failed"
	if _, err := svc.Open(ctx, OpenRequest{ProjectID: "p1", Trigger: run.TriggerPullRequest, SHA: "sha-fail", PlanOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Fail(ctx, "run-fail", run.StepPlan, "boom"); err != nil {
		t.Fatal(err)
	}
	last = reporter.calls[len(reporter.calls)-1]
	if last.sha != "sha-fail" || last.report.State != "failure" || last.report.Description != "Plan failed" {
		t.Fatalf("failed plan status: %+v", last)
	}
}

func TestBeginStepStartsAndSavesRun(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(100, 0).UTC()
	repo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New("p1", "infra", repo, now)
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	runs := &memRuns{items: map[run.ID]*run.Run{}}
	sched := &memSched{}
	svc := &Service{
		Projects: projects,
		Runs:     runs,
		Schedule: sched,
		Now:      func() time.Time { return now },
		NewID:    func() string { return "r1" },
	}
	if _, err := svc.Open(ctx, OpenRequest{
		ProjectID: "p1",
		Trigger:   run.TriggerPush,
		SHA:       "abc1234",
		Branch:    "main",
	}); err != nil {
		t.Fatal(err)
	}

	r, err := svc.BeginStep(ctx, "r1", run.StepInit)
	if err != nil {
		t.Fatalf("BeginStep: %v", err)
	}
	if r.Snapshot().Status != run.StatusRunning {
		t.Fatalf("got status %s, want running", r.Snapshot().Status)
	}
	saved, _ := runs.Get(ctx, "r1")
	if saved.Snapshot().Status != run.StatusRunning {
		t.Fatalf("saved status %s, want running", saved.Snapshot().Status)
	}

	// Calling BeginStep when already running is a no-op and returns r
	r2, err := svc.BeginStep(ctx, "r1", run.StepInit)
	if err != nil || r2.Snapshot().Status != run.StatusRunning {
		t.Fatalf("BeginStep already running: %v, %+v", err, r2)
	}
}


