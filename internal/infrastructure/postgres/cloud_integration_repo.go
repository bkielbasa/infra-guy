package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/cloudintegration"
	"infra-guy/internal/domain/project"
)

var _ cloudintegration.Repository = (*CloudIntegrationRepo)(nil)

type CloudIntegrationRepo struct {
	pool *pgxpool.Pool
}

func NewCloudIntegrationRepo(pool *pgxpool.Pool) *CloudIntegrationRepo {
	return &CloudIntegrationRepo{pool: pool}
}

const cloudIntegrationColumns = `id, tenant_id, name, provider, auth_mode, role_arn, region, audience,
	session_duration_seconds, external_id, access_key_id, secret_access_key_ciphertext,
	gcp_workload_identity_provider, gcp_service_account_email,
	azure_client_id, azure_tenant_id, azure_subscription_id,
	tags, created_at, updated_at`

func (r *CloudIntegrationRepo) Save(ctx context.Context, c *cloudintegration.Integration) error {
	s := c.Snapshot()
	tagsMap := s.Tags
	if tagsMap == nil {
		tagsMap = map[string]string{}
	}
	tagsJSON, err := json.Marshal(tagsMap)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO cloud_integrations (
			id, tenant_id, name, provider, auth_mode, role_arn, region, audience,
			session_duration_seconds, external_id, access_key_id, secret_access_key_ciphertext,
			gcp_workload_identity_provider, gcp_service_account_email,
			azure_client_id, azure_tenant_id, azure_subscription_id,
			tags, created_at, updated_at
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18::jsonb,$19,$20)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			provider = EXCLUDED.provider,
			auth_mode = EXCLUDED.auth_mode,
			role_arn = EXCLUDED.role_arn,
			region = EXCLUDED.region,
			audience = EXCLUDED.audience,
			session_duration_seconds = EXCLUDED.session_duration_seconds,
			external_id = EXCLUDED.external_id,
			access_key_id = EXCLUDED.access_key_id,
			secret_access_key_ciphertext = EXCLUDED.secret_access_key_ciphertext,
			gcp_workload_identity_provider = EXCLUDED.gcp_workload_identity_provider,
			gcp_service_account_email = EXCLUDED.gcp_service_account_email,
			azure_client_id = EXCLUDED.azure_client_id,
			azure_tenant_id = EXCLUDED.azure_tenant_id,
			azure_subscription_id = EXCLUDED.azure_subscription_id,
			tags = EXCLUDED.tags,
			updated_at = EXCLUDED.updated_at`,
		s.ID, s.TenantID, s.Name, s.Provider, s.AuthMode, s.RoleARN, s.Region, s.Audience,
		s.SessionDurationSeconds, s.ExternalID, s.AccessKeyID, s.SecretAccessKeyCiphertext,
		s.GCPWorkloadIdentityProvider, s.GCPServiceAccountEmail,
		s.AzureClientID, s.AzureTenantID, s.AzureSubscriptionID,
		string(tagsJSON), s.CreatedAt, s.UpdatedAt,
	)
	return err
}

func (r *CloudIntegrationRepo) Get(ctx context.Context, id cloudintegration.ID) (*cloudintegration.Integration, error) {
	c, err := scanCloudIntegration(r.pool.QueryRow(ctx, `SELECT `+cloudIntegrationColumns+` FROM cloud_integrations WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (r *CloudIntegrationRepo) ListByTenant(ctx context.Context, tenantID string) ([]*cloudintegration.Integration, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+cloudIntegrationColumns+`
		FROM cloud_integrations
		WHERE tenant_id = $1
		ORDER BY created_at ASC, id ASC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*cloudintegration.Integration
	for rows.Next() {
		c, err := scanCloudIntegration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *CloudIntegrationRepo) Delete(ctx context.Context, id cloudintegration.ID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM cloud_integrations WHERE id = $1`, id)
	return err
}

func (r *CloudIntegrationRepo) SetProjectIntegrations(ctx context.Context, projectID project.ID, ids []cloudintegration.ID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM project_cloud_integrations WHERE project_id = $1`, projectID); err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.Exec(ctx, `
			INSERT INTO project_cloud_integrations (project_id, integration_id, position)
			VALUES ($1, $2, $3)`, projectID, id, i); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *CloudIntegrationRepo) ListByProject(ctx context.Context, projectID project.ID) ([]*cloudintegration.Integration, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT ci.id, ci.tenant_id, ci.name, ci.provider, ci.auth_mode, ci.role_arn, ci.region, ci.audience,
			ci.session_duration_seconds, ci.external_id, ci.access_key_id, ci.secret_access_key_ciphertext,
			ci.gcp_workload_identity_provider, ci.gcp_service_account_email,
			ci.azure_client_id, ci.azure_tenant_id, ci.azure_subscription_id,
			ci.tags, ci.created_at, ci.updated_at
		FROM project_cloud_integrations pci
		JOIN cloud_integrations ci ON ci.id = pci.integration_id
		WHERE pci.project_id = $1
		ORDER BY pci.position ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*cloudintegration.Integration
	for rows.Next() {
		c, err := scanCloudIntegration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanCloudIntegration(row projectScanner) (*cloudintegration.Integration, error) {
	var s cloudintegration.Snapshot
	var tagsRaw []byte
	var created, updated time.Time
	err := row.Scan(
		&s.ID, &s.TenantID, &s.Name, &s.Provider, &s.AuthMode, &s.RoleARN, &s.Region, &s.Audience,
		&s.SessionDurationSeconds, &s.ExternalID, &s.AccessKeyID, &s.SecretAccessKeyCiphertext,
		&s.GCPWorkloadIdentityProvider, &s.GCPServiceAccountEmail,
		&s.AzureClientID, &s.AzureTenantID, &s.AzureSubscriptionID,
		&tagsRaw, &created, &updated,
	)
	if err != nil {
		return nil, err
	}
	if len(tagsRaw) > 0 {
		_ = json.Unmarshal(tagsRaw, &s.Tags)
	}
	s.CreatedAt = utcTime(created)
	s.UpdatedAt = utcTime(updated)
	return cloudintegration.Restore(s), nil
}
