package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	authapp "infra-guy/internal/application/auth"
)

func (u *ui) githubLogin(w http.ResponseWriter, r *http.Request) {
	loc, _, err := u.cfg.Auth.Start(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, loc, http.StatusFound)
}

func (u *ui) githubCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("error") != "" || q.Get("code") == "" || q.Get("state") == "" {
		http.Error(w, "github authorization failed", http.StatusBadRequest)
		return
	}
	id, _, err := u.cfg.Auth.Complete(r.Context(), q.Get("state"), q.Get("code"), u.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sess, err := u.cfg.Auth.Session(r.Context(), id)
	if err != nil || sess == nil {
		http.Error(w, "session", http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, u.cfg.PublicBaseURL, sess.ID, sess.ExpiresAt)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (u *ui) githubSetup(w http.ResponseWriter, r *http.Request) {
	installationID, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if err != nil || installationID <= 0 {
		http.Error(w, "missing installation_id", http.StatusBadRequest)
		return
	}
	sessionID := ""
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		sess, err := u.cfg.Auth.Session(r.Context(), c.Value)
		if err == nil && sess != nil && !sess.ExpiresAt.Before(u.now()) {
			sessionID = c.Value
		}
	}
	if _, err := u.cfg.Auth.EnterInstallation(r.Context(), sessionID, installationID, u.now()); err != nil {
		if errors.Is(err, authapp.ErrDenied) {
			http.Redirect(w, r, "/github/login", http.StatusFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if sessionID == "" {
		http.Redirect(w, r, "/github/login", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (u *ui) githubTenant(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		http.Redirect(w, r, "/github/login", http.StatusFound)
		return
	}
	if err := u.cfg.Auth.Select(r.Context(), c.Value, r.FormValue("tenant_id"), u.now()); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (u *ui) picker(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		http.Redirect(w, r, "/github/login", http.StatusFound)
		return
	}
	choices, err := u.cfg.Auth.Allowed(r.Context(), c.Value, u.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	data := map[string]any{"Tenants": choices}
	if len(choices) == 0 {
		install, err := u.cfg.Auth.InstallURL(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		data["InstallURL"] = install
	}
	u.page(w, "Tenants", "tenants", data)
}
