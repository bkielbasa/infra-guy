package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/folder"
)

type FolderRepo struct {
	pool *pgxpool.Pool
}

func NewFolderRepo(pool *pgxpool.Pool) *FolderRepo {
	return &FolderRepo{pool: pool}
}

const folderColumns = `id, tenant_id, COALESCE(parent_id, ''), name, pass_down, tags_set, tags, created_at, updated_at,
	COALESCE((SELECT array_agg(fp.policy_id ORDER BY fp.policy_id) FROM folder_policies fp WHERE fp.folder_id = folders.id), '{}'::text[])`

func (r *FolderRepo) ListByTenant(ctx context.Context, tenantID string) ([]folder.Snapshot, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+folderColumns+` FROM folders WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []folder.Snapshot
	for rows.Next() {
		s, err := r.scanFolder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *FolderRepo) Get(ctx context.Context, tenantID, id string) (folder.Snapshot, error) {
	return r.scanFolder(r.pool.QueryRow(ctx, `SELECT `+folderColumns+` FROM folders WHERE tenant_id = $1 AND id = $2`, tenantID, id))
}

func (r *FolderRepo) Save(ctx context.Context, s folder.Snapshot) error {
	tags := s.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return err
	}
	var parent any
	if s.ParentID != "" {
		parent = s.ParentID
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO folders (
			id, tenant_id, parent_id, name, pass_down, tags_set, tags, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9)
		ON CONFLICT (id) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id,
			parent_id = EXCLUDED.parent_id,
			name = EXCLUDED.name,
			pass_down = EXCLUDED.pass_down,
			tags_set = EXCLUDED.tags_set,
			tags = EXCLUDED.tags,
			created_at = EXCLUDED.created_at,
			updated_at = EXCLUDED.updated_at`,
		s.ID, s.TenantID, parent, s.Name, s.PassDown, s.TagsSet, string(tagsJSON), s.CreatedAt, s.UpdatedAt,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM folder_policies WHERE folder_id = $1`, s.ID); err != nil {
		return err
	}
	for _, id := range s.PolicyIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO folder_policies (folder_id, policy_id) VALUES ($1, $2)`, s.ID, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *FolderRepo) EnsureRoot(ctx context.Context, tenantID, id string, now time.Time) (folder.Snapshot, error) {
	existing, err := r.root(ctx, tenantID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return folder.Snapshot{}, err
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO folders (id, tenant_id, parent_id, name, pass_down, tags_set, tags, created_at, updated_at)
		VALUES ($1, $2, NULL, 'Default', false, true, '{}'::jsonb, $3, $3)`,
		id, tenantID, now)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return r.root(ctx, tenantID)
		}
		return folder.Snapshot{}, err
	}
	return r.Get(ctx, tenantID, id)
}

func (r *FolderRepo) DeleteAndReparent(ctx context.Context, tenantID, folderID, parentID string, now time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE projects
		   SET folder_id = $3, tags_overridden = false, tags = '{}'::jsonb, updated_at = $4
		 WHERE tenant_id = $1 AND folder_id = $2`,
		tenantID, folderID, parentID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE folders
		   SET parent_id = $3, updated_at = $4
		 WHERE tenant_id = $1 AND parent_id = $2`,
		tenantID, folderID, parentID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM folders WHERE tenant_id = $1 AND id = $2`, tenantID, folderID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *FolderRepo) root(ctx context.Context, tenantID string) (folder.Snapshot, error) {
	return r.scanFolder(r.pool.QueryRow(ctx, `SELECT `+folderColumns+` FROM folders WHERE tenant_id = $1 AND parent_id IS NULL`, tenantID))
}

func (r *FolderRepo) scanFolder(row projectScanner) (folder.Snapshot, error) {
	var s folder.Snapshot
	var tagsRaw []byte
	var policyIDs []string
	var created, updated time.Time
	if err := row.Scan(
		&s.ID, &s.TenantID, &s.ParentID, &s.Name, &s.PassDown, &s.TagsSet, &tagsRaw, &created, &updated, &policyIDs,
	); err != nil {
		return folder.Snapshot{}, err
	}
	if len(tagsRaw) > 0 {
		_ = json.Unmarshal(tagsRaw, &s.Tags)
	}
	if len(policyIDs) > 0 {
		s.PolicyIDs = policyIDs
	}
	s.CreatedAt = utcTime(created)
	s.UpdatedAt = utcTime(updated)
	return s, nil
}
