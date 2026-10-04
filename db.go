package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrations embed.FS

// openDB connects to Postgres, applies pending migrations, and backfills merchant keys.
func openDB(databaseURL string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	cfg.Tracer = otelpgx.NewTracer() // statements only, never their parameters; a no-op unless tracing is set up
	db := stdlib.OpenDB(*cfg)
	ctx := context.Background()
	if err = db.PingContext(ctx); err == nil {
		err = migrate(db)
	}
	// Backfill merchant_key for rows that pre-date the merchants feature (needs the Go cleaner, so not in SQL).
	if err == nil {
		err = assignMerchantKeys(ctx, db)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// describeDB names the target without credentials, for logging.
func describeDB(databaseURL string) string {
	u, err := url.Parse(databaseURL)
	if err != nil || u.Host == "" {
		return "postgres"
	}
	return "postgres " + u.Host + u.Path
}

// migrate applies migrations/NNNN_name.sql files not yet recorded in schema_migrations, each in its own transaction.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INT PRIMARY KEY)`); err != nil {
		return err
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, file := range files {
		n, err := strconv.Atoi(strings.SplitN(path.Base(file), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number: %w", file, err)
		}
		body, err := migrations.ReadFile(file)
		if err != nil {
			return err
		}
		if err := applyMigration(db, n, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", file, err)
		}
	}
	return nil
}

// applyMigration runs one file and records its version atomically. The table lock makes concurrent
// starts take turns instead of applying the same file twice.
func applyMigration(db *sql.DB, version int, body string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`LOCK TABLE schema_migrations IN EXCLUSIVE MODE`); err != nil {
		return err
	}
	var done bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&done); err != nil || done {
		return err
	}
	if _, err := tx.Exec(body); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations(version) VALUES($1)`, version); err != nil {
		return err
	}
	return tx.Commit()
}
