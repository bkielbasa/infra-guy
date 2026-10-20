package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/run"
)

var _ run.Repository = (*RunRepo)(nil)

type RunRepo struct{ pool *pgxpool.Pool }

func NewRunRepo(pool *pgxpool.Pool) *RunRepo { return &RunRepo{pool: pool} }

const runColumns = `id, project_id, trigger, status, commit_sha, branch, commit_message, author,
	triggered_by, pr_number, pr_title, plan_only, auto_approve, plan_text, plan_file, add_count,
	change_count, destroy_count, summary_present, terraform_version, error, approved_by, approved_at,
	created_at, started_at, finished_at`

const insertIfAbsentSQL = `
INSERT INTO runs (id, project_id, trigger, status, commit_sha, branch, commit_message, author, triggered_by, pr_number, pr_title, plan_only, auto_approve, created_at)
SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14
WHERE NOT EXISTS (
  SELECT 1 FROM runs
  WHERE project_id = $2 AND commit_sha = $5 AND trigger = $3
    AND pr_number IS NOT DISTINCT FROM $10
    AND status IN ('queued', 'running')
)`

func (r *RunRepo) InsertIfAbsent(ctx context.Context, in *run.Run) (bool, error) {
	s := in.Snapshot()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, insertIfAbsentSQL,
		s.ID, s.ProjectID, s.Trigger, s.Status, s.SHA, s.Branch, s.Message, s.Author,
		s.TriggeredBy, s.PRNumber, s.PRTitle, s.PlanOnly, s.AutoApprove, s.CreatedAt,
	)
	if err != nil {
		return false, err
	}
	inserted := tag.RowsAffected() == 1
	if inserted {
		if err := upsertSteps(ctx, tx, s); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return inserted, nil
}

func (r *RunRepo) Save(ctx context.Context, in *run.Run) error {
	s := in.Snapshot()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		UPDATE runs SET
			project_id = $2, trigger = $3, status = $4, commit_sha = $5, branch = $6,
			commit_message = $7, author = $8, triggered_by = $9, pr_number = $10, pr_title = $11,
			plan_only = $12, auto_approve = $13, plan_text = $14, plan_file = $15, add_count = $16,
			change_count = $17, destroy_count = $18, summary_present = $19, terraform_version = $20,
			error = $21, approved_by = $22, approved_at = $23, created_at = $24, started_at = $25,
			finished_at = $26
		WHERE id = $1`,
		s.ID, s.ProjectID, s.Trigger, s.Status, s.SHA, s.Branch, s.Message, s.Author,
		s.TriggeredBy, s.PRNumber, s.PRTitle, s.PlanOnly, s.AutoApprove, s.PlanText, s.PlanFile,
		s.Add, s.Change, s.Destroy, s.SummaryPresent, s.TerraformVersion, s.Error, s.ApprovedBy,
		s.ApprovedAt, s.CreatedAt, s.StartedAt, s.FinishedAt,
	); err != nil {
		return err
	}
	if err := upsertSteps(ctx, tx, s); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *RunRepo) Get(ctx context.Context, id run.ID) (*run.Run, error) {
	s, err := scanRun(r.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	steps, err := r.loadSteps(ctx, id)
	if err != nil {
		return nil, err
	}
	s.Steps = steps
	return run.Restore(s), nil
}

func (r *RunRepo) ListByProject(ctx context.Context, projectID run.ProjectID) ([]*run.Run, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+runColumns+` FROM runs WHERE project_id = $1 ORDER BY created_at DESC, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*run.Run
	for rows.Next() {
		s, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run.Restore(s))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rebuilt := make([]*run.Run, 0, len(out))
	for _, item := range out {
		s := item.Snapshot()
		steps, err := r.loadSteps(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		s.Steps = steps
		rebuilt = append(rebuilt, run.Restore(s))
	}
	return rebuilt, nil
}

func (r *RunRepo) loadSteps(ctx context.Context, id run.ID) ([]run.Step, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT name, position, status, logs, exit_code, started_at, finished_at
		FROM steps WHERE run_id = $1 ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var steps []run.Step
	for rows.Next() {
		var st run.Step
		var started, finished *time.Time
		if err := rows.Scan(&st.Name, &st.Position, &st.Status, &st.Logs, &st.ExitCode, &started, &finished); err != nil {
			return nil, err
		}
		st.StartedAt = utcTimePtr(started)
		st.FinishedAt = utcTimePtr(finished)
		steps = append(steps, st)
	}
	return steps, rows.Err()
}

func upsertSteps(ctx context.Context, tx pgx.Tx, s run.Snapshot) error {
	for _, st := range s.Steps {
		if _, err := tx.Exec(ctx, `
			INSERT INTO steps (run_id, name, position, status, logs, exit_code, started_at, finished_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (run_id, name) DO UPDATE SET
				position = EXCLUDED.position,
				status = EXCLUDED.status,
				logs = EXCLUDED.logs,
				exit_code = EXCLUDED.exit_code,
				started_at = EXCLUDED.started_at,
				finished_at = EXCLUDED.finished_at`,
			s.ID, st.Name, st.Position, st.Status, st.Logs, st.ExitCode, st.StartedAt, st.FinishedAt,
		); err != nil {
			return err
		}
	}
	return nil
}

type runScanner interface {
	Scan(dest ...any) error
}

func scanRun(row runScanner) (run.Snapshot, error) {
	var s run.Snapshot
	var created time.Time
	var started, finished, approved *time.Time
	err := row.Scan(
		&s.ID, &s.ProjectID, &s.Trigger, &s.Status, &s.SHA, &s.Branch, &s.Message, &s.Author,
		&s.TriggeredBy, &s.PRNumber, &s.PRTitle, &s.PlanOnly, &s.AutoApprove, &s.PlanText, &s.PlanFile,
		&s.Add, &s.Change, &s.Destroy, &s.SummaryPresent, &s.TerraformVersion, &s.Error, &s.ApprovedBy,
		&approved, &created, &started, &finished,
	)
	if err != nil {
		return run.Snapshot{}, err
	}
	s.ApprovedAt = utcTimePtr(approved)
	s.CreatedAt = utcTime(created)
	s.StartedAt = utcTimePtr(started)
	s.FinishedAt = utcTimePtr(finished)
	return s, nil
}
