package httpapi

import (
	"context"
	"net/http"
	"strings"

	authapp "infra-guy/internal/application/auth"
	"infra-guy/internal/application/installation"
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
	Projects      *projectapp.Service
	Runs          *runapp.Service
	States        *stateapp.Service
	Checkout      brancher
	Lookup        commitLookup
	PublicBaseURL string
	Auth          *authapp.Service
	Installations *installation.Service
	WebhookSecret string
}

func NewServer(cfg ServerConfig) http.Handler {
	api := New(cfg.States, cfg.Runs, cfg.Projects.Projects, WithWebhook(cfg.Installations, cfg.WebhookSecret))
	uiMux := http.NewServeMux()
	(&ui{cfg: cfg}).routes(uiMux)
	protected := http.NewCrossOriginProtection().Handler(withSession(uiMux, cfg))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/webhooks/") {
			api.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}
