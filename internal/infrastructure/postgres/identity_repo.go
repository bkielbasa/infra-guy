package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/tenant"
	"infra-guy/internal/domain/user"
)

// Session is a signed-in user's server-side session. TenantID is empty until a
// tenant has been selected.
type Session struct {
	ID          string
	UserID      string
	TenantID    string
	Role        string
	Login       string
	AccessToken string
	ExpiresAt   time.Time
}

type IdentityStore struct{ pool *pgxpool.Pool }

func NewIdentityStore(pool *pgxpool.Pool) *IdentityStore { return &IdentityStore{pool: pool} }

// UpsertUser inserts or updates by GitHub user id and keeps the stored id and
// GitHub identity row. It returns the stored user.
func (s *IdentityStore) UpsertUser(ctx context.Context, u user.User, now time.Time) (user.User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return user.User{}, err
	}
	defer tx.Rollback(ctx)
	var out user.User
	if err := tx.QueryRow(ctx, `
		INSERT INTO users (id, github_user_id, login, display_name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)
		ON CONFLICT (github_user_id) DO UPDATE SET
			login = EXCLUDED.login,
			display_name = EXCLUDED.display_name,
			updated_at = EXCLUDED.updated_at
		RETURNING id, github_user_id, login, display_name`,
		u.ID, u.GitHubUserID, u.Login, u.DisplayName, now,
	).Scan(&out.ID, &out.GitHubUserID, &out.Login, &out.DisplayName); err != nil {
		return user.User{}, err
	}
	ident := user.GitHubIdentity(out.ID, out.GitHubUserID)
	if _, err := tx.Exec(ctx, `
		INSERT INTO identities (user_id, provider, issuer, subject)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (provider, issuer, subject) DO NOTHING`,
		ident.UserID, ident.Provider, ident.Issuer, ident.Subject,
	); err != nil {
		return user.User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return user.User{}, err
	}
	return out, nil
}

// UpsertTenant inserts or updates by GitHub account id. It returns the stored
// tenant, which keeps its original id on update.
func (s *IdentityStore) UpsertTenant(ctx context.Context, t tenant.Tenant, now time.Time) (tenant.Tenant, error) {
	var out tenant.Tenant
	err := s.pool.QueryRow(ctx, `
		INSERT INTO tenants (id, name, github_account_id, account_type, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)
		ON CONFLICT (github_account_id) DO UPDATE SET
			name = EXCLUDED.name,
			account_type = EXCLUDED.account_type,
			updated_at = EXCLUDED.updated_at
		RETURNING id, name, account_type, github_account_id`,
		t.ID, t.Name, t.GitHubAccountID, t.AccountType, now,
	).Scan(&out.ID, &out.Name, &out.AccountType, &out.GitHubAccountID)
	return out, err
}

// UpsertInstallation makes in the tenant's only active installation.
func (s *IdentityStore) UpsertInstallation(ctx context.Context, in tenant.Installation, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE installations SET active = false, updated_at = $3
		WHERE tenant_id = $1 AND github_installation_id <> $2 AND active`,
		in.TenantID, in.GitHubInstallationID, now,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO installations (id, tenant_id, github_installation_id, account_login, account_type, active, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, true, $6, $6)
		ON CONFLICT (github_installation_id) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id,
			account_login = EXCLUDED.account_login,
			account_type = EXCLUDED.account_type,
			active = true,
			updated_at = EXCLUDED.updated_at`,
		in.ID, in.TenantID, in.GitHubInstallationID, in.AccountLogin, in.AccountType, now,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const installationColumns = `id, tenant_id, github_installation_id, account_login, account_type, active`

func scanInstallation(row pgx.Row) (*tenant.Installation, error) {
	var in tenant.Installation
	err := row.Scan(&in.ID, &in.TenantID, &in.GitHubInstallationID, &in.AccountLogin, &in.AccountType, &in.Active)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &in, nil
}

// ActiveInstallation returns nil when the tenant has no active installation.
func (s *IdentityStore) ActiveInstallation(ctx context.Context, tenantID string) (*tenant.Installation, error) {
	return scanInstallation(s.pool.QueryRow(ctx,
		`SELECT `+installationColumns+` FROM installations WHERE tenant_id = $1 AND active`, tenantID))
}

// InstallationByGitHubID returns nil when GitHub's installation id is unknown.
func (s *IdentityStore) InstallationByGitHubID(ctx context.Context, githubInstallationID int64) (*tenant.Installation, error) {
	return scanInstallation(s.pool.QueryRow(ctx,
		`SELECT `+installationColumns+` FROM installations WHERE github_installation_id = $1`, githubInstallationID))
}

func (s *IdentityStore) ListActiveInstallations(ctx context.Context) ([]tenant.Installation, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+installationColumns+` FROM installations WHERE active ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tenant.Installation
	for rows.Next() {
		var in tenant.Installation
		if err := rows.Scan(&in.ID, &in.TenantID, &in.GitHubInstallationID, &in.AccountLogin, &in.AccountType, &in.Active); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *IdentityStore) DeactivateInstallation(ctx context.Context, githubInstallationID int64, now time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE installations SET active = false, updated_at = $2 WHERE github_installation_id = $1`,
		githubInstallationID, now)
	return err
}

// SaveSession inserts or replaces the session row. An empty TenantID is stored as NULL.
func (s *IdentityStore) SaveSession(ctx context.Context, sess Session) error {
	var tenantID *string
	if sess.TenantID != "" {
		tenantID = &sess.TenantID
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, tenant_id, role, login, access_token, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			user_id = EXCLUDED.user_id,
			tenant_id = EXCLUDED.tenant_id,
			role = EXCLUDED.role,
			login = EXCLUDED.login,
			access_token = EXCLUDED.access_token,
			expires_at = EXCLUDED.expires_at`,
		sess.ID, sess.UserID, tenantID, sess.Role, sess.Login, sess.AccessToken, sess.ExpiresAt)
	return err
}

// GetSession returns nil when the session does not exist.
func (s *IdentityStore) GetSession(ctx context.Context, id string) (*Session, error) {
	var sess Session
	var tenantID *string
	var expires time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, tenant_id, role, login, access_token, expires_at
		FROM sessions WHERE id = $1`, id,
	).Scan(&sess.ID, &sess.UserID, &tenantID, &sess.Role, &sess.Login, &sess.AccessToken, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if tenantID != nil {
		sess.TenantID = *tenantID
	}
	sess.ExpiresAt = utcTime(expires)
	return &sess, nil
}

func (s *IdentityStore) SaveOAuthState(ctx context.Context, state string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO oauth_states (state, expires_at) VALUES ($1, $2)
		ON CONFLICT (state) DO UPDATE SET expires_at = EXCLUDED.expires_at`, state, expiresAt)
	return err
}

// ConsumeOAuthState deletes the state and reports whether it existed and had
// not expired at now.
func (s *IdentityStore) ConsumeOAuthState(ctx context.Context, state string, now time.Time) (bool, error) {
	var expires time.Time
	err := s.pool.QueryRow(ctx, `DELETE FROM oauth_states WHERE state = $1 RETURNING expires_at`, state).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !expires.Before(now), nil
}
