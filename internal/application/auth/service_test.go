package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"infra-guy/internal/domain/tenant"
	"infra-guy/internal/domain/user"
)

func TestCompleteAdmitsOrgAdmin(t *testing.T) {
	store := newMemIdentity()
	store.installations = []tenant.Installation{{
		ID: "ins-1", TenantID: "ten-1", AccountLogin: "acme", AccountType: "organization",
		GitHubInstallationID: 9, Active: true,
	}}
	gh := &fakeGitHub{userID: 4, login: "ada", membershipState: "active", membershipRole: "admin", membershipStatus: 200}
	svc := &Service{Store: store, GitHub: gh, Now: func() time.Time { return time.Unix(100, 0) }, NewID: func() string { return "user-1" }, PublicBaseURL: "https://infra.example"}
	if err := store.SaveOAuthState(context.Background(), "st", time.Unix(100, 0).Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	id, n, err := svc.Complete(context.Background(), "st", "code", time.Unix(100, 0))
	if err != nil || id == "" || n != 1 {
		t.Fatalf("id %q n %d err %v", id, n, err)
	}
	sess, _ := store.GetSession(context.Background(), id)
	if sess.TenantID != "ten-1" || sess.Role != "admin" || sess.Login != "ada" {
		t.Fatalf("%+v", sess)
	}
	if _, _, err := svc.Complete(context.Background(), "st", "code", time.Unix(100, 0)); err == nil {
		t.Fatal("reused state")
	}
	ident, ok := store.identities["github:4"]
	if !ok || ident.UserID != "user-1" || ident.Issuer != "https://github.com" || ident.Subject != "4" {
		t.Fatalf("expected github identity persisted, got %+v (ok=%v)", ident, ok)
	}
}

func TestOAuthFlowRejectsExpiredState(t *testing.T) {
	store := newMemIdentity()
	gh := &fakeGitHub{userID: 4, login: "ada"}
	now := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
	svc := &Service{
		Store:         store,
		GitHub:        gh,
		Now:           func() time.Time { return now },
		NewID:         func() string { return "user-1" },
		PublicBaseURL: "https://infra.example",
		ClientID:      "cid",
	}

	_, state, err := svc.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	now = now.Add(15 * time.Minute)
	if _, _, err := svc.Complete(context.Background(), state, "code-abc", now); err == nil {
		t.Fatalf("expected error for expired OAuth state")
	}
}

func TestCompleteMembershipErrorAdmitsNobody(t *testing.T) {
	store := newMemIdentity()
	store.installations = []tenant.Installation{{
		ID: "ins-1", TenantID: "ten-1", AccountLogin: "acme", AccountType: "organization",
		GitHubInstallationID: 9, Active: true,
	}}
	gh := &fakeGitHub{userID: 4, login: "ada", membershipErr: errors.New("membership down")}
	svc := &Service{Store: store, GitHub: gh, Now: func() time.Time { return time.Unix(100, 0) }, NewID: func() string { return "user-1" }, PublicBaseURL: "https://infra.example"}
	if err := store.SaveOAuthState(context.Background(), "st", time.Unix(100, 0).Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	id, n, err := svc.Complete(context.Background(), "st", "code", time.Unix(100, 0))
	if err != nil || n != 0 || id == "" {
		t.Fatalf("id %q n %d err %v", id, n, err)
	}
	sess, _ := store.GetSession(context.Background(), id)
	if sess == nil || sess.TenantID != "" || sess.AccessToken == "" || !sess.ExpiresAt.Equal(time.Unix(100, 0).Add(time.Hour)) {
		t.Fatalf("%+v", sess)
	}
}

func TestCompleteAdmitsPersonalOwner(t *testing.T) {
	store := newMemIdentity()
	store.installations = []tenant.Installation{{
		ID: "ins-p", TenantID: "ten-p", AccountLogin: "ada", AccountType: "user",
		GitHubInstallationID: 3, Active: true,
	}}
	gh := &fakeGitHub{userID: 4, login: "ada", accountID: 4}
	svc := &Service{Store: store, GitHub: gh, Now: func() time.Time { return time.Unix(100, 0) }, NewID: func() string { return "user-1" }, PublicBaseURL: "https://infra.example"}
	if err := store.SaveOAuthState(context.Background(), "st", time.Unix(100, 0).Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	id, n, err := svc.Complete(context.Background(), "st", "code", time.Unix(100, 0))
	if err != nil || id == "" || n != 1 {
		t.Fatalf("id %q n %d err %v", id, n, err)
	}
	sess, _ := store.GetSession(context.Background(), id)
	if sess.TenantID != "ten-p" || sess.Role != "admin" || sess.Login != "ada" {
		t.Fatalf("%+v", sess)
	}
}

func TestCompleteLeavesTenantEmptyWhenSeveral(t *testing.T) {
	store := newMemIdentity()
	store.installations = []tenant.Installation{
		{ID: "ins-1", TenantID: "ten-1", AccountLogin: "acme", AccountType: "organization", GitHubInstallationID: 9, Active: true},
		{ID: "ins-2", TenantID: "ten-2", AccountLogin: "other", AccountType: "organization", GitHubInstallationID: 10, Active: true},
	}
	gh := &fakeGitHub{userID: 4, login: "ada", membershipState: "active", membershipRole: "member", membershipStatus: 200}
	svc := &Service{Store: store, GitHub: gh, Now: func() time.Time { return time.Unix(100, 0) }, NewID: func() string { return "user-1" }}
	if err := store.SaveOAuthState(context.Background(), "st", time.Unix(100, 0).Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	id, n, err := svc.Complete(context.Background(), "st", "code", time.Unix(100, 0))
	if err != nil || n != 2 {
		t.Fatalf("id %q n %d err %v", id, n, err)
	}
	sess, _ := store.GetSession(context.Background(), id)
	if sess.TenantID != "" || sess.Role != "" {
		t.Fatalf("%+v", sess)
	}
}

func TestStartStoresState(t *testing.T) {
	store := newMemIdentity()
	svc := &Service{Store: store, ClientID: "cid", Now: func() time.Time { return time.Unix(100, 0) }, NewID: func() string { return "user-1" }}
	redirectURL, state, err := svc.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 64 {
		t.Fatalf("state %q", state)
	}
	want := "https://github.com/login/oauth/authorize?client_id=cid&state=" + state + "&scope=read:user%20read:org"
	if redirectURL != want {
		t.Fatalf("redirect %s", redirectURL)
	}
	if !store.states[state].Equal(time.Unix(100, 0).Add(10 * time.Minute)) {
		t.Fatalf("expiry %s", store.states[state])
	}
}

func TestSelectRechecksAndKeepsExpiry(t *testing.T) {
	store := newMemIdentity()
	store.installations = []tenant.Installation{{
		ID: "ins-1", TenantID: "ten-2", AccountLogin: "acme", AccountType: "organization",
		GitHubInstallationID: 9, Active: true,
	}}
	exp := time.Unix(100, 0).Add(time.Hour)
	if err := store.SaveSession(context.Background(), Session{
		ID: "s1", UserID: "user-1", Login: "ada", AccessToken: "tok", ExpiresAt: exp,
	}); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{userID: 4, login: "ada", membershipState: "active", membershipRole: "member", membershipStatus: 200}
	svc := &Service{Store: store, GitHub: gh, Now: func() time.Time { return time.Unix(100, 0) }}
	if err := svc.Select(context.Background(), "s1", "ten-2", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	sess, _ := store.GetSession(context.Background(), "s1")
	if sess.Role != "member" || sess.TenantID != "ten-2" || !sess.ExpiresAt.Equal(exp) || sess.AccessToken != "tok" {
		t.Fatalf("%+v", sess)
	}
	if err := svc.Select(context.Background(), "s1", "ten-2", exp.Add(time.Second)); err == nil {
		t.Fatal("expired session accepted")
	}
}

func TestEnterInstallationUpsertsAndSelects(t *testing.T) {
	store := newMemIdentity()
	exp := time.Unix(100, 0).Add(time.Hour)
	if err := store.SaveSession(context.Background(), Session{
		ID: "s1", UserID: "user-1", Login: "ada", AccessToken: "tok", ExpiresAt: exp,
	}); err != nil {
		t.Fatal(err)
	}
	n := 0
	gh := &fakeGitHub{
		userID: 4, login: "ada", accountID: 50, accountLogin: "acme", accountType: "Organization",
		membershipState: "active", membershipRole: "admin", membershipStatus: 200,
	}
	svc := &Service{Store: store, GitHub: gh, Now: func() time.Time { return time.Unix(100, 0) }, NewID: func() string {
		n++
		return "id-" + string(rune('0'+n))
	}}
	tenantID, err := svc.EnterInstallation(context.Background(), "s1", 9, time.Unix(100, 0))
	if err != nil || tenantID == "" {
		t.Fatalf("tenant %q err %v", tenantID, err)
	}
	sess, _ := store.GetSession(context.Background(), "s1")
	if sess.TenantID != tenantID || sess.Role != "admin" || !sess.ExpiresAt.Equal(exp) {
		t.Fatalf("%+v", sess)
	}
	active, err := store.ActiveInstallation(context.Background(), tenantID)
	if err != nil || active == nil || active.GitHubInstallationID != 9 || active.AccountType != "organization" {
		t.Fatalf("%+v %v", active, err)
	}
	alone, err := svc.EnterInstallation(context.Background(), "", 9, time.Unix(100, 0))
	if err != nil || alone == "" {
		t.Fatalf("logged out %q %v", alone, err)
	}
}

type memIdentity struct {
	users         []user.User
	identities    map[string]user.Identity
	tenants       []tenant.Tenant
	installations []tenant.Installation
	sessions      map[string]Session
	states        map[string]time.Time
}

func newMemIdentity() *memIdentity {
	return &memIdentity{
		identities: map[string]user.Identity{},
		sessions:   map[string]Session{},
		states:     map[string]time.Time{},
	}
}

func (m *memIdentity) UpsertUser(_ context.Context, u user.User, ident user.Identity, _ time.Time) (user.User, error) {
	if m.identities == nil {
		m.identities = map[string]user.Identity{}
	}
	for i, existing := range m.users {
		if existing.GitHubUserID == u.GitHubUserID {
			existing.Login = u.Login
			existing.DisplayName = u.DisplayName
			m.users[i] = existing
			ident.UserID = existing.ID
			m.identities[ident.Provider+":"+ident.Subject] = ident
			return existing, nil
		}
	}
	m.users = append(m.users, u)
	ident.UserID = u.ID
	m.identities[ident.Provider+":"+ident.Subject] = ident
	return u, nil
}

func (m *memIdentity) UpsertTenant(_ context.Context, t tenant.Tenant, _ time.Time) (tenant.Tenant, error) {
	for i, existing := range m.tenants {
		if existing.GitHubAccountID == t.GitHubAccountID {
			existing.Name = t.Name
			existing.AccountType = t.AccountType
			m.tenants[i] = existing
			return existing, nil
		}
	}
	m.tenants = append(m.tenants, t)
	return t, nil
}

func (m *memIdentity) UpsertInstallation(_ context.Context, in tenant.Installation, _ time.Time) error {
	for i := range m.installations {
		if m.installations[i].TenantID == in.TenantID && m.installations[i].GitHubInstallationID != in.GitHubInstallationID {
			m.installations[i].Active = false
		}
	}
	in.Active = true
	for i := range m.installations {
		if m.installations[i].GitHubInstallationID == in.GitHubInstallationID {
			if in.ID == "" {
				in.ID = m.installations[i].ID
			}
			m.installations[i] = in
			return nil
		}
	}
	m.installations = append(m.installations, in)
	return nil
}

func (m *memIdentity) ActiveInstallation(_ context.Context, tenantID string) (*tenant.Installation, error) {
	for i := range m.installations {
		if m.installations[i].TenantID == tenantID && m.installations[i].Active {
			in := m.installations[i]
			return &in, nil
		}
	}
	return nil, nil
}

func (m *memIdentity) ListActiveInstallations(context.Context) ([]tenant.Installation, error) {
	var out []tenant.Installation
	for _, in := range m.installations {
		if in.Active {
			out = append(out, in)
		}
	}
	return out, nil
}

func (m *memIdentity) SaveSession(_ context.Context, sess Session) error {
	if m.sessions == nil {
		m.sessions = map[string]Session{}
	}
	m.sessions[sess.ID] = sess
	return nil
}

func (m *memIdentity) GetSession(_ context.Context, id string) (*Session, error) {
	sess, ok := m.sessions[id]
	if !ok {
		return nil, nil
	}
	return &sess, nil
}

func (m *memIdentity) SaveOAuthState(_ context.Context, state string, expiresAt time.Time) error {
	if m.states == nil {
		m.states = map[string]time.Time{}
	}
	m.states[state] = expiresAt
	return nil
}

func (m *memIdentity) ConsumeOAuthState(_ context.Context, state string) (time.Time, bool, error) {
	exp, ok := m.states[state]
	if !ok {
		return time.Time{}, false, nil
	}
	delete(m.states, state)
	return exp, true, nil
}

type fakeGitHub struct {
	userID           int64
	login            string
	name             string
	token            string
	membershipState  string
	membershipRole   string
	membershipStatus int
	membershipErr    error
	accountID        int64
	accountLogin     string
	accountType      string
	slug             string
}

func (f *fakeGitHub) Exchange(context.Context, string) (string, error) {
	if f.token == "" {
		return "token", nil
	}
	return f.token, nil
}

func (f *fakeGitHub) CurrentUser(context.Context, string) (int64, string, string, error) {
	return f.userID, f.login, f.name, nil
}

func (f *fakeGitHub) Membership(context.Context, string, string) (string, string, int, error) {
	if f.membershipErr != nil {
		return "", "", 0, f.membershipErr
	}
	return f.membershipState, f.membershipRole, f.membershipStatus, nil
}

func (f *fakeGitHub) Installation(context.Context, int64) (int64, string, string, error) {
	login := f.accountLogin
	if login == "" {
		login = f.login
	}
	return f.accountID, login, f.accountType, nil
}

func (f *fakeGitHub) Slug(context.Context) (string, error) {
	if f.slug == "" {
		return "infra-guy", nil
	}
	return f.slug, nil
}
