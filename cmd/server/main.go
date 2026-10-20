package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	projectapp "infra-guy/internal/application/project"
	runapp "infra-guy/internal/application/run"
	stateapp "infra-guy/internal/application/state"
	"infra-guy/internal/application/worker"
	"infra-guy/internal/infrastructure/git"
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
	go w.Loop(ctx)

	h := httpapi.NewServer(httpapi.ServerConfig{
		Projects: &projectapp.Service{Projects: projects, Now: now, NewID: newID},
		Runs:     runSvc,
		States:   &stateapp.Service{Projects: projects, States: states, Now: now},
		Checkout: git.Checkout{}, Lookup: git.CommitLookup{},
		PublicBaseURL: public,
		BasicAuthUser: os.Getenv("BASIC_AUTH_USER"), BasicAuthPassword: os.Getenv("BASIC_AUTH_PASSWORD"),
	})

	srv := &http.Server{Addr: addr, Handler: h}
	go func() {
		<-ctx.Done()
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
