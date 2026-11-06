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
		log.Fatalf("connect to database: %v", err)
	}

	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		log.Fatalf("create schema_migrations table: %v", err)
	}

	path := os.Args[1]
	version := filepath.Base(path)
	var alreadyApplied bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&alreadyApplied); err != nil {
		log.Fatalf("check migration status: %v", err)
	}
	if alreadyApplied {
		fmt.Printf("migration already applied: %s\n", version)
		return
	}

	sqlBytes, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read migration %s: %v", version, err)
	}
	if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
		log.Fatalf("apply migration %s: %v", version, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`, version); err != nil {
		log.Fatalf("record migration %s: %v", version, err)
	}
	fmt.Printf("migration applied: %s\n", version)
}
