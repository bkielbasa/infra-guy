package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL  = "https://api.github.com"
	defaultOAuthURL = "https://github.com"
	githubAccept    = "application/vnd.github+json"
)

type Repo struct {
	ID    int64
	Owner string
	Name  string
}

type Client struct {
	AppID        int64
	Key          *rsa.PrivateKey
	ClientID     string
	ClientSecret string
	BaseURL      string
	OAuthURL     string
	HTTP         *http.Client
}

type Tokens struct {
	Client *Client
	mu     sync.Mutex
	cache  map[int64]struct {
		token   string
		expires time.Time
	}
}

func (c *Client) AppJWT(now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%d"}`, now.Unix()-60, now.Unix()+9*60, c.AppID)
	body := header + "." + base64.RawURLEncoding.EncodeToString([]byte(payload))
	sum := sha256.Sum256([]byte(body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return body + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (c *Client) InstallationToken(ctx context.Context, installationID int64) (token string, expires time.Time, err error) {
	jwt, err := c.AppJWT(time.Now())
	if err != nil {
		return "", time.Time{}, err
	}
	u := c.apiURL("/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens")
	resp, err := c.do(ctx, http.MethodPost, u, jwt, githubAccept, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return "", time.Time{}, err
	}
	var body struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", time.Time{}, err
	}
	return body.Token, body.ExpiresAt, nil
}

func (c *Client) Slug(ctx context.Context) (string, error) {
	jwt, err := c.AppJWT(time.Now())
	if err != nil {
		return "", err
	}
	resp, err := c.do(ctx, http.MethodGet, c.apiURL("/app"), jwt, githubAccept, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return "", err
	}
	var body struct {
		Slug string `json:"slug"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.Slug, nil
}

func (c *Client) Exchange(ctx context.Context, code string) (accessToken string, err error) {
	form := url.Values{
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"code":          {code},
	}
	u := c.oauth() + "/login/oauth/access_token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return "", err
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.AccessToken, nil
}

func (c *Client) CurrentUser(ctx context.Context, accessToken string) (id int64, login, name string, err error) {
	resp, err := c.do(ctx, http.MethodGet, c.apiURL("/user"), accessToken, githubAccept, nil)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return 0, "", "", err
	}
	var body struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, "", "", err
	}
	return body.ID, body.Login, body.Name, nil
}

func (c *Client) Membership(ctx context.Context, accessToken, org string) (state, role string, status int, err error) {
	u := c.apiURL("/user/memberships/orgs/" + url.PathEscape(org))
	resp, err := c.do(ctx, http.MethodGet, u, accessToken, githubAccept, nil)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", "", http.StatusNotFound, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", resp.StatusCode, fmt.Errorf("github membership: %s", resp.Status)
	}
	var body struct {
		State string `json:"state"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", resp.StatusCode, err
	}
	return body.State, body.Role, resp.StatusCode, nil
}

func (c *Client) Installation(ctx context.Context, installationID int64) (accountID int64, accountLogin, accountType string, err error) {
	jwt, err := c.AppJWT(time.Now())
	if err != nil {
		return 0, "", "", err
	}
	u := c.apiURL("/app/installations/" + strconv.FormatInt(installationID, 10))
	resp, err := c.do(ctx, http.MethodGet, u, jwt, githubAccept, nil)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return 0, "", "", err
	}
	var body struct {
		Account struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, "", "", err
	}
	return body.Account.ID, body.Account.Login, body.Account.Type, nil
}

func (c *Client) ListRepos(ctx context.Context, installationID int64) ([]Repo, error) {
	token, _, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var repos []Repo
	for page := 1; ; page++ {
		u := fmt.Sprintf("%s/installation/repositories?per_page=100&page=%d", c.base(), page)
		resp, err := c.do(ctx, http.MethodGet, u, token, githubAccept, nil)
		if err != nil {
			return nil, err
		}
		if err := checkStatus(resp); err != nil {
			resp.Body.Close()
			return nil, err
		}
		var body struct {
			Repositories []struct {
				ID    int64  `json:"id"`
				Name  string `json:"name"`
				Owner struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"repositories"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, r := range body.Repositories {
			repos = append(repos, Repo{ID: r.ID, Owner: r.Owner.Login, Name: r.Name})
		}
		if len(body.Repositories) < 100 {
			return repos, nil
		}
	}
}

func (t *Tokens) Token(ctx context.Context, installationID int64) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cache == nil {
		t.cache = make(map[int64]struct {
			token   string
			expires time.Time
		})
	}
	if ent, ok := t.cache[installationID]; ok && ent.expires.Sub(time.Now()) >= 5*time.Minute {
		return ent.token, nil
	}
	token, expires, err := t.Client.InstallationToken(ctx, installationID)
	if err != nil {
		return "", err
	}
	t.cache[installationID] = struct {
		token   string
		expires time.Time
	}{token: token, expires: expires}
	return token, nil
}

func (c *Client) do(ctx context.Context, method, rawURL, token, accept string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.http().Do(req)
}

func (c *Client) apiURL(path string) string {
	return c.base() + path
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultBaseURL
}

func (c *Client) oauth() string {
	if c.OAuthURL != "" {
		return strings.TrimRight(c.OAuthURL, "/")
	}
	return defaultOAuthURL
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("github %s %s: %s", resp.Request.Method, resp.Request.URL.Path, resp.Status)
}
