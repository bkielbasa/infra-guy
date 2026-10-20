package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const sessionCookie = "session"

type ctxKey struct{}

type principal struct {
	Login    string
	TenantID string
	Role     string
}

// SessionFrom returns the signed-in GitHub login, selected tenant, and role.
func SessionFrom(ctx context.Context) (login, tenantID, role string, ok bool) {
	p, ok := ctx.Value(ctxKey{}).(principal)
	if !ok {
		return "", "", "", false
	}
	return p.Login, p.TenantID, p.Role, true
}

func withSession(next http.Handler, cfg ServerConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicUI(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" || cfg.Auth == nil {
			http.Redirect(w, r, "/github/login", http.StatusFound)
			return
		}
		sess, err := cfg.Auth.Session(r.Context(), c.Value)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		now := time.Now()
		if cfg.Auth.Now != nil {
			now = cfg.Auth.Now()
		}
		if sess == nil || sess.ExpiresAt.Before(now) {
			http.Redirect(w, r, "/github/login", http.StatusFound)
			return
		}
		if sess.TenantID == "" && !emptyTenantUI(r) {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, principal{
			Login: sess.Login, TenantID: sess.TenantID, Role: sess.Role,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func publicUI(path string) bool {
	switch path {
	case "/github/login", "/github/callback", "/github/setup":
		return true
	default:
		return strings.HasPrefix(path, "/static/")
	}
}

func emptyTenantUI(r *http.Request) bool {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/github/setup":
		return true
	case r.Method == http.MethodPost && r.URL.Path == "/github/tenant":
		return true
	default:
		return false
	}
}

func setSessionCookie(w http.ResponseWriter, publicBase, id string, expires time.Time) {
	secure := false
	if u, err := url.Parse(publicBase); err == nil && u.Scheme == "https" {
		secure = true
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", Expires: expires,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}
