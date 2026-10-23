package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"infra-guy/internal/domain/access"
	"infra-guy/internal/domain/tenant"
	"infra-guy/internal/domain/user"
)

const (
	DefaultSessionTTL     = 7 * 24 * time.Hour
	DefaultRenewThreshold = time.Hour
)

// Session is the server-side sign-in record. TenantID stays empty until one
// tenant is selected.
type Session = user.Session

// Store is the identity store already persisted in Postgres.
type Store interface {
	UpsertUser(ctx context.Context, u user.User, ident user.Identity, now time.Time) (user.User, error)
	HasIdentity(ctx context.Context, userID, provider, issuer, subject string) (bool, error)
	UpsertTenant(ctx context.Context, t tenant.Tenant, now time.Time) (tenant.Tenant, error)
	UpsertInstallation(ctx context.Context, in tenant.Installation, now time.Time) error
	ActiveInstallation(ctx context.Context, tenantID string) (*tenant.Installation, error)
	ListActiveInstallations(ctx context.Context) ([]tenant.Installation, error)
	UpsertMembership(ctx context.Context, m tenant.Membership, now time.Time) error
	DeleteMembership(ctx context.Context, tenantID, userID string) error
	GetMembership(ctx context.Context, tenantID, userID string) (*tenant.Membership, error)
	ListUserMemberships(ctx context.Context, userID string) ([]tenant.Membership, error)
	SaveSession(ctx context.Context, sess Session) error
	GetSession(ctx context.Context, id string) (*Session, error)
	SaveOAuthState(ctx context.Context, state string, expiresAt time.Time) error
	ConsumeOAuthState(ctx context.Context, state string) (expiresAt time.Time, ok bool, err error)
}

// Client is the GitHub OAuth and App API used during sign-in.
type Client interface {
	Exchange(ctx context.Context, code string) (accessToken string, err error)
	CurrentUser(ctx context.Context, accessToken string) (id int64, login, name string, err error)
	Membership(ctx context.Context, accessToken, org string) (state, role string, status int, err error)
	Installation(ctx context.Context, installationID int64) (accountID int64, accountLogin, accountType string, err error)
	Slug(ctx context.Context) (string, error)
}

type Service struct {
	Store          Store
	GitHub         Client
	Now            func() time.Time
	NewID          func() string
	PublicBaseURL  string
	ClientID       string
	SessionTTL     time.Duration
	RenewThreshold time.Duration
}

// TenantChoice is one installation the signed-in user may enter.
type TenantChoice struct {
	ID   string
	Name string
	Role string
}

var (
	errOAuthState = errors.New("invalid oauth state")
	errNoSession  = errors.New("session not found")
	errExpired    = errors.New("session expired")
	ErrDenied     = errors.New("tenant access denied")
	errDenied     = ErrDenied
)

// Start stores a 32-byte hex state for ten minutes and returns the GitHub
// authorize URL.
func (s *Service) Start(ctx context.Context) (redirectURL, state string, err error) {
	state, err = randomHex(32)
	if err != nil {
		return "", "", err
	}
	if err := s.Store.SaveOAuthState(ctx, state, s.now().Add(10*time.Minute)); err != nil {
		return "", "", err
	}
	return fmt.Sprintf(
		"https://github.com/login/oauth/authorize?client_id=%s&state=%s&scope=read:user%%20read:org",
		url.QueryEscape(s.ClientID), url.QueryEscape(state),
	), state, nil
}

// Complete exchanges the code, records the user, syncs tenant memberships from
// GitHub, and opens the only allowed tenant. Zero or several allowed tenants
// leave TenantID empty.
func (s *Service) Complete(ctx context.Context, state, code string, now time.Time) (sessionID string, tenantCount int, err error) {
	expiresAt, ok, err := s.Store.ConsumeOAuthState(ctx, state)
	if err != nil {
		return "", 0, err
	}
	if !ok || expiresAt.Before(now) {
		return "", 0, errOAuthState
	}
	token, err := s.GitHub.Exchange(ctx, code)
	if err != nil {
		return "", 0, err
	}
	ghID, login, name, err := s.GitHub.CurrentUser(ctx, token)
	if err != nil {
		return "", 0, err
	}
	if s.NewID == nil {
		return "", 0, fmt.Errorf("missing id generator")
	}
	candidateID := s.NewID()
	ident := user.GitHubIdentity(candidateID, ghID)
	if ident.Provider != "github" || ident.Subject == "" {
		return "", 0, fmt.Errorf("github identity")
	}
	stored, err := s.Store.UpsertUser(ctx, user.User{
		ID: candidateID, Login: login, DisplayName: name,
	}, ident, now)
	if err != nil {
		return "", 0, err
	}
	if err := s.syncGitHubMemberships(ctx, stored.ID, token, ghID, now); err != nil {
		return "", 0, err
	}
	memberships, err := s.Store.ListUserMemberships(ctx, stored.ID)
	if err != nil {
		return "", 0, err
	}
	sessionID, err = randomHex(32)
	if err != nil {
		return "", 0, err
	}
	sess := Session{
		ID:        sessionID,
		UserID:    stored.ID,
		Login:     login,
		ExpiresAt: now.Add(s.sessionTTL()),
	}
	if len(memberships) == 1 {
		sess.TenantID = memberships[0].TenantID
		sess.Role = memberships[0].Role
	}
	if err := s.Store.SaveSession(ctx, sess); err != nil {
		return "", 0, err
	}
	return sessionID, len(memberships), nil
}

// Select checks the stored membership for one tenant and records the role on
// the session. It does not move ExpiresAt.
func (s *Service) Select(ctx context.Context, sessionID, tenantID string, now time.Time) error {
	sess, err := s.open(ctx, sessionID, now)
	if err != nil {
		return err
	}
	m, err := s.Store.GetMembership(ctx, tenantID, sess.UserID)
	if err != nil {
		return err
	}
	if m == nil {
		return ErrDenied
	}
	sess.TenantID = tenantID
	sess.Role = m.Role
	return s.Store.SaveSession(ctx, *sess)
}

// EnterInstallation loads the GitHub App installation, upserts the tenant, and
// selects it when sessionID is set.
func (s *Service) EnterInstallation(ctx context.Context, sessionID string, installationID int64, now time.Time) (string, error) {
	accountID, login, ghType, err := s.GitHub.Installation(ctx, installationID)
	if err != nil {
		return "", err
	}
	accountType, err := tenant.ParseAccountType(ghType)
	if err != nil {
		return "", err
	}
	if s.NewID == nil {
		return "", fmt.Errorf("missing id generator")
	}
	ten, err := s.Store.UpsertTenant(ctx, tenant.Tenant{
		ID: s.NewID(), Name: login, AccountType: accountType, GitHubAccountID: accountID,
	}, now)
	if err != nil {
		return "", err
	}
	if err := s.Store.UpsertInstallation(ctx, tenant.Installation{
		ID: s.NewID(), TenantID: ten.ID, AccountLogin: login, AccountType: accountType,
		GitHubInstallationID: installationID, Active: true,
	}, now); err != nil {
		return "", err
	}
	if sessionID != "" {
		sess, err := s.open(ctx, sessionID, now)
		if err != nil {
			return "", err
		}
		m, err := s.Store.GetMembership(ctx, ten.ID, sess.UserID)
		if err != nil {
			return "", err
		}
		if m == nil && accountType == "user" {
			owns, err := s.Store.HasIdentity(ctx, sess.UserID, "github", "https://github.com", strconv.FormatInt(accountID, 10))
			if err != nil {
				return "", err
			}
			if owns {
				if err := s.Store.UpsertMembership(ctx, tenant.Membership{
					TenantID: ten.ID, UserID: sess.UserID, Role: "admin",
				}, now); err != nil {
					return "", err
				}
			}
		}
		if err := s.Select(ctx, sessionID, ten.ID, now); err != nil {
			return "", err
		}
	}
	return ten.ID, nil
}

// Allowed lists installations the session user can enter from stored memberships.
func (s *Service) Allowed(ctx context.Context, sessionID string, now time.Time) ([]TenantChoice, error) {
	sess, err := s.open(ctx, sessionID, now)
	if err != nil {
		return nil, err
	}
	memberships, err := s.Store.ListUserMemberships(ctx, sess.UserID)
	if err != nil {
		return nil, err
	}
	var out []TenantChoice
	for _, m := range memberships {
		out = append(out, TenantChoice{ID: m.TenantID, Name: m.TenantName, Role: m.Role})
	}
	return out, nil
}

// Authenticate loads a session, returning nil when missing or expired. When at
// least RenewThreshold of the session's TTL has elapsed, it slides ExpiresAt
// forward by SessionTTL, persists the session, and reports renewed=true.
func (s *Service) Authenticate(ctx context.Context, sessionID string, now time.Time) (sess *Session, renewed bool, err error) {
	sess, err = s.Store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, false, err
	}
	if sess == nil || sess.ExpiresAt.Before(now) {
		return nil, false, nil
	}
	ttl := s.sessionTTL()
	if sess.ExpiresAt.Sub(now) <= ttl-s.renewThreshold() {
		sess.ExpiresAt = now.Add(ttl)
		if err := s.Store.SaveSession(ctx, *sess); err != nil {
			return nil, false, err
		}
		return sess, true, nil
	}
	return sess, false, nil
}

// InstallURL is the GitHub App installation page for this server's app slug.
func (s *Service) InstallURL(ctx context.Context) (string, error) {
	slug, err := s.GitHub.Slug(ctx)
	if err != nil {
		return "", err
	}
	return "https://github.com/apps/" + slug + "/installations/new", nil
}

// Session loads a session row. It returns nil when the id is unknown.
func (s *Service) Session(ctx context.Context, id string) (*Session, error) {
	return s.Store.GetSession(ctx, id)
}

func (s *Service) open(ctx context.Context, sessionID string, now time.Time) (*Session, error) {
	sess, err := s.Store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, errNoSession
	}
	if sess.ExpiresAt.Before(now) {
		return nil, errExpired
	}
	return sess, nil
}

func (s *Service) syncGitHubMemberships(ctx context.Context, userID, token string, ghUserID int64, now time.Time) error {
	installs, err := s.Store.ListActiveInstallations(ctx)
	if err != nil {
		return err
	}
	for _, in := range installs {
		if !in.Active {
			continue
		}
		role, ok, failed := s.decide(ctx, token, ghUserID, in)
		if ok {
			if err := s.Store.UpsertMembership(ctx, tenant.Membership{
				TenantID: in.TenantID, UserID: userID, Role: role,
			}, now); err != nil {
				return err
			}
		} else if !failed {
			if err := s.Store.DeleteMembership(ctx, in.TenantID, userID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) decide(ctx context.Context, token string, ghUserID int64, in tenant.Installation) (role string, ok, failed bool) {
	var fact access.Fact
	switch in.AccountType {
	case "user":
		accountID, _, _, err := s.GitHub.Installation(ctx, in.GitHubInstallationID)
		fact = access.Fact{Personal: true, UserID: ghUserID}
		if err != nil {
			fact.Failed = true
		} else {
			fact.AccountID = accountID
		}
	case "organization":
		state, role, status, err := s.GitHub.Membership(ctx, token, in.AccountLogin)
		if err != nil || (status != http.StatusOK && status != http.StatusNotFound) {
			fact.Failed = true
		} else if status == http.StatusOK {
			fact.State = state
			fact.OrgRole = role
		}
	default:
		return "", false, false
	}
	r, allowed := access.Decide(fact)
	return string(r), allowed, fact.Failed
}

func (s *Service) sessionTTL() time.Duration {
	if s.SessionTTL > 0 {
		return s.SessionTTL
	}
	return DefaultSessionTTL
}

func (s *Service) renewThreshold() time.Duration {
	if s.RenewThreshold > 0 {
		return s.RenewThreshold
	}
	return DefaultRenewThreshold
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
