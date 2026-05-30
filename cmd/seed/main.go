package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"os"

	"github.com/brunomvsouza/ynab.go"
	"github.com/deasa/YNAB_AutoCategorizer/AI"
	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/datastore"
	"github.com/deasa/YNAB_AutoCategorizer/migrate"
	"github.com/deasa/YNAB_AutoCategorizer/search"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func main() {
	logger := log.New(os.Stdout, "YNAB_Seed ", log.LstdFlags)
	ctx := context.Background()

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

	// Start from a clean slate so re-seeding does not duplicate categories.
	if err := mapper.DeleteAllEmbeddings(); err != nil {
		logger.Fatalf("error clearing existing categories: %v", err)
	}
	logger.Println("cleared existing categories from vector database")

	aiProvider, err := AI.NewFromConfig(ctx, cfg.GoogleCloudProject, cfg.GoogleCloudLocation, cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.ChatModel)
	if err != nil {
		logger.Fatalf("error creating AI provider: %v", err)
	}

	searchService, err := search.NewSearch(search.WithAI(aiProvider), search.WithMapper(mapper))
	if err != nil {
		logger.Fatalf("error creating search: %v", err)
	}

	// Fetch categories from YNAB
	client := ynab.NewClient(cfg.YNABAccessToken)
	snapshot, err := client.Category().GetCategories(cfg.YNABBudgetID, nil)
	if err != nil {
		logger.Fatalf("error fetching categories: %v", err)
	}

	seeded := 0
	for _, group := range snapshot.GroupWithCategories {
		if group.Deleted || group.Hidden {
			continue
		}
		for _, cat := range group.Categories {
			if cat.Deleted || cat.Hidden {
				continue
			}

			// Build a descriptive content string for embedding
			content := cat.Name
			if cat.Note != nil && *cat.Note != "" {
				content = fmt.Sprintf("%s: %s", cat.Name, *cat.Note)
			}

			logger.Printf("seeding category: %s (group: %s)", cat.Name, group.Name)
			if err := searchService.InsertContent(ctx, cat.Name, content); err != nil {
				logger.Printf("warning: failed to seed category %s: %v", cat.Name, err)
				continue
			}
			seeded++
		}
	}

	logger.Printf("done: seeded %d categories into vector database", seeded)
}
