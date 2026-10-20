package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/project"
)

var _ project.Repository = (*ProjectRepo)(nil)

type ProjectRepo struct{ pool *pgxpool.Pool }

func NewProjectRepo(pool *pgxpool.Pool) *ProjectRepo { return &ProjectRepo{pool: pool} }

const projectColumns = `id, name, repo_owner, repo_name, default_branch, working_directory,
	auto_approve, locked, github_token, webhook_secret, state_token, active_run_id, created_at, updated_at`

func (r *ProjectRepo) Save(ctx context.Context, p *project.Project) error {
	s := p.Snapshot()
	_, err := r.pool.Exec(ctx, `
		INSERT INTO projects (`+projectColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			repo_owner = EXCLUDED.repo_owner,
			repo_name = EXCLUDED.repo_name,
			default_branch = EXCLUDED.default_branch,
			working_directory = EXCLUDED.working_directory,
			auto_approve = EXCLUDED.auto_approve,
			locked = EXCLUDED.locked,
			github_token = EXCLUDED.github_token,
			webhook_secret = EXCLUDED.webhook_secret,
			state_token = EXCLUDED.state_token,
			active_run_id = EXCLUDED.active_run_id,
			created_at = EXCLUDED.created_at,
			updated_at = EXCLUDED.updated_at`,
		s.ID, s.Name, s.Repo.Owner, s.Repo.Name, s.DefaultBranch, s.WorkingDirectory,
		s.AutoApprove, s.Locked, s.GitHubToken, s.WebhookSecret, s.StateToken, s.ActiveRunID,
		s.CreatedAt, s.UpdatedAt,
	)
	return err
}

func (r *ProjectRepo) Get(ctx context.Context, id project.ID) (*project.Project, error) {
	p, err := scanProject(r.pool.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (r *ProjectRepo) List(ctx context.Context) ([]*project.Project, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*project.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *ProjectRepo) Delete(ctx context.Context, id project.ID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, id)
	return err
}

type projectScanner interface {
	Scan(dest ...any) error
}

func scanProject(row projectScanner) (*project.Project, error) {
	var s project.Snapshot
	var created, updated time.Time
	if err := row.Scan(
		&s.ID, &s.Name, &s.Repo.Owner, &s.Repo.Name, &s.DefaultBranch, &s.WorkingDirectory,
		&s.AutoApprove, &s.Locked, &s.GitHubToken, &s.WebhookSecret, &s.StateToken, &s.ActiveRunID,
		&created, &updated,
	); err != nil {
		return nil, err
	}
	s.CreatedAt = utcTime(created)
	s.UpdatedAt = utcTime(updated)
	return project.Restore(s), nil
}
