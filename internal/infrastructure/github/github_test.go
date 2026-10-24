package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{AppID: 42, Key: key, ClientID: "cid", ClientSecret: "sec", BaseURL: srv.URL, HTTP: srv.Client()}
}

func TestAppJWTWindow(t *testing.T) {
	c := testClient(t, http.NotFoundHandler())
	raw, err := c.AppJWT(time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	parts := splitJWT(t, raw)
	var claims struct {
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
		ISS string `json:"iss"`
	}
	if err := json.Unmarshal(parts, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.ISS != "42" || claims.IAT != 1_700_000_000-60 || claims.EXP != 1_700_000_000+9*60 {
		t.Fatalf("%+v", claims)
	}
}

func splitJWT(t *testing.T, raw string) []byte {
	t.Helper()
	var payload string
	for i, part := range split3(raw) {
		if i == 1 {
			payload = part
		}
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func split3(raw string) []string {
	out := make([]string, 0, 3)
	start := 0
	for i := 0; i <= len(raw); i++ {
		if i == len(raw) || raw[i] == '.' {
			out = append(out, raw[start:i])
			start = i + 1
		}
	}
	return out
}

func TestMembershipAndRepos(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/memberships/orgs/acme":
			w.Write([]byte(`{"state":"active","role":"admin"}`))
		case "/app/installations/9/access_tokens":
			w.Write([]byte(`{"token":"ghs_x","expires_at":"2099-01-01T00:00:00Z"}`))
		case "/installation/repositories":
			if r.Header.Get("Authorization") != "Bearer ghs_x" {
				t.Errorf("auth %s", r.Header.Get("Authorization"))
			}
			w.Write([]byte(`{"repositories":[{"id":5,"name":"infra","default_branch":"master","owner":{"login":"acme"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	state, role, status, err := c.Membership(context.Background(), "ghu_user", "acme")
	if err != nil || status != 200 || state != "active" || role != "admin" {
		t.Fatalf("%s %s %d %v", state, role, status, err)
	}
	repos, err := c.ListRepos(context.Background(), 9)
	if err != nil || len(repos) != 1 || repos[0].ID != 5 || repos[0].Owner != "acme" || repos[0].Name != "infra" || repos[0].DefaultBranch != "master" {
		t.Fatalf("%#v %v", repos, err)
	}
}

func TestMembershipNotFound(t *testing.T) {
	c := testClient(t, http.NotFoundHandler())
	state, role, status, err := c.Membership(context.Background(), "ghu_user", "missing")
	if err != nil || status != 404 || state != "" || role != "" {
		t.Fatalf("%q %q %d %v", state, role, status, err)
	}
}

func TestMembershipTransportError(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	c := &Client{AppID: 42, Key: key, ClientID: "cid", ClientSecret: "sec", BaseURL: srv.URL, HTTP: srv.Client()}
	srv.Close()
	state, role, status, err := c.Membership(context.Background(), "ghu_user", "acme")
	if err == nil || status != 0 || state != "" || role != "" {
		t.Fatalf("%q %q %d %v", state, role, status, err)
	}
}

func TestExchangeCurrentUserInstallationSlug(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			if r.Header.Get("Authorization") != "Bearer ghu_x" {
				t.Errorf("user auth %s", r.Header.Get("Authorization"))
			}
			w.Write([]byte(`{"id":4,"login":"ada","name":"Ada"}`))
		case "/app/installations/9":
			w.Write([]byte(`{"account":{"id":50,"login":"acme","type":"Organization"}}`))
		case "/app":
			w.Write([]byte(`{"slug":"infra-guy"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login/oauth/access_token" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("accept %s", r.Header.Get("Accept"))
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "sec" || r.Form.Get("code") != "abc" {
			t.Errorf("form %v", r.Form)
		}
		w.Write([]byte(`{"access_token":"ghu_x"}`))
	}))
	t.Cleanup(oauth.Close)
	c.OAuthURL = oauth.URL

	tok, err := c.Exchange(context.Background(), "abc")
	if err != nil || tok != "ghu_x" {
		t.Fatalf("%q %v", tok, err)
	}
	id, login, name, err := c.CurrentUser(context.Background(), tok)
	if err != nil || id != 4 || login != "ada" || name != "Ada" {
		t.Fatalf("%d %q %q %v", id, login, name, err)
	}
	accountID, accountLogin, accountType, err := c.Installation(context.Background(), 9)
	if err != nil || accountID != 50 || accountLogin != "acme" || accountType != "Organization" {
		t.Fatalf("%d %q %q %v", accountID, accountLogin, accountType, err)
	}
	slug, err := c.Slug(context.Background())
	if err != nil || slug != "infra-guy" {
		t.Fatalf("%q %v", slug, err)
	}
}

func TestExchangeOAuthError(t *testing.T) {
	c := testClient(t, http.NotFoundHandler())
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login/oauth/access_token" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`))
	}))
	t.Cleanup(oauth.Close)
	c.OAuthURL = oauth.URL

	tok, err := c.Exchange(context.Background(), "bad")
	if err == nil || tok != "" {
		t.Fatalf("tok=%q err=%v", tok, err)
	}
}

func TestTokensCachesUntilFiveMinutes(t *testing.T) {
	var calls int
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/9/access_tokens" {
			http.NotFound(w, r)
			return
		}
		calls++
		exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"token":"ghs_%d","expires_at":"%s"}`, calls, exp)
	}))
	toks := &Tokens{Client: c}
	a, err := toks.Token(context.Background(), 9)
	if err != nil || a != "ghs_1" {
		t.Fatalf("%q %v", a, err)
	}
	b, err := toks.Token(context.Background(), 9)
	if err != nil || b != a {
		t.Fatalf("%q %q %v", a, b, err)
	}
	if calls != 1 {
		t.Fatalf("calls %d", calls)
	}
}

func TestTokensRefreshesWhenUnderFiveMinutes(t *testing.T) {
	var calls int
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/9/access_tokens" {
			http.NotFound(w, r)
			return
		}
		calls++
		exp := time.Now().Add(4 * time.Minute).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"token":"ghs_%d","expires_at":"%s"}`, calls, exp)
	}))
	toks := &Tokens{Client: c}
	if _, err := toks.Token(context.Background(), 9); err != nil {
		t.Fatal(err)
	}
	if _, err := toks.Token(context.Background(), 9); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls %d", calls)
	}
}

func TestCreateCommitStatus(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody CommitStatus
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":1}`))
	}))
	err := c.CreateCommitStatus(context.Background(), "ghs_tok", "acme", "infra", "deadbeef", CommitStatus{
		State:       "pending",
		TargetURL:   "https://infra.example.com/runs/r-1",
		Description: "[add: 3, delete: 3]",
		Context:     "infra-guy/prod",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/repos/acme/infra/statuses/deadbeef" || gotAuth != "Bearer ghs_tok" {
		t.Fatalf("got %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
	if gotBody.State != "pending" || gotBody.TargetURL != "https://infra.example.com/runs/r-1" || gotBody.Description != "[add: 3, delete: 3]" || gotBody.Context != "infra-guy/prod" {
		t.Fatalf("got body %+v", gotBody)
	}
}

