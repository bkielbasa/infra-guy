package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"

	"infra-guy/internal/application/installation"
	stateapp "infra-guy/internal/application/state"
	"infra-guy/internal/domain/project"
)

type ProjectMatcher interface {
	SyncAndFindByRepo(ctx context.Context, tenantID string, repoID int64, ownerLogin, repoName string) ([]*project.Project, error)
}

type WebhookOption func(*api)

func WithWebhook(installations *installation.Service, secret string) WebhookOption {
	return func(a *api) {
		a.installations = installations
		a.webhookSecret = secret
	}
}

// folderEffective resolves inherited folder tags and policies for one project.
type folderEffective interface {
	Effective(ctx context.Context, p project.Snapshot) (project.Snapshot, error)
}

func WithFolders(folders folderEffective) WebhookOption {
	return func(a *api) {
		a.folders = folders
	}
}

type api struct {
	states        *stateapp.Service
	open          opener
	projects      ProjectMatcher
	installations *installation.Service
	webhookSecret string
	folders       folderEffective
}

func New(states *stateapp.Service, open opener, projects ProjectMatcher, opts ...WebhookOption) *http.ServeMux {
	a := &api{states: states, open: open, projects: projects}
	for _, opt := range opts {
		opt(a)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/projects/{id}/state", a.getState)
	mux.HandleFunc("POST /api/projects/{id}/state", a.putState)
	mux.HandleFunc("LOCK /api/projects/{id}/state/lock", a.lockState)
	mux.HandleFunc("UNLOCK /api/projects/{id}/state/lock", a.unlockState)
	mux.HandleFunc("POST /api/projects/{id}/state/lock", a.postLock)
	mux.HandleFunc("POST /webhooks/github", a.webhook)
	return mux
}

func (a *api) token(w http.ResponseWriter, r *http.Request) (string, bool) {
	user, pass, ok := r.BasicAuth()
	if !ok || subtle.ConstantTimeCompare([]byte(user), []byte("terraform")) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return "", false
	}
	return pass, true
}

func writeOutcome(w http.ResponseWriter, out stateapp.Outcome, err error) {
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if out.Code == http.StatusOK && out.MD5 != "" {
		w.Header().Set("Content-MD5", out.MD5)
	}
	w.WriteHeader(out.Code)
	if len(out.Body) > 0 {
		_, _ = w.Write(out.Body)
	}
}

func (a *api) getState(w http.ResponseWriter, r *http.Request) {
	token, ok := a.token(w, r)
	if !ok {
		return
	}
	out, err := a.states.Get(r.Context(), r.PathValue("id"), token)
	writeOutcome(w, out, err)
}

func (a *api) putState(w http.ResponseWriter, r *http.Request) {
	token, ok := a.token(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	out, err := a.states.Put(r.Context(), r.PathValue("id"), token, q.Get("ID"), body, q.Get("run"))
	writeOutcome(w, out, err)
}

func (a *api) lockState(w http.ResponseWriter, r *http.Request) {
	token, ok := a.token(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	out, err := a.states.Lock(r.Context(), r.PathValue("id"), token, body)
	writeOutcome(w, out, err)
}

func (a *api) unlockState(w http.ResponseWriter, r *http.Request) {
	token, ok := a.token(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var payload struct{ ID string }
	_ = json.Unmarshal(body, &payload)
	out, err := a.states.Unlock(r.Context(), r.PathValue("id"), token, payload.ID)
	writeOutcome(w, out, err)
}

func (a *api) postLock(w http.ResponseWriter, r *http.Request) {
	token, ok := a.token(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var payload struct{ ID string }
	if json.Unmarshal(body, &payload) == nil && payload.ID != "" {
		out, err := a.states.Unlock(r.Context(), r.PathValue("id"), token, payload.ID)
		writeOutcome(w, out, err)
		return
	}
	out, err := a.states.Lock(r.Context(), r.PathValue("id"), token, body)
	writeOutcome(w, out, err)
}
