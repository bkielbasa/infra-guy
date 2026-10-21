package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/tfcontext"
	"infra-guy/internal/infrastructure/crypto"
)

var _ tfcontext.Repository = (*ContextRepo)(nil)

type ContextRepo struct {
	pool   *pgxpool.Pool
	sealer *crypto.Sealer
}

func NewContextRepo(pool *pgxpool.Pool, sealer *crypto.Sealer) *ContextRepo {
	return &ContextRepo{pool: pool, sealer: sealer}
}

func (r *ContextRepo) Create(ctx context.Context, c *tfcontext.Context) error {
	s := c.Snapshot()
	_, err := r.pool.Exec(ctx, `
		INSERT INTO contexts (id, tenant_id, name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)`,
		s.ID, s.TenantID, s.Name, s.CreatedAt, s.UpdatedAt,
	)
	return err
}

func (r *ContextRepo) Get(ctx context.Context, id tfcontext.ID, decryptSecrets bool) (*tfcontext.Context, error) {
	var s tfcontext.Snapshot
	var created, updated time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, created_at, updated_at
		FROM contexts WHERE id = $1`, id,
	).Scan(&s.ID, &s.TenantID, &s.Name, &created, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt = utcTime(created)
	s.UpdatedAt = utcTime(updated)
	entries, err := r.loadEntries(ctx, s.ID, decryptSecrets)
	if err != nil {
		return nil, err
	}
	s.Entries = entries
	return tfcontext.Restore(s), nil
}

func (r *ContextRepo) ListByTenant(ctx context.Context, tenantID string) ([]*tfcontext.Context, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, name, created_at, updated_at
		FROM contexts WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tfcontext.Context
	for rows.Next() {
		var s tfcontext.Snapshot
		var created, updated time.Time
		if err := rows.Scan(&s.ID, &s.TenantID, &s.Name, &created, &updated); err != nil {
			return nil, err
		}
		s.CreatedAt = utcTime(created)
		s.UpdatedAt = utcTime(updated)
		out = append(out, tfcontext.Restore(s))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range out {
		entries, err := r.loadEntries(ctx, c.Snapshot().ID, false)
		if err != nil {
			return nil, err
		}
		c.SetEntries(entries)
	}
	return out, nil
}

func (r *ContextRepo) Delete(ctx context.Context, id tfcontext.ID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM contexts WHERE id = $1`, id)
	return err
}

func (r *ContextRepo) UpsertEntry(ctx context.Context, contextID tfcontext.ID, e tfcontext.Entry) error {
	var textVal string
	var cipherVal []byte
	if e.Secret {
		ct, err := r.sealer.Seal([]byte(e.Value))
		if err != nil {
			return err
		}
		cipherVal = ct
	} else {
		textVal = e.Value
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO context_entries (id, context_id, kind, key, value_text, value_ciphertext, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (context_id, kind, key) DO UPDATE SET
			value_text = EXCLUDED.value_text,
			value_ciphertext = EXCLUDED.value_ciphertext,
			updated_at = EXCLUDED.updated_at`,
		e.ID, contextID, e.Kind, e.Key, textVal, cipherVal, e.CreatedAt, e.UpdatedAt,
	)
	return err
}

func (r *ContextRepo) DeleteEntry(ctx context.Context, contextID tfcontext.ID, entryID string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM context_entries WHERE context_id = $1 AND id = $2`, contextID, entryID)
	return err
}

func (r *ContextRepo) SetProjectContexts(ctx context.Context, projectID project.ID, contextIDs []tfcontext.ID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM project_contexts WHERE project_id = $1`, projectID); err != nil {
		return err
	}
	seen := make(map[tfcontext.ID]bool, len(contextIDs))
	pos := 0
	for _, cid := range contextIDs {
		if cid == "" || seen[cid] {
			continue
		}
		seen[cid] = true
		if _, err := tx.Exec(ctx, `
			INSERT INTO project_contexts (project_id, context_id, position)
			VALUES ($1, $2, $3)`, projectID, cid, pos); err != nil {
			return err
		}
		pos++
	}
	return tx.Commit(ctx)
}

func (r *ContextRepo) ListByProject(ctx context.Context, projectID project.ID, decryptSecrets bool) ([]*tfcontext.Context, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT c.id, c.tenant_id, c.name, c.created_at, c.updated_at
		FROM project_contexts pc
		JOIN contexts c ON c.id = pc.context_id
		WHERE pc.project_id = $1
		ORDER BY pc.position ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tfcontext.Context
	for rows.Next() {
		var s tfcontext.Snapshot
		var created, updated time.Time
		if err := rows.Scan(&s.ID, &s.TenantID, &s.Name, &created, &updated); err != nil {
			return nil, err
		}
		s.CreatedAt = utcTime(created)
		s.UpdatedAt = utcTime(updated)
		out = append(out, tfcontext.Restore(s))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range out {
		entries, err := r.loadEntries(ctx, c.Snapshot().ID, decryptSecrets)
		if err != nil {
			return nil, err
		}
		c.SetEntries(entries)
	}
	return out, nil
}

func (r *ContextRepo) loadEntries(ctx context.Context, contextID tfcontext.ID, decryptSecrets bool) ([]tfcontext.Entry, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, kind, key, value_text, value_ciphertext, created_at, updated_at
		FROM context_entries WHERE context_id = $1 ORDER BY created_at, id`, contextID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tfcontext.Entry
	for rows.Next() {
		var e tfcontext.Entry
		var textVal string
		var cipherVal []byte
		var created, updated time.Time
		if err := rows.Scan(&e.ID, &e.Kind, &e.Key, &textVal, &cipherVal, &created, &updated); err != nil {
			return nil, err
		}
		e.CreatedAt = utcTime(created)
		e.UpdatedAt = utcTime(updated)
		e.Secret = e.Kind == tfcontext.KindSecretEnv || e.Kind == tfcontext.KindSecretFile
		if e.Secret {
			if decryptSecrets && len(cipherVal) > 0 {
				pt, err := r.sealer.Open(cipherVal)
				if err != nil {
					return nil, err
				}
				e.Value = string(pt)
			}
		} else {
			e.Value = textVal
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
