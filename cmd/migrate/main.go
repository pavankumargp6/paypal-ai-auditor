package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/migrate <migration.sql>")
		os.Exit(2)
	}
	_ = godotenv.Load()
	dbURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required in .env or the process environment")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("connect to database: %s", redact(err, dbURL))
	}

	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		log.Fatalf("create schema_migrations table: %s", redact(err, dbURL))
	}

	path := os.Args[1]
	version := filepath.Base(path)
	var alreadyApplied bool
	sqlBytes, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read migration %s: %v", version, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		log.Fatalf("begin migration transaction: %s", redact(err, dbURL))
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&alreadyApplied); err != nil {
		log.Fatalf("check migration status: %s", redact(err, dbURL))
	}
	if alreadyApplied {
		fmt.Printf("migration already applied: %s\n", version)
		return
	}

	rowCountBefore, totalBefore, err := captureTotals(ctx, tx, false)
	if err != nil {
		log.Fatalf("read pre-migration totals: %s", redact(err, dbURL))
	}
	if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
		log.Fatalf("apply migration %s: %s", version, redact(err, dbURL))
	}
	rowCountAfter, totalAfter, err := captureTotals(ctx, tx, true)
	if err != nil {
		log.Fatalf("read post-migration totals: %s", redact(err, dbURL))
	}
	if rowCountBefore != rowCountAfter || totalBefore != totalAfter {
		log.Fatalf("migration totals changed unexpectedly: rows %d -> %d, minor units %s -> %s", rowCountBefore, rowCountAfter, totalBefore, totalAfter)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
		log.Fatalf("record migration %s: %s", version, redact(err, dbURL))
	}
	if err := tx.Commit(); err != nil {
		log.Fatalf("commit migration %s: %s", version, redact(err, dbURL))
	}
	fmt.Printf("rows before: %d\ntotal minor units before: %s\nrows after: %d\ntotal minor units after: %s\n", rowCountBefore, totalBefore, rowCountAfter, totalAfter)
	fmt.Printf("migration applied: %s\n", version)
}

func captureTotals(ctx context.Context, tx *sql.Tx, migrated bool) (int64, string, error) {
	totalExpression := `COALESCE(SUM(ROUND(amount::numeric * CASE
		WHEN UPPER(currency) IN ('JPY', 'HUF', 'TWD') THEN 1::numeric
		ELSE 100::numeric
	END)::BIGINT), 0)::text`
	if migrated {
		totalExpression = `COALESCE(SUM(amount_minor), 0)::text`
	}
	query := `SELECT COUNT(*), ` + totalExpression + ` FROM captures`
	var rows int64
	var total string
	err := tx.QueryRowContext(ctx, query).Scan(&rows, &total)
	return rows, total, err
}

func redact(err error, dbURL string) string {
	return strings.ReplaceAll(err.Error(), dbURL, "[redacted DATABASE_URL]")
}
