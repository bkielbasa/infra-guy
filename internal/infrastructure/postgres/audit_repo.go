package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/audit"
)

var _ audit.Repository = (*AuditRepo)(nil)

type AuditRepo struct {
	pool *pgxpool.Pool
}

func NewAuditRepo(pool *pgxpool.Pool) *AuditRepo {
	return &AuditRepo{pool: pool}
}

const auditColumns = `id, tenant_id, actor_id, actor_login, actor_role, action, resource_type, resource_id, resource_name, summary, metadata, ip_address, prev_hash, entry_hash, created_at`

func (r *AuditRepo) Append(ctx context.Context, entry audit.Snapshot) (audit.Snapshot, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return audit.Snapshot{}, err
	}
	defer tx.Rollback(ctx)

	var prevHash string
	err = tx.QueryRow(ctx, `
		SELECT entry_hash
		FROM audit_logs
		WHERE tenant_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT 1`,
		entry.TenantID,
	).Scan(&prevHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return audit.Snapshot{}, err
	}

	entry.CreatedAt = entry.CreatedAt.UTC().Truncate(time.Microsecond)
	entry.PrevHash = prevHash
	entry.EntryHash = audit.ComputeHash(
		entry.PrevHash,
		entry.TenantID,
		entry.CreatedAt,
		entry.ActorLogin,
		entry.Action,
		entry.ResourceType,
		entry.ResourceID,
		entry.Summary,
	)

	metaJSON := []byte("{}")
	if len(entry.Metadata) > 0 {
		if b, mErr := json.Marshal(entry.Metadata); mErr == nil {
			metaJSON = b
		}
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO audit_logs (`+auditColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		entry.ID,
		entry.TenantID,
		entry.ActorID,
		entry.ActorLogin,
		entry.ActorRole,
		entry.Action,
		entry.ResourceType,
		entry.ResourceID,
		entry.ResourceName,
		entry.Summary,
		metaJSON,
		entry.IPAddress,
		entry.PrevHash,
		entry.EntryHash,
		entry.CreatedAt,
	)
	if err != nil {
		return audit.Snapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return audit.Snapshot{}, err
	}
	return entry, nil
}

func (r *AuditRepo) ListByTenant(ctx context.Context, tenantID string, filter audit.Filter) ([]audit.Snapshot, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+auditColumns+`
		FROM audit_logs
		WHERE tenant_id = $1
		ORDER BY created_at DESC, id DESC`,
		tenantID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []audit.Snapshot
	for rows.Next() {
		var s audit.Snapshot
		var metaRaw []byte
		var created time.Time
		if err := rows.Scan(
			&s.ID,
			&s.TenantID,
			&s.ActorID,
			&s.ActorLogin,
			&s.ActorRole,
			&s.Action,
			&s.ResourceType,
			&s.ResourceID,
			&s.ResourceName,
			&s.Summary,
			&metaRaw,
			&s.IPAddress,
			&s.PrevHash,
			&s.EntryHash,
			&created,
		); err != nil {
			return nil, err
		}
		if len(metaRaw) > 0 {
			_ = json.Unmarshal(metaRaw, &s.Metadata)
		}
		s.CreatedAt = utcTime(created)
		if !audit.MatchesFilter(s, filter) {
			continue
		}
		out = append(out, s)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, rows.Err()
}
