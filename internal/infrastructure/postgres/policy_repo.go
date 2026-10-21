package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/policy"
)

var _ policy.Repository = (*PolicyRepo)(nil)

type PolicyRepo struct {
	pool *pgxpool.Pool
}

func NewPolicyRepo(pool *pgxpool.Pool) *PolicyRepo {
	return &PolicyRepo{pool: pool}
}

const policyColumns = `id, tenant_id, name, description, rego, created_at, updated_at`

func (r *PolicyRepo) Insert(ctx context.Context, p *policy.Policy) error {
	s := p.Snapshot()
	_, err := r.pool.Exec(ctx, `
		INSERT INTO policies (`+policyColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		s.ID, s.TenantID, s.Name, s.Description, s.Rego, s.CreatedAt, s.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return policy.ErrDuplicateName
		}
		return err
	}
	return nil
}

func (r *PolicyRepo) Update(ctx context.Context, p *policy.Policy) error {
	s := p.Snapshot()
	tag, err := r.pool.Exec(ctx, `
		UPDATE policies SET
			tenant_id = $2,
			name = $3,
			description = $4,
			rego = $5,
			updated_at = $6
		WHERE id = $1`,
		s.ID, s.TenantID, s.Name, s.Description, s.Rego, s.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return policy.ErrDuplicateName
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return policy.ErrNotFound
	}
	return nil
}

func (r *PolicyRepo) Delete(ctx context.Context, id policy.ID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM policies WHERE id = $1`, id)
	return err
}

func (r *PolicyRepo) Get(ctx context.Context, id policy.ID) (*policy.Policy, error) {
	p, err := scanPolicy(r.pool.QueryRow(ctx, `SELECT `+policyColumns+` FROM policies WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (r *PolicyRepo) ListByTenant(ctx context.Context, tenantID string) ([]*policy.Policy, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+policyColumns+`
		FROM policies
		WHERE tenant_id = $1
		ORDER BY created_at ASC, id ASC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*policy.Policy
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type policyScanner interface {
	Scan(dest ...any) error
}

func scanPolicy(row policyScanner) (*policy.Policy, error) {
	var s policy.Snapshot
	var created, updated time.Time
	if err := row.Scan(
		&s.ID, &s.TenantID, &s.Name, &s.Description, &s.Rego, &created, &updated,
	); err != nil {
		return nil, err
	}
	s.CreatedAt = utcTime(created)
	s.UpdatedAt = utcTime(updated)
	return policy.Rehydrate(s), nil
}
