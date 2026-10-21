package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runapp "infra-guy/internal/application/run"
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
	m.items[r.Snapshot().ID] = run.Restore(r.Snapshot())
	return true, nil
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

type fakeQueue struct {
	claim      *Job
	requeue    []Job
	failed     []Job
	completed  []int64
	staleAfter time.Duration
}

func (q *fakeQueue) Claim(context.Context, string, project.Repository) (*Job, error) {
	return q.claim, nil
}
func (q *fakeQueue) Heartbeat(context.Context, int64) error { return nil }
func (q *fakeQueue) Complete(_ context.Context, jobID int64) error {
	q.completed = append(q.completed, jobID)
	return nil
}
func (q *fakeQueue) Reap(_ context.Context, staleAfter time.Duration) ([]Job, []Job, error) {
	q.staleAfter = staleAfter
	return q.requeue, q.failed, nil
}

type fakeCheckout struct {
	t                     *testing.T
	sha, dest, url, token string
	err                   error
}

func (c *fakeCheckout) Clone(_ context.Context, dest, cloneURL, token, sha string) error {
	c.dest, c.url, c.token, c.sha = dest, cloneURL, token, sha
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		c.t.Fatalf("clone dest %q still exists: %v", dest, err)
	}
	return c.err
}

type fakeExecutor struct {
	called bool
	req    Request
	res    Result
	err    error
	onRun  func(context.Context)
}

func (e *fakeExecutor) Run(ctx context.Context, req Request) (Result, error) {
	e.called = true
	e.req = req
	if e.onRun != nil {
		e.onRun(ctx)
	}
	return e.res, e.err
}

func testNow() time.Time { return time.Unix(50, 0).UTC() }

type tokenSourceFunc func(ctx context.Context, tenantID string) (string, error)

func (f tokenSourceFunc) InstallationToken(ctx context.Context, tenantID string) (string, error) {
	return f(ctx, tenantID)
}

func testProject(t *testing.T) *project.Project {
	t.Helper()
	repo, err := project.ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	p, err := project.New("p1", "infra", repo, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetPlacement("ten-1", 1); err != nil {
		t.Fatal(err)
	}
	p.SetStateToken("state-token")
	return p
}

func beginComplete(t *testing.T, r *run.Run, name run.StepName, result run.StepResult, now time.Time) {
	t.Helper()
	if err := r.BeginStep(name, now); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CompleteStep(name, result, now); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	w     *Worker
	queue *fakeQueue
	co    *fakeCheckout
	exec  *fakeExecutor
	runs  *memRuns
	sched *memSched
	ws    string
}

func newFixture(t *testing.T, r *run.Run, p *project.Project, q *fakeQueue, exec *fakeExecutor) *fixture {
	t.Helper()
	work := t.TempDir()
	ws := filepath.Join(work, string(r.Snapshot().ID))
	projects := &memProjects{items: map[project.ID]*project.Project{p.Snapshot().ID: p}}
	runs := &memRuns{items: map[run.ID]*run.Run{r.Snapshot().ID: r}}
	sched := &memSched{}
	svc := &runapp.Service{
		Projects: projects, Runs: runs, Schedule: sched, Now: testNow,
	}
	co := &fakeCheckout{t: t}
	if exec == nil {
		exec = &fakeExecutor{}
	}
	if q == nil {
		q = &fakeQueue{}
	}
	w := &Worker{
		WorkerID: "w1", Queue: q, Projects: projects, Runs: runs, Service: svc,
		Checkout: co, Executor: exec, WorkDir: work, InternalBaseURL: "http://127.0.0.1:8759",
		Now: testNow,
		Tokens: tokenSourceFunc(func(ctx context.Context, tenantID string) (string, error) {
			if tenantID != "ten-1" {
				t.Fatalf("tenant %s", tenantID)
			}
			return "ghs_install", nil
		}),
	}
	return &fixture{w: w, queue: q, co: co, exec: exec, runs: runs, sched: sched, ws: ws}
}

func TestStepTimeout(t *testing.T) {
	if HeartbeatEvery != 20*time.Second || StaleAfter != 2*time.Minute {
		t.Fatalf("heartbeat %s stale %s", HeartbeatEvery, StaleAfter)
	}
	if StepTimeout(run.StepInit) != 10*time.Minute || StepTimeout(run.StepPlan) != 20*time.Minute || StepTimeout(run.StepApply) != 60*time.Minute {
		t.Fatal("timeouts")
	}
}

func TestTickPlanOnlyAdvancesAndDeletesWorkspace(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	r, err := run.Open(run.OpenInput{
		ID: "run-1", ProjectID: "p1", PlanOnly: true, Trigger: run.TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	beginComplete(t, r, run.StepInit, run.StepResult{}, time.Unix(1, 0).UTC())
	exec := &fakeExecutor{res: Result{
		ExitCode: 0, PlanText: "Plan: 1 to add, 0 to change, 0 to destroy.", Version: "1.9.8",
	}}
	fx := newFixture(t, r, p, &fakeQueue{claim: &Job{ID: 3, RunID: "run-1", Step: run.StepPlan}}, exec)
	fx.exec.onRun = func(ctx context.Context) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("executor context missing deadline")
		}
		saved, err := fx.runs.Get(ctx, "run-1")
		if err != nil || saved == nil {
			t.Fatal(err)
		}
		st := saved.Snapshot().Steps
		var plan run.Step
		for _, s := range st {
			if s.Name == run.StepPlan {
				plan = s
			}
		}
		if plan.Status != run.StepRunning {
			t.Fatalf("plan status %s, want running", plan.Status)
		}
	}
	if err := os.MkdirAll(fx.ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.ws, "stale.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	saved, err := fx.runs.Get(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	snap := saved.Snapshot()
	if snap.Status != run.StatusPlanned {
		t.Fatalf("status %s", snap.Status)
	}
	if snap.TerraformVersion != "1.9.8" {
		t.Fatalf("version %q", snap.TerraformVersion)
	}
	if fx.co.sha != "abc" {
		t.Fatalf("cloned sha %q", fx.co.sha)
	}
	if fx.co.dest != fx.ws || fx.co.token != "ghs_install" || fx.co.url != p.Snapshot().Repo.CloneURL() {
		t.Fatalf("clone dest=%q url=%q token=%q", fx.co.dest, fx.co.url, fx.co.token)
	}
	if !fx.exec.called {
		t.Fatal("executor not called")
	}
	if fx.exec.req.WorkDir != fx.ws || fx.exec.req.InternalBaseURL != "http://127.0.0.1:8759" || fx.exec.req.ProjectID != "p1" || fx.exec.req.StateToken != "state-token" || fx.exec.req.Step != run.StepPlan ||
		fx.exec.req.RunID != "run-1" || fx.exec.req.GitHubToken != "ghs_install" {
		t.Fatalf("executor req %#v", fx.exec.req)
	}
	if _, err := os.Stat(fx.ws); !os.IsNotExist(err) {
		t.Fatalf("workspace still present: %v", err)
	}
	if len(fx.queue.completed) != 1 || fx.queue.completed[0] != 3 {
		t.Fatalf("complete %#v", fx.queue.completed)
	}
}

func TestTickApplyWithoutPlanFileFailsWithoutExecutor(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	r, err := run.Open(run.OpenInput{
		ID: "run-1", ProjectID: "p1", AutoApprove: true, Trigger: run.TriggerPush, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1, 0).UTC()
	beginComplete(t, r, run.StepInit, run.StepResult{}, now)
	beginComplete(t, r, run.StepPlan, run.StepResult{PlanText: "Plan: 1 to add, 0 to change, 0 to destroy."}, now)
	exec := &fakeExecutor{res: Result{ExitCode: 0}}
	fx := newFixture(t, r, p, &fakeQueue{claim: &Job{ID: 4, RunID: "run-1", Step: run.StepApply}}, exec)
	if err := os.MkdirAll(fx.ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if fx.exec.called {
		t.Fatal("executor called")
	}
	if len(fx.sched.steps) != 0 {
		t.Fatalf("enqueued %#v", fx.sched.steps)
	}
	saved, err := fx.runs.Get(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	snap := saved.Snapshot()
	if snap.Status != run.StatusFailed {
		t.Fatalf("status %s", snap.Status)
	}
	if !strings.Contains(snap.Error, "apply requires a stored plan file") {
		t.Fatalf("error %q", snap.Error)
	}
	if len(fx.queue.completed) != 1 || fx.queue.completed[0] != 4 {
		t.Fatalf("complete %#v", fx.queue.completed)
	}
	if _, err := os.Stat(fx.ws); !os.IsNotExist(err) {
		t.Fatalf("workspace still present: %v", err)
	}
}

func TestTickClaimedPlanAlreadyRunningFinishesPlanned(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	r, err := run.Open(run.OpenInput{
		ID: "run-1", ProjectID: "p1", PlanOnly: true, Trigger: run.TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	beginComplete(t, r, run.StepInit, run.StepResult{}, time.Unix(1, 0).UTC())
	if err := r.BeginStep(run.StepPlan, time.Unix(2, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{res: Result{
		ExitCode: 0, PlanText: "Plan: 1 to add, 0 to change, 0 to destroy.", Version: "1.9.8",
	}}
	fx := newFixture(t, r, p, &fakeQueue{claim: &Job{ID: 3, RunID: "run-1", Step: run.StepPlan}}, exec)
	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	saved, err := fx.runs.Get(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Snapshot().Status != run.StatusPlanned {
		t.Fatalf("status %s", saved.Snapshot().Status)
	}
}

func TestTickStepTimeoutFailsWithoutAdvance(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	r, err := run.Open(run.OpenInput{
		ID: "run-1", ProjectID: "p1", PlanOnly: true, Trigger: run.TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	beginComplete(t, r, run.StepInit, run.StepResult{}, time.Unix(1, 0).UTC())
	exec := &fakeExecutor{
		res: Result{ExitCode: 1},
		onRun: func(ctx context.Context) {
			<-ctx.Done()
		},
	}
	fx := newFixture(t, r, p, &fakeQueue{claim: &Job{ID: 5, RunID: "run-1", Step: run.StepPlan}}, exec)
	fx.w.Timeout = func(run.StepName) time.Duration { return 30 * time.Millisecond }
	start := time.Now()
	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("elapsed %s", elapsed)
	}
	saved, err := fx.runs.Get(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	snap := saved.Snapshot()
	if snap.Status != run.StatusFailed {
		t.Fatalf("status %s", snap.Status)
	}
	if snap.Error != "step timed out" {
		t.Fatalf("error %q", snap.Error)
	}
	for _, st := range snap.Steps {
		if st.Name == run.StepPlan && st.ExitCode != nil {
			t.Fatalf("plan step exit code set via Advance: %#v", st.ExitCode)
		}
	}
	if len(fx.queue.completed) != 1 || fx.queue.completed[0] != 5 {
		t.Fatalf("complete %#v", fx.queue.completed)
	}
}

func TestTickReapFailedApplyCallsFail(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	r, err := run.Open(run.OpenInput{
		ID: "run-1", ProjectID: "p1", Trigger: run.TriggerPush, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fx := newFixture(t, r, p, &fakeQueue{failed: []Job{{ID: 7, RunID: "run-1", Step: run.StepApply}}}, nil)
	if err := os.MkdirAll(fx.ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if fx.queue.staleAfter != StaleAfter {
		t.Fatalf("staleAfter %s", fx.queue.staleAfter)
	}
	if len(fx.queue.completed) != 0 {
		t.Fatalf("complete called %#v", fx.queue.completed)
	}
	if fx.exec.called {
		t.Fatal("executor called")
	}
	saved, err := fx.runs.Get(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	snap := saved.Snapshot()
	if snap.Status != run.StatusFailed {
		t.Fatalf("status %s", snap.Status)
	}
	if snap.Error != "worker lost the apply; check state before starting another run" {
		t.Fatalf("error %q", snap.Error)
	}
	if _, err := os.Stat(fx.ws); !os.IsNotExist(err) {
		t.Fatalf("workspace still present: %v", err)
	}
}

func TestLoopStopsWhenContextDone(t *testing.T) {
	r, err := run.Open(run.OpenInput{ID: "run-1", ProjectID: "p1", SHA: "abc", Now: testNow()})
	if err != nil {
		t.Fatal(err)
	}
	fx := newFixture(t, r, testProject(t), &fakeQueue{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		fx.w.Loop(ctx)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Loop did not return")
	}
}

func TestExecuteTokenErrorFailsBeforeClone(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	r, err := run.Open(run.OpenInput{
		ID: "run-1", ProjectID: "p1", PlanOnly: true, Trigger: run.TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fx := newFixture(t, r, p, &fakeQueue{claim: &Job{ID: 3, RunID: "run-1", Step: run.StepPlan}}, nil)
	fx.w.Tokens = tokenSourceFunc(func(ctx context.Context, tenantID string) (string, error) {
		return "", errors.New("installation inactive")
	})

	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	if fx.co.dest != "" {
		t.Fatalf("clone was called with dest: %s", fx.co.dest)
	}
	if fx.exec.called {
		t.Fatal("executor was called")
	}

	saved, err := fx.runs.Get(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	snap := saved.Snapshot()
	if snap.Status != run.StatusFailed {
		t.Fatalf("status %s, want failed", snap.Status)
	}
	if snap.Error != "installation inactive" {
		t.Fatalf("error %q, want installation inactive", snap.Error)
	}
	if len(fx.queue.completed) != 1 || fx.queue.completed[0] != 3 {
		t.Fatalf("completed jobs %#v, want [3]", fx.queue.completed)
	}
}

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

func TestTickContextResolutionErrorFailsWithoutExecutor(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	r, err := run.Open(run.OpenInput{
		ID: "run-ctx-err", ProjectID: "p1", PlanOnly: true, Trigger: run.TriggerManual, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{res: Result{ExitCode: 0}}
	fx := newFixture(t, r, p, &fakeQueue{claim: &Job{ID: 10, RunID: "run-ctx-err", Step: run.StepInit}}, exec)
	fx.w.Contexts = fakeContextResolver{
		err: errors.New("decrypt failed"),
	}
	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if fx.exec.called {
		t.Fatal("executor should not be called on context resolution error")
	}
	saved, err := fx.runs.Get(ctx, "run-ctx-err")
	if err != nil {
		t.Fatal(err)
	}
	snap := saved.Snapshot()
	if snap.Status != run.StatusFailed {
		t.Fatalf("status %s, want failed", snap.Status)
	}
	if snap.Error != "decrypt failed" {
		t.Fatalf("error %q, want decrypt failed", snap.Error)
	}
	if len(fx.queue.completed) != 1 || fx.queue.completed[0] != 10 {
		t.Fatalf("completed jobs %#v, want [10]", fx.queue.completed)
	}
}
