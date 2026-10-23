# Identity Provider Decoupling & Sliding Sessions Design

## Context & Problem

Currently, users are asked to re-authenticate frequently and sessions are tightly coupled to GitHub OAuth:

1. **Short, Fixed Session Lifetime:** `auth.Service.Complete` sets `ExpiresAt = now.Add(time.Hour)` with no sliding renewal.
2. **Session Coupled to Live GitHub OAuth Token:** `user.Session` and the `sessions` table store `access_token`. Both `auth.Service.Allowed` and `auth.Service.Select` call `GitHub.CurrentUser`, `GitHub.Membership`, and `GitHub.Installation` using `sess.AccessToken` after sign-in instead of reading persisted memberships from Postgres.
3. **GitHub-Specific User Table:** `users` requires `github_user_id bigint NOT NULL UNIQUE`, preventing future local password or OIDC users from sharing the same `users`, `sessions`, and tenant authorization model.

## Goals

1. **Treat GitHub Strictly as an Identity Provider (IdP):** Use the GitHub OAuth token only during `auth.Service.Complete` to identify the user via `identities (provider, issuer, subject)` and sync their tenant memberships into Postgres, then discard the OAuth token.
2. **Provider-Agnostic Users, Sessions & Tenant Memberships:**
   - Remove `GitHubUserID` from `user.User` and `github_user_id` from `users`; resolve users through `identities (provider, issuer, subject)`.
   - Remove `AccessToken` from `user.Session` and `access_token` from `sessions`.
   - Persist tenant access in `tenant_memberships (tenant_id, user_id, role, created_at, updated_at)` so `Allowed` and `Select` operate purely against Postgres for all IdPs (GitHub now; password and OIDC later).
3. **7-Day Sliding Sessions:** Give sessions a 7-day TTL (`DefaultSessionTTL = 7 * 24 * time.Hour`) and renew them on activity when at least 1 hour (`DefaultRenewThreshold = time.Hour`) has elapsed since creation or the last renewal.

---

## Architecture & Data Model

### 1. Database Migration (`008_idp_sessions.sql`)

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

### 2. Domain Model (`internal/domain/user`, `internal/domain/tenant`)

- **`internal/domain/user/user.go`:**
  - `user.User`:
    ```go
    type User struct {
        ID          string
        Login       string
        DisplayName string
    }
    ```
  - `user.Session`:
    ```go
    type Session struct {
        ID        string
        UserID    string
        TenantID  string
        Role      string
        Login     string
        ExpiresAt time.Time
    }
    ```
- **`internal/domain/tenant/tenant.go`:**
  - `tenant.Membership`:
    ```go
    type Membership struct {
        TenantID   string
        TenantName string
        UserID     string
        Role       string
    }
    ```

### 3. Persistence Layer (`internal/infrastructure/postgres/identity_repo.go`)

- **`UpsertUser(ctx, u user.User, ident user.Identity, now time.Time) (user.User, error)`:**
  - Runs in a transaction:
    1. Checks `SELECT user_id FROM identities WHERE provider = $1 AND issuer = $2 AND subject = $3`.
    2. If found, updates `users SET login = $2, display_name = $3, updated_at = $4 WHERE id = $1 RETURNING id, login, display_name`.
    3. If not found, inserts into `users (id, login, display_name, created_at, updated_at)` and `identities (user_id, provider, issuer, subject)`.
- **`HasIdentity(ctx, userID, provider, issuer, subject string) (bool, error)`:**
  - Checks whether `identities` has the given row for `userID`.
- **`UpsertMembership(ctx, m tenant.Membership, now time.Time) error`:**
  - Inserts or updates `(tenant_id, user_id, role, created_at, updated_at)` in `tenant_memberships`.
- **`DeleteMembership(ctx, tenantID, userID string) error`:**
  - Deletes `(tenant_id, user_id)` from `tenant_memberships`.
- **`GetMembership(ctx, tenantID, userID string) (*tenant.Membership, error)`:**
  - Returns the user's membership in `tenantID` (only when `tenantID` has an active installation) or `nil, nil`.
- **`ListUserMemberships(ctx, userID string) ([]tenant.Membership, error)`:**
  - Joins `tenant_memberships`, `tenants`, and active `installations` for `userID`, ordered by `installations.created_at, installations.id`, returning `TenantID`, `TenantName` (`installations.account_login`), `UserID`, and `Role`.
- **`SaveSession` / `GetSession`:**
  - Persist and load `id, user_id, tenant_id, role, login, expires_at` without `access_token`.

### 4. Application Service (`internal/application/auth/service.go`)

- **Constants:**
  - `DefaultSessionTTL = 7 * 24 * time.Hour`
  - `DefaultRenewThreshold = time.Hour`
- **`Complete(ctx, state, code string, now time.Time)`:**
  1. Consumes OAuth state and exchanges `code` for transient GitHub `token`.
  2. Fetches `ghID, login, name` from `s.GitHub.CurrentUser(ctx, token)`.
  3. Upserts user via `s.Store.UpsertUser(ctx, user.User{ID: candidateID, Login: login, DisplayName: name}, user.GitHubIdentity(candidateID, ghID), now)`.
  4. Syncs memberships across active installations (`s.syncGitHubMemberships(ctx, stored.ID, token, ghID, now)`):
     - For each active installation, calls `s.decide(ctx, token, ghID, in)`:
       - If allowed (`ok == true`), calls `s.Store.UpsertMembership(ctx, tenant.Membership{TenantID: in.TenantID, UserID: stored.ID, Role: role}, now)`.
       - If denied (`ok == false && !failed`), calls `s.Store.DeleteMembership(ctx, in.TenantID, stored.ID)`.
  5. Queries `allowed, err := s.Store.ListUserMemberships(ctx, stored.ID)`.
  6. Creates `Session` with `ExpiresAt: now.Add(s.sessionTTL())` (auto-selecting `allowed[0]` when `len(allowed) == 1`) and saves it via `s.Store.SaveSession`.
- **`Allowed(ctx, sessionID string, now time.Time)`:**
  - Validates the session (`s.open`) and returns `[]TenantChoice` mapped from `s.Store.ListUserMemberships(ctx, sess.UserID)`. Zero GitHub API calls.
- **`Select(ctx, sessionID, tenantID string, now time.Time)`:**
  - Validates the session (`s.open`), checks `s.Store.GetMembership(ctx, tenantID, sess.UserID)` (returning `errDenied` if `nil`), updates `sess.TenantID` and `sess.Role`, and saves the session. Zero GitHub API calls.
- **`EnterInstallation(ctx, sessionID string, installationID int64, now time.Time) (tenantID string, err error)`:**
  - Fetches installation metadata from `s.GitHub.Installation(ctx, installationID)` (using App JWT, not user OAuth token), upserts the tenant and active installation.
  - When `sessionID != ""`:
    - Opens the session.
    - Checks if the user already has a membership via `s.Store.GetMembership(ctx, ten.ID, sess.UserID)`.
    - If not, and `accountType == "user"`, checks if the user owns that GitHub account (`s.Store.HasIdentity(ctx, sess.UserID, "github", "https://github.com", strconv.FormatInt(accountID, 10))`); if so, upserts `admin` membership for `(ten.ID, sess.UserID)`.
    - Calls `s.Select(ctx, sessionID, ten.ID, now)` (returning `ErrDenied` if membership is not yet synced, e.g. for a newly installed org).
- **`Authenticate(ctx, sessionID string, now time.Time) (sess *Session, renewed bool, err error)`:**
  - Loads session via `s.Store.GetSession(ctx, sessionID)`. Returns `nil, false, nil` if missing or expired.
  - If `sess.ExpiresAt.Sub(now) <= s.sessionTTL()-s.renewThreshold()`, extends `sess.ExpiresAt = now.Add(s.sessionTTL())`, persists via `s.Store.SaveSession(ctx, *sess)`, and returns `sess, true, nil`.

### 5. HTTP Session Handling (`internal/interfaces/http/session.go`, `github_auth.go`)

- **`withSession`:**
  - Calls `cfg.Auth.Authenticate(r.Context(), c.Value, now)`.
  - If `sess == nil`, redirects to `/github/login`.
  - If `renewed == true`, calls `setSessionCookie(w, cfg.PublicBaseURL, sess.ID, sess.ExpiresAt)` so the browser cookie expiration stays in sync with the server-side sliding window.
- **`githubSetup`:**
  - Calls `u.cfg.Auth.EnterInstallation(r.Context(), sessionID, installationID, u.now())`.
  - If `errors.Is(err, authapp.ErrDenied)` (e.g., user installed the app on an org whose membership isn't in `tenant_memberships` yet), redirects to `/github/login` so the OAuth callback syncs their org membership.

---

## Testing Strategy

1. **Unit Tests (`internal/application/auth/service_test.go`):**
   - Verify `Complete` syncs memberships, discards token (no `AccessToken` on `Session`), and sets 7-day `ExpiresAt`.
   - Verify `Allowed` and `Select` work from stored memberships even when GitHub API would fail after login.
   - Verify `EnterInstallation` grants `admin` to personal account owner via stored `identities` and returns `ErrDenied` for un-synced orgs.
   - Verify `Authenticate` performs sliding renewal only after the 1-hour threshold has elapsed.
2. **Postgres Integration Tests (`internal/infrastructure/postgres/repos_test.go`):**
   - Test `UpsertUser` by `(provider, issuer, subject)` without `github_user_id`.
   - Test `UpsertMembership`, `DeleteMembership`, `GetMembership`, `ListUserMemberships`, and `HasIdentity`.
   - Test `SaveSession` and `GetSession` without `access_token`.
3. **HTTP UI Tests (`internal/interfaces/http/ui_test.go`):**
   - Verify sliding session cookie refresh on requests after >1 hour elapsed.
   - Verify `/github/setup` redirects to `/github/login` when org membership needs syncing.
