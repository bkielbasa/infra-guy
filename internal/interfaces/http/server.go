package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	projectapp "infra-guy/internal/application/project"
	runapp "infra-guy/internal/application/run"
	stateapp "infra-guy/internal/application/state"
	"infra-guy/internal/domain/project"
)

type brancher interface {
	BranchTip(ctx context.Context, cloneURL, token, branch string) (string, error)
}

type commitLookup interface {
	Lookup(ctx context.Context, repo project.SourceRepo, token, sha string) (message, author string, err error)
}

type ServerConfig struct {
	Projects          *projectapp.Service
	Runs              *runapp.Service
	States            *stateapp.Service
	Checkout          brancher
	Lookup            commitLookup
	PublicBaseURL     string
	BasicAuthUser     string
	BasicAuthPassword string
}

func NewServer(cfg ServerConfig) http.Handler {
	api := New(cfg.States, cfg.Runs, cfg.Projects.Projects)
	uiMux := http.NewServeMux()
	(&ui{cfg: cfg}).routes(uiMux)
	auth := http.NewCrossOriginProtection().Handler(withBasicAuth(uiMux, cfg.BasicAuthUser, cfg.BasicAuthPassword))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/webhooks/") {
			api.ServeHTTP(w, r)
			return
		}
		auth.ServeHTTP(w, r)
	})
}

func withBasicAuth(next http.Handler, user, pass string) http.Handler {
	if user == "" || pass == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="infra-guy"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
