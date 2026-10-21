package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	authapp "infra-guy/internal/application/auth"
	"infra-guy/internal/application/installation"
	projectapp "infra-guy/internal/application/project"
	runapp "infra-guy/internal/application/run"
	stateapp "infra-guy/internal/application/state"
	"infra-guy/internal/application/worker"
	"infra-guy/internal/domain/tenant"
	"infra-guy/internal/domain/user"
	"infra-guy/internal/infrastructure/git"
	"infra-guy/internal/infrastructure/github"
	"infra-guy/internal/infrastructure/postgres"
	"infra-guy/internal/infrastructure/terraform"
	httpapi "infra-guy/internal/interfaces/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	public := os.Getenv("PUBLIC_BASE_URL")
	if public == "" {
		public = "http://localhost:8080"
	}
	workDir := os.Getenv("WORK_DIR")
	if workDir == "" {
		workDir = "data"
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		log.Fatal(err)
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := postgres.Migrate(ctx, pool); err != nil {
		log.Fatal(err)
	}

	projects := postgres.NewProjectRepo(pool)
	runs := postgres.NewRunRepo(pool)
	states := postgres.NewStateRepo(pool)
	queue := postgres.NewQueue(pool)
	now := func() time.Time { return time.Now().UTC() }

	runSvc := &runapp.Service{Projects: projects, Runs: runs, Schedule: queue, Now: now, NewID: newID}
	w := &worker.Worker{
		WorkerID: workerID(), Queue: postgres.WorkerQueue{Queue: queue},
		Projects: projects, Runs: runs, Service: runSvc,
		Checkout: git.Checkout{}, Executor: terraform.WorkerExecutor{Executor: terraform.Executor{}},
		WorkDir: workDir, InternalBaseURL: internalBaseURL(addr), Now: now,
	}
	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		w.Loop(workerCtx)
	}()

	gh := githubAppClient()
	identStore := identityAdapter{store: postgres.NewIdentityStore(pool)}
	h := httpapi.NewServer(httpapi.ServerConfig{
		Projects: &projectapp.Service{Projects: projects, Now: now, NewID: newID},
		Runs:     runSvc,
		States:   &stateapp.Service{Projects: projects, States: states, Now: now},
		Checkout: git.Checkout{}, Lookup: git.CommitLookup{},
		PublicBaseURL: public,
		Auth: &authapp.Service{
			Store: identStore, GitHub: gh,
			Now: now, NewID: newID, PublicBaseURL: public, ClientID: gh.ClientID,
		},
		Installations: &installation.Service{
			Store: identStore, GitHub: githubAdapter{client: gh},
			Now: now, NewID: newID,
		},
	})

	srv := &http.Server{Addr: addr, Handler: h}
	go func() {
		<-ctx.Done()
		stopWorker()
		select {
		case <-workerDone:
		case <-time.After(terraform.WaitDelay):
		}
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	log.Printf("listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func internalBaseURL(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		log.Fatalf("HTTP_ADDR: %v", err)
	}
	return "http://127.0.0.1:" + port
}

func githubAppClient() *github.Client {
	c := &github.Client{
		ClientID: os.Getenv("GITHUB_APP_CLIENT_ID"), ClientSecret: os.Getenv("GITHUB_APP_CLIENT_SECRET"),
	}
	if raw := os.Getenv("GITHUB_APP_ID"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			log.Fatalf("GITHUB_APP_ID: %v", err)
		}
		c.AppID = id
	}
	if path := os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE"); path != "" {
		key, err := loadAppKey(path)
		if err != nil {
			log.Fatal(err)
		}
		c.Key = key
	}
	return c
}

func loadAppKey(path string) (*rsa.PrivateKey, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_FILE: no PEM block")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_FILE: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_FILE: not an RSA key")
	}
	return key, nil
}

type identityAdapter struct{ store *postgres.IdentityStore }

func (a identityAdapter) UpsertUser(ctx context.Context, u user.User, now time.Time) (user.User, error) {
	return a.store.UpsertUser(ctx, u, now)
}

func (a identityAdapter) UpsertTenant(ctx context.Context, t tenant.Tenant, now time.Time) (tenant.Tenant, error) {
	return a.store.UpsertTenant(ctx, t, now)
}

func (a identityAdapter) UpsertInstallation(ctx context.Context, in tenant.Installation, now time.Time) error {
	return a.store.UpsertInstallation(ctx, in, now)
}

func (a identityAdapter) ActiveInstallation(ctx context.Context, tenantID string) (*tenant.Installation, error) {
	return a.store.ActiveInstallation(ctx, tenantID)
}

func (a identityAdapter) ListActiveInstallations(ctx context.Context) ([]tenant.Installation, error) {
	return a.store.ListActiveInstallations(ctx)
}

func (a identityAdapter) SaveSession(ctx context.Context, sess authapp.Session) error {
	return a.store.SaveSession(ctx, postgres.Session{
		ID: sess.ID, UserID: sess.UserID, TenantID: sess.TenantID, Role: sess.Role,
		Login: sess.Login, AccessToken: sess.AccessToken, ExpiresAt: sess.ExpiresAt,
	})
}

func (a identityAdapter) GetSession(ctx context.Context, id string) (*authapp.Session, error) {
	sess, err := a.store.GetSession(ctx, id)
	if err != nil || sess == nil {
		return nil, err
	}
	return &authapp.Session{
		ID: sess.ID, UserID: sess.UserID, TenantID: sess.TenantID, Role: sess.Role,
		Login: sess.Login, AccessToken: sess.AccessToken, ExpiresAt: sess.ExpiresAt,
	}, nil
}

func (a identityAdapter) SaveOAuthState(ctx context.Context, state string, expiresAt time.Time) error {
	return a.store.SaveOAuthState(ctx, state, expiresAt)
}

func (a identityAdapter) ConsumeOAuthState(ctx context.Context, state string, now time.Time) (bool, error) {
	return a.store.ConsumeOAuthState(ctx, state, now)
}

func (a identityAdapter) InstallationByGitHubID(ctx context.Context, githubInstallationID int64) (*tenant.Installation, error) {
	return a.store.InstallationByGitHubID(ctx, githubInstallationID)
}

func (a identityAdapter) DeactivateInstallation(ctx context.Context, githubInstallationID int64, now time.Time) error {
	return a.store.DeactivateInstallation(ctx, githubInstallationID, now)
}

type githubAdapter struct{ client *github.Client }

func (g githubAdapter) ListRepos(ctx context.Context, installationID int64) ([]installation.Repo, error) {
	repos, err := g.client.ListRepos(ctx, installationID)
	if err != nil {
		return nil, err
	}
	out := make([]installation.Repo, len(repos))
	for i, r := range repos {
		out[i] = installation.Repo{ID: r.ID, Owner: r.Owner, Name: r.Name}
	}
	return out, nil
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		log.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func workerID() string {
	h, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", h, os.Getpid())
}
