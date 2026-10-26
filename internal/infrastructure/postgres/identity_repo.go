package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/tenant"
	"infra-guy/internal/domain/user"
	"infra-guy/internal/infrastructure/crypto"
)

// Session is a signed-in user's server-side session. TenantID is empty until a
// tenant has been selected.
type Session = user.Session

type IdentityStore struct{ pool *pgxpool.Pool }

func NewIdentityStore(pool *pgxpool.Pool) *IdentityStore { return &IdentityStore{pool: pool} }

// UpsertUser resolves the user by (provider, issuer, subject) in identities.
// If the identity exists, it updates the existing user's login and display_name;
// otherwise it inserts a new user and identity row.
func (s *IdentityStore) UpsertUser(ctx context.Context, u user.User, ident user.Identity, now time.Time) (user.User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return user.User{}, err
	}
	defer tx.Rollback(ctx)

	var existingUserID string
	err = tx.QueryRow(ctx, `
		SELECT user_id FROM identities
		WHERE provider = $1 AND issuer = $2 AND subject = $3`,
		ident.Provider, ident.Issuer, ident.Subject,
	).Scan(&existingUserID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return user.User{}, err
	}

	var out user.User
	if existingUserID != "" {
		if err := tx.QueryRow(ctx, `
			UPDATE users
			SET login = $2, display_name = $3, updated_at = $4
			WHERE id = $1
			RETURNING id, login, display_name`,
			existingUserID, u.Login, u.DisplayName, now,
		).Scan(&out.ID, &out.Login, &out.DisplayName); err != nil {
			return user.User{}, err
		}
	} else {
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (id, login, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $4)
			RETURNING id, login, display_name`,
			u.ID, u.Login, u.DisplayName, now,
		).Scan(&out.ID, &out.Login, &out.DisplayName); err != nil {
			return user.User{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO identities (user_id, provider, issuer, subject)
			VALUES ($1, $2, $3, $4)`,
			out.ID, ident.Provider, ident.Issuer, ident.Subject,
		); err != nil {
			return user.User{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return user.User{}, err
	}
	return out, nil
}

// HasIdentity reports whether userID is linked to (provider, issuer, subject).
func (s *IdentityStore) HasIdentity(ctx context.Context, userID, provider, issuer, subject string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM identities
			WHERE user_id = $1 AND provider = $2 AND issuer = $3 AND subject = $4
		)`, userID, provider, issuer, subject,
	).Scan(&exists)
	return exists, err
}

// UpsertMembership inserts or updates a user's role in a tenant.
func (s *IdentityStore) UpsertMembership(ctx context.Context, m tenant.Membership, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tenant_memberships (tenant_id, user_id, role, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET
			role = EXCLUDED.role,
			updated_at = EXCLUDED.updated_at`,
		m.TenantID, m.UserID, m.Role, now,
	)
	return err
}

// DeleteMembership removes a user's membership in a tenant.
func (s *IdentityStore) DeleteMembership(ctx context.Context, tenantID, userID string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID,
	)
	return err
}

// GetMembership returns the user's membership for an active tenant installation, or nil when not a member.
func (s *IdentityStore) GetMembership(ctx context.Context, tenantID, userID string) (*tenant.Membership, error) {
	var m tenant.Membership
	err := s.pool.QueryRow(ctx, `
		SELECT tm.tenant_id, ins.account_login, tm.user_id, tm.role
		FROM tenant_memberships tm
		JOIN installations ins ON ins.tenant_id = tm.tenant_id AND ins.active
		WHERE tm.tenant_id = $1 AND tm.user_id = $2`,
		tenantID, userID,
	).Scan(&m.TenantID, &m.TenantName, &m.UserID, &m.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListUserMemberships returns all active tenant memberships for userID.
func (s *IdentityStore) ListUserMemberships(ctx context.Context, userID string) ([]tenant.Membership, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tm.tenant_id, ins.account_login, tm.user_id, tm.role
		FROM tenant_memberships tm
		JOIN installations ins ON ins.tenant_id = tm.tenant_id AND ins.active
		WHERE tm.user_id = $1
		ORDER BY ins.created_at, ins.id`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tenant.Membership
	for rows.Next() {
		var m tenant.Membership
		if err := rows.Scan(&m.TenantID, &m.TenantName, &m.UserID, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
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

// SaveSession inserts or replaces the session row using a SHA-256 hash of the session token.
// An empty TenantID is stored as NULL.
func (s *IdentityStore) SaveSession(ctx context.Context, sess Session) error {
	var tenantID *string
	if sess.TenantID != "" {
		tenantID = &sess.TenantID
	}
	hashedID := crypto.HashToken(sess.ID)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, tenant_id, role, login, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			user_id = EXCLUDED.user_id,
			tenant_id = EXCLUDED.tenant_id,
			role = EXCLUDED.role,
			login = EXCLUDED.login,
			expires_at = EXCLUDED.expires_at`,
		hashedID, sess.UserID, tenantID, sess.Role, sess.Login, sess.ExpiresAt)
	return err
}

// GetSession returns nil when the session does not exist.
func (s *IdentityStore) GetSession(ctx context.Context, id string) (*Session, error) {
	if id == "" {
		return nil, nil
	}
	hashedID := crypto.HashToken(id)
	var sess Session
	var tenantID *string
	var expires time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, tenant_id, role, login, expires_at
		FROM sessions WHERE id = $1 OR id = $2
		ORDER BY (id = $1) DESC
		LIMIT 1`, hashedID, id,
	).Scan(&sess.ID, &sess.UserID, &tenantID, &sess.Role, &sess.Login, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sess.ID = id
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

// ConsumeOAuthState deletes the state and returns its expiration time when it
// existed.
func (s *IdentityStore) ConsumeOAuthState(ctx context.Context, state string) (time.Time, bool, error) {
	var expires time.Time
	err := s.pool.QueryRow(ctx, `DELETE FROM oauth_states WHERE state = $1 RETURNING expires_at`, state).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return utcTime(expires), true, nil
}
