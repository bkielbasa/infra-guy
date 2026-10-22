package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/dependency"
	"infra-guy/internal/domain/project"
)

var _ dependency.Repository = (*DependencyRepo)(nil)

type DependencyRepo struct {
	pool *pgxpool.Pool
}

func NewDependencyRepo(pool *pgxpool.Pool) *DependencyRepo {
	return &DependencyRepo{pool: pool}
}

func (r *DependencyRepo) Create(ctx context.Context, d *dependency.Dependency) error {
	s := d.Snapshot()
	_, err := r.pool.Exec(ctx, `
		INSERT INTO project_dependencies (id, tenant_id, project_id, depends_on_project_id, trigger_always, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		s.ID, s.TenantID, s.ProjectID, s.DependsOnProjectID, s.TriggerAlways, s.CreatedAt,
	)
	return err
}

func (r *DependencyRepo) Get(ctx context.Context, id dependency.ID) (*dependency.Dependency, error) {
	var s dependency.Snapshot
	var created time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, project_id, depends_on_project_id, trigger_always, created_at
		FROM project_dependencies WHERE id = $1`, id,
	).Scan(&s.ID, &s.TenantID, &s.ProjectID, &s.DependsOnProjectID, &s.TriggerAlways, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt = utcTime(created)
	mappings, err := r.loadMappings(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	s.Mappings = mappings
	return dependency.Restore(s), nil
}

func (r *DependencyRepo) SetTriggerAlways(ctx context.Context, id dependency.ID, triggerAlways bool) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE project_dependencies SET trigger_always = $2 WHERE id = $1`,
		id, triggerAlways,
	)
	return err
}

func (r *DependencyRepo) Delete(ctx context.Context, id dependency.ID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM project_dependencies WHERE id = $1`, id)
	return err
}

func (r *DependencyRepo) UpsertMapping(ctx context.Context, depID dependency.ID, m dependency.Mapping) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO project_dependency_mappings (id, dependency_id, output_name, input_name, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (dependency_id, input_name) DO UPDATE SET
			output_name = EXCLUDED.output_name`,
		m.ID, depID, m.OutputName, m.InputName, m.CreatedAt,
	)
	return err
}

func (r *DependencyRepo) DeleteMapping(ctx context.Context, depID dependency.ID, mappingID string) error {
	_, err := r.pool.Exec(ctx, `
		DELETE FROM project_dependency_mappings WHERE dependency_id = $1 AND id = $2`,
		depID, mappingID,
	)
	return err
}

func (r *DependencyRepo) ListByProject(ctx context.Context, projectID project.ID) ([]*dependency.Dependency, error) {
	return r.listWhere(ctx, `project_id = $1`, projectID)
}

func (r *DependencyRepo) ListDownstream(ctx context.Context, upstreamProjectID project.ID) ([]*dependency.Dependency, error) {
	return r.listWhere(ctx, `depends_on_project_id = $1`, upstreamProjectID)
}

func (r *DependencyRepo) ListByTenant(ctx context.Context, tenantID string) ([]*dependency.Dependency, error) {
	return r.listWhere(ctx, `tenant_id = $1`, tenantID)
}

func (r *DependencyRepo) listWhere(ctx context.Context, predicate string, arg any) ([]*dependency.Dependency, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, project_id, depends_on_project_id, trigger_always, created_at
		FROM project_dependencies WHERE `+predicate+` ORDER BY created_at, id`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dependency.Dependency
	for rows.Next() {
		var s dependency.Snapshot
		var created time.Time
		if err := rows.Scan(&s.ID, &s.TenantID, &s.ProjectID, &s.DependsOnProjectID, &s.TriggerAlways, &created); err != nil {
			return nil, err
		}
		s.CreatedAt = utcTime(created)
		out = append(out, dependency.Restore(s))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, d := range out {
		mappings, err := r.loadMappings(ctx, d.Snapshot().ID)
		if err != nil {
			return nil, err
		}
		d.SetMappings(mappings)
	}
	return out, nil
}

func (r *DependencyRepo) loadMappings(ctx context.Context, depID dependency.ID) ([]dependency.Mapping, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, output_name, input_name, created_at
		FROM project_dependency_mappings WHERE dependency_id = $1 ORDER BY created_at, id`, depID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dependency.Mapping
	for rows.Next() {
		var m dependency.Mapping
		var created time.Time
		if err := rows.Scan(&m.ID, &m.OutputName, &m.InputName, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = utcTime(created)
		out = append(out, m)
	}
	return out, rows.Err()
}
