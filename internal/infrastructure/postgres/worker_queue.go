package postgres

import (
	"context"
	"time"

	"infra-guy/internal/application/worker"
	"infra-guy/internal/domain/project"
)

var _ worker.Queue = WorkerQueue{}

type WorkerQueue struct{ *Queue }

func (q WorkerQueue) Claim(ctx context.Context, workerID string, projects project.Repository) (*worker.Job, error) {
	job, err := q.Queue.Claim(ctx, workerID, projects)
	if job == nil {
		return nil, err
	}
	out := toWorkerJob(*job)
	return &out, err
}

func (q WorkerQueue) Reap(ctx context.Context, staleAfter time.Duration) ([]worker.Job, []worker.Job, error) {
	requeue, failed, err := q.Queue.Reap(ctx, staleAfter)
	return toWorkerJobs(requeue), toWorkerJobs(failed), err
}

func toWorkerJob(j Job) worker.Job {
	return worker.Job{ID: j.ID, RunID: j.RunID, Step: j.Step, Attempts: j.Attempts}
}

func toWorkerJobs(jobs []Job) []worker.Job {
	if jobs == nil {
		return nil
	}
	out := make([]worker.Job, len(jobs))
	for i, j := range jobs {
		out[i] = toWorkerJob(j)
	}
	return out
}
