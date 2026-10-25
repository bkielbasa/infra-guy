package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
func (m *memProjects) List(_ context.Context, tenantID string) ([]*project.Project, error) {
	var out []*project.Project
	for _, p := range m.items {
		if tenantID == "" || p.Snapshot().TenantID == tenantID {
			out = append(out, p)
		}
	}
	return out, nil
}
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
func (m *memRuns) ListByProject(_ context.Context, projectID run.ProjectID) ([]*run.Run, error) {
	var out []*run.Run
	for _, r := range m.items {
		if r.Snapshot().ProjectID == projectID {
			out = append(out, r)
		}
	}
	return out, nil
}

type memSched struct{ steps []run.StepName }

func (m *memSched) Enqueue(_ context.Context, _ run.ID, step run.StepName) error {
	m.steps = append(m.steps, step)
	return nil
}

type fakeQueue struct {
	mu         sync.Mutex
	claim      *Job
	get        map[int64]*Job
	getErr     error
	requeue    []Job
	failed     []Job
	completed  []int64
	heartbeats []int64
	staleAfter time.Duration
}

func (q *fakeQueue) Claim(context.Context, string, time.Duration) (*Job, error) {
	return q.claim, nil
}
func (q *fakeQueue) Get(_ context.Context, jobID int64) (*Job, error) {
	if q.getErr != nil {
		return nil, q.getErr
	}
	if q.get != nil {
		return q.get[jobID], nil
	}
	return nil, nil
}
func (q *fakeQueue) Heartbeat(_ context.Context, jobID int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.heartbeats = append(q.heartbeats, jobID)
	return nil
}
func (q *fakeQueue) Complete(_ context.Context, jobID int64) error {
	q.completed = append(q.completed, jobID)
	return nil
}
func (q *fakeQueue) Reap(_ context.Context, staleAfter time.Duration, _ func(Job) run.ReapDecision) ([]Job, []Job, error) {
	q.staleAfter = staleAfter
	return q.requeue, q.failed, nil
}

type fakeDispatcher struct {
	dispatched []Job
	err        error
}

func (d *fakeDispatcher) Dispatch(_ context.Context, job Job) error {
	d.dispatched = append(d.dispatched, job)
	return d.err
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
		WorkerID: "w1", Queue: q, Projects: projects, Service: svc,
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

func TestReapPolicy(t *testing.T) {
	if d := ReapPolicy(run.Job{Step: run.StepPlan, Attempts: 1}); d.Fail {
		t.Fatalf("expected plan attempt 1 to requeue, got %+v", d)
	}
	if d := ReapPolicy(run.Job{Step: run.StepPlan, Attempts: 2}); !d.Fail || d.Reason == "" {
		t.Fatalf("expected plan attempt 2 to fail with reason, got %+v", d)
	}
	if d := ReapPolicy(run.Job{Step: run.StepApply, Attempts: 1}); !d.Fail || d.Reason == "" {
		t.Fatalf("expected apply attempt 1 to fail immediately, got %+v", d)
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
		PlanJSON: []byte(`{"format_version":"1.2"}`),
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
	if string(snap.PlanJSON) != `{"format_version":"1.2"}` {
		t.Fatalf("PlanJSON %q", string(snap.PlanJSON))
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

func TestTickWithDispatcherDispatchesWithoutInProcessExecution(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	r, err := run.Open(run.OpenInput{
		ID: "run-disp", ProjectID: "p1", PlanOnly: true, Trigger: run.TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed := Job{ID: 11, RunID: "run-disp", Step: run.StepInit, Attempts: 1}
	exec := &fakeExecutor{res: Result{ExitCode: 0}}
	fx := newFixture(t, r, p, &fakeQueue{claim: &claimed}, exec)
	disp := &fakeDispatcher{}
	fx.w.Dispatcher = disp

	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(disp.dispatched) != 1 || disp.dispatched[0] != claimed {
		t.Fatalf("dispatched = %#v, want [%#v]", disp.dispatched, claimed)
	}
	if fx.exec.called {
		t.Fatal("executor should not be called when Dispatcher is set")
	}
	if fx.co.dest != "" {
		t.Fatalf("checkout should not be called when Dispatcher is set, got dest=%q", fx.co.dest)
	}
	if len(fx.queue.completed) != 0 {
		t.Fatalf("completed = %#v, want empty (spawned worker completes job)", fx.queue.completed)
	}
}

func TestTickWithDispatcherErrorFailsRunAndCompletesJob(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	r, err := run.Open(run.OpenInput{
		ID: "run-disp-err", ProjectID: "p1", PlanOnly: true, Trigger: run.TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed := Job{ID: 12, RunID: "run-disp-err", Step: run.StepInit, Attempts: 1}
	exec := &fakeExecutor{res: Result{ExitCode: 0}}
	fx := newFixture(t, r, p, &fakeQueue{claim: &claimed}, exec)
	disp := &fakeDispatcher{err: errors.New("k8s api 403 forbidden")}
	fx.w.Dispatcher = disp
	if err := os.MkdirAll(fx.ws, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if fx.exec.called {
		t.Fatal("executor should not be called on dispatch error")
	}
	saved, err := fx.runs.Get(ctx, "run-disp-err")
	if err != nil {
		t.Fatal(err)
	}
	snap := saved.Snapshot()
	if snap.Status != run.StatusFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
	if snap.Error != "dispatch worker job: k8s api 403 forbidden" {
		t.Fatalf("error = %q, want %q", snap.Error, "dispatch worker job: k8s api 403 forbidden")
	}
	if len(fx.queue.completed) != 1 || fx.queue.completed[0] != 12 {
		t.Fatalf("completed = %#v, want [12]", fx.queue.completed)
	}
	if _, err := os.Stat(fx.ws); !os.IsNotExist(err) {
		t.Fatalf("workspace still present: %v", err)
	}
}

func TestExecuteClaimedRunsAndCompletesJob(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	r, err := run.Open(run.OpenInput{
		ID: "run-claimed", ProjectID: "p1", PlanOnly: true, Trigger: run.TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	beginComplete(t, r, run.StepInit, run.StepResult{}, time.Unix(1, 0).UTC())
	exec := &fakeExecutor{res: Result{
		ExitCode: 0, PlanText: "Plan: 1 to add, 0 to change, 0 to destroy.", Version: "1.9.8",
		PlanJSON: []byte(`{"format_version":"1.2"}`),
	}}
	q := &fakeQueue{
		get: map[int64]*Job{
			42: {ID: 42, RunID: "run-claimed", Step: run.StepPlan, Attempts: 1},
		},
	}
	fx := newFixture(t, r, p, q, exec)

	if err := fx.w.ExecuteClaimed(ctx, 42); err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	hbs := append([]int64(nil), q.heartbeats...)
	q.mu.Unlock()
	if len(hbs) == 0 || hbs[0] != 42 {
		t.Fatalf("heartbeats = %#v, want initial heartbeat for 42", hbs)
	}
	if !fx.exec.called {
		t.Fatal("executor was not called")
	}
	saved, err := fx.runs.Get(ctx, "run-claimed")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Snapshot().Status != run.StatusPlanned {
		t.Fatalf("status = %s, want planned", saved.Snapshot().Status)
	}
	if len(fx.queue.completed) != 1 || fx.queue.completed[0] != 42 {
		t.Fatalf("completed = %#v, want [42]", fx.queue.completed)
	}
}

func TestExecuteClaimedReturnsErrorWhenJobNotFound(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	r, err := run.Open(run.OpenInput{
		ID: "run-missing", ProjectID: "p1", PlanOnly: true, Trigger: run.TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fx := newFixture(t, r, p, &fakeQueue{}, nil)

	err = fx.w.ExecuteClaimed(ctx, 999)
	if err == nil || !strings.Contains(err.Error(), "job 999 not found or not running") {
		t.Fatalf("err = %v, want not found or not running", err)
	}
	if fx.exec.called {
		t.Fatal("executor should not be called when job is missing")
	}
}

type fakeWorkspaceCache struct {
	saved    map[string]string
	restored []string
	deleted  []string
	saveErr  error
	restErr  error
}

func newFakeWorkspaceCache() *fakeWorkspaceCache {
	return &fakeWorkspaceCache{saved: make(map[string]string)}
}

func (c *fakeWorkspaceCache) Save(_ context.Context, key, srcDir string) error {
	if c.saveErr != nil {
		return c.saveErr
	}
	data, _ := os.ReadFile(filepath.Join(srcDir, ".terraform.lock.hcl"))
	c.saved[key] = string(data)
	return nil
}

func (c *fakeWorkspaceCache) Restore(_ context.Context, key, destDir string) (bool, error) {
	if c.restErr != nil {
		return false, c.restErr
	}
	data, ok := c.saved[key]
	if !ok {
		return false, nil
	}
	c.restored = append(c.restored, key)
	if err := os.MkdirAll(filepath.Join(destDir, ".terraform"), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(destDir, ".terraform.lock.hcl"), []byte(data), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func (c *fakeWorkspaceCache) Delete(_ context.Context, key string) error {
	c.deleted = append(c.deleted, key)
	delete(c.saved, key)
	return nil
}

func TestTickInitSavesToCacheAndPlanAndApplyRestoreWithoutRecloning(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	r, err := run.Open(run.OpenInput{
		ID: "run-cached", ProjectID: "p1", AutoApprove: true, Trigger: run.TriggerPush, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	cache := newFakeWorkspaceCache()
	exec := &fakeExecutor{res: Result{ExitCode: 0, Version: "1.10.5"}}
	fx := newFixture(t, r, p, &fakeQueue{claim: &Job{ID: 1, RunID: "run-cached", Step: run.StepInit}}, exec)
	fx.w.Cache = cache
	fx.exec.onRun = func(context.Context) {
		_ = os.MkdirAll(fx.ws, 0o755)
		_ = os.WriteFile(filepath.Join(fx.ws, ".terraform.lock.hcl"), []byte("lock-v1"), 0o644)
	}

	// 1. Run StepInit: clones repo, runs terraform init, saves workspace zip to cache, deletes local ws.
	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	wantKey := "ten-1/p1/run-cached"
	if cache.saved[wantKey] != "lock-v1" {
		t.Fatalf("expected cache saved under %q, got %#v", wantKey, cache.saved)
	}
	if fx.co.dest != fx.ws {
		t.Fatalf("expected clone during init, got dest=%q", fx.co.dest)
	}
	if _, err := os.Stat(fx.ws); !os.IsNotExist(err) {
		t.Fatalf("local workspace should be removed after StepInit when cache is active: %v", err)
	}

	// 2. Run StepPlan: restores from cache, does NOT clone again, keeps cache for StepApply.
	fx.co.dest = ""
	fx.queue.claim = &Job{ID: 2, RunID: "run-cached", Step: run.StepPlan}
	fx.exec.res = Result{
		ExitCode: 0,
		PlanText: "Plan: 1 to add, 0 to change, 0 to destroy.",
		PlanFile: []byte("plan-bytes"),
		PlanJSON: []byte(`{"format_version":"1.2"}`),
	}
	var planSawLockfile string
	fx.exec.onRun = func(context.Context) {
		b, _ := os.ReadFile(filepath.Join(fx.ws, ".terraform.lock.hcl"))
		planSawLockfile = string(b)
	}
	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if fx.co.dest != "" {
		t.Fatalf("StepPlan should not clone when cache is configured, got dest=%q", fx.co.dest)
	}
	if planSawLockfile != "lock-v1" {
		t.Fatalf("StepPlan did not see restored lockfile, got %q", planSawLockfile)
	}
	if len(cache.deleted) != 0 {
		t.Fatalf("cache should not be deleted before apply, got deleted=%v", cache.deleted)
	}

	// 3. Run StepApply: restores from cache, does NOT clone, deletes cache when run succeeds.
	fx.queue.claim = &Job{ID: 3, RunID: "run-cached", Step: run.StepApply}
	fx.exec.res = Result{ExitCode: 0, Logs: "Apply complete!"}
	var applySawLockfile string
	fx.exec.onRun = func(context.Context) {
		b, _ := os.ReadFile(filepath.Join(fx.ws, ".terraform.lock.hcl"))
		applySawLockfile = string(b)
	}
	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if fx.co.dest != "" {
		t.Fatalf("StepApply should not clone when cache is configured, got dest=%q", fx.co.dest)
	}
	if applySawLockfile != "lock-v1" {
		t.Fatalf("StepApply did not see restored lockfile, got %q", applySawLockfile)
	}
	if len(cache.deleted) != 1 || cache.deleted[0] != wantKey {
		t.Fatalf("expected cache %q deleted on terminal status, got %v", wantKey, cache.deleted)
	}
}

func TestWorkerStreamsLogsDuringExecution(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	r, err := run.Open(run.OpenInput{
		ID: "run-stream", ProjectID: "p1", PlanOnly: true, Trigger: run.TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	beginComplete(t, r, run.StepInit, run.StepResult{}, time.Unix(1, 0).UTC())
	exec := &fakeExecutor{res: Result{
		ExitCode: 0,
		Logs:     "line 1\nline 2\nPlan: 1 to add, 0 to change, 0 to destroy.\n",
		PlanText: "Plan: 1 to add, 0 to change, 0 to destroy.",
		PlanJSON: []byte(`{"format_version":"1.2"}`),
	}}
	fx := newFixture(t, r, p, &fakeQueue{claim: &Job{ID: 55, RunID: "run-stream", Step: run.StepPlan}}, exec)
	var midExecutionLogs string
	fx.exec.onRun = func(ctx context.Context) {
		if fx.exec.req.OnLog == nil {
			t.Fatal("expected OnLog callback to be wired on executor Request")
		}
		fx.exec.req.OnLog("line 1\n")
		saved, err := fx.runs.Get(ctx, "run-stream")
		if err != nil || saved == nil {
			t.Fatalf("get run during execution: %v", err)
		}
		for _, st := range saved.Snapshot().Steps {
			if st.Name == run.StepPlan {
				midExecutionLogs = st.Logs
			}
		}
	}
	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(midExecutionLogs, "Cloning repository") || !strings.Contains(midExecutionLogs, "line 1\n") {
		t.Fatalf("mid-execution logs = %q, want workspace prep + %q", midExecutionLogs, "line 1\n")
	}
}

type fakeDependencyResolver struct {
	env      map[string]string
	messages []string
	err      error
}

func (f fakeDependencyResolver) ResolveForProject(_ context.Context, _ project.ID) (map[string]string, []string, error) {
	return f.env, f.messages, f.err
}

func TestWorkerResolvesUpstreamDependenciesIntoEnv(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(100, 0).UTC()
	p := testProject(t)
	projs := &memProjects{items: map[project.ID]*project.Project{p.Snapshot().ID: p}}
	runs := &memRuns{items: map[run.ID]*run.Run{}}
	sched := &memSched{}
	svc := &runapp.Service{Projects: projs, Runs: runs, Schedule: sched, Now: func() time.Time { return now }, NewID: func() string { return "r1" }}
	r, err := svc.Open(ctx, runapp.OpenRequest{ProjectID: "p1", Trigger: run.TriggerPipeline, SHA: "abcdef1234567", Branch: "main", TriggeredBy: "pipeline:net"})
	if err != nil {
		t.Fatal(err)
	}
	q := &fakeQueue{claim: &Job{ID: 1, RunID: r.Snapshot().ID, Step: run.StepInit}}
	exec := &fakeExecutor{res: Result{ExitCode: 0}}
	w := &Worker{
		WorkerID: "w1", Queue: q, Projects: projs, Service: svc,
		Checkout: &fakeCheckout{t: t}, Executor: exec,
		Tokens: tokenSourceFunc(func(ctx context.Context, tenantID string) (string, error) {
			return "ghs_install", nil
		}),
		Contexts: fakeContextResolver{env: map[string]string{"AWS_REGION": "eu-central-1", "TF_VAR_vpc_id": "from-ctx"}},
		Dependencies: fakeDependencyResolver{
			env:      map[string]string{"TF_VAR_vpc_id": "vpc-from-upstream"},
			messages: []string{`Injected TF_VAR_vpc_id from upstream project "network" output "vpc_id"`},
		},
		WorkDir: t.TempDir(), Now: func() time.Time { return now },
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	capturedEnv := exec.req.Env
	if capturedEnv["AWS_REGION"] != "eu-central-1" || capturedEnv["TF_VAR_vpc_id"] != "vpc-from-upstream" {
		t.Fatalf("expected merged env with upstream override, got %+v", capturedEnv)
	}
	updated, _ := runs.Get(ctx, r.Snapshot().ID)
	if !strings.Contains(updated.Snapshot().Steps[0].Logs, "Injected TF_VAR_vpc_id") {
		t.Fatalf("expected dependency injection log in step logs, got %q", updated.Snapshot().Steps[0].Logs)
	}
}

func TestWorkerDependencyResolutionErrorFailsWithoutExecutor(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	r, err := run.Open(run.OpenInput{
		ID: "run-dep-err", ProjectID: "p1", PlanOnly: true, Trigger: run.TriggerPipeline, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{res: Result{ExitCode: 0}}
	fx := newFixture(t, r, p, &fakeQueue{claim: &Job{ID: 21, RunID: "run-dep-err", Step: run.StepInit}}, exec)
	fx.w.Dependencies = fakeDependencyResolver{
		err: errors.New("upstream state load failed"),
	}
	if err := fx.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if fx.exec.called {
		t.Fatal("executor should not be called on dependency resolution error")
	}
	saved, err := fx.runs.Get(ctx, "run-dep-err")
	if err != nil {
		t.Fatal(err)
	}
	snap := saved.Snapshot()
	if snap.Status != run.StatusFailed {
		t.Fatalf("status %s, want failed", snap.Status)
	}
	if snap.Error != "upstream state load failed" {
		t.Fatalf("error %q, want upstream state load failed", snap.Error)
	}
	if len(fx.queue.completed) != 1 || fx.queue.completed[0] != 21 {
		t.Fatalf("completed jobs %#v, want [21]", fx.queue.completed)
	}
}

func TestWorkerTickLifecycle_DriftAndTTL(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

	driftProj := testProject(t)
	driftProj.SetDriftConfig(true, 60, now)

	expiredAt := now.Add(-10 * time.Minute)
	ttlSnap := testProject(t).Snapshot()
	ttlSnap.ID = "p-ttl"
	ttlSnap.Name = "ephemeral-pr-42"
	ttlSnap.TTLHours = 24
	ttlSnap.ExpiresAt = &expiredAt
	ttlSnap.AutoDestroy = true
	ttlProj := project.Restore(ttlSnap)

	projs := &memProjects{items: map[project.ID]*project.Project{
		driftProj.Snapshot().ID: driftProj,
		ttlProj.Snapshot().ID:   ttlProj,
	}}

	prevDriftRun := run.Restore(run.Snapshot{
		ID: "prev-1", ProjectID: "p1", Status: run.StatusSucceeded,
		SHA: "1111111", Branch: "main", CreatedAt: now.Add(-2 * time.Hour),
	})
	prevTTLRun := run.Restore(run.Snapshot{
		ID: "prev-2", ProjectID: "p-ttl", Status: run.StatusSucceeded,
		SHA: "2222222", Branch: "main", CreatedAt: now.Add(-25 * time.Hour),
	})
	runs := &memRuns{items: map[run.ID]*run.Run{
		"prev-1": prevDriftRun,
		"prev-2": prevTTLRun,
	}}
	sched := &memSched{}
	seq := 0
	svc := &runapp.Service{
		Projects: projs, Runs: runs, Schedule: sched,
		Now: func() time.Time { return now },
		NewID: func() string {
			seq++
			if seq == 1 {
				return "new-run-1"
			}
			return "new-run-2"
		},
	}

	w := &Worker{
		WorkerID: "w1", Projects: projs, Service: svc,
		Now: func() time.Time { return now },
	}
	if err := w.TickLifecycle(ctx); err != nil {
		t.Fatalf("TickLifecycle failed: %v", err)
	}

	var foundDrift, foundTTL bool
	for _, r := range runs.items {
		rs := r.Snapshot()
		if rs.Trigger == run.TriggerDrift && rs.ProjectID == "p1" && rs.PlanOnly {
			foundDrift = true
		}
		if rs.Trigger == run.TriggerTTL && rs.ProjectID == "p-ttl" && rs.IsDestroy && rs.AutoApprove {
			foundTTL = true
		}
	}
	if !foundDrift {
		t.Fatalf("expected scheduled drift check run to be opened")
	}
	if !foundTTL {
		t.Fatalf("expected TTL auto-destroy run to be opened")
	}
}
