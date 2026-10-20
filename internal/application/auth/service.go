package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"infra-guy/internal/domain/access"
	"infra-guy/internal/domain/tenant"
	"infra-guy/internal/domain/user"
)

// Session is the server-side sign-in record. TenantID stays empty until one
// tenant is selected.
type Session struct {
	ID          string
	UserID      string
	TenantID    string
	Role        string
	Login       string
	AccessToken string
	ExpiresAt   time.Time
}

// Store is the identity store already persisted in Postgres.
type Store interface {
	UpsertUser(ctx context.Context, u user.User, now time.Time) (user.User, error)
	UpsertTenant(ctx context.Context, t tenant.Tenant, now time.Time) (tenant.Tenant, error)
	UpsertInstallation(ctx context.Context, in tenant.Installation, now time.Time) error
	ActiveInstallation(ctx context.Context, tenantID string) (*tenant.Installation, error)
	ListActiveInstallations(ctx context.Context) ([]tenant.Installation, error)
	SaveSession(ctx context.Context, sess Session) error
	GetSession(ctx context.Context, id string) (*Session, error)
	SaveOAuthState(ctx context.Context, state string, expiresAt time.Time) error
	ConsumeOAuthState(ctx context.Context, state string, now time.Time) (bool, error)
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
	Store         Store
	GitHub        Client
	Now           func() time.Time
	NewID         func() string
	PublicBaseURL string
	ClientID      string
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
	errDenied     = errors.New("tenant access denied")
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

// Complete exchanges the code, records the user, and opens the only allowed
// tenant. Zero or several allowed tenants leave TenantID empty.
func (s *Service) Complete(ctx context.Context, state, code string, now time.Time) (sessionID string, tenantCount int, err error) {
	ok, err := s.Store.ConsumeOAuthState(ctx, state, now)
	if err != nil {
		return "", 0, err
	}
	if !ok {
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
	stored, err := s.Store.UpsertUser(ctx, user.User{
		ID: s.NewID(), GitHubUserID: ghID, Login: login, DisplayName: name,
	}, now)
	if err != nil {
		return "", 0, err
	}
	ident := user.GitHubIdentity(stored.ID, stored.GitHubUserID)
	if ident.Provider != "github" || ident.Subject == "" {
		return "", 0, fmt.Errorf("github identity")
	}
	allowed, err := s.admissions(ctx, token, ghID)
	if err != nil {
		return "", 0, err
	}
	sessionID, err = randomHex(32)
	if err != nil {
		return "", 0, err
	}
	sess := Session{
		ID: sessionID, UserID: stored.ID, Login: login, AccessToken: token,
		ExpiresAt: now.Add(time.Hour),
	}
	if len(allowed) == 1 {
		sess.TenantID = allowed[0].ID
		sess.Role = allowed[0].Role
	}
	if err := s.Store.SaveSession(ctx, sess); err != nil {
		return "", 0, err
	}
	return sessionID, len(allowed), nil
}

// Select rechecks one tenant with the stored user token and records the role.
// It does not move ExpiresAt.
func (s *Service) Select(ctx context.Context, sessionID, tenantID string, now time.Time) error {
	sess, err := s.open(ctx, sessionID, now)
	if err != nil {
		return err
	}
	in, err := s.Store.ActiveInstallation(ctx, tenantID)
	if err != nil {
		return err
	}
	if in == nil {
		return errDenied
	}
	ghID, _, _, err := s.GitHub.CurrentUser(ctx, sess.AccessToken)
	if err != nil {
		return err
	}
	role, ok := s.decide(ctx, sess.AccessToken, ghID, *in)
	if !ok {
		return errDenied
	}
	sess.TenantID = tenantID
	sess.Role = role
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
		if err := s.Select(ctx, sessionID, ten.ID, now); err != nil {
			return "", err
		}
	}
	return ten.ID, nil
}

// Allowed lists installations the session user can enter.
func (s *Service) Allowed(ctx context.Context, sessionID string, now time.Time) ([]TenantChoice, error) {
	sess, err := s.open(ctx, sessionID, now)
	if err != nil {
		return nil, err
	}
	ghID, _, _, err := s.GitHub.CurrentUser(ctx, sess.AccessToken)
	if err != nil {
		return nil, err
	}
	return s.admissions(ctx, sess.AccessToken, ghID)
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

func (s *Service) admissions(ctx context.Context, token string, ghUserID int64) ([]TenantChoice, error) {
	installs, err := s.Store.ListActiveInstallations(ctx)
	if err != nil {
		return nil, err
	}
	var out []TenantChoice
	for _, in := range installs {
		if !in.Active {
			continue
		}
		role, ok := s.decide(ctx, token, ghUserID, in)
		if ok {
			out = append(out, TenantChoice{ID: in.TenantID, Name: in.AccountLogin, Role: role})
		}
	}
	return out, nil
}

func (s *Service) decide(ctx context.Context, token string, ghUserID int64, in tenant.Installation) (string, bool) {
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
		return "", false
	}
	role, ok := access.Decide(fact)
	return string(role), ok
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
