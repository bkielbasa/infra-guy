package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

type principalResponseWriter struct {
	http.ResponseWriter
	principal  principal
	flash      *flashMessage
	publicBase string
}

func (w *principalResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func responsePrincipal(w http.ResponseWriter) (principal, bool) {
	if pw, ok := w.(*principalResponseWriter); ok {
		return pw.principal, true
	}
	return principal{}, false
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
		p := principal{
			UserID: sess.UserID, Login: sess.Login, TenantID: sess.TenantID, Role: sess.Role,
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, p)
		pw := &principalResponseWriter{
			ResponseWriter: w, principal: p, publicBase: cfg.PublicBaseURL,
		}
		if msg, ok := readFlash(r); ok {
			pw.flash = &msg
		}
		next.ServeHTTP(pw, r.WithContext(ctx))
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
	case r.Method == http.MethodGet && r.URL.Path == "/profile":
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/github/setup":
		return true
	case r.Method == http.MethodPost && r.URL.Path == "/github/tenant":
		return true
	case (r.Method == http.MethodGet || r.Method == http.MethodPost) && r.URL.Path == "/github/logout":
		return true
	default:
		return false
	}
}

func setSessionCookie(w http.ResponseWriter, publicBase, id string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", Expires: expires,
		HttpOnly: true, Secure: cookieSecure(publicBase), SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, publicBase string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		HttpOnly: true, Secure: cookieSecure(publicBase), SameSite: http.SameSiteLaxMode,
	})
}

const flashCookieName = "flash"

type flashMessage struct {
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	Href  string `json:"href"`
	Label string `json:"label"`
}

func cookieSecure(publicBase string) bool {
	u, err := url.Parse(publicBase)
	return err == nil && u.Scheme == "https"
}

func setFlash(w http.ResponseWriter, publicBase string, msg flashMessage) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: flashCookieName, Value: base64.RawURLEncoding.EncodeToString(raw), Path: "/",
		HttpOnly: true, Secure: cookieSecure(publicBase), SameSite: http.SameSiteLaxMode,
	})
}

func readFlash(r *http.Request) (flashMessage, bool) {
	c, err := r.Cookie(flashCookieName)
	if err != nil || c.Value == "" {
		return flashMessage{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return flashMessage{}, false
	}
	var msg flashMessage
	if err := json.Unmarshal(raw, &msg); err != nil || msg.Text == "" {
		return flashMessage{}, false
	}
	return msg, true
}

func clearFlashCookie(w http.ResponseWriter, publicBase string) {
	http.SetCookie(w, &http.Cookie{
		Name: flashCookieName, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		HttpOnly: true, Secure: cookieSecure(publicBase), SameSite: http.SameSiteLaxMode,
	})
}
