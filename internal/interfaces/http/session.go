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
	UserID   string
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

// PrincipalFrom returns the signed-in user ID, GitHub login, selected tenant, and role.
func PrincipalFrom(ctx context.Context) (userID, login, tenantID, role string, ok bool) {
	p, ok := ctx.Value(ctxKey{}).(principal)
	if !ok {
		return "", "", "", "", false
	}
	return p.UserID, p.Login, p.TenantID, p.Role, true
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
		if sess.TenantID == "" && !emptyTenantUI(r) {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, principal{
			UserID: sess.UserID, Login: sess.Login, TenantID: sess.TenantID, Role: sess.Role,
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
