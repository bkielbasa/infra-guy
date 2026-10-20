package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	runapp "infra-guy/internal/application/run"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/infrastructure/postgres"
	"infra-guy/internal/infrastructure/terraform"
)

const HeartbeatEvery = 20 * time.Second
const StaleAfter = 2 * time.Minute

func StepTimeout(step run.StepName) time.Duration {
	switch step {
	case run.StepPlan:
		return 20 * time.Minute
	case run.StepApply:
		return 60 * time.Minute
	default:
		return 10 * time.Minute
	}
}

type Queue interface {
	Claim(ctx context.Context, workerID string, projects project.Repository) (*postgres.Job, error)
	Heartbeat(ctx context.Context, jobID int64) error
	Complete(ctx context.Context, jobID int64) error
	Reap(ctx context.Context, staleAfter time.Duration) (requeue []postgres.Job, failed []postgres.Job, err error)
}

type Checkout interface {
	Clone(ctx context.Context, dest, cloneURL, token, sha string) error
}

type Executor interface {
	Run(ctx context.Context, req terraform.Request) (terraform.Result, error)
}

type Worker struct {
	WorkerID        string
	Queue           Queue
	Projects        project.Repository
	Runs            run.Repository
	Service         *runapp.Service
	Checkout        Checkout
	Executor        Executor
	WorkDir         string
	InternalBaseURL string
	Now             func() time.Time
	Timeout         func(run.StepName) time.Duration
}

func (w *Worker) Tick(ctx context.Context) error {
	_, failed, err := w.Queue.Reap(ctx, StaleAfter)
	if err != nil {
		return err
	}
	for _, job := range failed {
		msg := "worker lost the step"
		if job.Step == run.StepApply {
			msg = "worker lost the apply; check state before starting another run"
		}
		if err := w.Service.Fail(ctx, job.RunID, job.Step, msg); err != nil {
			return err
		}
		if err := w.removeIfTerminal(ctx, job.RunID); err != nil {
			return err
		}
	}

	job, err := w.Queue.Claim(ctx, w.WorkerID, w.Projects)
	if err != nil || job == nil {
		return err
	}

	hbCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.heartbeat(hbCtx, job.ID)
	return w.runJob(ctx, *job)
}

func (w *Worker) heartbeat(ctx context.Context, jobID int64) {
	ticker := time.NewTicker(HeartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = w.Queue.Heartbeat(ctx, jobID)
		}
	}
}

func (w *Worker) runJob(ctx context.Context, job postgres.Job) error {
	r, err := w.Runs.Get(ctx, job.RunID)
	if err != nil || r == nil {
		return fmt.Errorf("run %s not found", job.RunID)
	}
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	stepStatus := stepStatus(r, job.Step)
	switch stepStatus {
	case run.StepPending:
		if err := r.BeginStep(job.Step, now); err != nil {
			return err
		}
		if err := w.Runs.Save(ctx, r); err != nil {
			return err
		}
	case run.StepRunning:
	default:
		if err := r.BeginStep(job.Step, now); err != nil {
			return err
		}
		if err := w.Runs.Save(ctx, r); err != nil {
			return err
		}
	}

	p, err := w.Projects.Get(ctx, project.ID(r.Snapshot().ProjectID))
	if err != nil || p == nil {
		return fmt.Errorf("project %s not found", r.Snapshot().ProjectID)
	}
	ps := p.Snapshot()
	rs := r.Snapshot()
	dest := filepath.Join(w.WorkDir, string(job.RunID))
	if err := w.Checkout.Clone(ctx, dest, ps.Repo.CloneURL(), ps.GitHubToken, rs.SHA); err != nil {
		if ferr := w.Service.Fail(ctx, job.RunID, job.Step, err.Error()); ferr != nil {
			return ferr
		}
		return w.completeAndCleanup(ctx, job)
	}

	if job.Step == run.StepApply && len(rs.PlanFile) == 0 {
		if err := w.Service.Fail(ctx, job.RunID, job.Step, "apply requires a stored plan file"); err != nil {
			return err
		}
		return w.completeAndCleanup(ctx, job)
	}

	timeout := StepTimeout(job.Step)
	if w.Timeout != nil {
		timeout = w.Timeout(job.Step)
	}
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := w.Executor.Run(stepCtx, terraform.Request{
		WorkDir: dest, InternalBaseURL: w.InternalBaseURL, ProjectID: string(ps.ID),
		StateToken: ps.StateToken, WorkingDirectory: ps.WorkingDirectory,
		Step: job.Step, PlanFile: rs.PlanFile,
	})
	if errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
		if ferr := w.Service.Fail(ctx, job.RunID, job.Step, "step timed out"); ferr != nil {
			return ferr
		}
		return w.completeAndCleanup(ctx, job)
	}
	if err != nil {
		if ferr := w.Service.Fail(ctx, job.RunID, job.Step, err.Error()); ferr != nil {
			return ferr
		}
		return w.completeAndCleanup(ctx, job)
	}
	if res.Version != "" {
		if err := w.Service.SetTerraformVersion(ctx, job.RunID, res.Version); err != nil {
			return err
		}
	}
	if err := w.Service.Advance(ctx, job.RunID, job.Step, run.StepResult{
		ExitCode: res.ExitCode, Logs: res.Logs, PlanText: res.PlanText, PlanFile: res.PlanFile,
	}); err != nil {
		return err
	}
	return w.completeAndCleanup(ctx, job)
}

func (w *Worker) completeAndCleanup(ctx context.Context, job postgres.Job) error {
	if err := w.Queue.Complete(ctx, job.ID); err != nil {
		return err
	}
	return w.removeIfTerminal(ctx, job.RunID)
}

func stepStatus(r *run.Run, name run.StepName) run.StepStatus {
	for _, st := range r.Snapshot().Steps {
		if st.Name == name {
			return st.Status
		}
	}
	return ""
}

func (w *Worker) removeIfTerminal(ctx context.Context, id run.ID) error {
	r, err := w.Runs.Get(ctx, id)
	if err != nil || r == nil {
		return err
	}
	if r.Snapshot().Status.Terminal() {
		return os.RemoveAll(filepath.Join(w.WorkDir, string(id)))
	}
	return nil
}
