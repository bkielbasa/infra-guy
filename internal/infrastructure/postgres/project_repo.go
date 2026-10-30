package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/infrastructure/crypto"
)

var _ project.Repository = (*ProjectRepo)(nil)

type ProjectRepo struct {
	pool   *pgxpool.Pool
	sealer *crypto.Sealer
}

func NewProjectRepo(pool *pgxpool.Pool) *ProjectRepo { return &ProjectRepo{pool: pool} }

func (r *ProjectRepo) WithSealer(sealer *crypto.Sealer) *ProjectRepo {
	r.sealer = sealer
	return r
}

func ProjectStateTokenAAD(id project.ID) []byte {
	return []byte("project:" + string(id) + ":state_token")
}

const projectColumns = `id, tenant_id, github_repo_id, name, repo_owner, repo_name, default_branch, working_directory,
	engine, engine_config, terraform_version, auto_approve, tags, locked, state_token, state_token_hash, state_token_ciphertext, active_run_id,
	drift_enabled, drift_interval_minutes, drift_status, last_drift_check_at,
	ttl_hours, expires_at, auto_destroy, created_at, updated_at, folder_id, tags_overridden,
	COALESCE((SELECT array_agg(pp.policy_id ORDER BY pp.policy_id) FROM project_policies pp WHERE pp.project_id = projects.id), '{}'::text[])`

func (r *ProjectRepo) Save(ctx context.Context, p *project.Project) error {
	s := p.Snapshot()
	var tagsJSON []byte
	var err error
	if !s.TagsOverridden {
		tagsJSON = []byte("{}")
	} else {
		tagsMap := s.Tags
		if tagsMap == nil {
			tagsMap = map[string]string{}
		}
		tagsJSON, err = json.Marshal(tagsMap)
		if err != nil {
			return err
		}
	}
	engine := s.Engine
	if engine == "" {
		engine = project.DefaultEngine
	}
	engineConfigJSON, err := json.Marshal(s.EngineConfig)
	if err != nil {
		return err
	}
	driftInterval := s.DriftIntervalMinutes
	if driftInterval <= 0 {
		driftInterval = project.DefaultDriftIntervalMinutes
	}
	driftStatus := s.DriftStatus
	if driftStatus == "" {
		driftStatus = project.DriftStatusUnknown
	}
	stateTokenHash := s.StateTokenHash
	if s.StateToken != "" {
		stateTokenHash = crypto.HashToken(s.StateToken)
		p.SetStateTokenHash(stateTokenHash)
	}
	plainStateToken := s.StateToken
	var stateTokenCiphertext []byte
	if r.sealer != nil && s.StateToken != "" {
		ct, err := r.sealer.SealWithAAD([]byte(s.StateToken), ProjectStateTokenAAD(s.ID))
		if err != nil {
			return fmt.Errorf("seal project state token: %w", err)
		}
		stateTokenCiphertext = ct
		plainStateToken = ""
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO projects (
			id, tenant_id, github_repo_id, name, repo_owner, repo_name, default_branch, working_directory,
			engine, engine_config, terraform_version, folder_id, tags_overridden, auto_approve, tags, locked, state_token, state_token_hash, state_token_ciphertext, active_run_id,
			drift_enabled, drift_interval_minutes, drift_status, last_drift_check_at,
			ttl_hours, expires_at, auto_destroy, created_at, updated_at
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12,$13,$14,$15::jsonb,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29)
		ON CONFLICT (id) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id,
			github_repo_id = EXCLUDED.github_repo_id,
			name = EXCLUDED.name,
			repo_owner = EXCLUDED.repo_owner,
			repo_name = EXCLUDED.repo_name,
			default_branch = EXCLUDED.default_branch,
			working_directory = EXCLUDED.working_directory,
			engine = EXCLUDED.engine,
			engine_config = EXCLUDED.engine_config,
			terraform_version = EXCLUDED.terraform_version,
			folder_id = EXCLUDED.folder_id,
			tags_overridden = EXCLUDED.tags_overridden,
			auto_approve = EXCLUDED.auto_approve,
			tags = EXCLUDED.tags,
			state_token = EXCLUDED.state_token,
			state_token_hash = EXCLUDED.state_token_hash,
			state_token_ciphertext = COALESCE(EXCLUDED.state_token_ciphertext, projects.state_token_ciphertext),
			drift_enabled = EXCLUDED.drift_enabled,
			drift_interval_minutes = EXCLUDED.drift_interval_minutes,
			drift_status = EXCLUDED.drift_status,
			last_drift_check_at = EXCLUDED.last_drift_check_at,
			ttl_hours = EXCLUDED.ttl_hours,
			expires_at = EXCLUDED.expires_at,
			auto_destroy = EXCLUDED.auto_destroy,
			created_at = EXCLUDED.created_at,
			updated_at = EXCLUDED.updated_at`,
		s.ID, s.TenantID, s.GitHubRepoID, s.Name, s.Repo.Owner, s.Repo.Name, s.DefaultBranch, s.WorkingDirectory,
		engine, string(engineConfigJSON), s.TerraformVersion, s.FolderID, s.TagsOverridden, s.AutoApprove, string(tagsJSON), s.Locked, plainStateToken, stateTokenHash, stateTokenCiphertext, s.ActiveRunID,
		s.DriftEnabled, driftInterval, driftStatus, s.LastDriftCheckAt,
		s.TTLHours, s.ExpiresAt, s.AutoDestroy, s.CreatedAt, s.UpdatedAt,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM project_policies WHERE project_id = $1`, s.ID); err != nil {
		return err
	}
	for _, id := range s.PolicyIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO project_policies (project_id, policy_id) VALUES ($1, $2)`, s.ID, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
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
	p, err := r.scanProject(r.pool.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (r *ProjectRepo) List(ctx context.Context, tenantID string) ([]*project.Project, error) {
	if tenantID == "" {
		return r.query(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY created_at, id`)
	}
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
		p, err := r.scanProject(rows)
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
	return (&ProjectRepo{}).scanProject(row)
}

func (r *ProjectRepo) scanProject(row projectScanner) (*project.Project, error) {
	var s project.Snapshot
	var engineConfigRaw, tagsRaw, stateTokenCiphertext []byte
	var policyIDs []string
	var lastDrift, expiresAt *time.Time
	var created, updated time.Time
	if err := row.Scan(
		&s.ID, &s.TenantID, &s.GitHubRepoID, &s.Name, &s.Repo.Owner, &s.Repo.Name, &s.DefaultBranch, &s.WorkingDirectory,
		&s.Engine, &engineConfigRaw, &s.TerraformVersion, &s.AutoApprove, &tagsRaw, &s.Locked, &s.StateToken, &s.StateTokenHash, &stateTokenCiphertext, &s.ActiveRunID,
		&s.DriftEnabled, &s.DriftIntervalMinutes, &s.DriftStatus, &lastDrift,
		&s.TTLHours, &expiresAt, &s.AutoDestroy, &created, &updated, &s.FolderID, &s.TagsOverridden, &policyIDs,
	); err != nil {
		return nil, err
	}
	if len(policyIDs) > 0 {
		s.PolicyIDs = policyIDs
	}
	if len(engineConfigRaw) > 0 {
		_ = json.Unmarshal(engineConfigRaw, &s.EngineConfig)
	}
	if s.StateToken == "" && len(stateTokenCiphertext) > 0 && r.sealer != nil {
		pt, err := r.sealer.OpenWithAAD(stateTokenCiphertext, ProjectStateTokenAAD(s.ID))
		if err != nil {
			return nil, fmt.Errorf("decrypt project state token: %w", err)
		}
		s.StateToken = string(pt)
	}
	if s.StateTokenHash == "" && s.StateToken != "" {
		s.StateTokenHash = crypto.HashToken(s.StateToken)
	}
	if len(tagsRaw) > 0 {
		_ = json.Unmarshal(tagsRaw, &s.Tags)
	}
	s.LastDriftCheckAt = utcTimePtr(lastDrift)
	s.ExpiresAt = utcTimePtr(expiresAt)
	s.CreatedAt = utcTime(created)
	s.UpdatedAt = utcTime(updated)
	return project.Restore(s), nil
}
