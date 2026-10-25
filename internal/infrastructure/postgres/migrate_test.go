package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func TestParseLegacyVersionNumber(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"001_init", 1},
		{"013_audit_logs", 13},
		{"014_milestone3_drift_finops_cloud_ttl", 14},
		{"42", 42},
		{"invalid", 0},
		{"", 0},
	}
	for _, tc := range cases {
		if got := parseLegacyVersionNumber(tc.in); got != tc.want {
			t.Errorf("parseLegacyVersionNumber(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestEmbeddedMigrationsIOFSSource(t *testing.T) {
	d, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	defer func() { _ = d.Close() }()

	first, err := d.First()
	if err != nil {
		t.Fatalf("d.First: %v", err)
	}
	if first != 1 {
		t.Fatalf("expected first version 1, got %d", first)
	}

	cur := first
	count := 0
	for {
		count++
		upReader, _, err := d.ReadUp(cur)
		if err != nil {
			t.Fatalf("ReadUp(%d): %v", cur, err)
		}
		_ = upReader.Close()

		downReader, _, err := d.ReadDown(cur)
		if err != nil {
			t.Fatalf("ReadDown(%d): %v", cur, err)
		}
		_ = downReader.Close()

		next, err := d.Next(cur)
		if err != nil {
			break
		}
		cur = next
	}
	if count != 14 || cur != 14 {
		t.Fatalf("expected 14 migrations ending at 14, got count=%d last=%d", count, cur)
	}
}

func TestMigrateAndLegacyUpgrade(t *testing.T) {
	ctx := context.Background()
	basePool := openTestPool(t, ctx)

	schemaName := fmt.Sprintf("migtest_%d", time.Now().UnixNano())
	if _, err := basePool.Exec(ctx, "CREATE SCHEMA "+schemaName); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = basePool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaName+" CASCADE")
	})

	cfg := basePool.Config()
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schemaName

	// 1. Fresh migration up, version check, down 1 step, up again
	if err := Migrate(ctx, basePool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	v, dirty, err := MigrateVersion(ctx, basePool)
	if err != nil {
		t.Fatalf("MigrateVersion: %v", err)
	}
	if v != 14 || dirty {
		t.Fatalf("expected version=14 dirty=false, got version=%d dirty=%v", v, dirty)
	}
}
