# Identity Provider Decoupling & Sliding Sessions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Decouple `user.User`, `user.Session`, and tenant authorization from GitHub OAuth tokens so GitHub acts strictly as an Identity Provider (IdP) during sign-in, persist tenant memberships in Postgres (`tenant_memberships`), and replace the 1-hour fixed session expiry with a 7-day sliding session window.

**Architecture:**
1. **Identity-First Users & Persisted Memberships:** Drop `github_user_id` from `users` and `access_token` from `sessions` (`008_idp_sessions.sql`). Resolve users via `identities (provider, issuer, subject)` and persist tenant access in `tenant_memberships (tenant_id, user_id, role)`.
2. **IdP Sign-In & DB-Backed Authorization:** `auth.Service.Complete` uses the GitHub OAuth token once during sign-in to sync `tenant_memberships`, then discards the token. `Allowed` and `Select` read strictly from `tenant_memberships` in Postgres with zero GitHub API calls.
3. **7-Day Sliding Sessions:** Sessions expire after 7 days (`DefaultSessionTTL = 7 * 24 * time.Hour`) and slide forward on activity when at least 1 hour (`DefaultRenewThreshold = time.Hour`) has elapsed since creation or the last renewal, refreshing both the Postgres row and the HTTP cookie.

**Tech Stack:** Go 1.24+, PostgreSQL (`pgx/v5`), standard `net/http`.

---

## File Map

| File | Action | Responsibility |
|---|---|---|
| `internal/domain/user/user.go` | Modify | Remove `GitHubUserID` from `User` and `AccessToken` from `Session`. |
| `internal/domain/tenant/tenant.go` | Modify | Add `Membership` struct (`TenantID`, `TenantName`, `UserID`, `Role`). |
| `internal/infrastructure/postgres/migrations/008_idp_sessions.sql` | Create | Backfill `identities`, drop `users.github_user_id` and `sessions.access_token`, create and backfill `tenant_memberships`. |
| `internal/infrastructure/postgres/identity_repo.go` | Modify | Resolve `UpsertUser` via `identities`, drop `access_token` from session queries, and add `HasIdentity`, `UpsertMembership`, `DeleteMembership`, `GetMembership`, and `ListUserMemberships`. |
| `internal/infrastructure/postgres/repos_test.go` | Modify | Update `TestIdentityStore` for identity-first user upsert, session persistence without `AccessToken`, and `tenant_memberships` CRUD. |
| `internal/application/auth/service.go` | Modify | Sync `tenant_memberships` in `Complete`, query DB in `Allowed`/`Select`/`EnterInstallation`, and add `Authenticate` with 7-day TTL and 1-hour sliding renewal. |
| `internal/application/auth/service_test.go` | Modify | Test IdP membership sync, DB-backed `Allowed`/`Select`, personal vs org `EnterInstallation`, and sliding session `Authenticate`. |
| `internal/interfaces/http/session.go` | Modify | Use `cfg.Auth.Authenticate` in `withSession` and refresh session cookie when renewed. |
| `internal/interfaces/http/github_auth.go` | Modify | Redirect `/github/setup` to `/github/login` on `auth.ErrDenied` when org membership needs syncing. |
| `internal/interfaces/http/ui_test.go` | Modify | Update `uiIdentity` mock and add tests for sliding session cookie renewal and `/github/setup` org redirect. |

---

### Task 1: Domain Models, Migration, Postgres `IdentityStore`, and `auth.Service`

**Files:**
- Modify: `internal/domain/user/user.go`
- Modify: `internal/domain/tenant/tenant.go`
- Create: `internal/infrastructure/postgres/migrations/008_idp_sessions.sql`
- Modify: `internal/infrastructure/postgres/identity_repo.go`
- Modify: `internal/infrastructure/postgres/repos_test.go`
- Modify: `internal/application/auth/service.go`
- Modify: `internal/application/auth/service_test.go`
- Modify: `internal/interfaces/http/ui_test.go` (update `uiIdentity` mock to satisfy `auth.Store`)

- [ ] **Step 1: Update `internal/domain/user/user.go` and `internal/domain/tenant/tenant.go`**

In `internal/domain/user/user.go`, remove `GitHubUserID` from `User` and `AccessToken` from `Session`:
```go
package user

import (
	"strconv"
	"time"
)

type User struct {
	ID          string
	Login       string
	DisplayName string
}

type Identity struct {
	UserID   string
	Provider string
	Issuer   string
	Subject  string
}

func GitHubIdentity(userID string, githubUserID int64) Identity {
	return Identity{
		UserID: userID, Provider: "github",
		Issuer: "https://github.com", Subject: strconv.FormatInt(githubUserID, 10),
	}
}

type Session struct {
	ID        string
	UserID    string
	TenantID  string
	Role      string
	Login     string
	ExpiresAt time.Time
}
```

In `internal/domain/tenant/tenant.go`, add `Membership`:
```go
type Membership struct {
	TenantID   string
	TenantName string
	UserID     string
	Role       string
}
```

- [ ] **Step 2: Create migration `internal/infrastructure/postgres/migrations/008_idp_sessions.sql`**

```sql
INSERT INTO identities (user_id, provider, issuer, subject)
SELECT id, 'github', 'https://github.com', github_user_id::text
FROM users
ON CONFLICT (provider, issuer, subject) DO NOTHING;

ALTER TABLE users DROP COLUMN github_user_id;

CREATE TABLE tenant_memberships (
    tenant_id text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, user_id)
);

CREATE INDEX tenant_memberships_user_idx ON tenant_memberships (user_id);

INSERT INTO tenant_memberships (tenant_id, user_id, role, created_at, updated_at)
SELECT DISTINCT tenant_id, user_id, role, now(), now()
FROM sessions
WHERE tenant_id IS NOT NULL AND tenant_id <> '' AND role <> ''
ON CONFLICT (tenant_id, user_id) DO NOTHING;

ALTER TABLE sessions DROP COLUMN access_token;
```

- [ ] **Step 3: Update `internal/infrastructure/postgres/identity_repo.go`**

Update `UpsertUser`, `SaveSession`, `GetSession`, and add `HasIdentity`, `UpsertMembership`, `DeleteMembership`, `GetMembership`, and `ListUserMemberships`:

```go
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
```

And update `SaveSession` and `GetSession` in `internal/infrastructure/postgres/identity_repo.go` to omit `access_token`:
```go
// SaveSession inserts or replaces the session row. An empty TenantID is stored as NULL.
func (s *IdentityStore) SaveSession(ctx context.Context, sess Session) error {
	var tenantID *string
	if sess.TenantID != "" {
		tenantID = &sess.TenantID
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, tenant_id, role, login, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			user_id = EXCLUDED.user_id,
			tenant_id = EXCLUDED.tenant_id,
			role = EXCLUDED.role,
			login = EXCLUDED.login,
			expires_at = EXCLUDED.expires_at`,
		sess.ID, sess.UserID, tenantID, sess.Role, sess.Login, sess.ExpiresAt)
	return err
}

// GetSession returns nil when the session does not exist.
func (s *IdentityStore) GetSession(ctx context.Context, id string) (*Session, error) {
	var sess Session
	var tenantID *string
	var expires time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, tenant_id, role, login, expires_at
		FROM sessions WHERE id = $1`, id,
	).Scan(&sess.ID, &sess.UserID, &tenantID, &sess.Role, &sess.Login, &expires)
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
```

- [ ] **Step 4: Update `internal/application/auth/service.go`**

Update `auth.Store` and `auth.Service`:
- Add `DefaultSessionTTL = 7 * 24 * time.Hour` and `DefaultRenewThreshold = time.Hour`.
- Export `ErrDenied = errors.New("tenant access denied")`.
- In `Complete`: sync GitHub memberships (`UpsertMembership` / `DeleteMembership`) using the transient OAuth token, load `ListUserMemberships(ctx, stored.ID)`, and save `Session` with `ExpiresAt: now.Add(s.sessionTTL())` (and without `AccessToken`).
- In `Allowed`: load `ListUserMemberships(ctx, sess.UserID)` and map to `[]TenantChoice` without calling GitHub.
- In `Select`: load `GetMembership(ctx, tenantID, sess.UserID)` (returning `ErrDenied` if `nil`) and update `sess.TenantID` and `sess.Role` without calling GitHub.
- In `EnterInstallation`: when `sessionID != ""`, open the session, check `GetMembership(ctx, ten.ID, sess.UserID)`, and if `nil` and `accountType == "user"`, check `HasIdentity(ctx, sess.UserID, "github", "https://github.com", strconv.FormatInt(accountID, 10))` and upsert `admin` membership if true, then call `Select`.
- Add `Authenticate(ctx context.Context, sessionID string, now time.Time) (sess *Session, renewed bool, err error)` that returns `nil, false, nil` when unknown or expired, and extends `sess.ExpiresAt = now.Add(s.sessionTTL())` + calls `SaveSession` when `sess.ExpiresAt.Sub(now) <= s.sessionTTL()-s.renewThreshold()`.

- [ ] **Step 5: Update tests in `internal/application/auth/service_test.go`, `internal/infrastructure/postgres/repos_test.go`, and mock in `internal/interfaces/http/ui_test.go`**

1. In `internal/application/auth/service_test.go`:
   - Update `memIdentity` to implement `HasIdentity`, `UpsertMembership`, `DeleteMembership`, `GetMembership`, and `ListUserMemberships`, and resolve `UpsertUser` by `ident.Provider+":"+ident.Issuer+":"+ident.Subject`.
   - Verify `Complete` sets 7-day `ExpiresAt`, syncs memberships, and works when `Allowed`/`Select` are called later even if GitHub fails.
   - Verify `EnterInstallation` grants `admin` to personal account owner via `HasIdentity` and returns `ErrDenied` for an un-synced org installation.
   - Add `TestAuthenticateSlidingRenewal` testing that `Authenticate` does not renew within the first hour, renews after >1 hour elapsed, and returns `nil` when expired.
2. In `internal/infrastructure/postgres/repos_test.go`:
   - Update `TestIdentityStore` for `UpsertUser` (without `GitHubUserID`), `HasIdentity`, `UpsertMembership`, `GetMembership`, `ListUserMemberships`, `DeleteMembership`, and `SaveSession`/`GetSession` (without `AccessToken`).
3. In `internal/interfaces/http/ui_test.go`:
   - Update `uiIdentity` to remove `AccessToken` and `GitHubUserID` and implement the new `auth.Store` methods (`HasIdentity`, `UpsertMembership`, `DeleteMembership`, `GetMembership`, `ListUserMemberships`).

- [ ] **Step 6: Run tests and commit Task 1**

Run:
```bash
go test -count=1 ./...
```
Commit:
```bash
git add internal/domain/user/user.go internal/domain/tenant/tenant.go internal/infrastructure/postgres/migrations/008_idp_sessions.sql internal/infrastructure/postgres/identity_repo.go internal/infrastructure/postgres/repos_test.go internal/application/auth/service.go internal/application/auth/service_test.go internal/interfaces/http/ui_test.go
git commit -m "feat(auth): decouple users and sessions from github token and persist tenant memberships"
```

---

### Task 2: HTTP Sliding Session Cookie Renewal & Setup Redirect

**Files:**
- Modify: `internal/interfaces/http/session.go`
- Modify: `internal/interfaces/http/github_auth.go`
- Modify: `internal/interfaces/http/ui_test.go`

- [ ] **Step 1: Write failing HTTP tests in `internal/interfaces/http/ui_test.go`**

Add two tests in `internal/interfaces/http/ui_test.go`:
1. `TestSessionSlidingRenewalRefreshesCookie`:
   - Set up a session whose `ExpiresAt` is `now.Add(authapp.DefaultSessionTTL - 2*time.Hour)` (more than 1 hour elapsed since creation/renewal).
   - Send a `GET /` request with the session cookie.
   - Assert response status is `200 OK`, the response includes a `Set-Cookie` header for `session` with the extended expiry (`now.Add(authapp.DefaultSessionTTL)`), and the stored session's `ExpiresAt` in `store` was updated.
2. `TestGitHubSetupRedirectsToLoginWhenOrgMembershipUnsynced`:
   - Set up a signed-in user session with no membership for the newly installed organization (`installation_id=99`).
   - Send `GET /github/setup?installation_id=99`.
   - Assert the tenant & installation are upserted and the response is `302 Found` redirecting to `/github/login` (instead of `502 Bad Gateway`).

- [ ] **Step 2: Update `internal/interfaces/http/session.go` and `internal/interfaces/http/github_auth.go`**

In `internal/interfaces/http/session.go`:
```go
		now := time.Now().UTC()
		if cfg.Auth.Now != nil {
			now = cfg.Auth.Now()
		}
		sess, renewed, err := cfg.Auth.Authenticate(r.Context(), c.Value, now)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if sess == nil {
			http.Redirect(w, r, "/github/login", http.StatusFound)
			return
		}
		if renewed {
			setSessionCookie(w, cfg.PublicBaseURL, sess.ID, sess.ExpiresAt)
		}
```

In `internal/interfaces/http/github_auth.go`:
```go
	if _, err := u.cfg.Auth.EnterInstallation(r.Context(), sessionID, installationID, u.now()); err != nil {
		if errors.Is(err, authapp.ErrDenied) {
			http.Redirect(w, r, "/github/login", http.StatusFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
```

- [ ] **Step 3: Run full test suite and commit Task 2**

Run:
```bash
go test -count=1 ./...
```
Commit:
```bash
git add internal/interfaces/http/session.go internal/interfaces/http/github_auth.go internal/interfaces/http/ui_test.go
git commit -m "feat(http): renew sliding session cookies and redirect unsynced org setup to login"
```
