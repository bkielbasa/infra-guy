package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	runapp "infra-guy/internal/application/run"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
)

var _ runapp.Scheduler = (*Queue)(nil)

type Job = run.Job

type Queue struct{ pool *pgxpool.Pool }

func NewQueue(pool *pgxpool.Pool) *Queue { return &Queue{pool: pool} }

func (q *Queue) Enqueue(ctx context.Context, id run.ID, step run.StepName) error {
	_, err := q.pool.Exec(ctx, `
		INSERT INTO jobs (run_id, step_name, status, attempts, available_at)
		VALUES ($1, $2, 'pending', 0, now())`, id, step)
	return err
}

func (q *Queue) Claim(ctx context.Context, workerID string, retryDelay time.Duration) (*run.Job, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var job run.Job
	err = tx.QueryRow(ctx, `
		SELECT id, run_id, step_name, attempts
		FROM jobs
		WHERE status = 'pending' AND available_at <= now()
		ORDER BY id ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1`).Scan(&job.ID, &job.RunID, &job.Step, &job.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var projectID project.ID
	if err := tx.QueryRow(ctx, `SELECT project_id FROM runs WHERE id = $1`, job.RunID).Scan(&projectID); err != nil {
		return nil, err
	}

	p, err := scanProject(tx.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1 FOR UPDATE`, projectID))
	if err != nil {
		return nil, err
	}

	claimErr := p.ClaimActiveRun(string(job.RunID))
	if errors.Is(claimErr, project.ErrLocked) || errors.Is(claimErr, project.ErrBusy) {
		if _, err := tx.Exec(ctx, `
			UPDATE jobs SET available_at = now() + make_interval(secs => $2) WHERE id = $1`, job.ID, retryDelay.Seconds()); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if claimErr != nil {
		return nil, claimErr
	}

	if _, err := tx.Exec(ctx, `
		UPDATE jobs
		SET status = 'running', locked_at = now(), locked_by = $2, attempts = attempts + 1
		WHERE id = $1`, job.ID, workerID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE projects SET active_run_id = $1 WHERE id = $2`,
		p.Snapshot().ActiveRunID, projectID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	job.Attempts++
	return &job, nil
}

func (q *Queue) Get(ctx context.Context, jobID int64) (*run.Job, error) {
	var job run.Job
	err := q.pool.QueryRow(ctx, `
		SELECT id, run_id, step_name, attempts
		FROM jobs
		WHERE id = $1 AND status = 'running'`, jobID).Scan(&job.ID, &job.RunID, &job.Step, &job.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func (q *Queue) Heartbeat(ctx context.Context, jobID int64) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE jobs SET locked_at = now() WHERE id = $1 AND status = 'running'`, jobID)
	return err
}

func (q *Queue) Complete(ctx context.Context, jobID int64) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE jobs SET status = 'succeeded', locked_at = NULL WHERE id = $1`, jobID)
	return err
}

func (q *Queue) Reap(ctx context.Context, staleAfter time.Duration, decide func(run.Job) run.ReapDecision) (requeue []run.Job, failed []run.Job, err error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id, run_id, step_name, attempts
		FROM jobs
		WHERE status = 'running' AND locked_at < now() - make_interval(secs => $1)
		FOR UPDATE SKIP LOCKED`, staleAfter.Seconds())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var stale []run.Job
	for rows.Next() {
		var job run.Job
		if err := rows.Scan(&job.ID, &job.RunID, &job.Step, &job.Attempts); err != nil {
			return nil, nil, err
		}
		stale = append(stale, job)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows.Close()

	for _, job := range stale {
		d := decide(job)
		if d.Fail {
			if _, err := tx.Exec(ctx, `
				UPDATE jobs SET status = 'failed', last_error = $2 WHERE id = $1`, job.ID, d.Reason); err != nil {
				return nil, nil, err
			}
			failed = append(failed, job)
			continue
		}
		if _, err := tx.Exec(ctx, `
			UPDATE jobs SET status = 'pending', locked_at = NULL WHERE id = $1`, job.ID); err != nil {
			return nil, nil, err
		}
		requeue = append(requeue, job)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return requeue, failed, nil
}
