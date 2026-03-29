package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
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

	url := fmt.Sprintf("%s?authToken=%s", cfg.LibSQLDatabaseURL, cfg.LibSQLAuthToken)
	db, err := sql.Open("libsql", url)
	if err != nil {
		logger.Fatalf("failed to open db: %v", err)
	}
	defer db.Close()

	if err = migrate.Run(db); err != nil {
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
	cat := categorizer.New(ynab, searchService, mapper, logger, cfg.ConfidenceThreshold, cfg.DryRun)

	run := func() {
		if err := cat.Learn(); err != nil {
			logger.Printf("learn step failed: %v", err)
		}
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

