package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"infra-guy/internal/infrastructure/crypto"
	"infra-guy/internal/infrastructure/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var dbURL string
	flag.StringVar(&dbURL, "database-url", "", "PostgreSQL connection string (defaults to DATABASE_URL env var)")
	flag.Parse()

	if strings.TrimSpace(dbURL) == "" {
		dbURL = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer pool.Close()

	cmd := "up"
	args := flag.Args()
	if len(args) > 0 {
		cmd = strings.ToLower(strings.TrimSpace(args[0]))
	}

	switch cmd {
	case "up":
		if err := postgres.Migrate(ctx, pool); err != nil {
			log.Fatalf("migration up failed: %v", err)
		}
		v, dirty, err := postgres.MigrateVersion(ctx, pool)
		if err != nil {
			log.Fatalf("migration version failed: %v", err)
		}
		log.Printf("migrations up to date (version=%d dirty=%v)", v, dirty)

	case "down":
		steps := 1
		if len(args) > 1 {
			if strings.EqualFold(args[1], "all") {
				steps = 0
			} else {
				parsed, err := strconv.Atoi(args[1])
				if err != nil || parsed <= 0 {
					log.Fatalf("invalid steps %q: must be positive integer or 'all'", args[1])
				}
				steps = parsed
			}
		}
		if err := postgres.MigrateDown(ctx, pool, steps); err != nil {
			log.Fatalf("migration down failed: %v", err)
		}
		v, dirty, err := postgres.MigrateVersion(ctx, pool)
		if err != nil {
			log.Fatalf("migration version failed: %v", err)
		}
		log.Printf("migrations rolled back (version=%d dirty=%v)", v, dirty)

	case "version":
		v, dirty, err := postgres.MigrateVersion(ctx, pool)
		if err != nil {
			log.Fatalf("migration version failed: %v", err)
		}
		fmt.Printf("version=%d dirty=%v\n", v, dirty)

	case "rotate-keys":
		var fallbackSecret []byte
		if rawKey := os.Getenv("CONTEXT_ENCRYPTION_KEY"); rawKey != "" {
			fallbackSecret = []byte(rawKey)
		} else if keyPath := os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE"); keyPath != "" {
			pemBytes, err := os.ReadFile(keyPath)
			if err != nil {
				log.Fatalf("read GITHUB_APP_PRIVATE_KEY_FILE: %v", err)
			}
			fallbackSecret = append([]byte("infra-guy-context-v1:"), pemBytes...)
		}
		sealer, err := crypto.ParseKeyring(os.Getenv("ENCRYPTION_KEYRING"), os.Getenv("ENCRYPTION_ACTIVE_KEY_ID"), fallbackSecret)
		if err != nil {
			log.Fatalf("initialize encryption keyring: %v", err)
		}
		sum, err := postgres.RotateAndSealAll(ctx, pool, sealer)
		if err != nil {
			log.Fatalf("rotate-keys failed: %v", err)
		}
		log.Printf("key rotation complete (activeKeyID=%s contextEntries=%d cloudIntegrations=%d projectTokens=%d states=%d runArtifacts=%d)",
			sealer.ActiveKeyID(), sum.ContextEntries, sum.CloudIntegrations, sum.ProjectTokens, sum.States, sum.RunPlanArtifacts)

	default:
		log.Fatalf("unknown command %q (expected: up, down [steps|all], version, rotate-keys)", cmd)
	}
}
