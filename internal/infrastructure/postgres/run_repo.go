package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/run"
)

var (
	_ run.Repository              = (*RunRepo)(nil)
	_ run.TenantSummaryRepository = (*RunRepo)(nil)
)

type RunRepo struct{ pool *pgxpool.Pool }

func NewRunRepo(pool *pgxpool.Pool) *RunRepo { return &RunRepo{pool: pool} }

const runColumns = `id, project_id, trigger, status, commit_sha, branch, commit_message, author,
	triggered_by, pr_number, pr_title, plan_only, auto_approve, destroy, plan_text, plan_file, plan_json, add_count,
	change_count, destroy_count, summary_present, monthly_cost, diff_monthly_cost, terraform_version, error, approved_by, approved_at,
	committed_at, created_at, started_at, finished_at`

const runSummaryColumns = `r.id, r.project_id, r.trigger, r.status, r.commit_sha, r.branch, r.commit_message, r.author,
	r.triggered_by, r.pr_number, r.pr_title, r.plan_only, r.auto_approve, r.destroy, r.add_count,
	r.change_count, r.destroy_count, r.summary_present, r.monthly_cost, r.diff_monthly_cost, r.terraform_version, r.error, r.approved_by, r.approved_at,
	r.committed_at, r.created_at, r.started_at, r.finished_at`

const insertIfAbsentSQL = `
INSERT INTO runs (id, project_id, trigger, status, commit_sha, branch, commit_message, author, triggered_by, pr_number, pr_title, plan_only, auto_approve, destroy, plan_text, plan_file, plan_json, monthly_cost, diff_monthly_cost, committed_at, created_at)
SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21
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
		s.TriggeredBy, s.PRNumber, s.PRTitle, s.PlanOnly, s.AutoApprove, s.IsDestroy,
		s.PlanText, s.PlanFile, s.PlanJSON, s.MonthlyCost, s.DiffMonthlyCost, s.CommittedAt, s.CreatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return false, nil
		}
		return false, err
	}
	inserted := tag.RowsAffected() == 1
	if inserted {
		if err := upsertSteps(ctx, tx, s); err != nil {
			return false, err
		}
		if err := upsertApprovals(ctx, tx, s); err != nil {
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
			plan_only = $12, auto_approve = $13, destroy = $14, plan_text = $15, plan_file = $16, plan_json = $17,
			add_count = $18, change_count = $19, destroy_count = $20, summary_present = $21,
			monthly_cost = $22, diff_monthly_cost = $23,
			terraform_version = $24, error = $25, approved_by = $26, approved_at = $27,
			committed_at = $28, created_at = $29, started_at = $30, finished_at = $31
		WHERE id = $1`,
		s.ID, s.ProjectID, s.Trigger, s.Status, s.SHA, s.Branch, s.Message, s.Author,
		s.TriggeredBy, s.PRNumber, s.PRTitle, s.PlanOnly, s.AutoApprove, s.IsDestroy, s.PlanText, s.PlanFile, s.PlanJSON,
		s.Add, s.Change, s.Destroy, s.SummaryPresent, s.MonthlyCost, s.DiffMonthlyCost,
		s.TerraformVersion, s.Error, s.ApprovedBy, s.ApprovedAt, s.CommittedAt, s.CreatedAt, s.StartedAt, s.FinishedAt,
	); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return run.ErrActiveConflict
		}
		return err
	}
	if err := upsertSteps(ctx, tx, s); err != nil {
		return err
	}
	if err := upsertApprovals(ctx, tx, s); err != nil {
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
	approvals, err := r.loadApprovals(ctx, id)
	if err != nil {
		return nil, err
	}
	s.Approvals = approvals
	return run.Restore(s), nil
}

func (r *RunRepo) ListByProject(ctx context.Context, projectID run.ProjectID) ([]*run.Run, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+runColumns+` FROM runs WHERE project_id = $1 ORDER BY created_at DESC, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snaps []run.Snapshot
	var runIDs []string
	for rows.Next() {
		s, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, s)
		runIDs = append(runIDs, string(s.ID))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, nil
	}
	stepsByRun, err := r.loadStepsBatch(ctx, runIDs, true)
	if err != nil {
		return nil, err
	}
	approvalsByRun, err := r.loadApprovalsBatch(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	out := make([]*run.Run, 0, len(snaps))
	for _, s := range snaps {
		s.Steps = stepsByRun[s.ID]
		s.Approvals = approvalsByRun[s.ID]
		out = append(out, run.Restore(s))
	}
	return out, nil
}

func (r *RunRepo) ListSummariesByTenant(ctx context.Context, tenantID string) ([]*run.Run, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+runSummaryColumns+`
		FROM runs r
		JOIN projects p ON p.id = r.project_id
		WHERE p.tenant_id = $1
		ORDER BY r.created_at DESC, r.id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snaps []run.Snapshot
	var runIDs []string
	for rows.Next() {
		s, err := scanRunSummary(rows)
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, s)
		runIDs = append(runIDs, string(s.ID))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, nil
	}
	stepsByRun, err := r.loadStepsBatch(ctx, runIDs, false)
	if err != nil {
		return nil, err
	}
	approvalsByRun, err := r.loadApprovalsBatch(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	out := make([]*run.Run, 0, len(snaps))
	for _, s := range snaps {
		s.Steps = stepsByRun[s.ID]
		s.Approvals = approvalsByRun[s.ID]
		out = append(out, run.Restore(s))
	}
	return out, nil
}

func (r *RunRepo) loadStepsBatch(ctx context.Context, runIDs []string, includeLogs bool) (map[run.ID][]run.Step, error) {
	query := `
		SELECT run_id, name, position, status, '' AS logs, exit_code, started_at, finished_at
		FROM steps WHERE run_id = ANY($1) ORDER BY position`
	if includeLogs {
		query = `
			SELECT run_id, name, position, status, logs, exit_code, started_at, finished_at
			FROM steps WHERE run_id = ANY($1) ORDER BY position`
	}
	rows, err := r.pool.Query(ctx, query, runIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[run.ID][]run.Step, len(runIDs))
	for rows.Next() {
		var runID run.ID
		var st run.Step
		var started, finished *time.Time
		if err := rows.Scan(&runID, &st.Name, &st.Position, &st.Status, &st.Logs, &st.ExitCode, &started, &finished); err != nil {
			return nil, err
		}
		st.StartedAt = utcTimePtr(started)
		st.FinishedAt = utcTimePtr(finished)
		out[runID] = append(out[runID], st)
	}
	return out, rows.Err()
}

func (r *RunRepo) loadApprovalsBatch(ctx context.Context, runIDs []string) (map[run.ID][]run.Approval, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT run_id, user_id, user_login, user_role, approved_at
		FROM run_approvals WHERE run_id = ANY($1) ORDER BY approved_at ASC, user_login ASC`, runIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[run.ID][]run.Approval, len(runIDs))
	for rows.Next() {
		var runID run.ID
		var a run.Approval
		var approvedAt time.Time
		if err := rows.Scan(&runID, &a.UserID, &a.Login, &a.Role, &approvedAt); err != nil {
			return nil, err
		}
		a.ApprovedAt = utcTime(approvedAt)
		out[runID] = append(out[runID], a)
	}
	return out, rows.Err()
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

func (r *RunRepo) loadApprovals(ctx context.Context, id run.ID) ([]run.Approval, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT user_id, user_login, user_role, approved_at
		FROM run_approvals WHERE run_id = $1 ORDER BY approved_at ASC, user_login ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var approvals []run.Approval
	for rows.Next() {
		var a run.Approval
		var approvedAt time.Time
		if err := rows.Scan(&a.UserID, &a.Login, &a.Role, &approvedAt); err != nil {
			return nil, err
		}
		a.ApprovedAt = utcTime(approvedAt)
		approvals = append(approvals, a)
	}
	return approvals, rows.Err()
}

func upsertApprovals(ctx context.Context, tx pgx.Tx, s run.Snapshot) error {
	if _, err := tx.Exec(ctx, `DELETE FROM run_approvals WHERE run_id = $1`, s.ID); err != nil {
		return err
	}
	for _, a := range s.Approvals {
		if _, err := tx.Exec(ctx, `
			INSERT INTO run_approvals (run_id, user_id, user_login, user_role, approved_at)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (run_id, user_login) DO UPDATE SET
				user_id = EXCLUDED.user_id,
				user_role = EXCLUDED.user_role,
				approved_at = EXCLUDED.approved_at`,
			s.ID, a.UserID, a.Login, a.Role, a.ApprovedAt,
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
	var started, finished, approved, committed *time.Time
	err := row.Scan(
		&s.ID, &s.ProjectID, &s.Trigger, &s.Status, &s.SHA, &s.Branch, &s.Message, &s.Author,
		&s.TriggeredBy, &s.PRNumber, &s.PRTitle, &s.PlanOnly, &s.AutoApprove, &s.IsDestroy, &s.PlanText, &s.PlanFile, &s.PlanJSON,
		&s.Add, &s.Change, &s.Destroy, &s.SummaryPresent, &s.MonthlyCost, &s.DiffMonthlyCost, &s.TerraformVersion, &s.Error, &s.ApprovedBy,
		&approved, &committed, &created, &started, &finished,
	)
	if err != nil {
		return run.Snapshot{}, err
	}
	s.ApprovedAt = utcTimePtr(approved)
	s.CommittedAt = utcTimePtr(committed)
	s.CreatedAt = utcTime(created)
	s.StartedAt = utcTimePtr(started)
	s.FinishedAt = utcTimePtr(finished)
	return s, nil
}

func scanRunSummary(row runScanner) (run.Snapshot, error) {
	var s run.Snapshot
	var created time.Time
	var started, finished, approved, committed *time.Time
	err := row.Scan(
		&s.ID, &s.ProjectID, &s.Trigger, &s.Status, &s.SHA, &s.Branch, &s.Message, &s.Author,
		&s.TriggeredBy, &s.PRNumber, &s.PRTitle, &s.PlanOnly, &s.AutoApprove, &s.IsDestroy,
		&s.Add, &s.Change, &s.Destroy, &s.SummaryPresent, &s.MonthlyCost, &s.DiffMonthlyCost, &s.TerraformVersion, &s.Error, &s.ApprovedBy,
		&approved, &committed, &created, &started, &finished,
	)
	if err != nil {
		return run.Snapshot{}, err
	}
	s.ApprovedAt = utcTimePtr(approved)
	s.CommittedAt = utcTimePtr(committed)
	s.CreatedAt = utcTime(created)
	s.StartedAt = utcTimePtr(started)
	s.FinishedAt = utcTimePtr(finished)
	return s, nil
}
