package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	auditapp "infra-guy/internal/application/audit"
	authapp "infra-guy/internal/application/auth"
	cloudintegrationapp "infra-guy/internal/application/cloudintegration"
	dependencyapp "infra-guy/internal/application/dependency"
	folderapp "infra-guy/internal/application/folder"
	"infra-guy/internal/application/installation"
	policyapp "infra-guy/internal/application/policy"
	projectapp "infra-guy/internal/application/project"
	runapp "infra-guy/internal/application/run"
	stateapp "infra-guy/internal/application/state"
	tfcontextapp "infra-guy/internal/application/tfcontext"
	"infra-guy/internal/application/worker"
	"infra-guy/internal/domain/audit"
	"infra-guy/internal/domain/helmrelease"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/infrastructure/telemetry"
)

type brancher interface {
	BranchTip(ctx context.Context, cloneURL, token, branch string) (string, error)
}

type commitLookup interface {
	Lookup(ctx context.Context, repo project.SourceRepo, token, sha string) (message, author string, err error)
}

type VersionLister interface {
	List(ctx context.Context) []string
}

type ServerConfig struct {
	Projects          *projectapp.Service
	Folders           *folderapp.Service
	Runs              *runapp.Service
	States            *stateapp.Service
	HelmReleases      helmrelease.Repository
	Contexts          *tfcontextapp.Service
	Policies          *policyapp.Service
	Dependencies      *dependencyapp.Service
	CloudIntegrations *cloudintegrationapp.Service
	Audits            *auditapp.Service
	Versions          VersionLister
	Checkout          brancher
	Lookup            commitLookup
	Tokens            worker.TokenSource
	PublicBaseURL     string
	Auth              *authapp.Service
	Installations     *installation.Service
	WebhookSecret     string
}

func NewServer(cfg ServerConfig) http.Handler {
	if cfg.Audits == nil {
		cfg.Audits = &auditapp.Service{
			Repo: audit.NewMemoryRepository(),
		}
		if cfg.Auth != nil && cfg.Auth.Now != nil {
			cfg.Audits.Now = cfg.Auth.Now
		}
	}
	if cfg.CloudIntegrations == nil {
		cfg.CloudIntegrations = &cloudintegrationapp.Service{
			PublicBaseURL: cfg.PublicBaseURL,
		}
	}
	api := New(cfg.States, cfg.Runs, cfg.Projects, WithWebhook(cfg.Installations, cfg.WebhookSecret))
	uiMux := http.NewServeMux()
	(&ui{cfg: cfg}).routes(uiMux)
	protected := http.NewCrossOriginProtection().Handler(withSession(uiMux, cfg))
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/metrics" {
			telemetry.Default().MetricsHandler().ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/.well-known/openid-configuration" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cfg.CloudIntegrations.DiscoveryDocument())
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/.well-known/jwks.json" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cfg.CloudIntegrations.JWKS())
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/webhooks/") {
			api.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
	return telemetry.Default().Middleware(root)
}
