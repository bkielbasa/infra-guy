package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/helmrelease"
)

var _ helmrelease.Repository = (*HelmReleaseRepo)(nil)

type HelmReleaseRepo struct {
	pool *pgxpool.Pool
}

func NewHelmReleaseRepo(pool *pgxpool.Pool) *HelmReleaseRepo {
	return &HelmReleaseRepo{pool: pool}
}

func (r *HelmReleaseRepo) Replace(ctx context.Context, projectID, release, namespace string, revisions []helmrelease.Revision) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM helm_revisions WHERE project_id = $1`, projectID); err != nil {
		return err
	}
	for _, rev := range revisions {
		if rev.Revision <= 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO helm_revisions (
				project_id, release_name, namespace, revision, updated, status, chart, app_version, description
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			projectID, release, namespace, rev.Revision, rev.Updated, rev.Status, rev.Chart, rev.AppVersion, rev.Description,
		); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *HelmReleaseRepo) List(ctx context.Context, projectID string) (string, string, []helmrelease.Revision, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT release_name, namespace, revision, updated, status, chart, app_version, description
		FROM helm_revisions
		WHERE project_id = $1
		ORDER BY revision DESC`, projectID)
	if err != nil {
		return "", "", nil, err
	}
	defer rows.Close()
	var release, namespace string
	var out []helmrelease.Revision
	for rows.Next() {
		var rev helmrelease.Revision
		if err := rows.Scan(&release, &namespace, &rev.Revision, &rev.Updated, &rev.Status, &rev.Chart, &rev.AppVersion, &rev.Description); err != nil {
			return "", "", nil, err
		}
		out = append(out, rev)
	}
	return release, namespace, out, rows.Err()
}
