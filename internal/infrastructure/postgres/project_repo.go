package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/project"
)

var _ project.Repository = (*ProjectRepo)(nil)

type ProjectRepo struct{ pool *pgxpool.Pool }

func NewProjectRepo(pool *pgxpool.Pool) *ProjectRepo { return &ProjectRepo{pool: pool} }

const projectColumns = `id, tenant_id, github_repo_id, name, repo_owner, repo_name, default_branch, working_directory,
	terraform_version, COALESCE(approval_policy_id, ''), auto_approve, tags, locked, state_token, active_run_id, created_at, updated_at`

func (r *ProjectRepo) Save(ctx context.Context, p *project.Project) error {
	s := p.Snapshot()
	tagsMap := s.Tags
	if tagsMap == nil {
		tagsMap = map[string]string{}
	}
	tagsJSON, err := json.Marshal(tagsMap)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO projects (
			id, tenant_id, github_repo_id, name, repo_owner, repo_name, default_branch, working_directory,
			terraform_version, approval_policy_id, auto_approve, tags, locked, state_token, active_run_id, created_at, updated_at
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10, ''),$11,$12::jsonb,$13,$14,$15,$16,$17)
		ON CONFLICT (id) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id,
			github_repo_id = EXCLUDED.github_repo_id,
			name = EXCLUDED.name,
			repo_owner = EXCLUDED.repo_owner,
			repo_name = EXCLUDED.repo_name,
			default_branch = EXCLUDED.default_branch,
			working_directory = EXCLUDED.working_directory,
			terraform_version = EXCLUDED.terraform_version,
			approval_policy_id = EXCLUDED.approval_policy_id,
			auto_approve = EXCLUDED.auto_approve,
			tags = EXCLUDED.tags,
			state_token = EXCLUDED.state_token,
			created_at = EXCLUDED.created_at,
			updated_at = EXCLUDED.updated_at`,
		s.ID, s.TenantID, s.GitHubRepoID, s.Name, s.Repo.Owner, s.Repo.Name, s.DefaultBranch, s.WorkingDirectory,
		s.TerraformVersion, s.ApprovalPolicyID, s.AutoApprove, string(tagsJSON), s.Locked, s.StateToken, s.ActiveRunID,
		s.CreatedAt, s.UpdatedAt,
	)
	return err
}

func (r *ProjectRepo) Insert(ctx context.Context, p *project.Project) error {
	return r.Save(ctx, p)
}

func (r *ProjectRepo) Update(ctx context.Context, p *project.Project) error {
	return r.Save(ctx, p)
}

func (r *ProjectRepo) SetLocked(ctx context.Context, id project.ID, locked bool) error {
	_, err := r.pool.Exec(ctx, `UPDATE projects SET locked = $2 WHERE id = $1`, id, locked)
	return err
}

func (r *ProjectRepo) ReleaseActiveRun(ctx context.Context, id project.ID, runID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE projects SET active_run_id = '' WHERE id = $1 AND active_run_id = $2`, id, runID)
	return err
}

func (r *ProjectRepo) Get(ctx context.Context, id project.ID) (*project.Project, error) {
	p, err := scanProject(r.pool.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (r *ProjectRepo) List(ctx context.Context, tenantID string) ([]*project.Project, error) {
	return r.query(ctx, `SELECT `+projectColumns+` FROM projects WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
}

func (r *ProjectRepo) FindByRepo(ctx context.Context, tenantID string, repoID int64) ([]*project.Project, error) {
	return r.query(ctx, `SELECT `+projectColumns+` FROM projects WHERE tenant_id = $1 AND github_repo_id = $2 ORDER BY created_at, id`, tenantID, repoID)
}

func (r *ProjectRepo) query(ctx context.Context, sql string, args ...any) ([]*project.Project, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
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
	var tagsRaw []byte
	var created, updated time.Time
	if err := row.Scan(
		&s.ID, &s.TenantID, &s.GitHubRepoID, &s.Name, &s.Repo.Owner, &s.Repo.Name, &s.DefaultBranch, &s.WorkingDirectory,
		&s.TerraformVersion, &s.ApprovalPolicyID, &s.AutoApprove, &tagsRaw, &s.Locked, &s.StateToken, &s.ActiveRunID,
		&created, &updated,
	); err != nil {
		return nil, err
	}
	if len(tagsRaw) > 0 {
		_ = json.Unmarshal(tagsRaw, &s.Tags)
	}
	s.CreatedAt = utcTime(created)
	s.UpdatedAt = utcTime(updated)
	return project.Restore(s), nil
}
