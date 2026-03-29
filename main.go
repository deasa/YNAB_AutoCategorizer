package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/AI"
	"github.com/deasa/YNAB_AutoCategorizer/categorizer"
	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/datastore"
	"github.com/deasa/YNAB_AutoCategorizer/search"
	ynabclient "github.com/deasa/YNAB_AutoCategorizer/ynab"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func main() {
	ctx := context.Background()
	logger := log.New(os.Stdout, "YNAB_AutoCat ", log.LstdFlags)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("failed to load config: %v", err)
	}

	url := fmt.Sprintf("%s?authToken=%s", cfg.LibSQLDatabaseURL, cfg.LibSQLAuthToken)
	db, err := sql.Open("libsql", url)
	if err != nil {
		logger.Fatalf("failed to open db: %v", err)
	}
	defer db.Close()

	if err = runMigrations(db); err != nil {
		logger.Fatalf("failed to run migrations: %v", err)
	}

	mapper := datastore.NewMapper(db)

	var aiProvider AI.AI
	if cfg.GoogleCloudProject != "" {
		aiProvider, err = AI.NewVertexAI(ctx, AI.WithProjectID(cfg.GoogleCloudProject), AI.WithLocation(cfg.GoogleCloudLocation))
	} else {
		aiProvider, err = AI.NewAI(AI.WithAPIKey(cfg.OpenAIAPIKey), AI.WithBaseURL(cfg.OpenAIBaseURL))
	}
	if err != nil {
		logger.Fatalf("failed to create AI provider: %v", err)
	}

	searchService, err := search.NewSearch(search.WithAI(aiProvider), search.WithMapper(mapper))
	if err != nil {
		logger.Fatalf("failed to create search service: %v", err)
	}

	ynab := ynabclient.NewClient(cfg.YNABAccessToken, cfg.YNABBudgetID, logger)
	cat := categorizer.New(ynab, searchService, logger, cfg.ConfidenceThreshold, cfg.DryRun)

	run := func() {
		if err := cat.Run(); err != nil {
			logger.Printf("categorization run failed: %v", err)
		}
	}

	run()

	ticker := time.NewTicker(cfg.RunInterval)
	defer ticker.Stop()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-ticker.C:
			run()
		case <-stop:
			logger.Println("shutting down")
			return
		}
	}
}

// runMigrations executes all .sql files in the migrations directory in order.
// Each migration is tracked in a schema_migrations table and only runs once.
func runMigrations(db *sql.DB) error {
	// Create the tracking table if it doesn't exist.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		filename TEXT PRIMARY KEY,
		applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("creating schema_migrations table: %w", err)
	}

	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("reading migrations dir: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		// Skip already-applied migrations.
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE filename = ?`, entry.Name()).Scan(&count); err != nil {
			return fmt.Errorf("checking migration %s: %w", entry.Name(), err)
		}
		if count > 0 {
			continue
		}

		data, err := fs.ReadFile(migrationFiles, "migrations/"+entry.Name())
		if err != nil {
			return fmt.Errorf("reading %s: %w", entry.Name(), err)
		}
		for _, stmt := range strings.Split(string(data), ";") {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if _, err = db.Exec(stmt); err != nil {
				return fmt.Errorf("executing %s: %w", entry.Name(), err)
			}
		}

		// Record that this migration has been applied.
		if _, err := db.Exec(`INSERT INTO schema_migrations (filename) VALUES (?)`, entry.Name()); err != nil {
			return fmt.Errorf("recording migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}
