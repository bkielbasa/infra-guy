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
	now := time.Unix(100, 0)
	svc := &Service{Store: store, GitHub: gh, Now: func() time.Time { return now }, NewID: func() string { return "user-1" }, PublicBaseURL: "https://infra.example"}
	if err := store.SaveOAuthState(context.Background(), "st", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	id, n, err := svc.Complete(context.Background(), "st", "code", now)
	if err != nil || id == "" || n != 1 {
		t.Fatalf("id %q n %d err %v", id, n, err)
	}
	sess, _ := store.GetSession(context.Background(), id)
	if sess.TenantID != "ten-1" || sess.Role != "admin" || sess.Login != "ada" || !sess.ExpiresAt.Equal(now.Add(DefaultSessionTTL)) {
		t.Fatalf("%+v", sess)
	}
	if _, _, err := svc.Complete(context.Background(), "st", "code", now); err == nil {
		t.Fatal("reused state")
	}
	ident, ok := store.identities["github:https://github.com:4"]
	if !ok || ident.UserID != "user-1" || ident.Issuer != "https://github.com" || ident.Subject != "4" {
		t.Fatalf("expected github identity persisted, got %+v (ok=%v)", ident, ok)
	}
	has, err := store.HasIdentity(context.Background(), "user-1", "github", "https://github.com", "4")
	if err != nil || !has {
		t.Fatalf("HasIdentity: has=%v err=%v", has, err)
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
	now := time.Unix(100, 0)
	svc := &Service{Store: store, GitHub: gh, Now: func() time.Time { return now }, NewID: func() string { return "user-1" }, PublicBaseURL: "https://infra.example"}
	if err := store.SaveOAuthState(context.Background(), "st", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	id, n, err := svc.Complete(context.Background(), "st", "code", now)
	if err != nil || n != 0 || id == "" {
		t.Fatalf("id %q n %d err %v", id, n, err)
	}
	sess, _ := store.GetSession(context.Background(), id)
	if sess == nil || sess.TenantID != "" || !sess.ExpiresAt.Equal(now.Add(DefaultSessionTTL)) {
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

func TestCompleteLeavesTenantEmptyWhenSeveralAndSyncsRevokedMembership(t *testing.T) {
	store := newMemIdentity()
	store.installations = []tenant.Installation{
		{ID: "ins-1", TenantID: "ten-1", AccountLogin: "acme", AccountType: "organization", GitHubInstallationID: 9, Active: true},
		{ID: "ins-2", TenantID: "ten-2", AccountLogin: "other", AccountType: "organization", GitHubInstallationID: 10, Active: true},
	}
	now := time.Unix(100, 0)
	gh := &fakeGitHub{userID: 4, login: "ada", membershipState: "active", membershipRole: "member", membershipStatus: 200}
	svc := &Service{Store: store, GitHub: gh, Now: func() time.Time { return now }, NewID: func() string { return "user-1" }}
	if err := store.SaveOAuthState(context.Background(), "st", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	id, n, err := svc.Complete(context.Background(), "st", "code", now)
	if err != nil || n != 2 {
		t.Fatalf("id %q n %d err %v", id, n, err)
	}
	sess, _ := store.GetSession(context.Background(), id)
	if sess.TenantID != "" || sess.Role != "" {
		t.Fatalf("%+v", sess)
	}

	// Allowed and Select work from persisted memberships even if GitHub fails after sign-in.
	gh.currentUserErr = errors.New("github unreachable")
	gh.membershipErr = errors.New("github unreachable")
	choices, err := svc.Allowed(context.Background(), id, now)
	if err != nil || len(choices) != 2 {
		t.Fatalf("Allowed without GitHub: choices=%+v err=%v", choices, err)
	}
	if err := svc.Select(context.Background(), id, "ten-1", now); err != nil {
		t.Fatalf("Select without GitHub: %v", err)
	}

	// Re-login when membership is 404 removes persisted membership.
	gh.currentUserErr = nil
	gh.membershipErr = nil
	gh.membershipStatus = 404
	gh.membershipState = ""
	if err := store.SaveOAuthState(context.Background(), "st2", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	id2, n2, err := svc.Complete(context.Background(), "st2", "code", now)
	if err != nil || n2 != 0 {
		t.Fatalf("expected 0 memberships after 404, got n=%d err=%v", n2, err)
	}
	choices2, err := svc.Allowed(context.Background(), id2, now)
	if err != nil || len(choices2) != 0 {
		t.Fatalf("expected 0 allowed tenants after revocation, got %+v err=%v", choices2, err)
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

func TestSelectUsesPersistedMembershipAndKeepsExpiry(t *testing.T) {
	store := newMemIdentity()
	store.installations = []tenant.Installation{{
		ID: "ins-1", TenantID: "ten-2", AccountLogin: "acme", AccountType: "organization",
		GitHubInstallationID: 9, Active: true,
	}}
	now := time.Unix(100, 0)
	if err := store.UpsertMembership(context.Background(), tenant.Membership{
		TenantID: "ten-2", UserID: "user-1", Role: "member",
	}, now); err != nil {
		t.Fatal(err)
	}
	exp := now.Add(DefaultSessionTTL)
	if err := store.SaveSession(context.Background(), Session{
		ID: "s1", UserID: "user-1", Login: "ada", ExpiresAt: exp,
	}); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{currentUserErr: errors.New("should not call github"), membershipErr: errors.New("should not call github")}
	svc := &Service{Store: store, GitHub: gh, Now: func() time.Time { return now }}
	if err := svc.Select(context.Background(), "s1", "ten-2", now); err != nil {
		t.Fatal(err)
	}
	sess, _ := store.GetSession(context.Background(), "s1")
	if sess.Role != "member" || sess.TenantID != "ten-2" || !sess.ExpiresAt.Equal(exp) {
		t.Fatalf("%+v", sess)
	}
	if err := svc.Select(context.Background(), "s1", "ten-unknown", now); !errors.Is(err, ErrDenied) {
		t.Fatalf("expected ErrDenied for non-member tenant, got %v", err)
	}
	if err := svc.Select(context.Background(), "s1", "ten-2", exp.Add(time.Second)); err == nil {
		t.Fatal("expired session accepted")
	}
}

func TestEnterInstallationPersonalAndOrg(t *testing.T) {
	store := newMemIdentity()
	now := time.Unix(100, 0)
	if _, err := store.UpsertUser(context.Background(), user.User{
		ID: "user-1", Login: "ada",
	}, user.GitHubIdentity("user-1", 4), now); err != nil {
		t.Fatal(err)
	}
	exp := now.Add(DefaultSessionTTL)
	if err := store.SaveSession(context.Background(), Session{
		ID: "s1", UserID: "user-1", Login: "ada", ExpiresAt: exp,
	}); err != nil {
		t.Fatal(err)
	}
	n := 0
	newID := func() string {
		n++
		return "id-" + string(rune('0'+n))
	}

	// 1. Personal installation owned by user-1 (GitHub accountID=4) auto-grants admin and selects tenant.
	ghPersonal := &fakeGitHub{
		accountID: 4, accountLogin: "ada", accountType: "User",
	}
	svcPersonal := &Service{Store: store, GitHub: ghPersonal, Now: func() time.Time { return now }, NewID: newID}
	personalTenID, err := svcPersonal.EnterInstallation(context.Background(), "s1", 11, now)
	if err != nil || personalTenID == "" {
		t.Fatalf("personal EnterInstallation: tenant %q err %v", personalTenID, err)
	}
	sess, _ := store.GetSession(context.Background(), "s1")
	if sess.TenantID != personalTenID || sess.Role != "admin" || !sess.ExpiresAt.Equal(exp) {
		t.Fatalf("personal session: %+v", sess)
	}

	// 2. Org installation without synced membership upserts tenant & installation, then returns ErrDenied.
	ghOrg := &fakeGitHub{
		accountID: 50, accountLogin: "acme", accountType: "Organization",
	}
	svcOrg := &Service{Store: store, GitHub: ghOrg, Now: func() time.Time { return now }, NewID: newID}
	if _, err := svcOrg.EnterInstallation(context.Background(), "s1", 9, now); !errors.Is(err, ErrDenied) {
		t.Fatalf("expected ErrDenied for unsynced org installation, got %v", err)
	}
	// When membership is present, EnterInstallation selects the org tenant.
	var orgTenID string
	for _, ten := range store.tenants {
		if ten.GitHubAccountID == 50 {
			orgTenID = ten.ID
		}
	}
	if orgTenID == "" {
		t.Fatal("expected org tenant to be upserted before ErrDenied")
	}
	if err := store.UpsertMembership(context.Background(), tenant.Membership{
		TenantID: orgTenID, UserID: "user-1", Role: "admin",
	}, now); err != nil {
		t.Fatal(err)
	}
	gotTenID, err := svcOrg.EnterInstallation(context.Background(), "s1", 9, now)
	if err != nil || gotTenID != orgTenID {
		t.Fatalf("synced org EnterInstallation: tenant %q err %v", gotTenID, err)
	}
	sess, _ = store.GetSession(context.Background(), "s1")
	if sess.TenantID != orgTenID || sess.Role != "admin" {
		t.Fatalf("org session: %+v", sess)
	}

	// 3. Logged-out EnterInstallation upserts tenant & installation without selecting.
	alone, err := svcOrg.EnterInstallation(context.Background(), "", 9, now)
	if err != nil || alone != orgTenID {
		t.Fatalf("logged out %q %v", alone, err)
	}
}

func TestAuthenticateSlidingRenewal(t *testing.T) {
	store := newMemIdentity()
	created := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	initialExpiry := created.Add(DefaultSessionTTL)
	if err := store.SaveSession(context.Background(), Session{
		ID: "s1", UserID: "user-1", TenantID: "ten-1", Role: "admin", Login: "ada", ExpiresAt: initialExpiry,
	}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: store}

	// 1. Within the first hour (30m elapsed): valid, not renewed.
	t30m := created.Add(30 * time.Minute)
	sess, renewed, err := svc.Authenticate(context.Background(), "s1", t30m)
	if err != nil || sess == nil || renewed {
		t.Fatalf("at +30m: sess=%+v renewed=%v err=%v", sess, renewed, err)
	}
	if !sess.ExpiresAt.Equal(initialExpiry) {
		t.Fatalf("expected expiry unchanged %v, got %v", initialExpiry, sess.ExpiresAt)
	}

	// 2. After 2 hours elapsed (>= DefaultRenewThreshold): renewed and persisted.
	t2h := created.Add(2 * time.Hour)
	sess, renewed, err = svc.Authenticate(context.Background(), "s1", t2h)
	if err != nil || sess == nil || !renewed {
		t.Fatalf("at +2h: sess=%+v renewed=%v err=%v", sess, renewed, err)
	}
	wantExpiry := t2h.Add(DefaultSessionTTL)
	if !sess.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("expected renewed expiry %v, got %v", wantExpiry, sess.ExpiresAt)
	}
	persisted, _ := store.GetSession(context.Background(), "s1")
	if !persisted.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("expected persisted expiry %v, got %v", wantExpiry, persisted.ExpiresAt)
	}

	// 3. Expired session returns nil, false, nil.
	sess, renewed, err = svc.Authenticate(context.Background(), "s1", wantExpiry.Add(time.Second))
	if err != nil || sess != nil || renewed {
		t.Fatalf("after expiry: sess=%+v renewed=%v err=%v", sess, renewed, err)
	}

	// 4. Unknown session returns nil, false, nil.
	sess, renewed, err = svc.Authenticate(context.Background(), "missing", t2h)
	if err != nil || sess != nil || renewed {
		t.Fatalf("missing session: sess=%+v renewed=%v err=%v", sess, renewed, err)
	}

	// 5. Logout expires an active session so Authenticate returns nil.
	if err := store.SaveSession(context.Background(), Session{
		ID: "s-logout", UserID: "user-1", TenantID: "ten-1", Role: "admin", Login: "ada", ExpiresAt: created.Add(DefaultSessionTTL),
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(context.Background(), "s-logout", created); err != nil {
		t.Fatalf("Logout error: %v", err)
	}
	sess, _, err = svc.Authenticate(context.Background(), "s-logout", created)
	if err != nil || sess != nil {
		t.Fatalf("after Logout: sess=%+v err=%v", sess, err)
	}
}

type memIdentity struct {
	users         []user.User
	identities    map[string]user.Identity
	tenants       []tenant.Tenant
	installations []tenant.Installation
	memberships   map[string]tenant.Membership
	sessions      map[string]Session
	states        map[string]time.Time
}

func newMemIdentity() *memIdentity {
	return &memIdentity{
		identities:  map[string]user.Identity{},
		memberships: map[string]tenant.Membership{},
		sessions:    map[string]Session{},
		states:      map[string]time.Time{},
	}
}

func (m *memIdentity) UpsertUser(_ context.Context, u user.User, ident user.Identity, _ time.Time) (user.User, error) {
	if m.identities == nil {
		m.identities = map[string]user.Identity{}
	}
	key := ident.Provider + ":" + ident.Issuer + ":" + ident.Subject
	if existingIdent, ok := m.identities[key]; ok {
		for i, existing := range m.users {
			if existing.ID == existingIdent.UserID {
				existing.Login = u.Login
				existing.DisplayName = u.DisplayName
				m.users[i] = existing
				return existing, nil
			}
		}
	}
	m.users = append(m.users, u)
	ident.UserID = u.ID
	m.identities[key] = ident
	return u, nil
}

func (m *memIdentity) HasIdentity(_ context.Context, userID, provider, issuer, subject string) (bool, error) {
	ident, ok := m.identities[provider+":"+issuer+":"+subject]
	return ok && ident.UserID == userID, nil
}

func (m *memIdentity) UpsertMembership(_ context.Context, mem tenant.Membership, _ time.Time) error {
	if m.memberships == nil {
		m.memberships = map[string]tenant.Membership{}
	}
	m.memberships[mem.TenantID+":"+mem.UserID] = mem
	return nil
}

func (m *memIdentity) DeleteMembership(_ context.Context, tenantID, userID string) error {
	delete(m.memberships, tenantID+":"+userID)
	return nil
}

func (m *memIdentity) GetMembership(ctx context.Context, tenantID, userID string) (*tenant.Membership, error) {
	mem, ok := m.memberships[tenantID+":"+userID]
	if !ok {
		return nil, nil
	}
	in, err := m.ActiveInstallation(ctx, tenantID)
	if err != nil || in == nil {
		return nil, err
	}
	mem.TenantName = in.AccountLogin
	return &mem, nil
}

func (m *memIdentity) ListUserMemberships(_ context.Context, userID string) ([]tenant.Membership, error) {
	var out []tenant.Membership
	for _, in := range m.installations {
		if !in.Active {
			continue
		}
		if mem, ok := m.memberships[in.TenantID+":"+userID]; ok {
			mem.TenantName = in.AccountLogin
			out = append(out, mem)
		}
	}
	return out, nil
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
	currentUserErr   error
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
	if f.currentUserErr != nil {
		return 0, "", "", f.currentUserErr
	}
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
