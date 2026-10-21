package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	authapp "infra-guy/internal/application/auth"
	"infra-guy/internal/application/installation"
	policyapp "infra-guy/internal/application/policy"
	projectapp "infra-guy/internal/application/project"
	runapp "infra-guy/internal/application/run"
	stateapp "infra-guy/internal/application/state"
	tfcontextapp "infra-guy/internal/application/tfcontext"
	"infra-guy/internal/application/worker"
	"infra-guy/internal/domain/tenant"
	"infra-guy/internal/domain/user"
	"infra-guy/internal/infrastructure/crypto"
	"infra-guy/internal/infrastructure/git"
	"infra-guy/internal/infrastructure/github"
	"infra-guy/internal/infrastructure/k8s"
	"infra-guy/internal/infrastructure/opa"
	"infra-guy/internal/infrastructure/postgres"
	"infra-guy/internal/infrastructure/terraform"
	httpapi "infra-guy/internal/interfaces/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	jobID, err := parseJobID(os.Args[1:], os.Getenv)
	if err != nil {
		log.Fatal(err)
	}

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
		log.Fatal("PUBLIC_BASE_URL is required")
	}
	appIDRaw := os.Getenv("GITHUB_APP_ID")
	if appIDRaw == "" {
		log.Fatal("GITHUB_APP_ID is required")
	}
	appID, err := strconv.ParseInt(appIDRaw, 10, 64)
	if err != nil {
		log.Fatalf("GITHUB_APP_ID: %v", err)
	}
	keyPath := os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE")
	if keyPath == "" {
		log.Fatal("GITHUB_APP_PRIVATE_KEY_FILE is required")
	}
	appKey, err := loadAppKey(keyPath)
	if err != nil {
		log.Fatal(err)
	}
	webhookSecret := os.Getenv("GITHUB_APP_WEBHOOK_SECRET")
	if webhookSecret == "" {
		log.Fatal("GITHUB_APP_WEBHOOK_SECRET is required")
	}
	clientID := os.Getenv("GITHUB_APP_CLIENT_ID")
	if clientID == "" {
		log.Fatal("GITHUB_APP_CLIENT_ID is required")
	}
	clientSecret := os.Getenv("GITHUB_APP_CLIENT_SECRET")
	if clientSecret == "" {
		log.Fatal("GITHUB_APP_CLIENT_SECRET is required")
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
	if jobID == 0 {
		if err := postgres.Migrate(ctx, pool); err != nil {
			log.Fatal(err)
		}
	}

	projects := postgres.NewProjectRepo(pool)
	runs := postgres.NewRunRepo(pool)
	states := postgres.NewStateRepo(pool)
	queue := postgres.NewQueue(pool)
	now := func() time.Time { return time.Now().UTC() }

	var encryptionSecret []byte
	if rawKey := os.Getenv("CONTEXT_ENCRYPTION_KEY"); rawKey != "" {
		encryptionSecret = []byte(rawKey)
	} else {
		pemBytes, err := os.ReadFile(keyPath)
		if err != nil {
			log.Fatal(err)
		}
		encryptionSecret = append([]byte("infra-guy-context-v1:"), pemBytes...)
	}
	sealer, err := crypto.NewSealer(encryptionSecret)
	if err != nil {
		log.Fatal(err)
	}
	contextRepo := postgres.NewContextRepo(pool, sealer)
	contextSvc := &tfcontextapp.Service{Contexts: contextRepo, Now: now, NewID: newID}

	policyRepo := postgres.NewPolicyRepo(pool)
	opaEngine := &opa.Evaluator{}
	policySvc := &policyapp.Service{
		Policies: policyRepo,
		Engine:   opaEngine,
		Now:      now,
		NewID:    newID,
	}

	tfInstaller := &terraform.Installer{CacheDir: filepath.Join(workDir, "bin")}
	tfCatalog := &terraform.VersionCatalog{}

	gh := &github.Client{
		AppID:        appID,
		Key:          appKey,
		ClientID:     clientID,
		ClientSecret: clientSecret,
	}
	tokens := &github.Tokens{Client: gh}
	identStore := identityAdapter{store: postgres.NewIdentityStore(pool)}
	tokenSource := tenantTokenSource{store: identStore, tokens: tokens}

	runSvc := &runapp.Service{Projects: projects, Runs: runs, Schedule: queue, Now: now, NewID: newID}
	w := &worker.Worker{
		WorkerID: workerID(), Queue: postgres.WorkerQueue{Queue: queue},
		Projects: projects, Runs: runs, Service: runSvc,
		Checkout: git.Checkout{}, Executor: terraform.WorkerExecutor{Executor: terraform.Executor{Binary: tfInstaller}},
		Tokens:          tokenSource,
		Contexts:        contextSvc,
		WorkDir:         workDir, InternalBaseURL: internalBaseURL(addr), Now: now,
	}
	if jobID > 0 {
		if err := w.ExecuteClaimed(ctx, jobID); err != nil {
			log.Fatalf("execute job %d: %v", jobID, err)
		}
		return
	}

	workerMode := strings.TrimSpace(os.Getenv("WORKER_MODE"))
	if workerMode == "" {
		workerMode = "local"
	}
	switch workerMode {
	case "local":
		w.Dispatcher = nil
	case "k8s":
		dispatcher, err := k8s.NewInClusterDispatcher(k8s.Config{
			Image:              os.Getenv("WORKER_IMAGE"),
			ImagePullPolicy:    os.Getenv("WORKER_IMAGE_PULL_POLICY"),
			Namespace:          os.Getenv("WORKER_K8S_NAMESPACE"),
			ServiceAccountName: os.Getenv("WORKER_SERVICE_ACCOUNT"),
			EnvSecretName:      os.Getenv("WORKER_ENV_SECRET"),
			PassThroughEnv:     buildWorkerPassThroughEnv(os.Getenv, w.InternalBaseURL),
		})
		if err != nil {
			log.Fatalf("k8s worker dispatcher: %v", err)
		}
		w.Dispatcher = dispatcher
	default:
		log.Fatalf("unsupported WORKER_MODE: %s", workerMode)
	}

	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		w.Loop(workerCtx)
	}()

	h := httpapi.NewServer(httpapi.ServerConfig{
		Projects:      &projectapp.Service{Projects: projects, Now: now, NewID: newID},
		Runs:          runSvc,
		States:        &stateapp.Service{Projects: projects, States: states, Now: now},
		Contexts:      contextSvc,
		Policies:      policySvc,
		Versions:      tfCatalog,
		Checkout:      git.Checkout{},
		Lookup:        git.CommitLookup{},
		Tokens:        tokenSource,
		PublicBaseURL: public,
		Auth: &authapp.Service{
			Store: identStore, GitHub: gh,
			Now: now, NewID: newID, PublicBaseURL: public, ClientID: gh.ClientID,
		},
		Installations: &installation.Service{
			Store: identStore, GitHub: githubAdapter{client: gh},
			Now: now, NewID: newID,
		},
		WebhookSecret: webhookSecret,
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

func parseJobID(args []string, getenv func(string) string) (int64, error) {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jobID int64
	fs.Int64Var(&jobID, "job-id", 0, "execute a single claimed job by ID and exit")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if jobID == 0 && getenv != nil {
		if raw := strings.TrimSpace(getenv("WORKER_JOB_ID")); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("WORKER_JOB_ID: %w", err)
			}
			jobID = parsed
		}
	}
	if jobID < 0 {
		return 0, fmt.Errorf("job-id must be non-negative: %d", jobID)
	}
	return jobID, nil
}

func internalBaseURL(addr string) string {
	return resolveInternalBaseURL(addr, os.Getenv("INTERNAL_BASE_URL"))
}

func resolveInternalBaseURL(addr, override string) string {
	if trimmed := strings.TrimRight(strings.TrimSpace(override), "/"); trimmed != "" {
		return trimmed
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		log.Fatalf("HTTP_ADDR: %v", err)
	}
	return "http://127.0.0.1:" + port
}

func buildWorkerPassThroughEnv(getenv func(string) string, internalURL string) map[string]string {
	env := map[string]string{
		"DATABASE_URL":                getenv("DATABASE_URL"),
		"PUBLIC_BASE_URL":             getenv("PUBLIC_BASE_URL"),
		"INTERNAL_BASE_URL":           internalURL,
		"GITHUB_APP_ID":               getenv("GITHUB_APP_ID"),
		"GITHUB_APP_PRIVATE_KEY_FILE": getenv("GITHUB_APP_PRIVATE_KEY_FILE"),
		"GITHUB_APP_WEBHOOK_SECRET":   getenv("GITHUB_APP_WEBHOOK_SECRET"),
		"GITHUB_APP_CLIENT_ID":        getenv("GITHUB_APP_CLIENT_ID"),
		"GITHUB_APP_CLIENT_SECRET":    getenv("GITHUB_APP_CLIENT_SECRET"),
		"WORK_DIR":                    "/work",
	}
	if key := getenv("CONTEXT_ENCRYPTION_KEY"); key != "" {
		env["CONTEXT_ENCRYPTION_KEY"] = key
	}
	return env
}

type tenantTokenSource struct {
	store  identityAdapter
	tokens *github.Tokens
}

func (s tenantTokenSource) InstallationToken(ctx context.Context, tenantID string) (string, error) {
	in, err := s.store.ActiveInstallation(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if in == nil || !in.Active {
		return "", installation.ErrNotInstalled
	}
	return s.tokens.Token(ctx, in.GitHubInstallationID)
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
