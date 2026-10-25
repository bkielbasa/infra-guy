package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// NewMigrator constructs a golang-migrate instance backed by the embedded SQL migrations
// and the provided pgxpool.Pool. Callers must invoke the returned cleanup function when done.
func NewMigrator(ctx context.Context, pool *pgxpool.Pool) (*migrate.Migrate, func(), error) {
	if err := upgradeLegacySchemaMigrations(ctx, pool); err != nil {
		return nil, nil, fmt.Errorf("upgrade legacy schema_migrations: %w", err)
	}
	srcDriver, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		return nil, nil, fmt.Errorf("iofs migration source: %w", err)
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	dbDriver, err := migratepg.WithInstance(sqlDB, &migratepg.Config{})
	if err != nil {
		_ = srcDriver.Close()
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("postgres migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", srcDriver, "postgres", dbDriver)
	if err != nil {
		_ = srcDriver.Close()
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("init migrator: %w", err)
	}
	cleanup := func() {
		_, _ = m.Close()
		_ = sqlDB.Close()
	}
	return m, cleanup, nil
}

// Migrate applies all pending up migrations using golang-migrate.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	m, cleanup, err := NewMigrator(ctx, pool)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// MigrateDown rolls back steps migrations (or all migrations if steps <= 0).
func MigrateDown(ctx context.Context, pool *pgxpool.Pool, steps int) error {
	m, cleanup, err := NewMigrator(ctx, pool)
	if err != nil {
		return err
	}
	defer cleanup()
	var migErr error
	if steps <= 0 {
		migErr = m.Down()
	} else {
		migErr = m.Steps(-steps)
	}
	if migErr != nil && !errors.Is(migErr, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", migErr)
	}
	return nil
}

// MigrateVersion returns the currently applied migration version and dirty state.
func MigrateVersion(ctx context.Context, pool *pgxpool.Pool) (uint, bool, error) {
	m, cleanup, err := NewMigrator(ctx, pool)
	if err != nil {
		return 0, false, err
	}
	defer cleanup()
	v, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	return v, dirty, err
}

// upgradeLegacySchemaMigrations converts the pre-golang-migrate schema_migrations table
// (version text PRIMARY KEY, applied_at timestamptz) to golang-migrate's format
// (version bigint not null primary key, dirty boolean not null), preserving the highest
// applied migration number.
func upgradeLegacySchemaMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	var dataType string
	err := pool.QueryRow(ctx, `
		SELECT data_type
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'schema_migrations'
		  AND column_name = 'version'
	`).Scan(&dataType)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if dataType != "text" && dataType != "character varying" {
		return nil
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	var maxVersion int64
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		if v := parseLegacyVersionNumber(raw); v > maxVersion {
			maxVersion = v
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if _, err := tx.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE schema_migrations (
			version bigint NOT NULL PRIMARY KEY,
			dirty boolean NOT NULL
		)
	`); err != nil {
		return err
	}
	if maxVersion > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, dirty) VALUES ($1, false)`, maxVersion); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func parseLegacyVersionNumber(version string) int64 {
	prefix := strings.TrimSpace(version)
	if idx := strings.IndexByte(prefix, '_'); idx > 0 {
		prefix = prefix[:idx]
	}
	n, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func utcTime(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

func utcTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
