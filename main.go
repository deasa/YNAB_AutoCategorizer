package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/AI"
	"github.com/deasa/YNAB_AutoCategorizer/categorizer"
	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/datastore"
	"github.com/deasa/YNAB_AutoCategorizer/migrate"
	"github.com/deasa/YNAB_AutoCategorizer/search"
	ynabclient "github.com/deasa/YNAB_AutoCategorizer/ynab"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func main() {
	ctx := context.Background()
	logger := log.New(os.Stdout, "YNAB_AutoCat ", log.LstdFlags)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("failed to load config: %v", err)
	}

	dbURL := fmt.Sprintf("%s?authToken=%s", cfg.LibSQLDatabaseURL, url.QueryEscape(cfg.LibSQLAuthToken))
	db, err := sql.Open("libsql", dbURL)
	if err != nil {
		logger.Fatalf("failed to open db: %v", err)
	}
	defer db.Close()

	if err = migrate.Run(db); err != nil {
		logger.Fatalf("failed to run migrations: %v", err)
	}

	mapper := datastore.NewMapper(db)

	aiProvider, err := AI.NewFromConfig(ctx, cfg.GoogleCloudProject, cfg.GoogleCloudLocation, cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.ChatModel)
	if err != nil {
		logger.Fatalf("failed to create AI provider: %v", err)
	}

	searchService, err := search.NewSearch(search.WithAI(aiProvider), search.WithMapper(mapper))
	if err != nil {
		logger.Fatalf("failed to create search service: %v", err)
	}

	ynab := ynabclient.NewClient(cfg.YNABAccessToken, cfg.YNABBudgetID, logger)
	cat := categorizer.New(ynab, aiProvider, searchService, logger, cfg.ConfidenceThreshold, cfg.DryRun)
	cat.SetMaxTransactions(cfg.MaxTransactions)

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
